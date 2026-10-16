package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
)

// tweaked hides repositories from listings (an incomplete listing, e.g. a
// token without access to private repositories) and redirects lookups of
// moved repositories to their new canonical identity, like GitHub does.
type tweaked struct {
	*hostingtest.Fake
	hidden map[string]bool         // lower-case slugs missing from ListRepos
	moved  map[string]hosting.Repo // lower-case old slug -> current repo
}

func (h tweaked) ListRepos(ctx context.Context, owner string) ([]hosting.Repo, error) {
	rs, err := h.Fake.ListRepos(ctx, owner)
	return slices.DeleteFunc(rs, func(r hosting.Repo) bool { return h.hidden[strings.ToLower(r.Slug())] }), err
}

func (h tweaked) Repo(ctx context.Context, owner, name string) (hosting.Repo, error) {
	if r, ok := h.moved[strings.ToLower(owner+"/"+name)]; ok {
		return r, nil
	}
	return h.Fake.Repo(ctx, owner, name)
}

func TestSyncAliasHostEntryIsNotNew(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a-work", Slug: "o/a", Origin: "adopted", Host: "github_work"})
	listed(f, "o", "a")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r)
	if n := r.Orgs[0].Notes; len(n) != 1 || !strings.Contains(n[0], "already in the bundle as a-work (host github_work)") {
		t.Fatalf("notes = %q", n)
	}
}

func TestAddOrgSkipsSlugRecordedUnderAliasHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, nil, bundle.Repository{Name: "a-work", Slug: "o/a", Origin: "adopted", Host: "github_work"})
	f.repo(t, "o", "a", "main", false)
	s, err := f.add(t, AddRequest{Org: "o", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(s); !mapsEqual(got, map[string]Status{"a-work": StatusSkipped}) {
		t.Fatalf("statuses = %v", got)
	}
	if !strings.Contains(s.Results[0].Message, "host github_work") || exists(f.w.Root+"/a") {
		t.Fatalf("result = %+v", s.Results[0])
	}
}

func TestSyncGoneIsConfirmedByLookup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "hidden", Slug: "o/hidden", Origin: "org:o"},
		bundle.Repository{Name: "deleted", Slug: "o/deleted", Origin: "org:o"},
		bundle.Repository{Name: "d", Slug: "o/d", Origin: "org:o"},
	)
	listed(f, "o", "a", "hidden", "d")
	h := tweaked{Fake: f.fake, hidden: map[string]bool{"o/hidden": true}}
	r := syncNow(t, f, h, SyncOptions{})
	wantProposals(t, r, "gone o/deleted")
	if r.Proposals[0].MassRemoval {
		t.Fatal("1 of 4 gone flagged as mass removal")
	}
	w := r.Orgs[0].Warnings
	if len(w) != 1 || !strings.Contains(w[0], "listing for org o looks incomplete (missing visible repo o/hidden)") {
		t.Fatalf("warnings = %q", w)
	}
}

func TestSyncMovedRepoBetweenTrackedOrgs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}, {Org: "p"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "b", Slug: "o/b", Origin: "org:o"},
		bundle.Repository{Name: "z", Slug: "p/z", Origin: "org:p"},
	)
	listed(f, "o", "b")
	listed(f, "p", "z", "a")
	moved := f.mustRepo(t, "p", "a")
	r := syncNow(t, f, tweaked{Fake: f.fake, moved: map[string]hosting.Repo{"o/a": moved}}, SyncOptions{})
	wantProposals(t, r, "moved o/a")
	p := r.Proposals[0]
	if p.Key != "moved:o/a" || p.Slug != "o/a" || p.Repo.Slug() != "p/a" || !strings.Contains(p.Note, "o/a → p/a") {
		t.Fatalf("proposal = %+v", p)
	}
	s := SyncRemoveGone(context.Background(), f.w, r.Proposals, SyncRemoveOptions{Remove: RemoveOptions{Force: true, Confirm: yes}}, 1, Discard)
	if s.Count(StatusDone) != 0 || !slices.Equal(repoNames(f.onDisk(t)), []string{"a", "b", "z"}) {
		t.Fatalf("moved repository removed: %+v", s)
	}
}

