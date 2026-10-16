package ops

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

const forkUser = "me"

// forkedRepo registers owner/name in the fake, backed by a fresh forked
// fixture, and the user's fork me/<forkName> of it (forkName may differ from
// name: forks can be renamed).
func (f *fixture) forkedRepo(t *testing.T, owner, name, forkName string) testutil.ForkedOrigin {
	t.Helper()
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	f.fake.User = forkUser
	f.fake.AddRepo(hosting.Repo{Owner: owner, Name: name, CloneURL: fo.Upstream.URL, DefaultBranch: "main"})
	f.fake.AddFork(hosting.Fork{Repo: hosting.Repo{Owner: forkUser, Name: forkName, CloneURL: fo.Fork.URL, Fork: true, DefaultBranch: "main"},
		ParentOwner: owner, ParentName: name})
	return fo
}

func remoteURLOf(t *testing.T, dir, remote string) string {
	t.Helper()
	u, err := gitx.RemoteURL(context.Background(), dir, remote)
	if err != nil {
		return ""
	}
	return u
}

func wantForkLayout(t *testing.T, primary string, fo testutil.ForkedOrigin) {
	t.Helper()
	if got := remoteURLOf(t, primary, "origin"); got != fo.Fork.URL {
		t.Fatalf("origin = %q, want the fork %q", got, fo.Fork.URL)
	}
	if got := remoteURLOf(t, primary, "upstream"); got != fo.Upstream.URL {
		t.Fatalf("upstream = %q, want the reference %q", got, fo.Upstream.URL)
	}
	if up := testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "main@{upstream}"); up != "upstream/main" {
		t.Fatalf("main tracks %s, want upstream/main", up)
	}
}

func wantSingleOrigin(t *testing.T, primary, url string) {
	t.Helper()
	if got := remoteURLOf(t, primary, "origin"); got != url {
		t.Fatalf("origin = %q, want %q", got, url)
	}
	if got := remoteURLOf(t, primary, "upstream"); got != "" {
		t.Fatalf("unexpected upstream remote %q", got)
	}
}

func TestAddDetectsForkAndClonesWithTwoRemotes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := f.forkedRepo(t, "Up", "x", "x-renamed")
	rec := &recorder{}
	s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"up/X"}}, 2, rec)
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	wantForkLayout(t, filepath.Join(f.w.Root, "x", "main"), fo)
	r, _ := f.onDisk(t).Repository("x")
	want := bundle.Repository{Name: "x", Slug: "Up/x", URL: fo.Upstream.URL, DefaultBranch: "main", Origin: bundle.OriginExplicit,
		Fork: bundle.Fork{Slug: "me/x-renamed", URL: fo.Fork.URL}}
	if *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", *r, want)
	}
	if !strings.Contains(s.Results[0].Message, "your fork me/x-renamed") {
		t.Fatalf("done message does not report the fork: %q", s.Results[0].Message)
	}
	found := false
	for _, e := range rec.events {
		found = found || strings.Contains(e.Message, "using your fork me/x-renamed as origin")
	}
	if !found {
		t.Fatalf("no progress event about the fork: %+v", rec.events)
	}
	if n := f.fake.ForkCalls(); n != 1 {
		t.Fatalf("ListForks called %d times", n)
	}
}

func TestAddListsForksOnceForSeveralRepositories(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fa := f.forkedRepo(t, "up", "a", "a")
	fb := f.forkedRepo(t, "up", "b", "b")
	plain := f.repo(t, "up", "c", "main", false)
	s, err := f.add(t, AddRequest{Slugs: []string{"up/a", "up/b", "up/c"}})
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	if n := f.fake.ForkCalls(); n != 1 {
		t.Fatalf("ListForks called %d times, want 1", n)
	}
	wantForkLayout(t, filepath.Join(f.w.Root, "a", "main"), fa)
	wantForkLayout(t, filepath.Join(f.w.Root, "b", "main"), fb)
	wantSingleOrigin(t, filepath.Join(f.w.Root, "c", "main"), plain.CloneURL)
	if c, _ := f.onDisk(t).Repository("c"); c.Forked() {
		t.Fatalf("c recorded with a fork: %+v", c)
	}
}

func TestAddNoForkOptsOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := f.forkedRepo(t, "up", "x", "x")
	s, err := f.add(t, AddRequest{Slugs: []string{"up/x"}, NoFork: true})
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	if n := f.fake.ForkCalls(); n != 0 {
		t.Fatalf("ListForks called %d times with NoFork", n)
	}
	wantSingleOrigin(t, filepath.Join(f.w.Root, "x", "main"), fo.Upstream.URL)
	if r, _ := f.onDisk(t).Repository("x"); r.Forked() {
		t.Fatalf("recorded with a fork: %+v", r)
	}
}

