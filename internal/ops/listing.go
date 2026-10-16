package ops

import (
	"context"
	"errors"

	"github.com/jponge/sheaf/internal/gitx"
)

// repoEntry is one work tree of a repository as listed by enumerateRepo.
type repoEntry struct {
	gitx.Worktree
	// Primary is set for the main work tree, the first entry listed by git.
	Primary bool
	// Outside is the answer of enumPolicy.outside for the entry's path.
	Outside bool
}

// enumPolicy holds the steps on which the two listings built on
// enumerateRepo differ: listRepo (worktree commands, WorktreeInfo) and
// coListRepo (checkout commands, Checkout). Backlog B3 merged their
// enumeration and kept these differences as they were.
type enumPolicy struct {
	// strictMain requires the first entry to be the primary checkout, not
	// bare (errNotMainWorkTree otherwise), and keeps any later entry.
	// Without it, bare entries are dropped and the first entry listed by git
	// is the primary.
	strictMain bool
	// primaryOnly lists the primary alone: linked work trees are neither
	// verified nor classified.
	primaryOnly bool
	// outside tells whether the work tree at path (as listed by git) is
	// outside the repository directory.
	outside func(path string) bool
	// verify, when set, checks each linked work tree before it is
	// classified: one that fails is returned as a problem instead of an
	// entry.
	verify func(wt gitx.Worktree) error
}

var errNotMainWorkTree = errors.New("not the main work tree of its repository")

// enumerateRepo lists the work trees of the repository of primary (a checked
// primary checkout of the repository repo) with one `git worktree list`, in
// git's order: the primary first, then the linked work trees. Linked work
// trees rejected by pol.verify are returned as problems, in the same order.
// The error is set when git fails or, under pol.strictMain, when the first
// entry is not the primary.
func enumerateRepo(ctx context.Context, repo, primary string, pol enumPolicy) ([]repoEntry, []Problem, error) {
	wts, err := gitx.WorktreeList(ctx, primary)
	if err != nil {
		return nil, nil, err
	}
	if pol.strictMain && (len(wts) == 0 || wts[0].Bare || !coSameFile(wts[0].Path, primary)) {
		return nil, nil, errNotMainWorkTree
	}
	var (
		entries []repoEntry
		probs   []Problem
	)
	for i, wt := range wts {
		if wt.Bare && !pol.strictMain {
			continue
		}
		if i > 0 && pol.primaryOnly {
			break
		}
		if i > 0 && pol.verify != nil {
			if err := pol.verify(wt); err != nil {
				probs = append(probs, Problem{Repo: repo, Path: wt.Path, Err: err})
				continue
			}
		}
		entries = append(entries, repoEntry{Worktree: wt, Primary: i == 0, Outside: pol.outside(wt.Path)})
	}
	return entries, probs, nil
}
