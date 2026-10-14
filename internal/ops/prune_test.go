package ops

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// originWork clones the bare origin into a temporary work repository, to
// prepare branches and merges that are then pushed back.
func originWork(t *testing.T, o testutil.Origin) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	testutil.Git(t, filepath.Dir(dir), "clone", "-q", o.Path, dir)
	return dir
}

// pushBranch creates branch on origin from origin's main with one commit
// and returns its tip.
func pushBranch(t *testing.T, o testutil.Origin, branch string) string {
	t.Helper()
	work := originWork(t, o)
	testutil.Git(t, work, "switch", "-q", "-c", branch, "origin/main")
	testutil.CommitFile(t, work, strings.ReplaceAll(branch, "/", "_")+".txt", branch+"\n", "work on "+branch)
	testutil.Git(t, work, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return testutil.Git(t, work, "rev-parse", "HEAD")
}

// mergeOnOrigin merges branch into origin's main with a merge commit.
func mergeOnOrigin(t *testing.T, o testutil.Origin, branch string) {
	t.Helper()
	work := originWork(t, o)
	testutil.Git(t, work, "merge", "-q", "--no-ff", "-m", "merge "+branch, "origin/"+branch)
	testutil.Git(t, work, "push", "-q", "origin", "main")
}

func (f *fixture) prunePlan(t *testing.T, opts PruneOptions) PrunePlan {
	t.Helper()
	if opts.WorkingDir == "" {
		opts.WorkingDir = f.w.Root
	}
	plan, err := PlanPrune(context.Background(), f.w, opts, Discard)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func candidateNames(p PrunePlan) []string {
	var out []string
	for _, c := range p.Candidates {
		kind := "wt"
		if !c.Worktree {
			kind = "branch"
		}
		var why []string
		if c.Gone {
			why = append(why, "gone")
		}
		if c.Merged {
			why = append(why, "merged")
		}
		out = append(out, c.Repo+"/"+c.Branch+":"+kind+":"+strings.Join(why, "+"))
	}
	return out
}

func skipFor(p PrunePlan, branch string) (PruneSkip, bool) {
	for _, s := range p.Skipped {
		if s.Branch == branch {
			return s, true
		}
	}
	return PruneSkip{}, false
}

// pruneScenario builds repository a with:
//   - feat: worktree tracking origin/feat, deleted on origin, unmerged (gone);
//   - merged: worktree tracking origin/merged, merged into main with a merge
//     commit, still on origin (merged);
//   - done: worktree tracking origin/done, merged then deleted (gone+merged);
//   - fresh: worktree of a new branch from origin/main without commits;
//   - wip: worktree tracking origin/wip, unmerged;
//   - old: branch without worktree tracking origin/old, merged then deleted;
//   - stale: branch without worktree tracking origin/stale, deleted unmerged.
func pruneScenario(t *testing.T) (*fixture, string, testutil.Origin) {
	t.Helper()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	for _, b := range []string{"merged", "done", "wip", "old", "stale"} {
		pushBranch(t, o, b)
	}
	testutil.Git(t, primary, "fetch", "-q", "origin")
	for _, b := range []string{"feat", "merged", "done", "fresh", "wip"} {
		f.mustAddWT(t, "a", b)
	}
	testutil.Git(t, primary, "branch", "--track", "old", "origin/old")
	testutil.Git(t, primary, "branch", "--track", "stale", "origin/stale")
	mergeOnOrigin(t, o, "merged")
	mergeOnOrigin(t, o, "done")
	mergeOnOrigin(t, o, "old")
	for _, b := range []string{"feat", "done", "old", "stale"} {
		testutil.Git(t, o.Path, "branch", "-q", "-D", b)
	}
	return f, primary, o
}

func TestPlanPruneFindsGoneAndMergedWorktrees(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	plan := f.prunePlan(t, PruneOptions{})
	want := []string{"a/done:wt:gone+merged", "a/feat:wt:gone", "a/merged:wt:merged"}
	if got := candidateNames(plan); !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v (skipped %+v, problems %v)", got, want, plan.Skipped, plan.Problems)
	}
	for _, c := range plan.Candidates {
		if c.Path != filepath.Join(f.w.Root, "a", c.Dir) || c.Head == "" || c.Reason == "" {
			t.Errorf("candidate %+v", c)
		}
	}
	// Branches without worktree are listed as skipped, not proposed.
	for _, b := range []string{"old", "stale"} {
		if s, ok := skipFor(plan, b); !ok || !strings.Contains(s.Reason, "--include-branches") {
			t.Errorf("skip %s = %+v, %v", b, s, ok)
		}
	}
	// Not gone nor merged: not mentioned at all.
	for _, b := range []string{"fresh", "wip", "main"} {
		if s, ok := skipFor(plan, b); ok {
			t.Errorf("%s skipped: %+v", b, s)
		}
	}

	plan = f.prunePlan(t, PruneOptions{IncludeBranches: true})
	want = []string{"a/done:wt:gone+merged", "a/feat:wt:gone", "a/merged:wt:merged", "a/old:branch:gone+merged", "a/stale:branch:gone"}
	if got := candidateNames(plan); !slices.Equal(got, want) {
		t.Fatalf("with branches: candidates = %v, want %v", got, want)
	}
}

func TestPlanPruneFetchesUnlessOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, o := f.clonedRepo(t, "a")
	f.mustAddWT(t, "a", "feat")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "feat")
	if got := candidateNames(f.prunePlan(t, PruneOptions{Offline: true})); len(got) != 0 {
		t.Fatalf("offline: candidates %v, want none (origin/feat still known locally)", got)
	}
	if got := candidateNames(f.prunePlan(t, PruneOptions{})); !slices.Equal(got, []string{"a/feat:wt:gone"}) {
		t.Fatalf("after fetch: candidates %v", got)
	}
}

