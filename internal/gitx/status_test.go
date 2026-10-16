package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func mustStatus(t *testing.T, dir string) *RepoStatus {
	t.Helper()
	st, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wantCounts compares the counters and Dirty() of st.
func wantCounts(t *testing.T, st *RepoStatus, staged, unstaged, untracked, conflicted int, dirty bool) {
	t.Helper()
	if st.Staged != staged || st.Unstaged != unstaged || st.Untracked != untracked || st.Conflicted != conflicted {
		t.Errorf("counts staged/unstaged/untracked/conflicted = %d/%d/%d/%d, want %d/%d/%d/%d",
			st.Staged, st.Unstaged, st.Untracked, st.Conflicted, staged, unstaged, untracked, conflicted)
	}
	if st.Dirty() != dirty {
		t.Errorf("Dirty() = %v, want %v (%+v)", st.Dirty(), dirty, *st)
	}
}

func TestStatusClean(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	st := mustStatus(t, dir)
	if st.Branch != "main" || st.Detached || st.Unborn {
		t.Errorf("branch = %q detached=%v unborn=%v", st.Branch, st.Detached, st.Unborn)
	}
	if want := testutil.Git(t, dir, "rev-parse", "HEAD"); st.Head != want {
		t.Errorf("Head = %q, want %q", st.Head, want)
	}
	if st.Upstream != "origin/main" || st.UpstreamGone || st.Ahead != 0 || st.Behind != 0 {
		t.Errorf("upstream = %q gone=%v ahead=%d behind=%d", st.Upstream, st.UpstreamGone, st.Ahead, st.Behind)
	}
	if st.Stashes != 0 {
		t.Errorf("Stashes = %d", st.Stashes)
	}
	wantCounts(t, st, 0, 0, 0, 0, false)
}

func TestStatusStagedUnstagedUntracked(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Commits: 3})

	writeFile(t, dir, "file-1.txt", "staged change\n")
	testutil.Git(t, dir, "add", "file-1.txt")
	wantCounts(t, mustStatus(t, dir), 1, 0, 0, 0, true)

	writeFile(t, dir, "file-2.txt", "unstaged change\n")
	wantCounts(t, mustStatus(t, dir), 1, 1, 0, 0, true)

	// Same file both staged and modified again counts once on each side.
	writeFile(t, dir, "file-1.txt", "modified again\n")
	wantCounts(t, mustStatus(t, dir), 1, 2, 0, 0, true)

	testutil.Git(t, dir, "reset", "-q", "--hard")
	writeFile(t, dir, "new.txt", "untracked\n")
	// Untracked files are reported but do not make the tree dirty.
	wantCounts(t, mustStatus(t, dir), 0, 0, 1, 0, false)
}

func TestStatusMergeConflict(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "-b", "other")
	testutil.CommitFile(t, dir, "file-1.txt", "other side\n", "other")
	testutil.Git(t, dir, "checkout", "-q", "main")
	testutil.CommitFile(t, dir, "file-1.txt", "main side\n", "main")
	// The merge is expected to fail with a conflict.
	if _, err := Run(context.Background(), dir, "merge", "-q", "other"); err == nil {
		t.Fatal("merge unexpectedly succeeded")
	}
	wantCounts(t, mustStatus(t, dir), 0, 0, 0, 1, true)
}

func TestStatusAheadBehindDiverged(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})

	testutil.CommitFile(t, dir, "local.txt", "x\n", "local")
	st := mustStatus(t, dir)
	if st.Ahead != 1 || st.Behind != 0 {
		t.Fatalf("ahead: %d/%d", st.Ahead, st.Behind)
	}

	testutil.AdvanceOrigin(t, o, "main", "remote.txt", "y\n", "remote")
	testutil.Git(t, dir, "fetch", "-q")
	st = mustStatus(t, dir)
	if st.Ahead != 1 || st.Behind != 1 {
		t.Fatalf("diverged: %d/%d", st.Ahead, st.Behind)
	}

	testutil.Git(t, dir, "reset", "-q", "--hard", "origin/main~1")
	st = mustStatus(t, dir)
	if st.Ahead != 0 || st.Behind != 1 {
		t.Fatalf("behind: %d/%d", st.Ahead, st.Behind)
	}
}

func TestStatusNoUpstreamAndGoneUpstream(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "-b", "local-only")
	st := mustStatus(t, dir)
	if st.Branch != "local-only" || st.Upstream != "" || st.UpstreamGone || st.Ahead != 0 || st.Behind != 0 {
		t.Fatalf("no upstream: %+v", *st)
	}

	testutil.Git(t, dir, "config", "branch.local-only.remote", "origin")
	testutil.Git(t, dir, "config", "branch.local-only.merge", "refs/heads/vanished")
	st = mustStatus(t, dir)
	if st.Upstream != "origin/vanished" || !st.UpstreamGone {
		t.Fatalf("gone upstream: %+v", *st)
	}
}

