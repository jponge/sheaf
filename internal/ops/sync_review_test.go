package ops

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
)

// tweaked hides repositories from listings (an incomplete listing, e.g. a
// token without access to private repositories) and redirects lookups of
// moved repositories to their new canonical identity, like GitHub does.
type tweaked struct {
	*hosting.Fake
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
	f := newFixture(t)
	syncRecord(t, f, []bundle.Source{{Org: "o"}})
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "empty", CloneURL: "file:///nowhere.git"})
	r := syncNow(t, f, f.fake, SyncOptions{})
	wantProposals(t, r, "new o/empty")
	s := SyncAddNew(context.Background(), f.w, r.Proposals, 1, Discard)
	if got := statuses(s); !mapsEqual(got, map[string]Status{"empty": StatusSkipped}) || !strings.Contains(s.Results[0].Message, "empty") {
		t.Fatalf("summary = %+v", s)
	}
}

func TestSyncRemoveGoneSkipsEntriesChangedSinceSync(t *testing.T) {
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
