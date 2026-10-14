package ops

import (
	"context"
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

// onlyEntries fails unless dir contains exactly the given names.
func onlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s contains %v, want %v", dir, got, want)
	}
}

// The primary is <repo>/<default branch>/ whatever branch it has checked
// out: a plain clone on another branch is moved as it is, nothing switched.
func TestAdoptPlainCloneOnFeatureBranchMovesAsIs(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat/x")
	testutil.CommitFile(t, dir, "local.txt", "local\n", "unpushed work on feat/x")
	writeFile(t, filepath.Join(dir, ".git", "info", "exclude"), "*.log\n")
	writeFile(t, filepath.Join(dir, "build.log"), "ignored\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")
	writeFile(t, filepath.Join(dir, "file-1.txt"), "modified\n")

	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(dir, "main")
	if b := branchOf(t, primary); b != "feat/x" {
		t.Fatalf("primary branch = %q, want feat/x (nothing switched)", b)
	}
	for name, want := range map[string]string{"local.txt": "local\n", "build.log": "ignored\n", "untracked.txt": "untracked\n", "file-1.txt": "modified\n"} {
		if got := readFile(t, filepath.Join(primary, name)); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	onlyEntries(t, dir, "main") // no sibling worktree created
	if r, _ := f.onDisk(t).Repository("p"); r == nil || r.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", r)
	}
}

func TestAdoptPlainCloneDetachedMovesAsIs(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{DefaultBranch: "trunk"})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	head := testutil.Git(t, dir, "rev-parse", "HEAD")
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(dir, "trunk")
	if _, err := gitx.CurrentBranch(context.Background(), primary); err != gitx.ErrDetachedHead {
		t.Fatalf("primary HEAD: %v, want still detached", err)
	}
	if got := testutil.Git(t, primary, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved: %s -> %s", head, got)
	}
}

// The default branch checked out in a linked worktree does not matter any
// more: nothing is switched.
func TestAdoptFeatureBranchWithExistingLinkedWorktrees(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat/x")
	ext := filepath.Join(t.TempDir(), "ext")
	testutil.Git(t, dir, "worktree", "add", "-q", ext, "main")

	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if b := branchOf(t, ext); b != "main" {
		t.Fatalf("external worktree broken: %q", b)
	}
	wts, err := gitx.WorktreeList(context.Background(), ext)
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || wts[0].Path != filepath.Join(dir, "main") || wts[1].Path != ext || wts[1].Prunable {
		t.Fatalf("worktrees = %+v", wts)
	}
	if b := branchOf(t, filepath.Join(dir, "main")); b != "feat/x" {
		t.Fatalf("primary branch = %q", b)
	}
}

// Laid out: the primary is <repo>/<normalized default branch>/ with any
// branch checked out (or detached); it is only registered.
func TestAdoptLaidOutPrimaryOnAnyBranchOnlyRegisters(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, primary string){
		"feature branch": func(t *testing.T, p string) { testutil.Git(t, p, "checkout", "-q", "feat/x") },
		"detached":       func(t *testing.T, p string) { testutil.Git(t, p, "checkout", "-q", "--detach") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
			primary := filepath.Join(f.w.Root, "foo", "main")
			plainClone(t, o, primary)
			setup(t, primary)
			before := testutil.Git(t, primary, "status", "--porcelain=v2", "--branch")
			cands, err := Candidates(context.Background(), f.w.Root, f.w.Bundle())
			if err != nil || len(cands) != 1 || cands[0].Kind != CandidateLaidOut || cands[0].Primary != primary {
				t.Fatalf("candidates = %+v, %v", cands, err)
			}
			if _, err := adopt(t, f, filepath.Join(f.w.Root, "foo"), AdoptOptions{}); err != nil {
				t.Fatal(err)
			}
			if after := testutil.Git(t, primary, "status", "--porcelain=v2", "--branch"); after != before {
				t.Fatalf("checkout changed:\n%s\n->\n%s", before, after)
			}
			onlyEntries(t, filepath.Join(f.w.Root, "foo"), "main")
			if r, _ := f.onDisk(t).Repository("foo"); r == nil || r.DefaultBranch != "main" {
				t.Fatalf("entry = %+v", r)
			}
		})
	}
}

