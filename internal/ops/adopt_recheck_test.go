package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

func recheck(t *testing.T, f *fixture, dir string) (string, string, error) {
	t.Helper()
	var msgs []string
	msg, err := Adopt(context.Background(), f.w, dir, AdoptOptions{Recheck: true}, func(e Event) { msgs = append(msgs, e.Message) })
	return msg, strings.Join(msgs, "\n"), err
}

// adoptedSingleRemote adopts a clone of the fork whose upstream remote was
// added but not fetched, so the entry is recorded without a fork.
func adoptedSingleRemote(t *testing.T, f *fixture, fo testutil.ForkedOrigin, name string) string {
	t.Helper()
	dir := filepath.Join(f.w.Root, name)
	plainClone(t, fo.Fork, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", fo.Upstream.URL)
	adoptEvents(t, f, dir)
	if r, _ := f.onDisk(t).Repository(name); r == nil || r.Forked() {
		t.Fatalf("setup: entry = %+v", r)
	}
	return dir
}

func TestAdoptFetchHintUsesTheFinalPrimaryPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, fo.Fork, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", fo.Upstream.URL) // not fetched
	_, evs := adoptEvents(t, f, dir)
	want := "git -C " + filepath.Join(dir, "main") + " fetch upstream"
	if !strings.Contains(evs, want) {
		t.Fatalf("want hint %q in events:\n%s", want, evs)
	}
}

func TestAdoptRegisteredSuggestsRecheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := adoptedSingleRemote(t, f, fo, "x")
	_, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit)
	if err == nil || !strings.Contains(err.Error(), "already registered") || !strings.Contains(err.Error(), "use --recheck to re-examine its remotes") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecheckRecordsForkAfterFetchingUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := adoptedSingleRemote(t, f, fo, "x")
	primary := filepath.Join(dir, "main")
	testutil.Git(t, primary, "fetch", "-q", "upstream")
	head := testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "HEAD")
	tracking := testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "main@{upstream}")
	msg, evs, err := recheck(t, f, dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.onDisk(t).Repository("x")
	want := bundle.Repository{Name: "x", Slug: "upstream/repo", URL: fo.Upstream.URL, DefaultBranch: "main", Origin: bundle.OriginAdopted, Host: bundle.HostLocal,
		Fork: bundle.Fork{Slug: "fork/repo", URL: fo.Fork.URL}}
	if r == nil || *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", r, want)
	}
	if !strings.Contains(msg, "recorded as a fork of upstream/repo (origin = your fork)") {
		t.Fatalf("message = %q", msg)
	}
	fix := "git -C " + primary + " branch --set-upstream-to=upstream/main main"
	if !strings.Contains(evs, "warning: main tracks origin/main, not upstream/main") || !strings.Contains(evs, fix) {
		t.Fatalf("events:\n%s", evs)
	}
	// Nothing moved, nothing switched, tracking untouched.
	if ents, _ := os.ReadDir(dir); len(ents) != 1 || ents[0].Name() != "main" {
		t.Fatalf("directory changed: %v", ents)
	}
	if testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "HEAD") != head || testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "main@{upstream}") != tracking {
		t.Fatal("branch or tracking changed")
	}
}

func TestRecheckUnrelatedUpstreamIsUnchangedWithWarning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "z")
	plainClone(t, o, dir)
	adoptEvents(t, f, dir)
	primary := filepath.Join(dir, "main")
	testutil.Git(t, primary, "remote", "add", "upstream", unrelatedOrigin(t))
	testutil.Git(t, primary, "fetch", "-q", "upstream")
	before, _ := f.onDisk(t).Repository("z")
	b := *before
	msg, evs, err := recheck(t, f, dir)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := f.onDisk(t).Repository("z"); *after != b {
		t.Fatalf("entry changed: %+v", after)
	}
	if !strings.Contains(msg, "unchanged") || !strings.Contains(evs, "warning: upstream (other/tooling) shares no history with origin") {
		t.Fatalf("message = %q, events:\n%s", msg, evs)
	}
}

func TestRecheckForkedEntryUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir)
	adoptEvents(t, f, dir)
	before, _ := f.onDisk(t).Repository("x")
	b := *before
	msg, _, err := recheck(t, f, dir)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := f.onDisk(t).Repository("x"); *after != b || !strings.Contains(msg, "unchanged") {
		t.Fatalf("entry = %+v, message = %q", after, msg)
	}
}

func TestRecheckForkedEntryWhoseRemotesNoLongerQualifyIsKeptWithWarning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir)
	adoptEvents(t, f, dir)
	testutil.Git(t, filepath.Join(dir, "main"), "remote", "remove", "upstream")
	before, _ := f.onDisk(t).Repository("x")
	b := *before
	msg, evs, err := recheck(t, f, dir)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := f.onDisk(t).Repository("x"); *after != b {
		t.Fatalf("entry changed: %+v", after)
	}
	if !strings.Contains(msg, "unchanged") || !strings.Contains(evs, "warning: ") || !strings.Contains(evs, "no longer") {
		t.Fatalf("message = %q, events:\n%s", msg, evs)
	}
}

func TestRecheckRefusesReferenceSlugCollision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := adoptedSingleRemote(t, f, fo, "x")
	testutil.Git(t, filepath.Join(dir, "main"), "fetch", "-q", "upstream")
	other := bundle.Repository{Name: "other", Slug: "Upstream/Repo", URL: "file:///elsewhere/Upstream/Repo.git", DefaultBranch: "main", Origin: bundle.OriginAdopted, Host: bundle.HostLocal}
	if err := f.w.update(func(b *bundle.Bundle) error { return b.AddRepository(other) }); err != nil {
		t.Fatal(err)
	}
	before, _ := f.onDisk(t).Repository("x")
	b := *before
	_, _, err := recheck(t, f, dir)
	if err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := f.onDisk(t).Repository("x"); *after != b {
		t.Fatalf("entry changed: %+v", after)
	}
}

func TestRecheckOfUnregisteredDirectoryFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "u")
	plainClone(t, o, dir)
	_, _, err := recheck(t, f, dir)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(dir, ".git")) || len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("the directory was adopted or moved")
	}
}
