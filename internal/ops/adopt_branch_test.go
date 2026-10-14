package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func branchOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := gitx.CurrentBranch(context.Background(), dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return b
}

func localBranchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	return strings.TrimSpace(testutil.Git(t, dir, "branch", "--list", branch)) != ""
}

// The primary is always <repo>/<default branch>/; the branch the clone was
// on becomes a sibling worktree.
func TestAdoptPlainCloneOnFeatureBranch(t *testing.T) {
	for _, localDefault := range []bool{true, false} {
		name := "default branch missing locally"
		if localDefault {
			name = "default branch exists locally"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
			dir := filepath.Join(f.w.Root, "p")
			if localDefault {
				plainClone(t, o, dir)
				testutil.Git(t, dir, "checkout", "-q", "feat/x")
			} else {
				testutil.Git(t, f.w.Root, "clone", "-q", "--branch", "feat/x", o.URL, dir)
			}
			testutil.CommitFile(t, dir, "local.txt", "local\n", "unpushed work on feat/x")

			if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
				t.Fatal(err)
			}
			primary, wt := filepath.Join(dir, "main"), filepath.Join(dir, "feat-x")
			if b := branchOf(t, primary); b != "main" {
				t.Fatalf("primary branch = %q, want main", b)
			}
			if up := testutil.Git(t, primary, "rev-parse", "--abbrev-ref", "main@{upstream}"); up != "origin/main" {
				t.Fatalf("main upstream = %q, want origin/main", up)
			}
			if b := branchOf(t, wt); b != "feat/x" {
				t.Fatalf("worktree branch = %q, want feat/x", b)
			}
			if got := readFile(t, filepath.Join(wt, "local.txt")); got != "local\n" {
				t.Fatalf("feat/x commit missing in worktree: %q", got)
			}
			if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || fi.IsDir() {
				t.Fatalf("%s is not a linked worktree: %v", wt, err)
			}
			if r, _ := f.onDisk(t).Repository("p"); r == nil || r.DefaultBranch != "main" {
				t.Fatalf("entry = %+v", r)
			}
		})
	}
}

func TestAdoptFeatureBranchWithExistingLinkedWorktree(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x", "other"}})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat/x")
	ext := filepath.Join(t.TempDir(), "ext")
	testutil.Git(t, dir, "worktree", "add", "-q", ext, "other")

	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	if b := branchOf(t, ext); b != "other" {
		t.Fatalf("external worktree broken: %q", b)
	}
	wts, err := gitx.WorktreeList(context.Background(), ext)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, w := range wts {
		paths = append(paths, w.Path)
	}
	want := []string{filepath.Join(dir, "main"), filepath.Join(dir, "feat-x"), ext}
	if len(paths) == len(want) {
		slices.Sort(paths[1:])
		slices.Sort(want[1:])
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("worktrees = %v, want %v", paths, want)
	}
}

func TestAdoptPlainCloneRefusesUnsafeStates(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, dir string)
		want  string
	}{
		"detached HEAD": {func(t *testing.T, dir string) {
			testutil.Git(t, dir, "checkout", "-q", "--detach")
		}, "detached"},
		"untracked file on a feature branch": {func(t *testing.T, dir string) {
			testutil.Git(t, dir, "checkout", "-q", "feat")
			writeFile(t, filepath.Join(dir, "new.txt"), "wip\n")
		}, "commit or stash"},
		"modified file on a feature branch": {func(t *testing.T, dir string) {
			testutil.Git(t, dir, "checkout", "-q", "feat")
			writeFile(t, filepath.Join(dir, "file-1.txt"), "changed\n")
		}, "commit or stash"},
		"default branch checked out in a linked worktree": {func(t *testing.T, dir string) {
			testutil.Git(t, dir, "checkout", "-q", "feat")
			testutil.Git(t, dir, "worktree", "add", "-q", filepath.Join(t.TempDir(), "m"), "main")
		}, "already checked out"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
			dir := filepath.Join(f.w.Root, "p")
			plainClone(t, o, dir)
			c.setup(t, dir)
			before := testutil.Git(t, dir, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
			_, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want refusal mentioning %q", err, c.want)
			}
			if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
				t.Fatalf("clone moved: %v", err)
			}
			if after := testutil.Git(t, dir, "status", "--porcelain=v2", "--branch", "--untracked-files=all"); after != before {
				t.Fatalf("state changed:\n%s\n->\n%s", before, after)
			}
			if len(f.onDisk(t).Repositories) != 0 {
				t.Fatal("refused adoption was recorded")
			}
		})
	}
}

func TestAdoptRestoresOnFailureAfterSwitch(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x", "other"}})
	dir := filepath.Join(f.w.Root, "p")
	// No local main: adopt creates it, the rollback must delete it again.
	testutil.Git(t, f.w.Root, "clone", "-q", "--branch", "feat/x", o.URL, dir)
	ext := filepath.Join(t.TempDir(), "ext")
	testutil.Git(t, dir, "worktree", "add", "-q", "-b", "other", ext, "origin/other")
	// Ignored files are not "dirty" and must survive the round trip.
	writeFile(t, filepath.Join(dir, ".git", "info", "exclude"), "build.log\n")
	ignored := filepath.Join(dir, "build.log")
	writeFile(t, ignored, "keep\n")

	boom := errors.New("injected")
	old := adoptAfterSwitch
	adoptAfterSwitch = func() error { return boom }
	t.Cleanup(func() { adoptAfterSwitch = old })

	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want injected", err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("clone not back at %s: %v", dir, err)
	}
	if b := branchOf(t, dir); b != "feat/x" {
		t.Fatalf("branch after rollback = %q, want feat/x", b)
	}
	if localBranchExists(t, dir, "main") {
		t.Fatal("main created by adopt survived the rollback")
	}
	if got := readFile(t, ignored); got != "keep\n" {
		t.Fatalf("ignored file = %q", got)
	}
	for _, p := range []string{"main", "feat-x"} {
		if exists(filepath.Join(dir, p)) {
			t.Fatalf("%s left behind", p)
		}
	}
	if b := branchOf(t, ext); b != "other" {
		t.Fatalf("external worktree broken after rollback: %q", b)
	}
	wts, err := gitx.WorktreeList(context.Background(), dir)
	if err != nil || len(wts) != 2 || wts[0].Path != dir || wts[1].Path != ext {
		t.Fatalf("worktrees after rollback = %+v, %v", wts, err)
	}
	if st := testutil.Git(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("status after rollback not clean:\n%s", st)
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("failed adoption was recorded")
	}
}