// Laid out with another child name: renamed to the default branch's
// directory, checkout untouched, worktrees repaired.
func TestAdoptLaidOutRenamesPrimaryToDefaultBranchDir(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"dev", "feat", "other"}})
	repo := filepath.Join(f.w.Root, "foo")
	dev := filepath.Join(repo, "dev")
	plainClone(t, o, dev)
	testutil.Git(t, dev, "checkout", "-q", "dev")
	writeFile(t, filepath.Join(dev, "wip.txt"), "dirty is fine\n")
	testutil.Git(t, dev, "worktree", "add", "-q", "../feat", "feat")
	ext := filepath.Join(t.TempDir(), "ext")
	testutil.Git(t, dev, "worktree", "add", "-q", ext, "other")
	writeFile(t, filepath.Join(dev, ".git", "info", "exclude"), ".wt/\n")
	testutil.Git(t, dev, "worktree", "add", "-q", "-b", "nested", ".wt/nested", "origin/main")

	if _, err := adopt(t, f, repo, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(repo, "main")
	if b := branchOf(t, primary); b != "dev" {
		t.Fatalf("primary branch = %q, want dev (untouched)", b)
	}
	if got := readFile(t, filepath.Join(primary, "wip.txt")); got != "dirty is fine\n" {
		t.Fatalf("wip.txt = %q", got)
	}
	onlyEntries(t, repo, "feat", "main")
	wts, err := gitx.WorktreeList(context.Background(), primary)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{primary: "dev", filepath.Join(repo, "feat"): "feat", ext: "other", filepath.Join(primary, ".wt", "nested"): "nested"}
	if len(wts) != len(want) {
		t.Fatalf("worktrees = %+v", wts)
	}
	for _, w := range wts {
		if want[w.Path] != w.Branch || w.Prunable {
			t.Fatalf("worktree %+v unexpected or broken (want %v)", w, want)
		}
	}
	for p, b := range want {
		if got := branchOf(t, p); got != b {
			t.Fatalf("%s on %q, want %q", p, got, b)
		}
	}
}

func TestAdoptLaidOutRefusesWhenDefaultBranchDirExists(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"dev"}})
	repo := filepath.Join(f.w.Root, "foo")
	dev := filepath.Join(repo, "dev")
	plainClone(t, o, dev)
	testutil.Git(t, dev, "checkout", "-q", "dev")
	if err := os.Mkdir(filepath.Join(repo, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := adopt(t, f, repo, AdoptOptions{})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(repo, "main")) {
		t.Fatalf("err = %v, want refusal naming foo/main", err)
	}
	if !exists(filepath.Join(dev, ".git")) || len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("something changed")
	}
}

// A single clone in a directory is a laid-out repository only when the
// names fit: the child is named after its current or default branch, or the
// origin repository is named like the directory.
func TestAdoptLaidOutPlausibility(t *testing.T) {
	t.Run("docs/somerepo refused", func(t *testing.T) {
		f := newFixture(t)
		o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		child := filepath.Join(f.w.Root, "docs", "somerepo")
		plainClone(t, o, child)
		testutil.Git(t, child, "checkout", "-q", "feat")
		_, err := adopt(t, f, filepath.Join(f.w.Root, "docs"), AdoptOptions{})
		if err == nil || !strings.Contains(err.Error(), "somerepo") {
			t.Fatalf("err = %v, want refusal mentioning somerepo", err)
		}
		if len(f.onDisk(t).Repositories) != 0 || !exists(filepath.Join(child, ".git")) {
			t.Fatal("something changed")
		}
		if cands, err := Candidates(context.Background(), f.w.Root, f.w.Bundle()); err != nil || len(cands) != 0 {
			t.Fatalf("candidates = %+v, %v; want none", cands, err)
		}
	})
	t.Run("origin named like the directory", func(t *testing.T) {
		f := newFixture(t)
		o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		child := filepath.Join(f.w.Root, "docs", "somerepo")
		plainClone(t, o, child)
		testutil.Git(t, child, "remote", "set-url", "origin", "https://github.com/o/Docs.git")
		cands, err := Candidates(context.Background(), f.w.Root, f.w.Bundle())
		if err != nil || len(cands) != 1 {
			t.Fatalf("candidates = %+v, %v", cands, err)
		}
		if _, err := adopt(t, f, filepath.Join(f.w.Root, "docs"), AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
		onlyEntries(t, filepath.Join(f.w.Root, "docs"), "main")
	})
	t.Run("child named after the current branch", func(t *testing.T) {
		f := newFixture(t)
		o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
		child := filepath.Join(f.w.Root, "r", "Feat-X")
		plainClone(t, o, child)
		testutil.Git(t, child, "checkout", "-q", "feat/x")
		if _, err := adopt(t, f, filepath.Join(f.w.Root, "r"), AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
		if b := branchOf(t, filepath.Join(f.w.Root, "r", "main")); b != "feat/x" {
			t.Fatalf("branch = %q", b)
		}
	})
}
