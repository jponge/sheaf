package ops

import (
	"context"
	"errors"
	"os"
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

const unfetchedNote = "also on other/work, which does not count: only the remote-tracking refs that sheaf's fetch of origin updates count, others may be stale"

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

// With --offline, remove does not trust the fork's refs (as wt rm and
// prune): topic was pushed to the fork, then deleted there.
func TestRemoveOfflineDoesNotTrustForkRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	testutil.Git(t, fo.Fork.Path, "branch", "-q", "-D", "topic")
	_, err := f.removeOpts(t, "x", RemoveOptions{Offline: true, WorkingDir: f.w.Root, Confirm: yes})
	want := primary + " [main]: 1 unpushed commit on local branches: topic (--offline: the state of your fork (origin) is unknown: its remote-tracking refs origin/* are not trusted)\n"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !exists(filepath.Join(f.w.Root, "x")) {
		t.Fatal("repository deleted")
	}
}

// A fetch refspec that does not write refs/remotes/origin/* (a mirror
// namespace, or a single branch): origin/work is never updated nor pruned
// by sheaf's fetch, so it proves nothing.
func TestRemoveStaleRefOutsideTheFetchRefspecIsNotPushed(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"+refs/heads/*:refs/remotes/mirror/*", "+refs/heads/main:refs/remotes/origin/main"} {
		f := newFixture(t)
		primary, _ := f.clonedRepo(t, "a")
		testutil.Git(t, primary, "config", "remote.origin.fetch", spec)
		testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
		testutil.Git(t, primary, "update-ref", "refs/remotes/origin/work", "refs/heads/work")
		_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
		want := primary + " [main]: 1 unpushed commit on local branches: work (also on origin/work, which does not count"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", spec, err, want)
		}
		if !exists(filepath.Join(f.w.Root, "a")) {
			t.Fatalf("%s: repository deleted", spec)
		}
	}
}

