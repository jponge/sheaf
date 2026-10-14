package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// stopRebase starts a rebase of the current branch of dir onto its parent
// commit that stops right away (an --exec that fails), leaving HEAD detached.
func stopRebase(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "rebase", "--exec", "false", "HEAD~1")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("rebase did not stop: %s", out)
	}
	if _, err := CurrentBranch(context.Background(), dir); err != ErrDetachedHead {
		t.Fatalf("HEAD not detached during the rebase: %v", err)
	}
}

func TestBranchesInUse(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Commits: 2, Branches: []string{"feat", "rb", "bis", "free"}})
	ctx := context.Background()
	for _, b := range []string{"feat", "rb", "bis", "free"} {
		testutil.Git(t, dir, "branch", "--track", b, "origin/"+b)
	}
	base := resolved(t, filepath.Dir(dir))
	featWT, rbWT, bisWT, goneWT := filepath.Join(base, "feat"), filepath.Join(base, "rb"), filepath.Join(base, "bis"), filepath.Join(base, "gone")
	testutil.Git(t, dir, "worktree", "add", "-q", featWT, "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", rbWT, "rb")
	stopRebase(t, rbWT)
	testutil.Git(t, dir, "worktree", "add", "-q", bisWT, "bis")
	testutil.Git(t, bisWT, "bisect", "start", "HEAD", "HEAD~2")
	testutil.Git(t, dir, "worktree", "add", "-q", "-b", "vanished", goneWT)
	testutil.Git(t, dir, "worktree", "add", "-q", "--detach", filepath.Join(base, "detached"))
	if err := os.RemoveAll(goneWT); err != nil {
		t.Fatal(err)
	}
	got, err := BranchesInUse(ctx, featWT)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]BranchUse{
		"main":     {Path: dir, How: "checked out"},
		"feat":     {Path: featWT, How: "checked out"},
		"rb":       {Path: rbWT, How: "rebasing"},
		"bis":      {Path: bisWT, How: "bisecting"},
		"vanished": {Path: goneWT, How: "checked out"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	for b, u := range want {
		gs := got[b]
		if len(gs) != 1 {
			t.Errorf("%s: %+v", b, gs)
			continue
		}
		g, ok := gs[0], true
		if !ok || g.How != u.How || resolvePath(g.Path) != resolvePath(u.Path) {
			t.Errorf("%s: got %+v, %v; want %+v", b, g, ok, u)
		}
	}

	// The primary checkout mid-rebase.
	testutil.Git(t, dir, "switch", "-q", "free")
	stopRebase(t, dir)
	got, err = BranchesInUse(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := got["free"]; len(g) != 1 || g[0].How != "rebasing" || resolvePath(g[0].Path) != resolvePath(dir) {
		t.Errorf("free = %+v", g)
	}
	if _, ok := got["main"]; ok {
		t.Errorf("main reported in use: %+v", got["main"])
	}
}

// N1: in a reftable repository HEAD files hold "ref: refs/heads/.invalid";
// checked-out branches come from `git worktree list`.
func TestBranchesInUseReftable(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := filepath.Join(t.TempDir(), "r")
	if out, err := exec.Command("git", "init", "-q", "--ref-format=reftable", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git init --ref-format=reftable unsupported: %v: %s", err, out)
	}
	testutil.CommitFile(t, dir, "a.txt", "a\n", "a")
	wt := filepath.Join(filepath.Dir(dir), "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", "-b", "feat", wt)
	got, err := BranchesInUse(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for b, p := range map[string]string{"main": dir, "feat": wt} {
		if u := got[b]; len(u) != 1 || u[0].How != "checked out" || resolvePath(u[0].Path) != resolvePath(p) {
			t.Errorf("%s: %+v", b, u)
		}
	}
	if _, ok := got[".invalid"]; ok {
		t.Errorf("bogus branch .invalid: %+v", got)
	}
}

// NIT: a relative gitdir file (worktree.useRelativePaths, git >= 2.48) is
// resolved against the directory holding it.
func TestBranchesInUseRelativeGitdir(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat2"}})
	testutil.Git(t, dir, "branch", "--track", "feat2", "origin/feat2")
	wt := filepath.Join(filepath.Dir(dir), "wt")
	testutil.Git(t, dir, "worktree", "add", "-q", "--detach", wt)
	admin := filepath.Join(dir, ".git", "worktrees", "wt")
	rel, err := filepath.Rel(admin, filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A synthetic rebase of feat2 in that worktree.
	if err := os.MkdirAll(filepath.Join(admin, "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "rebase-merge", "head-name"), []byte("refs/heads/feat2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := BranchesInUse(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if u := got["feat2"]; len(u) != 1 || u[0].How != "rebasing" || resolvePath(u[0].Path) != resolvePath(wt) {
		t.Fatalf("feat2: %+v", u)
	}
}
