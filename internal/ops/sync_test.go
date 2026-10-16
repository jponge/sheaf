package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
)

// syncRecord writes repository entries and sources straight into sheaf.yaml
// (no clone): Sync only reads the metadata.
func syncRecord(t *testing.T, f *fixture, sources []bundle.Source, repos ...bundle.Repository) {
	t.Helper()
	err := f.w.update(func(b *bundle.Bundle) error {
		b.Sources = append(b.Sources, sources...)
		for _, r := range repos {
			if r.URL == "" {
				r.URL = "file:///nowhere/" + r.Name + ".git"
			}
			if r.DefaultBranch == "" {
				r.DefaultBranch = "main"
			}
			if err := b.AddRepository(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func listed(f *fixture, owner string, names ...string) {
	f.fake.AddOwner(owner)
	for _, n := range names {
		f.fake.AddRepo(hosting.Repo{Owner: owner, Name: n, CloneURL: "file:///nowhere/" + n + ".git", DefaultBranch: "main"})
	}
}

func syncNow(t *testing.T, f *fixture, h hosting.Hosting, opts SyncOptions) SyncReport {
	t.Helper()
	r, err := Sync(context.Background(), f.w, h, opts)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return r
}

// proposals renders the proposals as "kind org/name" (Name, not Slug).
func proposals(r SyncReport) []string {
	var out []string
	for _, p := range r.Proposals {
		out = append(out, fmt.Sprintf("%s %s/%s", p.Kind, p.Org, p.Name))
	}
	return out
}

func wantProposals(t *testing.T, r SyncReport, want ...string) {
	t.Helper()
	if got := proposals(r); !slices.Equal(got, want) {
		t.Fatalf("proposals =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestSyncProposesNewReposOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "x", Slug: "O/X", Origin: "explicit"}, // in the org, added explicitly
	)
	listed(f, "o", "a", "x", "c")
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "b", CloneURL: "file:///b.git", DefaultBranch: "main", Fork: true, Description: "a fork"})
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/b", "new o/c")
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	p := r.Proposals[0]
	if p.Key != "new:o/b" || p.Slug != "o/b" || !p.Repo.Fork || p.Repo.CloneURL != "file:///b.git" {
		t.Fatalf("proposal = %+v", p)
	}
	if len(r.Orgs) != 1 || r.Orgs[0].Org != "o" || r.Orgs[0].Listed != 4 || r.Orgs[0].Truncated || r.Orgs[0].Err != nil {
		t.Fatalf("orgs = %+v", r.Orgs)
	}
}

func TestSyncHonoursExcludeCaseInsensitively(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o", Exclude: []string{"Legacy"}}})
	listed(f, "o", "legacy", "fresh")
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{}), "new o/fresh")
}

func TestSyncArchivedHandling(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "kept", Slug: "o/kept", Origin: "org:o"},
		bundle.Repository{Name: "mine", Slug: "o/mine", Origin: "explicit"},
	)
	listed(f, "o")
	for _, n := range []string{"kept", "mine", "old"} {
		f.fake.AddRepo(hosting.Repo{Owner: "o", Name: n, CloneURL: "file:///x.git", DefaultBranch: "main", Archived: true})
	}
	// Archived repositories of the bundle are reported (never gone), and
	// archived ones outside it are not proposed by default.
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "archived o/kept", "archived o/mine")
	for _, p := range r.Proposals {
		if p.Key != "archived:o/"+p.Name || !p.Repo.Archived {
			t.Fatalf("proposal = %+v", p)
		}
	}
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{IncludeArchived: true}), "new o/old", "archived o/kept", "archived o/mine")
}

func TestSyncProposesGoneOrgRepos(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "deleted", Slug: "o/deleted", Origin: "org:O"}, // origin compared case-insensitively
		bundle.Repository{Name: "old-name", Slug: "o/old-name", Origin: "org:o"},
		// Not candidates for gone: explicit, adopted, other host, other org.
		bundle.Repository{Name: "explicit", Slug: "o/explicit", Origin: "explicit"},
		bundle.Repository{Name: "adopted", Slug: "o/adopted", Origin: "adopted"},
		bundle.Repository{Name: "lab", Slug: "o/lab", Origin: "org:o", Host: "gitlab.com"},
		bundle.Repository{Name: "local", Slug: "o/local", Origin: "org:o", Host: "local"},
		bundle.Repository{Name: "p1", Slug: "p/p1", Origin: "org:p"},
	)
	listed(f, "o", "a", "new-name") // old-name was renamed to new-name
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/new-name", "gone o/deleted", "gone o/old-name")
	g := r.Proposals[1]
	if g.Key != "gone:o/deleted" || g.Slug != "o/deleted" || g.Repo != (hosting.Repo{}) {
		t.Fatalf("gone proposal = %+v", g)
	}
	if !strings.Contains(g.Note, "renamed") {
		t.Fatalf("gone note %q does not mention a possible rename", g.Note)
	}
}

