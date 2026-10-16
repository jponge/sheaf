package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

// manualForkClone clones the fork as origin and adds the reference as
// upstream, the way users set up forks by hand.
func manualForkClone(t *testing.T, fo testutil.ForkedOrigin, dest string) {
	t.Helper()
	plainClone(t, fo.Fork, dest)
	testutil.Git(t, dest, "remote", "add", "upstream", fo.Upstream.URL)
	testutil.Git(t, dest, "fetch", "-q", "upstream")
}

func TestAdoptRecognisesForkRemotes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir)
	// The fork's default branch must not matter: the reference's does.
	testutil.Git(t, dir, "remote", "set-head", "origin", "feat")
	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	r, ok := f.onDisk(t).Repository("x")
	if !ok {
		t.Fatal("not registered")
	}
	want := bundle.Repository{Name: "x", Slug: "upstream/repo", URL: fo.Upstream.URL, DefaultBranch: "main", Origin: bundle.OriginAdopted, Host: bundle.HostLocal,
		Fork: bundle.Fork{Slug: "fork/repo", URL: fo.Fork.URL}}
	if *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", *r, want)
	}
	if !exists(filepath.Join(dir, "main", ".git")) {
		t.Fatal("not moved to x/main")
	}
}

func TestAdoptUpstreamSameAsOriginIsNotAFork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "y")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", o.URL)
	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.onDisk(t).Repository("y"); r.Forked() || r.URL != o.URL {
		t.Fatalf("entry = %+v", r)
	}
}

func TestAdoptedForkUsesUpstreamForNewWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir)
	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u.txt", "u\n", "upstream")
	res, _, err := f.addWT(t, "x", "topic", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "upstream/main" || head(t, res.Path, "HEAD") != head(t, fo.Upstream.Path, "main") {
		t.Fatalf("result = %+v", res)
	}
}

// unrelatedOrigin builds a bare repository other/tooling.git sharing no
// history with any fixture.
func unrelatedOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	testutil.Git(t, root, "init", "-q", "-b", "main", work)
	testutil.CommitFile(t, work, "tooling.txt", "unrelated\n", "unrelated root")
	bare := filepath.Join(root, "other", "tooling.git")
	testutil.Git(t, root, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

func adoptEvents(t *testing.T, f *fixture, dir string) (string, string) {
	t.Helper()
	var msgs []string
	msg, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, func(e Event) { msgs = append(msgs, e.Message) })
	if err != nil {
		t.Fatal(err)
	}
	return msg, strings.Join(msgs, "\n")
}

func TestAdoptUnrelatedUpstreamIsNotAFork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "z")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", unrelatedOrigin(t))
	testutil.Git(t, dir, "fetch", "-q", "upstream")
	_, evs := adoptEvents(t, f, dir)
	r, _ := f.onDisk(t).Repository("z")
	if r.Forked() || r.URL != o.URL || r.Slug != originSlug(t, o) {
		t.Fatalf("entry = %+v", r)
	}
	if !strings.Contains(evs, "warning: upstream (other/tooling) shares no history with origin") {
		t.Fatalf("events:\n%s", evs)
	}
}

func TestAdoptUnfetchedUpstreamIsNotAFork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, fo.Fork, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", fo.Upstream.URL) // not fetched
	_, evs := adoptEvents(t, f, dir)
	if r, _ := f.onDisk(t).Repository("x"); r.Forked() || r.URL != fo.Fork.URL {
		t.Fatalf("entry = %+v", r)
	}
	if !strings.Contains(evs, "warning: upstream/main is missing locally") || !strings.Contains(evs, "git -C") {
		t.Fatalf("events:\n%s", evs)
	}
}

func TestAdoptForkReportsAndWarnsAboutTracking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir) // main tracks origin/main
	msg, evs := adoptEvents(t, f, dir)
	if !strings.Contains(msg, "recorded as a fork of upstream/repo (origin = your fork)") {
		t.Fatalf("message = %q", msg)
	}
	want := "git -C " + filepath.Join(dir, "main") + " branch --set-upstream-to=upstream/main main"
	if !strings.Contains(evs, "warning: main tracks origin/main, not upstream/main") || !strings.Contains(evs, want) {
		t.Fatalf("events:\n%s", evs)
	}
}
