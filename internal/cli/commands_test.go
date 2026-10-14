package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

type env struct {
	fake *hosting.Fake
	cwd  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	testutil.IsolateGit(t)
	return &env{fake: hosting.NewFake(), cwd: t.TempDir()}
}

func (e *env) deps() Deps {
	return Deps{
		Hosting: func() hosting.Hosting { return e.fake },
		Getwd:   func() (string, error) { return e.cwd, nil },
	}
}

// run executes the CLI like Execute does and returns the exit code.
func (e *env) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &errb}, e.deps())
	code = execute(cmd, args)
	return out.String(), errb.String(), code
}

func (e *env) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, errs, code := e.run(t, args...)
	if code != 0 {
		t.Fatalf("sheaf %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

func (e *env) origin(t *testing.T, owner, name string, archived bool) testutil.Origin {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	e.fake.AddRepo(hosting.Repo{Owner: owner, Name: name, CloneURL: o.URL, DefaultBranch: "main", Archived: archived})
	return o
}

func load(t *testing.T, root string) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func names(b *bundle.Bundle) []string {
	var n []string
	for _, r := range b.Repositories {
		n = append(n, r.Name)
	}
	return n
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	e.origin(t, "o", "b", false)
	e.origin(t, "o", "c", false)
	e.origin(t, "o", "old", true)
	root := filepath.Join(t.TempDir(), "bundle")

	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root // later commands discover the bundle from the working directory

	out := e.mustRun(t, "add", "o/a", "o/b")
	if !strings.Contains(out, "2 done") {
		t.Fatalf("add summary:\n%s", out)
	}
	for _, p := range []string{"a/main/.git", "b/main/.git"} {
		if !exists(filepath.Join(root, p)) {
			t.Fatalf("%s missing", p)
		}
	}

	out = e.mustRun(t, "add", "--org", "o", "--all")
	if !strings.Contains(out, "1 done, 2 skipped, 0 failed") {
		t.Fatalf("add --org summary:\n%s", out)
	}
	b := load(t, root)
	if got := names(b); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("repositories = %v (archived must be excluded)", got)
	}
	if len(b.Sources) != 1 || b.Sources[0].Org != "o" {
		t.Fatalf("sources = %+v", b.Sources)
	}

	plain := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, root, "clone", "-q", plain.URL, filepath.Join(root, "p"))
	e.mustRun(t, "adopt", "p")
	if !exists(filepath.Join(root, "p", "main", ".git")) {
		t.Fatal("adopted clone not moved to p/main")
	}
	if r, ok := load(t, root).Repository("p"); !ok || r.Origin != "adopted" {
		t.Fatalf("p entry = %+v", r)
	}

	scratch := filepath.Join(root, "a", "main", "scratch.txt")
	if err := os.WriteFile(scratch, []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code := e.run(t, "remove", "a", "--yes")
	if code == 0 || !strings.Contains(errs, "untracked") || !strings.Contains(errs, "--force") {
		t.Fatalf("remove with risks: exit %d, stderr:\n%s", code, errs)
	}
	if !exists(scratch) {
		t.Fatal("refused removal deleted data")
	}
	e.mustRun(t, "remove", "a", "--force", "--yes")
	if exists(filepath.Join(root, "a")) {
		t.Fatal("a still on disk")
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"b", "c", "p"}) {
		t.Fatalf("repositories after remove = %v", got)
	}
}

func TestCreateAndInit(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "new")
	out := e.mustRun(t, "create", dir)
	if !strings.Contains(out, "created") || !exists(filepath.Join(dir, bundle.FileName)) {
		t.Fatalf("create: %q", out)
	}
	if _, errs, code := e.run(t, "create", dir); code == 0 || !strings.Contains(errs, "already exists") {
		t.Fatalf("second create: exit %d, %q", code, errs)
	}

	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	existing := t.TempDir()
	testutil.Git(t, existing, "clone", "-q", o.URL, filepath.Join(existing, "foo"))
	e.cwd = existing
	out = e.mustRun(t, "init")
	if !strings.Contains(out, "foo") || !strings.Contains(out, "sheaf adopt") {
		t.Fatalf("init output:\n%s", out)
	}
	if !exists(filepath.Join(existing, "foo", ".git")) {
		t.Fatal("init without --adopt-all moved something")
	}

	other := t.TempDir()
	testutil.Git(t, other, "clone", "-q", o.URL, filepath.Join(other, "bar"))
	e.mustRun(t, "init", other, "--adopt-all")
	if !exists(filepath.Join(other, "bar", "main", ".git")) {
		t.Fatal("init --adopt-all did not adopt bar")
	}
	if got := names(load(t, other)); !slices.Equal(got, []string{"bar"}) {
		t.Fatalf("repositories = %v", got)
	}
}

func TestAddCLIErrors(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	e.origin(t, "o", "zz", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"add"}, "owner/repo"},
		{[]string{"add", "o/a", "o/zz", "--as", "x"}, "--as"},
		{[]string{"add", "--org", "o"}, "zz"},
		{[]string{"add", "--all"}, "organization"},
		{[]string{"add", "o/a", "--org", "o", "--all"}, "cannot be combined"},
	} {
		args := append([]string{"--bundle", root}, c.args...)
		_, errs, code := e.run(t, args...)
		if code == 0 || !strings.Contains(errs, c.want) {
			t.Errorf("sheaf %v: exit %d, stderr %q; want error containing %q", c.args, code, errs, c.want)
		}
	}
	if len(load(t, root).Repositories) != 0 {
		t.Fatal("failed commands added repositories")
	}
}