func TestAddForkListingFailureWarnsAndContinues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*hosting.Fake)
	}{
		{"user", func(h *hosting.Fake) { h.UserErr = errors.New("gh: not logged in") }},
		{"forks", func(h *hosting.Fake) { h.ForksErr = errors.New("HTTP 502") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			fo := f.forkedRepo(t, "up", "x", "x")
			tc.set(f.fake)
			rec := &recorder{}
			s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"up/x"}}, 1, rec)
			if err != nil || s.Err() != nil {
				t.Fatalf("add must not fail: %+v, %v", s, err)
			}
			wantSingleOrigin(t, filepath.Join(f.w.Root, "x", "main"), fo.Upstream.URL)
			warned := false
			for _, e := range rec.events {
				warned = warned || strings.Contains(e.Message, "warning: cannot look up your forks")
			}
			if !warned {
				t.Fatalf("no warning: %+v", rec.events)
			}
		})
	}
}

func TestAddNoGHCallWhenNothingToClone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.forkedRepo(t, "up", "x", "x")
	if s, err := f.add(t, AddRequest{Slugs: []string{"up/x"}}); err != nil || s.Err() != nil {
		t.Fatal(err)
	}
	if s, err := f.add(t, AddRequest{Slugs: []string{"up/x", "up/missing"}}); err != nil || s.Count(StatusSkipped) != 1 {
		t.Fatalf("%+v, %v", s, err)
	}
	if n := f.fake.ForkCalls(); n != 1 {
		t.Fatalf("ListForks called %d times, want 1 (none for the second add)", n)
	}
}

func TestAddOrgDetectsForks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := f.forkedRepo(t, "smallrye", "x", "x")
	plain := f.repo(t, "smallrye", "y", "main", false)
	s, err := f.add(t, AddRequest{Org: "smallrye", All: true})
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	wantForkLayout(t, filepath.Join(f.w.Root, "x", "main"), fo)
	wantSingleOrigin(t, filepath.Join(f.w.Root, "y", "main"), plain.CloneURL)
	if x, _ := f.onDisk(t).Repository("x"); x.Fork.URL != fo.Fork.URL || x.Origin != "org:smallrye" {
		t.Fatalf("entry = %+v", x)
	}
}

func TestAddUnreachableForkFailsCleanly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	f.fake.User = forkUser
	f.fake.AddRepo(hosting.Repo{Owner: "up", Name: "x", CloneURL: fo.Upstream.URL, DefaultBranch: "main"})
	f.fake.AddFork(hosting.Fork{Repo: hosting.Repo{Owner: forkUser, Name: "x", CloneURL: "file:///nonexistent/x.git"}, ParentOwner: "up", ParentName: "x"})
	s, err := f.add(t, AddRequest{Slugs: []string{"up/x"}})
	if err != nil || s.Count(StatusFailed) != 1 {
		t.Fatalf("%+v, %v", s, err)
	}
	if !strings.Contains(s.Results[0].Err.Error(), "--no-fork") {
		t.Fatalf("error should hint at --no-fork: %v", s.Results[0].Err)
	}
	if exists(filepath.Join(f.w.Root, "x")) || len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("failed fork clone left something behind")
	}
}

func TestSyncAddNewDetectsForks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := f.forkedRepo(t, "smallrye", "x", "x")
	syncRecord(t, f, []bundle.Source{{Org: "smallrye"}})
	rep := syncNow(t, f, f.fake, SyncOptions{})
	s := SyncAddNewWithOptions(context.Background(), f.w, rep.Proposals, CloneOptions{Hosting: f.fake}, 1, Discard)
	if s.Err() != nil {
		t.Fatalf("%+v", s)
	}
	wantForkLayout(t, filepath.Join(f.w.Root, "x", "main"), fo)

	g := newFixture(t)
	go2 := g.forkedRepo(t, "smallrye", "x", "x")
	syncRecord(t, g, []bundle.Source{{Org: "smallrye"}})
	rep = syncNow(t, g, g.fake, SyncOptions{})
	if s := SyncAddNewWithOptions(context.Background(), g.w, rep.Proposals, CloneOptions{Hosting: g.fake, NoFork: true}, 1, Discard); s.Err() != nil {
		t.Fatalf("%+v", s)
	}
	wantSingleOrigin(t, filepath.Join(g.w.Root, "x", "main"), go2.Upstream.URL)
	if g.fake.ForkCalls() != 0 {
		t.Fatal("NoFork listed forks")
	}
}

func hostingRepoBranch(owner, name, url, def string) hosting.Repo {
	return hosting.Repo{Owner: owner, Name: name, CloneURL: url, DefaultBranch: def}
}

func hostingFork(name, url string) hosting.Fork {
	return hosting.Fork{Repo: hosting.Repo{Owner: forkUser, Name: name, CloneURL: url, Fork: true}, ParentOwner: "up", ParentName: name}
}
