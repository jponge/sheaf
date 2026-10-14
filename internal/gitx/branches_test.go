package gitx

import (
	"context"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func mustUnpushed(t *testing.T, dir string, want int) {
	t.Helper()
	got, err := UnpushedCommits(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("UnpushedCommits = %d, want %d", got, want)
	}
}

func TestUnpushedCommits(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Commits: 2})
	mustUnpushed(t, dir, 0)

	testutil.CommitFile(t, dir, "a.txt", "a\n", "on main")
	mustUnpushed(t, dir, 1)

	// Commits on a branch that is not checked out count too.
	testutil.Git(t, dir, "checkout", "-q", "-b", "side")
	testutil.CommitFile(t, dir, "b.txt", "b\n", "side 1")
	testutil.CommitFile(t, dir, "c.txt", "c\n", "side 2")
	testutil.Git(t, dir, "checkout", "-q", "main")
	mustUnpushed(t, dir, 3)

	// A commit only reachable from a detached HEAD counts.
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	testutil.CommitFile(t, dir, "d.txt", "d\n", "detached")
	mustUnpushed(t, dir, 4)
}

func TestUnpushedCommitsWithoutRemoteAndUnborn(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	mustUnpushed(t, dir, 0)
	testutil.CommitFile(t, dir, "a.txt", "a\n", "1")
	testutil.CommitFile(t, dir, "b.txt", "b\n", "2")
	mustUnpushed(t, dir, 2)
}

func TestGoneBranches(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat/gone", "keep"}})
	testutil.Git(t, dir, "branch", "--track", "feat/gone", "origin/feat/gone")
	testutil.Git(t, dir, "branch", "--track", "keep", "origin/keep")
	testutil.Git(t, dir, "branch", "local-only")
	testutil.Git(t, o.Path, "branch", "-D", "feat/gone")
	if err := Fetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	got, err := GoneBranches(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"feat/gone"}) {
		t.Fatalf("GoneBranches = %q", got)
	}
}

func TestMergedBranches(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "branch", "merged/one")
	testutil.Git(t, dir, "checkout", "-q", "-b", "unmerged")
	testutil.CommitFile(t, dir, "u.txt", "u\n", "unmerged work")
	testutil.Git(t, dir, "checkout", "-q", "main")
	got, err := MergedBranches(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"main", "merged/one"}) {
		t.Fatalf("MergedBranches = %q", got)
	}
	if _, err := MergedBranches(context.Background(), dir, "no-such-ref"); err == nil {
		t.Fatal("unknown target: expected error")
	}
}