func TestAddReportsFailuresWithExitCode(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	out, errs, code := e.run(t, "--bundle", root, "add", "o/a", "o/missing")
	if code == 0 {
		t.Fatalf("exit 0 although o/missing failed\n%s\n%s", out, errs)
	}
	if !strings.Contains(out, "1 done, 0 skipped, 1 failed") || !strings.Contains(out, "o/missing") {
		t.Fatalf("summary:\n%s", out)
	}
	// One line per event on stderr.
	for _, want := range []string{"a: clone started", "a: clone done", "o/missing: resolve failed"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errs)
		}
	}
	if !exists(filepath.Join(root, "a", "main")) {
		t.Fatal("a was not cloned")
	}
}

func TestRemoveRequiresYesInCLIMode(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	e.mustRun(t, "--bundle", root, "add", "o/a")
	_, errs, code := e.run(t, "--bundle", root, "remove", "a")
	if code == 0 || !strings.Contains(errs, "--yes") {
		t.Fatalf("remove without --yes: exit %d, %q", code, errs)
	}
	if !exists(filepath.Join(root, "a", "main")) || len(load(t, root).Repositories) != 1 {
		t.Fatal("remove without --yes changed something")
	}
}

func TestCommandsHonourCancelledContext(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	var out, errb bytes.Buffer
	cmd := NewRootCmdWithDeps(Streams{Out: &out, Err: &errb}, e.deps())
	cmd.SetArgs([]string{"--bundle", root, "add", "o/a"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cmd.ExecuteContext(ctx); err == nil {
		t.Fatalf("add with a cancelled context succeeded\n%s", out.String())
	}
	if exists(filepath.Join(root, "a")) {
		t.Fatal("cancelled add left a directory")
	}
}

func TestRemoveDisplaysInformationalRisks(t *testing.T) {
	e := newEnv(t)
	o := e.origin(t, "o", "a", false)
	work := filepath.Join(t.TempDir(), "work")
	testutil.Git(t, filepath.Dir(work), "clone", "-q", o.URL, work)
	testutil.Git(t, work, "checkout", "-q", "--detach")
	testutil.Git(t, work, "commit", "-q", "--allow-empty", "-m", "release")
	testutil.Git(t, work, "tag", "v1.0")
	testutil.Git(t, work, "push", "-q", "origin", "v1.0")
	root := t.TempDir()
	e.mustRun(t, "create", root)
	e.mustRun(t, "--bundle", root, "add", "o/a")
	_, errs, code := e.run(t, "--bundle", root, "remove", "a", "--yes")
	if code != 0 {
		t.Fatalf("remove: exit %d\n%s", code, errs)
	}
	if !strings.Contains(errs, "commits only reachable from local tags") || !strings.Contains(errs, filepath.Join("a", "main")) {
		t.Fatalf("stderr does not show the informational risk:\n%s", errs)
	}
}

func TestAdoptDefaultBranchFlag(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(root, "x")
	testutil.Git(t, root, "clone", "-q", o.URL, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	testutil.Git(t, dir, "remote", "set-head", "origin", "-d")
	testutil.Git(t, dir, "remote", "set-url", "origin", "file:///nonexistent/o/x.git")
	if out, _, code := e.run(t, "--bundle", root, "adopt", dir); code == 0 || !strings.Contains(out, "--default-branch") {
		t.Fatalf("adopt without --default-branch: exit %d\n%s", code, out)
	}
	e.mustRun(t, "--bundle", root, "adopt", dir, "--default-branch", "main")
	if !exists(filepath.Join(dir, "main", ".git")) || exists(filepath.Join(dir, "feat")) {
		t.Fatal("not moved as is to x/main")
	}
	if b := testutil.Git(t, filepath.Join(dir, "main"), "branch", "--show-current"); b != "feat" {
		t.Fatalf("x/main on %q, want feat (nothing switched)", b)
	}
}

func TestRemoveOfflineFlag(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	e.mustRun(t, "--bundle", root, "add", "o/a")
	// Unreachable, state unknown (no transport for the scheme, no network);
	// a missing local path would count as a gone remote, which --force overrides.
	testutil.Git(t, filepath.Join(root, "a", "main"), "remote", "set-url", "origin", "sheaf-no-such-transport://example.invalid/a.git")
	if _, errs, code := e.run(t, "--bundle", root, "remove", "a", "--yes", "--force"); code == 0 || !strings.Contains(errs, "--offline") {
		t.Fatalf("remove with unreachable origin: exit %d\n%s", code, errs)
	}
	_, errs, code := e.run(t, "--bundle", root, "remove", "a", "--yes", "--offline")
	if code != 0 || !strings.Contains(errs, "stale") {
		t.Fatalf("remove --offline: exit %d\n%s", code, errs)
	}
	if exists(filepath.Join(root, "a")) {
		t.Fatal("not removed")
	}
}

func TestAdoptHostFlag(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(root, "x")
	testutil.Git(t, root, "clone", "-q", o.URL, dir)
	testutil.Git(t, dir, "remote", "set-url", "origin", "git@github_work:o/x.git")
	e.mustRun(t, "--bundle", root, "adopt", dir, "--default-branch", "main", "--host", "github.com")
	if r, _ := load(t, root).Repository("x"); r == nil || !r.IsGitHub() {
		t.Fatalf("x = %+v, want github.com", r)
	}
}

func TestRemoveUsesInjectedWorkingDir(t *testing.T) {
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	root := t.TempDir()
	e.mustRun(t, "create", root)
	e.mustRun(t, "--bundle", root, "add", "o/a")
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = filepath.Join(root, "a", "main")
	if _, errs, code := e.run(t, "remove", "a", "--yes", "--force"); code == 0 || !strings.Contains(errs, "working directory") {
		t.Fatalf("exit %d\n%s", code, errs)
	}
}
