package ops

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// Remotes other than the ones sheaf fetches (origin; upstream and the fork
// for a forked repository) are never fetched by remove, wt rm or prune:
// their remote-tracking refs may be stale (a branch deleted there, a dead
// remote), so they neither prove that commits are pushed nor serve as
// merge targets.

// addExtraRemote adds the remote "other", an empty bare repository, to the
// repository of dir and returns the path of that bare repository.
func addExtraRemote(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "other.git")
	testutil.Git(t, dir, "init", "-q", "--bare", bare)
	testutil.Git(t, dir, "remote", "add", "other", "file://"+bare)
	return bare
}

// localCommit creates a commit on top of main in the repository of dir,
// on no ref, and returns it.
func localCommit(t *testing.T, dir string) string {
	t.Helper()
	return testutil.Git(t, dir, "commit-tree", "-p", "main", "-m", "local", "main^{tree}")
}

// staleOtherRef makes refs/remotes/other/<branch> point at the tip of the
// local branch while the remote other has no such branch (ls-remote shows
// nothing), as after `git fetch other` and a deletion of the branch on
// other.
func staleOtherRef(t *testing.T, dir, branch string) {
	t.Helper()
	testutil.Git(t, dir, "update-ref", "refs/remotes/other/"+branch, "refs/heads/"+branch)
	if out := testutil.Git(t, dir, "ls-remote", "other"); out != "" {
		t.Fatalf("setup: other is not empty: %s", out)
	}
}

const unfetchedNote = "also on other/work, which does not count: sheaf only fetches origin, and the refs of other remotes may be stale"

func wantRemoveRefusedUnpushedWork(t *testing.T, f *fixture, primary string, err error) {
	t.Helper()
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if want := primary + " [main]: 1 unpushed commit on local branches: work (" + unfetchedNote + ")\n"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("repository deleted")
	}
}

func TestRemoveStaleRefOfExtraRemoteIsNotPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	addExtraRemote(t, primary)
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	staleOtherRef(t, primary, "work")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantRemoveRefusedUnpushedWork(t, f, primary, err)
	_, err = f.removeOpts(t, "a", RemoveOptions{Offline: true, WorkingDir: f.w.Root, Confirm: yes})
	wantRemoveRefusedUnpushedWork(t, f, primary, err)
}

// The realistic sequence: pushed to other, fetched, then deleted on other.
func TestRemoveBranchDeletedOnExtraRemoteIsNotPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	bare := addExtraRemote(t, primary)
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	testutil.Git(t, primary, "push", "-q", "other", "work")
	testutil.Git(t, primary, "fetch", "-q", "other")
	testutil.Git(t, bare, "branch", "-q", "-D", "work")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantRemoveRefusedUnpushedWork(t, f, primary, err)
}

// Decision: sheaf does not fetch extra remotes, so even a remote that still
// has the commits does not count (push to origin, or use --force).
func TestRemoveCommitsOnlyOnLiveExtraRemoteAreNotPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	addExtraRemote(t, primary)
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	testutil.Git(t, primary, "push", "-q", "other", "work")
	testutil.Git(t, primary, "fetch", "-q", "other")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantRemoveRefusedUnpushedWork(t, f, primary, err)
	if _, err := f.removeOpts(t, "a", RemoveOptions{Force: true, WorkingDir: f.w.Root, Confirm: yes}); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
}

// Commits pushed to origin count, whatever an extra remote holds.
func TestRemoveCommitsOnOriginAndExtraRemoteArePushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	addExtraRemote(t, primary)
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	testutil.Git(t, primary, "push", "-q", "origin", "work")
	staleOtherRef(t, primary, "work")
	if _, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("not removed")
	}
}

// A gone fork: a commit on its stale refs and on the stale ref of an extra
// remote only exists in this clone.
func TestRemoveForkGoneCommitAlsoOnStaleExtraRemoteIsForkOnly(t *testing.T) {
	t.Parallel()
	f, primary := forkGoneTopic(t)
	addExtraRemote(t, primary)
	testutil.Git(t, primary, "update-ref", "refs/remotes/other/topic", "refs/remotes/origin/topic")
	_, err := f.remove(t, "x", false, yes)
	if err == nil || !strings.Contains(err.Error(), "your fork (origin) appears to be gone and 1 commit exists only on it") {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(f.w.Root, "x")) {
		t.Fatal("repository deleted")
	}
}