// truncating lists like the gh adapter does past its limit: the repositories
// it got together with hosting.ErrPossiblyTruncated.
type truncating struct{ *hosting.Fake }

func (h truncating) ListRepos(ctx context.Context, owner string) ([]hosting.Repo, error) {
	rs, err := h.Fake.ListRepos(ctx, owner)
	if err != nil {
		return nil, err
	}
	return rs, fmt.Errorf("%w: %q has more than %d repositories", hosting.ErrPossiblyTruncated, owner, len(rs))
}

func TestSyncTruncatedListingNeverProposesGone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "beyond-limit", Slug: "o/beyond-limit", Origin: "org:o"})
	listed(f, "o", "fresh")
	r := syncNow(t, f, truncating{f.fake}, SyncOptions{})
	wantProposals(t, r, "new o/fresh")
	if len(r.Orgs) != 1 || !r.Orgs[0].Truncated || r.Orgs[0].Err != nil {
		t.Fatalf("orgs = %+v, want truncated without error", r.Orgs)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v (a truncated listing is a warning)", err)
	}
}

func TestSyncListingErrorIsPerOrg(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "broken"}, {Org: "o"}},
		bundle.Repository{Name: "b1", Slug: "broken/b1", Origin: "org:broken"})
	listed(f, "o", "fresh")
	f.fake.ListErr["broken"] = errors.New("boom")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/fresh")
	if len(r.Orgs) != 2 || r.Orgs[0].Org != "broken" || r.Orgs[0].Err == nil || r.Orgs[1].Err != nil {
		t.Fatalf("orgs = %+v", r.Orgs)
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Err = %v, want one naming broken", err)
	}
}

func TestSyncOrgSelection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}, {Org: "p"}})
	listed(f, "o", "o1")
	listed(f, "p", "p1")
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{}), "new o/o1", "new p/p1")
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{Orgs: []string{"P"}}), "new p/p1")
	if _, err := Sync(context.Background(), f.w, f.fake, SyncOptions{Orgs: []string{"q"}}); err == nil || !strings.Contains(err.Error(), "q") {
		t.Fatalf("unknown org: err = %v", err)
	}
}

func TestSyncWithoutSourcesFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := Sync(context.Background(), f.w, f.fake, SyncOptions{}); err == nil {
		t.Fatal("Sync without sources succeeded")
	}
}

func TestSyncNotesDirectoryNameClash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "p"}},
		bundle.Repository{Name: "common", Slug: "o/common", Origin: "explicit"})
	listed(f, "p", "common")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new p/common")
	if !strings.Contains(r.Proposals[0].Note, "--as") {
		t.Fatalf("note = %q, want a hint about --as", r.Proposals[0].Note)
	}
}

func TestSyncAddNewClonesWithOrgOrigin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	if s, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v %v", s, err)
	}
	f.repo(t, "o", "b", "dev/x", false)
	f.repo(t, "o", "c", "main", false)
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/b", "new o/c")
	s := SyncAddNewWithOptions(context.Background(), f.w, r.Proposals[:1], CloneOptions{}, 2, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"b": StatusDone}) {
		t.Fatalf("statuses = %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.w.Root, "b", "dev-x", ".git")); err != nil {
		t.Fatal(err)
	}
	e, ok := f.onDisk(t).Repository("b")
	if !ok || e.Origin != "org:o" || e.Slug != "o/b" || e.DefaultBranch != "dev/x" {
		t.Fatalf("entry = %+v", e)
	}
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{}), "new o/c")
}

func TestSyncAddNewIgnoresOtherKinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := SyncAddNewWithOptions(context.Background(), f.w, []Proposal{{Key: "gone:o/a", Kind: ProposalGone, Org: "o", Name: "a", Slug: "o/a"}}, CloneOptions{}, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"a": StatusFailed}) {
		t.Fatalf("statuses = %v, want a failed (not a new proposal)", got)
	}
}