// The refs a custom refspec writes do count.
func TestRemoveRefsWrittenByACustomRefspecArePushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/mirror/*")
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	testutil.Git(t, primary, "push", "-q", "origin", "work")
	if _, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes}); err != nil {
		t.Fatal(err)
	}
}

// A remote named origin/x has its refs under refs/remotes/origin/x/: they
// are not origin's.
func TestRemoveOfflineRefsOfRemoteNamedLikeOriginSubdirAreNotPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	bare := filepath.Join(t.TempDir(), "x.git")
	testutil.Git(t, primary, "init", "-q", "--bare", bare)
	testutil.Git(t, primary, "remote", "add", "origin/x", "file://"+bare)
	testutil.Git(t, primary, "update-ref", "refs/heads/work", localCommit(t, primary))
	testutil.Git(t, primary, "update-ref", "refs/remotes/origin/x/work", "refs/heads/work")
	_, err := f.removeOpts(t, "a", RemoveOptions{Offline: true, WorkingDir: f.w.Root, Confirm: yes})
	want := primary + " [main]: 1 unpushed commit on local branches: work (also on origin/x/work, which does not count"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// The refs of a nested repository's remote inside the deleted tree prove
// nothing: that remote is deleted too.
func TestRemoveNestedRefsOfRemoteInsideTheTreeAreNotPushed(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	n := filepath.Join(primary, "vendor", "n")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), n)
	inner := filepath.Join(primary, "vendor", "inner.git")
	testutil.Git(t, n, "init", "-q", "--bare", inner)
	testutil.Git(t, n, "remote", "add", "inner", "file://"+inner)
	testutil.Git(t, n, "update-ref", "refs/heads/work", localCommit(t, n))
	testutil.Git(t, n, "push", "-q", "inner", "work")
	testutil.Git(t, n, "fetch", "-q", "inner")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "vendor/n (1 unpushed commit on local branches)") {
		t.Fatalf("err = %v", err)
	}
	if !exists(n) {
		t.Fatal("nested repository deleted")
	}
}

// A nested repository is fetched with `git fetch --all`, which skips
// remotes with skipFetchAll: their refs prove nothing.
func TestRemoveNestedStaleRefOfSkippedRemoteIsNotPushed(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	n := filepath.Join(primary, "vendor", "n")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), n)
	addExtraRemote(t, n)
	testutil.Git(t, n, "config", "remote.other.skipFetchAll", "true")
	testutil.Git(t, n, "update-ref", "refs/heads/work", localCommit(t, n))
	staleOtherRef(t, n, "work")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "1 nested repository: vendor/n (1 unpushed commit on local branches)") {
		t.Fatalf("err = %v", err)
	}
	if !exists(n) {
		t.Fatal("nested repository deleted")
	}
}

// A gone fork's stale refs are at risk whatever its refspecs say: an exact
// refspec (git remote set-branches) or a negative one makes them untrusted,
// not safe to lose.
func TestRemoveForkGoneOnlyCopyIsAtRiskWithNarrowedRefspec(t *testing.T) {
	t.Parallel()
	for _, narrow := range [][]string{
		{"remote", "set-branches", "origin", "main"},
		{"config", "--add", "remote.origin.fetch", "^refs/heads/nope"},
	} {
		f := newFixture(t)
		fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
		f.forkOnlyBranch(t)
		wt := filepath.Join(f.w.Root, "x", "topic")
		testutil.Git(t, primary, "worktree", "remove", wt)
		testutil.Git(t, primary, "branch", "-q", "-D", "topic")
		testutil.Git(t, primary, narrow...)
		if err := os.RemoveAll(fo.Fork.Path); err != nil {
			t.Fatal(err)
		}
		_, err := f.remove(t, "x", false, yes)
		if err == nil || !strings.Contains(err.Error(), "your fork (origin) appears to be gone and 1 commit exists only on it") {
			t.Fatalf("%v: err = %v", narrow, err)
		}
		if !exists(filepath.Join(f.w.Root, "x")) {
			t.Fatalf("%v: repository deleted", narrow)
		}
	}
}

// The fetch runs in the primary checkout: a worktree's own configuration
// (extensions.worktreeConfig) does not decide which refs it writes.
func TestRemoveWorktreeConfigRefspecIsNotTrusted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	w := f.mustAddWT(t, "a", "w").Path
	testutil.Git(t, primary, "config", "core.repositoryFormatVersion", "1")
	testutil.Git(t, primary, "config", "extensions.worktreeConfig", "true")
	testutil.Git(t, w, "config", "--worktree", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/stale/*")
	testutil.Git(t, w, "switch", "-q", "--detach")
	testutil.CommitFile(t, w, "d.txt", "d\n", "only on the detached HEAD")
	testutil.Git(t, primary, "branch", "-q", "-D", "w")
	testutil.Git(t, w, "update-ref", "refs/remotes/stale/x", "HEAD")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "1 commit only on detached HEAD") {
		t.Fatalf("err = %v", err)
	}
	if !exists(w) {
		t.Fatal("worktree deleted")
	}
}

// The URL deciding whether a nested remote is inside the deleted tree is
// the one fetch uses, after url.<base>.insteadOf.
func TestRemoveNestedRemoteInsideTheTreeThroughInsteadOf(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	n := filepath.Join(primary, "vendor", "n")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), n)
	inner := filepath.Join(primary, "vendor", "inner.git")
	testutil.Git(t, n, "init", "-q", "--bare", inner)
	testutil.Git(t, n, "config", "url.file://"+inner+".insteadOf", "https://example.invalid/inner")
	testutil.Git(t, n, "remote", "add", "inner", "https://example.invalid/inner")
	testutil.Git(t, n, "update-ref", "refs/heads/work", localCommit(t, n))
	testutil.Git(t, n, "push", "-q", "inner", "work")
	testutil.Git(t, n, "fetch", "-q", "inner")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "vendor/n (1 unpushed commit on local branches)") {
		t.Fatalf("err = %v", err)
	}
	if !exists(n) {
		t.Fatal("nested repository deleted")
	}
}

// A negative refspec: origin's fetch neither updates nor prunes the refs
// of the excluded branches, so none of origin's refs is trusted.
func TestRemoveNegativeRefspecRefsAreNotPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "config", "--add", "remote.origin.fetch", "^refs/heads/old")
	testutil.Git(t, primary, "update-ref", "refs/heads/old", localCommit(t, primary))
	testutil.Git(t, primary, "update-ref", "refs/remotes/origin/old", "refs/heads/old")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if err == nil || !strings.Contains(err.Error(), "unpushed commits on local branches: ") || !strings.Contains(err.Error(), "old (also on ") {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("repository deleted")
	}
}

// A merge target that exists but is not trusted is not reported as missing.
func TestRemoveWorktreeDeleteBranchUntrustedTargetIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := workWorktree(t)
	testutil.Git(t, primary, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/mirror/*")
	_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if u := refused.Plan.Unmerged; !strings.Contains(u, "branch work has no merge target (origin/main is not trusted:") || strings.Contains(u, "does not exist") {
		t.Fatalf("unmerged = %q", u)
	}
}