// workWorktree adds repository a with a worktree on the new branch work
// holding one commit that exists nowhere else, and the remote other.
func workWorktree(t *testing.T) (f *fixture, primary, bare string, o testutil.Origin) {
	t.Helper()
	f = newFixture(t)
	primary, o = f.clonedRepo(t, "a")
	bare = addExtraRemote(t, primary)
	res := f.mustAddWT(t, "a", "work")
	testutil.CommitFile(t, res.Path, "w.txt", "w\n", "work")
	return f, primary, bare, o
}

func wantBranchKept(t *testing.T, err error, primary string) {
	t.Helper()
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if u := refused.Plan.Unmerged; !strings.Contains(u, "branch work is not merged into origin/main; its 1 commit would be lost") ||
		!strings.Contains(u, "("+unfetchedNote+")") || strings.Contains(u, "into other/work") {
		t.Fatalf("unmerged = %q", u)
	}
	if !branchExists(t, primary, "work") {
		t.Fatal("branch deleted")
	}
}

func TestRemoveWorktreeDeleteBranchStaleRefOfExtraRemoteIsNotAMergeTarget(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := workWorktree(t)
	staleOtherRef(t, primary, "work")
	testutil.Git(t, primary, "config", "branch.work.remote", "other")
	testutil.Git(t, primary, "config", "branch.work.merge", "refs/heads/work")
	for _, offline := range []bool{false, true} {
		_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true, Offline: offline})
		wantBranchKept(t, err, primary)
	}
}

// Without an upstream: the stale ref merely contains the branch.
func TestRemoveWorktreeDeleteBranchContainedOnlyInStaleExtraRefIsUnmerged(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := workWorktree(t)
	staleOtherRef(t, primary, "work")
	_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	wantBranchKept(t, err, primary)
}

// The realistic sequence: pushed to other with -u, then deleted on other.
func TestRemoveWorktreeDeleteBranchDeletedOnExtraRemoteIsUnmerged(t *testing.T) {
	t.Parallel()
	f, primary, bare, _ := workWorktree(t)
	testutil.Git(t, primary, "push", "-q", "-u", "other", "work")
	testutil.Git(t, bare, "branch", "-q", "-D", "work")
	_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	wantBranchKept(t, err, primary)
}

// Pushed to origin: merged into its upstream, whatever other holds.
func TestRemoveWorktreeDeleteBranchPushedToOriginWithExtraRemote(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := workWorktree(t)
	testutil.Git(t, primary, "push", "-q", "-u", "origin", "work")
	testutil.Git(t, primary, "update-ref", "refs/remotes/other/work", "refs/heads/work")
	if _, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "work") {
		t.Fatal("branch kept")
	}
}

// A detached HEAD whose commit is only on the stale ref of other blocks.
func TestRemoveWorktreeDetachedCommitOnlyOnStaleExtraRefBlocks(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := workWorktree(t)
	staleOtherRef(t, primary, "work")
	wt := filepath.Join(f.w.Root, "a", "work")
	testutil.Git(t, wt, "switch", "-q", "--detach")
	testutil.Git(t, primary, "branch", "-q", "-D", "work")
	_, err := f.rmWTT(t, "a", WorktreeTarget{Name: "work", Kind: TargetDir}, WorktreeRemoveOptions{})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) || !hasKind(refused.Plan.Blocking(), gitx.RiskDetachedCommits) {
		t.Fatalf("err = %v, want a refusal for the detached commit", err)
	}
	if !exists(wt) {
		t.Fatal("worktree removed")
	}
}

// prune: work was pushed to origin and deleted there (gone after prune's
// fetch); only the stale ref of other still has its commit.
func TestPruneDeleteBranchesStaleRefOfExtraRemoteKeepsBranch(t *testing.T) {
	t.Parallel()
	for _, worktree := range []bool{true, false} {
		f, primary, _, o := workWorktree(t)
		testutil.Git(t, primary, "push", "-q", "-u", "origin", "work")
		testutil.Git(t, o.Path, "branch", "-q", "-D", "work")
		staleOtherRef(t, primary, "work")
		opts := PruneOptions{DeleteBranches: true, IncludeBranches: !worktree, WorkingDir: f.w.Root}
		if !worktree {
			if _, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		plan := f.prunePlan(t, opts)
		rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
		r, ok := resultFor(rs, "work")
		if !ok || !strings.Contains(r.Message, "kept branch work: branch work is not merged into origin/main; its 1 commit would be lost") ||
			!strings.Contains(r.Message, unfetchedNote) {
			t.Fatalf("worktree %v: result = %+v (plan %v)", worktree, r, candidateNames(plan))
		}
		if !branchExists(t, primary, "work") {
			t.Fatalf("worktree %v: branch deleted", worktree)
		}
	}
}
