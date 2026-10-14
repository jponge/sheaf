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
)

// failingLookup lists normally but every lookup fails (e.g. rate limit).
type failingLookup struct{ *hosting.Fake }

func (h failingLookup) Repo(context.Context, string, string) (hosting.Repo, error) {
	return hosting.Repo{}, errors.New("HTTP 403: API rate limit exceeded")
}

// A gone candidate whose remote still answers contradicts the hosting
// service (token scope): refused, even with AllowMassRemoval, unless Force.
func TestSyncRemoveGoneRefusesWhenRemoteStillAnswers(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"p1", "p2", "p3", "p4", "p5"} {
		f.repo(t, "p", n, "main", false)
	}
	if s, err := f.add(t, AddRequest{Org: "p", All: true}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v %v", s, err)
	}
	// The token no longer sees p4 and p5 (partial scope): 2 of 5, no mass flag.
	visible := hosting.NewFake()
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
	s := SyncAddNew(context.Background(), f.w, []Proposal{r.Proposals[0], r.Proposals[2]}, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"a": StatusSkipped, "renamed": StatusSkipped}) {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := os.Stat(filepath.Join(f.w.Root, "renamed")); err == nil {
		t.Fatal("renamed cloned")
	}
}

func TestSyncUnconfirmedGoneCandidatesAreAnError(t *testing.T) {
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
