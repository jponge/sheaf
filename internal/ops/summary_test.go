package ops

import (
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
)

// The refusals of the removals span several lines (the data at risk, then
// the command to run anyway); the places that show one line get their
// Summary: what is refused and why, without the details (B52 A5).

func TestWorktreeRefusedErrorSummary(t *testing.T) {
	t.Parallel()
	plan := func(risks []gitx.Risk, unmerged string, hard ...string) WorktreeRemovePlan {
		return WorktreeRemovePlan{Repo: "api", Worktree: WorktreeInfo{Dir: "abc", Branch: "#abc"}, Risks: risks, Unmerged: unmerged, Hard: hard}
	}
	untracked := gitx.Risk{Kind: gitx.RiskUntracked, Count: 1}
	unpushed := gitx.Risk{Kind: gitx.RiskDetachedCommits, Count: 2, Detail: "abc1234"}
	for _, tc := range []struct {
		name string
		err  WorktreeRefusedError
		want string
	}{
		{"risk", WorktreeRefusedError{Plan: plan([]gitx.Risk{untracked}, "")},
			"refusing to remove api/abc: 1 untracked file"},
		{"risks without their details", WorktreeRefusedError{Plan: plan([]gitx.Risk{untracked, unpushed}, "")},
			"refusing to remove api/abc: 1 untracked file, 2 commits only on detached HEAD"},
		{"repository-wide and informational risks do not block", WorktreeRefusedError{Plan: plan([]gitx.Risk{
			{Kind: gitx.RiskStash, Count: 1}, {Kind: gitx.RiskIgnoredFiles, Count: 3}, untracked}, "")},
			"refusing to remove api/abc: 1 untracked file"},
		{"forced risks are not listed", WorktreeRefusedError{Plan: plan([]gitx.Risk{untracked}, "branch #abc is not merged into origin/main; its 1 commit would be lost"), Force: true},
			"refusing to remove api/abc: branch #abc is not merged into origin/main"},
		{"unmerged branch", WorktreeRefusedError{Plan: plan(nil, "branch #abc has no merge target (origin/main does not exist); its 1 commit would be lost")},
			"refusing to remove api/abc: branch #abc has no merge target"},
		{"hard problems", WorktreeRefusedError{Plan: plan([]gitx.Risk{untracked}, "",
			"its directory is missing; clean it up with `git worktree prune`", fetchProblem(errorString("git fetch (in /x): boom")))},
			"refusing to remove api/abc: its directory is missing, " + fetchFailed + ", 1 untracked file"},
		{"outside", WorktreeRefusedError{Plan: WorktreeRemovePlan{Repo: "api", Worktree: WorktreeInfo{Dir: "abc", Path: "/elsewhere/abc", Outside: true},
			Hard: []string{"refusing to remove /elsewhere/abc: worktree outside /b/api; sheaf only removes worktrees located in it"}}},
			"refusing to remove /elsewhere/abc: worktree outside /b/api"},
	} {
		got := tc.err.Summary()
		if got != tc.want {
			t.Errorf("%s: Summary() = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("%s: Summary() has several lines: %q", tc.name, got)
		}
	}
}

func TestRisksErrorSummary(t *testing.T) {
	t.Parallel()
	item := func(path string, problem string, hard bool, risks ...gitx.Risk) RemoveItem {
		return RemoveItem{Path: path, Problem: problem, Hard: hard, Risks: risks}
	}
	untracked := func(n int) gitx.Risk { return gitx.Risk{Kind: gitx.RiskUntracked, Count: n} }
	unpushed := gitx.Risk{Kind: gitx.RiskUnpushedBranchCommits, Count: 1, Detail: "feat"}
	ignored := gitx.Risk{Kind: gitx.RiskIgnoredFiles, Count: 4}
	for _, tc := range []struct {
		name string
		err  RisksError
		want string
	}{
		{"one checkout", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{item("/b/old/main", "", false, untracked(1), unpushed, ignored)}}},
			"refusing to remove old: 1 untracked file, 1 unpushed commit on local branches"},
		{"counts added up over the checkouts", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{
			item("/b/old/main", "", false, untracked(1)), item("/b/old/feat", "", false, untracked(2), unpushed)}}},
			"refusing to remove old: 3 untracked files, 1 unpushed commit on local branches"},
		{"problems, once each", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{
			item("/b/old/main", goneRemoteProblem, false), item("/b/old/x", "not a checkout of this repository", false),
			item("/b/old/y", "not a checkout of this repository", false)}}},
			"refusing to remove old: the remote repository appears to be gone, not a checkout of this repository"},
		{"forced items are not listed", RisksError{Force: true, Plan: RemovePlan{Repo: "old", Items: []RemoveItem{
			item("/b/old/main", "", false, untracked(1)), item("/b/old/x", "unreadable .git file (boom); remove or move it first", true)}}},
			"refusing to remove old: unreadable .git file"},
	} {
		got := tc.err.Summary()
		if got != tc.want {
			t.Errorf("%s: Summary() = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("%s: Summary() has several lines: %q", tc.name, got)
		}
	}
}