func TestSyncExcludeStopsProposals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}})
	listed(f, "O", "noise", "keep")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/keep", "new o/noise")
	if err := SyncExclude(context.Background(), f.w, r.Proposals[1:]); err != nil { // noise
		t.Fatal(err)
	}
	if got := f.onDisk(t).Sources; len(got) != 1 || !slices.Equal(got[0].Exclude, []string{"noise"}) {
		t.Fatalf("sources = %+v", got)
	}
	wantProposals(t, syncNow(t, f, f.fake, SyncOptions{}), "new o/keep")
	gone := Proposal{Key: "gone:o/a", Kind: ProposalGone, Org: "o", Name: "a", Slug: "o/a"}
	if err := SyncExclude(context.Background(), f.w, []Proposal{gone}); err == nil {
		t.Fatal("excluding a gone proposal succeeded")
	}
}

func TestSyncRemoveGoneKeepsSafetyOfRemove(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, n := range []string{"clean", "risky", "stays"} {
		f.repo(t, "o", n, "main", false)
	}
	if s, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v %v", s, err)
	}
	writeFile(t, filepath.Join(f.w.Root, "risky", "main", "precious.txt"), "unsaved work")
	// clean and risky are deleted from the organization, remotes included
	// (a remote that still answered would contradict the hosting service).
	for _, n := range []string{"clean", "risky"} {
		u := f.mustRepo(t, "o", n).CloneURL
		if err := os.RemoveAll(strings.TrimPrefix(u, "file://")); err != nil {
			t.Fatal(err)
		}
	}
	stays := f.mustRepo(t, "o", "stays")
	f.fake = hosting.NewFake()
	f.fake.AddRepo(stays)
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "gone o/clean", "gone o/risky")

	// 2 of 3 repositories of o are gone: a mass removal, allowed here. A
	// gone remote is itself a data risk (the clone may be the only copy),
	// so even the clean one needs Force; risky also lists its local data.
	opts := SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir(), Confirm: func(RemovePlan) bool { return true }}, AllowMassRemoval: true}
	s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 2, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"clean": StatusFailed, "risky": StatusFailed}) {
		t.Fatalf("statuses = %v", got)
	}
	for i, want := range []string{"appears to be gone", "untracked"} {
		var re *RisksError
		if !errors.As(s.Results[i].Err, &re) || !strings.Contains(re.Error(), want) {
			t.Fatalf("%s: err = %v, want *RisksError mentioning %q", s.Results[i].Repo, s.Results[i].Err, want)
		}
	}
	if got := repoNames(f.onDisk(t)); !slices.Equal(got, []string{"clean", "risky", "stays"}) {
		t.Fatalf("recorded = %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.w.Root, "risky", "main", "precious.txt")); err != nil {
		t.Fatal(err)
	}

	// Without confirmation nothing is removed; with Force both go.
	r = syncNow(t, f, f.fake, SyncOptions{})
	s = SyncRemoveGone(context.Background(), f.w, r.Proposals, SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: opts.Remove.WorkingDir, Force: true}, AllowMassRemoval: true}, 1, Discard)
	if s.Count(StatusFailed) != 2 || !errors.Is(s.Results[0].Err, ErrNotConfirmed) || !errors.Is(s.Results[1].Err, ErrNotConfirmed) {
		t.Fatalf("unconfirmed: %+v", s)
	}
	opts.Remove.Force = true
	s = SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 1, Discard)
	if s.Err() != nil {
		t.Fatalf("forced: %+v", s)
	}
	if got := repoNames(f.onDisk(t)); !slices.Equal(got, []string{"stays"}) {
		t.Fatalf("recorded = %v", got)
	}
}

func TestSyncRemoveGoneIgnoresOtherKinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}}, bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"})
	opts := SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir(), Confirm: func(RemovePlan) bool { return true }, Force: true}}
	s := SyncRemoveGone(context.Background(), f.w, []Proposal{{Key: "new:o/a", Kind: ProposalNew, Org: "o", Name: "a", Slug: "o/a"}}, opts, 1, Discard)
	if s.Count(StatusFailed) != 1 {
		t.Fatalf("summary = %+v, want a failure", s)
	}
	if got := repoNames(f.onDisk(t)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("recorded = %v", got)
	}
}