func TestStatusDetached(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	st := mustStatus(t, dir)
	if !st.Detached || st.Branch != "" || st.Upstream != "" {
		t.Fatalf("detached: %+v", *st)
	}
	if want := testutil.Git(t, dir, "rev-parse", "HEAD"); st.Head != want {
		t.Fatalf("Head = %q, want %q", st.Head, want)
	}
}

func TestStatusStashes(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	for i, c := range []string{"one\n", "two\n"} {
		writeFile(t, dir, "file-1.txt", c)
		testutil.Git(t, dir, "stash", "push", "-q", "-m", "s"+string(rune('0'+i)))
	}
	st := mustStatus(t, dir)
	if st.Stashes != 2 {
		t.Fatalf("Stashes = %d, want 2", st.Stashes)
	}
	wantCounts(t, st, 0, 0, 0, 0, false)
}

func TestStatusUnborn(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "a.txt", "a\n")
	st := mustStatus(t, dir)
	if !st.Unborn || st.Head != "" || st.Branch != "main" || st.Detached {
		t.Fatalf("unborn: %+v", *st)
	}
	wantCounts(t, st, 0, 0, 1, 0, false)
}

func TestStatusAwkwardPaths(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.CommitFile(t, dir, "old name.txt", "o\n", "awkward")
	testutil.CommitFile(t, dir, "sp ace.txt", "s\n", "awkward 2")

	// Staged rename to a path containing a newline: porcelain v2 -z emits the
	// original path as an extra NUL-terminated field.
	testutil.Git(t, dir, "mv", "old name.txt", "new\nname.txt")
	writeFile(t, dir, "sp ace.txt", "changed\n")
	writeFile(t, dir, "un\ntracked 1.txt", "u\n")
	writeFile(t, dir, "1 M. trap.txt", "u\n") // looks like an entry header
	wantCounts(t, mustStatus(t, dir), 1, 1, 2, 0, true)
}

func TestParseStatusRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"# branch.ab +x -1\x00",
		"Z what\x00",
		"2 R. N... 100644 100644 100644 a b R100 new\x00", // missing original path
	} {
		if _, err := parseStatus(in); err == nil {
			t.Errorf("parseStatus(%q): expected error", in)
		}
	}
}

// Configuration that hides changes must not hide them from Status.
func TestStatusIgnoresConfigHidingUntrackedFiles(t *testing.T) {
	t.Run("repo config", func(t *testing.T) {
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		testutil.Git(t, dir, "config", "status.showUntrackedFiles", "no")
		writeFile(t, dir, "new.txt", "u\n")
		wantCounts(t, mustStatus(t, dir), 0, 0, 1, 0, false)
	})
	t.Run("GIT_CONFIG_COUNT", func(t *testing.T) {
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "status.showUntrackedFiles")
		t.Setenv("GIT_CONFIG_VALUE_0", "no")
		writeFile(t, dir, "new.txt", "u\n")
		wantCounts(t, mustStatus(t, dir), 0, 0, 1, 0, false)
	})
}

// cloneWithSubmodule returns a clone with a committed submodule at "sub".
func cloneWithSubmodule(t *testing.T) string {
	t.Helper()
	sub := testutil.NewOrigin(t, testutil.OriginOpts{})
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "submodule", "add", "-q", sub.URL, "sub")
	testutil.Git(t, dir, "commit", "-q", "-m", "add submodule")
	return dir
}

func TestStatusIgnoresConfigHidingSubmoduleChanges(t *testing.T) {
	t.Run("repo config", func(t *testing.T) {
		dir := cloneWithSubmodule(t)
		testutil.Git(t, dir, "config", "diff.ignoreSubmodules", "all")
		writeFile(t, filepath.Join(dir, "sub"), "file-1.txt", "modified in submodule\n")
		wantCounts(t, mustStatus(t, dir), 0, 1, 0, 0, true)
	})
	t.Run("GIT_CONFIG_COUNT", func(t *testing.T) {
		dir := cloneWithSubmodule(t)
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "diff.ignoreSubmodules")
		t.Setenv("GIT_CONFIG_VALUE_0", "all")
		writeFile(t, filepath.Join(dir, "sub"), "file-1.txt", "modified in submodule\n")
		wantCounts(t, mustStatus(t, dir), 0, 1, 0, 0, true)
	})
}

// A submodule whose only change is untracked content counts as untracked,
// consistently with Dirty's rule that untracked files never make a tree dirty.
func TestStatusSubmoduleWithOnlyUntrackedContent(t *testing.T) {
	t.Parallel()
	dir := cloneWithSubmodule(t)
	writeFile(t, filepath.Join(dir, "sub"), "scratch.txt", "u\n")
	wantCounts(t, mustStatus(t, dir), 0, 0, 1, 0, false)

	// A new commit inside the submodule is a real modification.
	testutil.CommitFile(t, filepath.Join(dir, "sub"), "c.txt", "c\n", "in sub")
	wantCounts(t, mustStatus(t, dir), 0, 1, 0, 0, true)
}