func TestSyncMassGoneNeedsExplicitPermission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "b", Slug: "o/b", Origin: "org:o"},
		bundle.Repository{Name: "c", Slug: "o/c", Origin: "org:o"},
	)
	listed(f, "o") // empty but successful listing
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "gone o/a", "gone o/b", "gone o/c")
	if !r.Orgs[0].MassRemoval {
		t.Fatalf("orgs = %+v", r.Orgs)
	}
	for _, p := range r.Proposals {
		if !p.MassRemoval {
			t.Fatalf("proposal %+v not flagged", p)
		}
	}
	opts := SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir(), Confirm: yes}}
	s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 2, Discard)
	if s.Count(StatusFailed) != 3 || !strings.Contains(s.Results[0].Err.Error(), "--allow-mass-removal") {
		t.Fatalf("summary = %+v", s)
	}
	if got := repoNames(f.onDisk(t)); len(got) != 3 {
		t.Fatalf("removed without permission: %v", got)
	}
	opts.AllowMassRemoval = true
	if s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 2, Discard); s.Err() != nil {
		t.Fatalf("allowed: %+v", s)
	}
	if got := repoNames(f.onDisk(t)); len(got) != 0 {
		t.Fatalf("recorded = %v", got)
	}
}

func TestSyncAddNewSkipsEmptyRepositories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}})
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "empty", CloneURL: "file:///nowhere.git"})
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/empty")
	s := SyncAddNewWithOptions(context.Background(), f.w, r.Proposals, CloneOptions{}, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"empty": StatusSkipped}) || !strings.Contains(s.Results[0].Message, "empty") {
		t.Fatalf("summary = %+v", s)
	}
}

func TestSyncRemoveGoneSkipsEntriesChangedSinceSync(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "b", Slug: "o/b", Origin: "org:o"},
		bundle.Repository{Name: "k", Slug: "o/k", Origin: "org:o"},
		bundle.Repository{Name: "l", Slug: "o/l", Origin: "org:o"},
	)
	listed(f, "o", "k", "l")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "gone o/a", "gone o/b")
	// Meanwhile a is replaced by another repository with the same
	// directory name, and b becomes an explicit entry.
	err := f.w.update(func(b *bundle.Bundle) error {
		if err := b.RemoveRepository("a"); err != nil {
			return err
		}
		r, _ := b.Repository("b")
		r.Origin = bundle.OriginExplicit
		return b.AddRepository(bundle.Repository{Name: "a", Slug: "x/a", URL: "file:///x.git", DefaultBranch: "main", Origin: "explicit"})
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir(), Confirm: yes, Force: true}}
	s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"a": StatusSkipped, "b": StatusSkipped}) {
		t.Fatalf("summary = %+v", s)
	}
	for _, res := range s.Results {
		if !strings.Contains(res.Message, "changed since sync") {
			t.Fatalf("result = %+v", res)
		}
	}
	if got := repoNames(f.onDisk(t)); len(got) != 4 {
		t.Fatalf("recorded = %v", got)
	}
}

// failingLookup lists normally but every lookup fails (e.g. rate limit).
type failingLookup struct{ *hostingtest.Fake }

func (h failingLookup) Repo(context.Context, string, string) (hosting.Repo, error) {
	return hosting.Repo{}, errors.New("HTTP 403: API rate limit exceeded")
}

// A gone candidate whose remote still answers contradicts the hosting
// service (token scope): refused, even with AllowMassRemoval, unless Force.
func TestSyncRemoveGoneRefusesWhenRemoteStillAnswers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, n := range []string{"p1", "p2", "p3", "p4", "p5"} {
		f.repo(t, "p", n, "main", false)
	}
	if s, err := f.add(t, AddRequest{Org: "p", All: true}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v %v", s, err)
	}
	// The token no longer sees p4 and p5 (partial scope): 2 of 5, no mass flag.
	visible := hostingtest.NewFake()
	for _, n := range []string{"p1", "p2", "p3"} {
		visible.AddRepo(f.mustRepo(t, "p", n))
	}
	r := syncNow(t, f, visible, SyncOptions{})
	wantProposals(t, r, "gone p/p4", "gone p/p5")
	if r.Orgs[0].MassRemoval {
		t.Fatal("2 of 5 flagged as mass removal")
	}
	opts := SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir(), Confirm: yes}, AllowMassRemoval: true}
	s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 2, Discard)
	if s.Count(StatusFailed) != 2 {
		t.Fatalf("summary = %+v", s)
	}
	for _, res := range s.Results {
		want := "the GitHub API cannot see p/" + res.Repo + " but its remote still answers: check gh authentication scope (not removed)"
		if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", res.Repo, res.Err, want)
		}
	}
	if got := repoNames(f.onDisk(t)); len(got) != 5 {
		t.Fatalf("recorded = %v", got)
	}
	opts.Remove.Force = true
	if s := SyncRemoveGone(context.Background(), f.w, r.Proposals, opts, 2, Discard); s.Err() != nil {
		t.Fatalf("forced: %+v", s)
	}
	if got := repoNames(f.onDisk(t)); !slices.Equal(got, []string{"p1", "p2", "p3"}) {
		t.Fatalf("recorded = %v", got)
	}
}

