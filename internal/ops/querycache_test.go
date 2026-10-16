package ops

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// The re-check of Remove after confirmation reads git afresh: a linked
// worktree switched to another branch while Confirm was pending (a change
// only `git worktree list` shows) aborts the removal.
func TestRemoveRecheckSeesBranchSwitchedDuringConfirm(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	wt := f.mustAddWT(t, "a", "feat").Path
	_, err := f.remove(t, "a", false, func(RemovePlan) bool {
		testutil.Git(t, wt, "switch", "-q", "-c", "other")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") || !exists(wt) {
		t.Fatalf("err = %v, worktree exists: %v", err, exists(wt))
	}
}

// The re-check of RemoveWorktree under the bundle lock reads git afresh: the
// branch checked out in another worktree while Confirm was pending makes the
// plan differ (the branch would now be kept), so nothing is removed.
func TestRemoveWorktreeRecheckSeesBranchCheckedOutDuringConfirm(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := f.mustAddWT(t, "a", "feat").Path
	other := filepath.Join(filepath.Dir(primary), "other")
	_, err := f.rmWT(t, "a", "feat", WorktreeRemoveOptions{DeleteBranch: true, Confirm: func(WorktreeRemovePlan) bool {
		testutil.Git(t, primary, "worktree", "add", "-q", "-f", other, "feat")
		return true
	}})
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") || !exists(wt) {
		t.Fatalf("err = %v, worktree exists: %v", err, exists(wt))
	}
}

// Mutations made by one command are seen by the next one sharing its cache
// (ops commands reuse a cache already present in their context).
func TestSharedQueryCacheSeesMutationsOfEarlierCommands(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	ctx := gitx.WithQueryCache(context.Background())
	dirs := func() []string {
		t.Helper()
		infos, fails, err := ListWorktrees(ctx, f.w, "a")
		if err != nil || len(fails) > 0 {
			t.Fatalf("list: %v %v", err, fails)
		}
		var out []string
		for _, in := range infos {
			out = append(out, in.Dir)
		}
		return out
	}
	if got := strings.Join(dirs(), " "); got != "main" {
		t.Fatalf("before add: %s", got)
	}
	if _, err := AddWorktree(ctx, f.w, "a", "topic", WorktreeAddOptions{NoFetch: true}, noEmit); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(dirs(), " "); got != "main topic" {
		t.Fatalf("after add: %s", got)
	}
	if ok, err := refExists(ctx, primary, "refs/heads/topic"); err != nil || !ok {
		t.Fatalf("topic after add: %v %v", ok, err)
	}
	if _, err := RemoveWorktree(ctx, f.w, "a", WorktreeTarget{Name: "topic"}, WorktreeRemoveOptions{DeleteBranch: true, WorkingDir: f.w.Root,
		Confirm: func(WorktreeRemovePlan) bool { return true }}, noEmit); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(dirs(), " "); got != "main" {
		t.Fatalf("after remove: %s", got)
	}
	if ok, err := refExists(ctx, primary, "refs/heads/topic"); err != nil || ok {
		t.Fatalf("topic after remove -d: %v %v", ok, err)
	}
}

// ApplyPrune re-checks the plan with fresh reads even when its caller's
// context carries a cache that the planning filled: a commit made in a
// candidate worktree after the plan keeps it.
func TestApplyPruneRechecksFreshUnderASharedCache(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	ctx := gitx.WithQueryCache(context.Background())
	plan, err := PlanPrune(ctx, f.w, PruneOptions{WorkingDir: f.w.Root}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Only this candidate: removing another one first would invalidate the
	// repository's cached answers anyway.
	plan.Candidates = slices.DeleteFunc(plan.Candidates, func(c PruneCandidate) bool { return c.Branch != "merged" })
	if len(plan.Candidates) != 1 {
		t.Fatalf("candidates = %v", candidateNames(plan))
	}
	wt := filepath.Join(f.w.Root, "a", "merged")
	testutil.CommitFile(t, wt, "more.txt", "m\n", "more work after plan")
	rs := ApplyPrune(ctx, f.w, plan, PruneOptions{WorkingDir: f.w.Root, Offline: true}, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || !exists(wt) || !strings.Contains(r.Message, "new commits since it was proposed") {
		t.Fatalf("%+v", r)
	}
}