func TestPlanPruneNeverProposesThePrimary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	// The primary checkout is temporarily on feat, which then disappears.
	testutil.Git(t, primary, "switch", "-q", "feat")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "feat")
	plan := f.prunePlan(t, PruneOptions{IncludeBranches: true})
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates %v", candidateNames(plan))
	}
	if s, ok := skipFor(plan, "feat"); !ok || !strings.Contains(s.Reason, "primary") {
		t.Fatalf("skip = %+v, %v", s, ok)
	}
}

func TestPlanPruneSkipsPullRequestBranches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	pushBranch(t, o, "contrib")
	mergeOnOrigin(t, o, "contrib")
	// refs/pull/9/head is the merged tip, checked out as pr/9 like
	// `sheaf wt pr 9` does for forks.
	testutil.Git(t, o.Path, "update-ref", "refs/pull/9/head", "refs/heads/contrib")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "contrib")
	testutil.Git(t, primary, "fetch", "-q", "origin", "refs/pull/9/head:refs/heads/pr/9")
	testutil.Git(t, primary, "config", "branch.pr/9.remote", "origin")
	testutil.Git(t, primary, "config", "branch.pr/9.merge", "refs/pull/9/head")
	f.mustAddWT(t, "a", "pr/9")
	plan := f.prunePlan(t, PruneOptions{IncludeBranches: true})
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates %v", candidateNames(plan))
	}
	if s, ok := skipFor(plan, "pr/9"); !ok || !strings.Contains(s.Reason, "pull request") {
		t.Fatalf("skip = %+v, %v", s, ok)
	}
}

func resultFor(rs []PruneResult, branch string) (PruneResult, bool) {
	for _, r := range rs {
		if r.Branch == branch {
			return r, true
		}
	}
	return PruneResult{}, false
}

func TestApplyPruneRemovesWorktreesKeepsBranches(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{IncludeBranches: true, WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	// feat has uncommitted changes: kept.
	writeFile(t, filepath.Join(f.w.Root, "a", "feat", "file-1.txt"), "changed\n")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	if len(rs) != len(plan.Candidates) {
		t.Fatalf("%d results for %d candidates", len(rs), len(plan.Candidates))
	}
	for _, b := range []string{"merged", "done"} {
		r, _ := resultFor(rs, b)
		if r.Outcome != PruneRemoved || exists(r.Path) || !branchExists(t, primary, b) {
			t.Errorf("%s: %+v (dir exists %v, branch exists %v)", b, r, exists(r.Path), branchExists(t, primary, b))
		}
	}
	if r, _ := resultFor(rs, "feat"); r.Outcome != PruneKept || !exists(r.Path) || !strings.Contains(r.Message, "unstaged") {
		t.Errorf("dirty feat: %+v", r)
	}
	// Branch candidates: deleted only when merged or contained (-d).
	if r, _ := resultFor(rs, "old"); r.Outcome != PruneRemoved || branchExists(t, primary, "old") {
		t.Errorf("old: %+v", r)
	}
	if r, _ := resultFor(rs, "stale"); r.Outcome != PruneKept || !branchExists(t, primary, "stale") || !strings.Contains(r.Message, "not merged") {
		t.Errorf("stale: %+v", r)
	}
	for _, d := range []string{"main", "fresh", "wip"} {
		if !exists(filepath.Join(f.w.Root, "a", d)) {
			t.Errorf("%s removed", d)
		}
	}
}

func TestApplyPruneDeleteBranchesUsesMergedSemantics(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{DeleteBranches: true, WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	for _, b := range []string{"merged", "done"} {
		r, _ := resultFor(rs, b)
		if r.Outcome != PruneRemoved || exists(r.Path) || branchExists(t, primary, b) {
			t.Errorf("%s: %+v", b, r)
		}
	}
	// feat is gone but not merged (e.g. squash-merged): the worktree goes,
	// the branch and its commit stay.
	r, _ := resultFor(rs, "feat")
	if r.Outcome != PruneRemoved || exists(r.Path) || !branchExists(t, primary, "feat") || !strings.Contains(r.Message, "kept branch feat") {
		t.Errorf("feat: %+v", r)
	}
}

func TestApplyPruneKeepsWorktreeContainingWorkingDir(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	wd := filepath.Join(f.w.Root, "a", "merged")
	opts := PruneOptions{WorkingDir: wd}
	plan := f.prunePlan(t, opts)
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	if r, _ := resultFor(rs, "merged"); r.Outcome != PruneKept || !exists(wd) {
		t.Fatalf("merged: %+v", r)
	}
}

func TestApplyPruneKeepsWorktreeChangedSincePlan(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root, Offline: true}
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root})
	// The merged worktree switches to another branch after the plan.
	testutil.Git(t, filepath.Join(f.w.Root, "a", "merged"), "switch", "-q", "-c", "new-work")
	testutil.CommitFile(t, filepath.Join(f.w.Root, "a", "merged"), "n.txt", "n\n", "new work")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || !exists(filepath.Join(f.w.Root, "a", "merged")) || !branchExists(t, primary, "new-work") {
		t.Fatalf("merged: %+v", r)
	}
}