func TestSyncEmptyListingFlagsMassRemovalForSingleRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}}, bundle.Repository{Name: "only", Slug: "o/only", Origin: "org:o"})
	listed(f, "o")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "gone o/only")
	if !r.Orgs[0].MassRemoval || !r.Proposals[0].MassRemoval {
		t.Fatalf("report = %+v", r)
	}
}

func TestAddNotBlockedByMirrorOnRealOtherHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, nil, bundle.Repository{Name: "a-lab", Slug: "o/a", Origin: "explicit", Host: "gitlab.com"})
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "main", false)
	syncRecord(t, f, nil, bundle.Repository{Name: "b-lab", Slug: "o/b", Origin: "explicit", Host: "gitlab.com"})
	s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}})
	if err != nil || !mapsEqual(statuses(s), map[string]Status{"a": StatusDone}) {
		t.Fatalf("explicit add: %+v %v", s, err)
	}
	s, err = f.add(t, AddRequest{Org: "o", All: true})
	if err != nil || !mapsEqual(statuses(s), map[string]Status{"a": StatusSkipped, "b": StatusDone}) {
		t.Fatalf("add --org: %+v %v", s, err)
	}
	if !exists(filepath.Join(f.w.Root, "b", "main", ".git")) {
		t.Fatal("b not cloned")
	}
}

func TestSyncMirrorOnRealOtherHostDoesNotSuppressNew(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a-lab", Slug: "o/a", Origin: "explicit", Host: "gitlab.com"},
		bundle.Repository{Name: "b-work", Slug: "o/b", Origin: "adopted", Host: "GitHub-Work.example"})
	listed(f, "o", "a", "b")
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/a")
	if n := r.Orgs[0].Notes; len(n) != 1 || !strings.Contains(n[0], "b-work") {
		t.Fatalf("notes = %q", n)
	}
}

func TestSyncSingleOrgFlagsPossiblyMovedRepos(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "keep", "main", false)
	if s, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v %v", s, err)
	}
	a := f.mustRepo(t, "o", "a")
	syncRecord(t, f, []bundle.Source{{Org: "p"}})
	f.fake.AddRepo(hosting.Repo{Owner: "p", Name: "a", CloneURL: "file:///elsewhere.git", DefaultBranch: "main"}) // same name
	f.fake.AddRepo(hosting.Repo{Owner: "p", Name: "renamed", CloneURL: a.CloneURL, DefaultBranch: "main"})        // same URL
	f.fake.AddRepo(hosting.Repo{Owner: "p", Name: "fresh", CloneURL: "file:///fresh.git", DefaultBranch: "main"}) // unrelated
	r := syncNow(t, f, f.fake, SyncOptions{Orgs: []string{"p"}})
	wantProposals(t, r, "new p/a", "new p/fresh", "new p/renamed")
	for _, i := range []int{0, 2} {
		p := r.Proposals[i]
		if p.PossiblyMoved != "o/a" || !strings.Contains(p.Note, "possibly moved from o/a; run sync for that org too") {
			t.Fatalf("proposal = %+v", p)
		}
	}
	if r.Proposals[1].PossiblyMoved != "" {
		t.Fatalf("fresh = %+v", r.Proposals[1])
	}
	s := SyncAddNewWithOptions(context.Background(), f.w, []Proposal{r.Proposals[0], r.Proposals[2]}, CloneOptions{}, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"a": StatusSkipped, "renamed": StatusSkipped}) {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := os.Stat(filepath.Join(f.w.Root, "renamed")); err == nil {
		t.Fatal("renamed cloned")
	}
}

func TestSyncUnconfirmedGoneCandidatesAreAnError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}},
		bundle.Repository{Name: "a", Slug: "o/a", Origin: "org:o"},
		bundle.Repository{Name: "b", Slug: "o/b", Origin: "org:o"})
	listed(f, "o", "b")
	r := syncNow(t, f, failingLookup{f.fake}, SyncOptions{})
	wantProposals(t, r)
	if o := r.Orgs[0]; !slices.Equal(o.Unconfirmed, []string{"o/a"}) || len(o.Warnings) != 1 {
		t.Fatalf("org = %+v", o)
	}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "o/a") {
		t.Fatalf("Err = %v", err)
	}
}
