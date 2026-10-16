package ops

import (
	"context"
	"errors"
	"fmt"
)

// ResolvePath implements `sheaf path [<repo>] [<branch-or-dir>]` with the
// optional --dir/--branch (q.Sel) and --repo (q.Repo) selectors:
//
//   - --repo: that repository, with at most one positional target (or a
//     --dir/--branch selector); without target its primary checkout;
//   - no argument nor selector: the bundle root;
//   - one argument inside a checkout of repository A: the worktree of A it
//     names if any, else the repository it names (its primary checkout);
//     when both exist it is ambiguous;
//   - otherwise as ParseRepoArgs.
//
// Missing (prunable) checkouts are errors.
func ResolvePath(ctx context.Context, w *Workspace, q PathQuery) (string, error) {
	ctx = withCommandCache(ctx)
	args, sel, cwd := q.Args, q.Sel, q.Cwd
	if q.Repo != "" {
		maxArgs := 1
		if sel != nil {
			maxArgs = 0
		}
		if len(args) > maxArgs {
			return "", fmt.Errorf("with --repo, expected at most %d positional arguments, got %d", maxArgs, len(args))
		}
		rc, err := ResolveRepo(w, q.Repo, cwd)
		if err != nil {
			return "", err
		}
		target := sel
		if len(args) == 1 {
			target = &WorktreeTarget{Name: args[0]}
		}
		return checkoutPath(ctx, w, rc.Name, target)
	}
	if len(args) == 0 && sel == nil {
		return w.Root, nil
	}
	if len(args) == 1 && sel == nil {
		if rc, err := ResolveRepo(w, "", cwd); err == nil {
			wt, werr := selectCheckout(ctx, w, rc.Name, &WorktreeTarget{Name: args[0]})
			r, isRepo := lookupRepo(w.Bundle(), args[0])
			switch {
			case werr == nil && isRepo:
				return "", fmt.Errorf("%q is ambiguous: it is the worktree %s of %s and the repository %s; use --dir or --branch for the worktree, or --repo %s for the repository",
					args[0], describe(wt), rc.Name, r.Name, r.Name)
			case werr == nil:
				if wt.Prunable {
					return "", fmt.Errorf("the directory of worktree %s is missing (%s)", wt.Path, wt.PrunableReason)
				}
				return wt.Path, nil
			case isRepo && errors.Is(werr, errNoWorktree):
				return checkoutPath(ctx, w, r.Name, nil)
			default:
				return "", werr
			}
		}
	}
	repo, target, err := ParseRepoArgs(w, args, sel, cwd)
	if err != nil {
		return "", err
	}
	return checkoutPath(ctx, w, repo, target)
}

// PathQuery is the input of ResolvePath.
type PathQuery struct {
	Args []string        // positional arguments
	Sel  *WorktreeTarget // --dir/--branch selector, nil if none
	Repo string          // --repo selector, empty if none
	Cwd  string          // working directory
}

// ParseRepoArgs interprets the positional [<repo>] [<target>] arguments of
// the worktree commands and of path, with an optional --dir/--branch
// selector sel:
//
//   - two arguments: a repository and a target;
//   - one argument: when cwd is inside a repository of the bundle, a target
//     in that repository; otherwise a repository name (no target);
//   - none: the repository containing cwd (no target).
//
// With a selector, the target is sel and at most one argument (the
// repository) is accepted. The returned target is nil when there is none.
func ParseRepoArgs(w *Workspace, args []string, sel *WorktreeTarget, cwd string) (string, *WorktreeTarget, error) {
	maxArgs := 2
	if sel != nil {
		maxArgs = 1
	}
	if len(args) > maxArgs {
		return "", nil, fmt.Errorf("expected at most %d arguments, got %d", maxArgs, len(args))
	}
	var repoArg string
	target := sel
	switch {
	case len(args) == 2:
		repoArg, target = args[0], &WorktreeTarget{Name: args[1]}
	case len(args) == 1 && sel != nil:
		repoArg = args[0]
	case len(args) == 1:
		if rc, err := ResolveRepo(w, "", cwd); err == nil {
			return rc.Name, &WorktreeTarget{Name: args[0]}, nil
		}
		repoArg = args[0]
	}
	rc, err := ResolveRepo(w, repoArg, cwd)
	if err != nil {
		return "", nil, err
	}
	return rc.Name, target, nil
}

// selectCheckout returns the primary checkout of repo (target nil) or the
// checkout selected by target.
func selectCheckout(ctx context.Context, w *Workspace, repo string, target *WorktreeTarget) (WorktreeInfo, error) {
	r, _ := lookupRepo(w.Bundle(), repo)
	infos, _, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return WorktreeInfo{}, fmt.Errorf("%s: %w", repo, err)
	}
	if len(infos) == 0 || !infos[0].Primary {
		return WorktreeInfo{}, fmt.Errorf("%s: no primary checkout listed by git", repo)
	}
	if target == nil {
		return infos[0], nil // the primary checkout comes first
	}
	return findWorktree(infos, repo, *target)
}

// checkoutPath returns the path of selectCheckout's result, refusing a
// missing (prunable) worktree.
func checkoutPath(ctx context.Context, w *Workspace, repo string, target *WorktreeTarget) (string, error) {
	wt, err := selectCheckout(ctx, w, repo, target)
	if err != nil {
		return "", err
	}
	if wt.Prunable {
		return "", fmt.Errorf("the directory of worktree %s is missing (%s)", wt.Path, wt.PrunableReason)
	}
	return wt.Path, nil
}
