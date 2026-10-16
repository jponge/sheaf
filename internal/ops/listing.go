package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/gitx"
)

// repoEntry is one work tree of a repository as listed by enumerateRepo.
type repoEntry struct {
	gitx.Worktree
	// Primary is set for the main work tree, the first entry listed by git.
	Primary bool
	// Outside is set for a work tree that is not directly in the repository
	// directory (see outsideRepoDir).
	Outside bool
	// Problem is set for a linked work tree that cannot be used: its
	// directory is missing (unless enumPolicy.keepMissing), or it is not a
	// work tree of the repository (another repository's, or a directory
	// without .git).
	Problem error
}

// enumPolicy holds what the two views built on enumerateRepo need
// differently: listRepo (worktree commands, WorktreeInfo) and listRepoCheckouts
// (checkout commands, Checkout). Their other differences are in the views
// themselves (backlog B3): the fields (Checkout.Dir is set for an outside
// worktree and Checkout.Path of the primary is the bundle path, while
// WorktreeInfo has Dir empty when outside, git's path and OffDefault), the
// failures (Problem with a path, RepoError per repository) and the
// sequencing (ListCheckouts lists repositories in parallel, ListWorktrees one
// after the other and stops on cancellation).
type enumPolicy struct {
	// primaryOnly lists the primary alone (CheckoutFilter.PrimaryOnly):
	// linked work trees are not verified.
	primaryOnly bool
	// keepMissing keeps a linked work tree whose directory is missing as a
	// usable entry, which the worktree commands show (git marks it
	// prunable); the checkout commands report it as a problem.
	keepMissing bool
	// noVerify skips the verification of linked work trees (no Problem is
	// set, and no git invocation is made for them), for callers that only
	// need the names and paths of the entries (collision checks).
	noVerify bool
}

var errNotMainWorkTree = errors.New("not the main work tree of its repository")

// enumerateRepo lists the work trees of the repository of primary, a primary
// checkout accepted by checkPrimary (common is its common git directory),
// with one `git worktree list`, plus at most one git invocation per linked
// work tree to verify it (none with pol.noVerify). The primary comes first,
// then the work trees in the repository directory by directory name (then
// path), then those outside it by path. The error is set when git fails or when the first entry listed by
// git is not the primary.
func enumerateRepo(ctx context.Context, primary, common string, pol enumPolicy) ([]repoEntry, error) {
	wts, err := gitx.WorktreeList(ctx, primary)
	if err != nil {
		return nil, err
	}
	// An invariant for a primary accepted by checkPrimary (its .git is a
	// directory that is its own common directory, so git lists it first and
	// not bare); checked anyway, as cheap defence in depth.
	if len(wts) == 0 || wts[0].Bare || !sameFile(wts[0].Path, primary) {
		return nil, errNotMainWorkTree
	}
	if pol.primaryOnly {
		wts = wts[:1]
	}
	repoDir := filepath.Dir(primary)
	entries := make([]repoEntry, 0, len(wts))
	for i, wt := range wts {
		e := repoEntry{Worktree: wt, Primary: i == 0, Outside: outsideRepoDir(wt.Path, repoDir)}
		if i > 0 && !pol.noVerify {
			e.Problem = verifyLinked(ctx, wt.Path, primary, common, pol.keepMissing)
		}
		entries = append(entries, e)
	}
	dir := func(e repoEntry) string {
		if e.Outside {
			return ""
		}
		return filepath.Base(filepath.Clean(e.Path))
	}
	slices.SortStableFunc(entries, func(a, b repoEntry) int {
		switch {
		case a.Primary != b.Primary:
			if a.Primary {
				return -1
			}
			return 1
		case a.Outside != b.Outside:
			if b.Outside {
				return -1
			}
			return 1
		case dir(a) != dir(b):
			return strings.Compare(dir(a), dir(b))
		}
		return strings.Compare(filepath.Clean(a.Path), filepath.Clean(b.Path))
	})
	return entries, nil
}

// outsideRepoDir reports whether the work tree at path is not directly in
// repoDir (a directory that resolves to itself): both its parent and its
// resolved parent must be repoDir.
func outsideRepoDir(path, repoDir string) bool {
	p := filepath.Clean(path)
	return filepath.Dir(resolveExisting(p)) != repoDir || filepath.Dir(p) != repoDir
}

// foreignWorktreeError is the problem of a linked work tree whose directory
// holds another repository.
type foreignWorktreeError struct{ commonDir string }

func (e *foreignWorktreeError) Error() string {
	return "worktree of another repository (" + e.commonDir + ")"
}

// verifyLinked checks that the linked work tree at path of the repository of
// primary exists and is a work tree of that repository, whose common git
// directory is common (see inspect). With keepMissing, a missing directory
// is not a problem. A directory without .git is reported without running
// git, with the command that restores the .git file (`git worktree prune`
// would only drop git's entry and leave the directory).
func verifyLinked(ctx context.Context, path, primary, common string, keepMissing bool) error {
	if _, err := os.Stat(path); err != nil {
		switch {
		case !errors.Is(err, fs.ErrNotExist):
			return err
		case keepMissing:
			return nil
		}
		return errors.New("worktree directory missing (still registered in git; `git worktree prune` removes the entry)")
	}
	if _, err := os.Lstat(filepath.Join(path, ".git")); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("not a git work tree: its .git file is missing (`git -C %s worktree repair %s` restores it)", primary, path)
	}
	insp, err := inspect(ctx, path)
	if err == nil && !sameFile(insp.commonDir, common) {
		err = &foreignWorktreeError{commonDir: insp.commonDir}
	}
	return err
}
