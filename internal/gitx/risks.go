package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// RiskKind is a category of data that deleting a work tree or repository
// would lose.
type RiskKind int

const (
	// Work-tree-local kinds (lost when the work tree is deleted).
	RiskStaged     RiskKind = iota + 1 // entries with staged changes
	RiskUnstaged                       // entries with unstaged changes (incl. submodules with new commits or modified content)
	RiskConflicted                     // unmerged entries
	RiskUntracked                      // untracked entries (incl. submodules with only untracked content)
	// Repository-wide kinds (lost when the repository, i.e. its last work
	// tree, is deleted; irrelevant to removing a linked worktree).
	RiskStash                 // stash entries
	RiskUnpushedBranchCommits // commits on local branches not on any remote-tracking ref
	// RiskDetachedCommits: commits reachable from this work tree's detached
	// HEAD but from no branch, tag or remote-tracking ref (work-tree-local).
	RiskDetachedCommits
	// RiskTagOnlyCommits: commits reachable only from tags (neither from a
	// local branch nor from a remote-tracking ref); repository-wide.
	RiskTagOnlyCommits
	// RiskIgnoredFiles: ignored entries; informational (usually build
	// output), see Risk.Informational.
	RiskIgnoredFiles
)

func (k RiskKind) String() string {
	switch k {
	case RiskStaged:
		return "staged changes"
	case RiskUnstaged:
		return "unstaged changes"
	case RiskConflicted:
		return "conflicts"
	case RiskUntracked:
		return "untracked files"
	case RiskStash:
		return "stashes"
	case RiskUnpushedBranchCommits:
		return "unpushed commits on local branches"
	case RiskDetachedCommits:
		return "commits only on detached HEAD"
	case RiskTagOnlyCommits:
		return "commits only reachable from local tags"
	case RiskIgnoredFiles:
		return "ignored files"
	}
	return fmt.Sprintf("RiskKind(%d)", int(k))
}

// Risk is one kind of data at risk, with the number of entries or commits.
type Risk struct {
	Kind  RiskKind
	Count int
}

func (r Risk) String() string { return fmt.Sprintf("%d %v", r.Count, r.Kind) }

// Informational reports whether the risk should be shown but not block a
// deletion (ignored files).
func (r Risk) Informational() bool { return r.Kind == RiskIgnoredFiles }

// SafeToDelete reports whether risks contains only informational entries.
func SafeToDelete(risks []Risk) bool {
	for _, r := range risks {
		if !r.Informational() {
			return false
		}
	}
	return true
}

// DeletionRisks lists what deleting the work tree containing dir, or the
// whole repository, would lose. Only kinds with a non-zero count are
// returned, in RiskKind order. The kinds are documented as work-tree-local
// or repository-wide: removing a linked worktree only needs the former, while
// removing a repository needs every kind, checked in each of its work trees
// (each may have its own detached HEAD and changes).
//
// Commit-based kinds compare with remote-tracking refs as of the last fetch;
// commits on a branch that was pushed but whose remote-tracking ref was
// pruned count as unpushed. Status configuration that hides changes is
// overridden (see Status).
//
// Known gaps (not detected): unpushed commits, stashes or changes inside
// submodules' own repositories (only the submodule's state as seen from the
// superproject is reported); commits only reachable from reflogs, notes or
// other refs (refs/pull/*, refs/replace/*, ...); detached HEADs of other
// worktrees (call DeletionRisks in each); content of .git itself (hooks,
// config, info/exclude); unpushed Git LFS objects. Tags fetched from the
// remote count as local tags, so RiskTagOnlyCommits may report upstream
// tags pointing to commits not on any remote branch (false positives).
func DeletionRisks(ctx context.Context, dir string) ([]Risk, error) {
	st, err := status(ctx, dir, "--ignored=traditional")
	if err != nil {
		return nil, err
	}
	branchCommits, err := revCount(ctx, dir, "--branches", "--not", "--remotes")
	if err != nil {
		return nil, err
	}
	detachedCommits := 0
	if st.Detached && !st.Unborn {
		if detachedCommits, err = revCount(ctx, dir, "HEAD", "--not", "--branches", "--tags", "--remotes"); err != nil {
			return nil, err
		}
	}
	tagCommits, err := revCount(ctx, dir, "--tags", "--not", "--branches", "--remotes")
	if err != nil {
		return nil, err
	}
	var risks []Risk
	for _, r := range []Risk{
		{RiskStaged, st.Staged},
		{RiskUnstaged, st.Unstaged},
		{RiskConflicted, st.Conflicted},
		{RiskUntracked, st.Untracked},
		{RiskStash, st.Stashes},
		{RiskUnpushedBranchCommits, branchCommits},
		{RiskDetachedCommits, detachedCommits},
		{RiskTagOnlyCommits, tagCommits},
		{RiskIgnoredFiles, st.ignored},
	} {
		if r.Count > 0 {
			risks = append(risks, r)
		}
	}
	return risks, nil
}

// revCount runs `git rev-list --count args...` and parses the result.
func revCount(ctx context.Context, dir string, args ...string) (int, error) {
	out, err := Run(ctx, dir, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("gitx: rev-list --count in %s: unexpected output %q", dir, out)
	}
	return n, nil
}
