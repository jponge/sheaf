package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Clone clones url into dest. An empty branch checks out the remote's default
// branch; otherwise branch is checked out. Missing parent directories of dest
// are created; dest itself must not exist or must be empty (git's rule). A
// relative dest is resolved against the process working directory.
func (n Net) Clone(ctx context.Context, url, dest, branch string) error {
	return n.clone(ctx, url, dest, branch, "")
}

// clone is Clone with the name of the remote (empty: git's default, origin).
func (n Net) clone(ctx context.Context, url, dest, branch, remote string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("gitx: clone destination %q: %w", dest, err)
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("gitx: clone destination %q: %w", dest, err)
	}
	args := []string{"clone", "--quiet"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if remote != "" {
		args = append(args, "--origin", remote)
	}
	args = append(args, "--", url, abs)
	_, err = run(ctx, n.opts(), parent, args...)
	return err
}

// ErrDetachedHead is returned by CurrentBranch when HEAD is detached.
var ErrDetachedHead = errors.New("gitx: HEAD is detached")

// ErrNoDefaultBranch is returned by DefaultBranchOf when neither
// refs/remotes/<remote>/HEAD nor the remote itself names a default branch.
var ErrNoDefaultBranch = errors.New("gitx: cannot determine the default branch of origin")

func (n Net) defaultBranchOf(ctx context.Context, repoDir, remote string) (string, error) {
	if b, err := localDefaultBranchOf(ctx, repoDir, remote); err == nil {
		return b, nil
	}
	out, err := run(ctx, n.opts(), repoDir, "ls-remote", "--symref", "--", remote, "HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoDefaultBranch, err)
	}
	// Expected first line: "ref: refs/heads/<branch>\tHEAD".
	for line := range strings.SplitSeq(out, "\n") {
		target, name, ok := strings.Cut(line, "\t")
		if !ok || name != "HEAD" {
			continue
		}
		if ref, ok := strings.CutPrefix(target, "ref: "); ok {
			if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && b != "" {
				return b, nil
			}
		}
	}
	return "", ErrNoDefaultBranch
}

// localDefaultBranch reads refs/remotes/origin/HEAD only (no network). It
// returns an error wrapping ErrNoDefaultBranch when the ref is missing.
func localDefaultBranch(ctx context.Context, repoDir string) (string, error) {
	return localDefaultBranchOf(ctx, repoDir, "origin")
}

func localDefaultBranchOf(ctx context.Context, repoDir, remote string) (string, error) {
	head := "refs/remotes/" + remote + "/HEAD"
	out, err := Run(ctx, repoDir, "symbolic-ref", "--quiet", head)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrNoDefaultBranch, head, err)
	}
	if b, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/"+remote+"/"); ok && b != "" {
		return b, nil
	}
	return "", fmt.Errorf("%w: %s points to %q", ErrNoDefaultBranch, head, strings.TrimSpace(out))
}

// CurrentBranch returns the short name of the branch checked out in dir. It
// works on an unborn branch and returns ErrDetachedHead when HEAD is detached.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", ErrDetachedHead
		}
		return "", err
	}
	ref := strings.TrimSpace(out)
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b, nil
	}
	return "", fmt.Errorf("gitx: HEAD points outside refs/heads: %q", ref)
}

// RemoteURL returns the (fetch) URL configured for remote in dir as written
// (remote.<remote>.url; the first value when there are several, the one git
// fetches from), not the URL `git remote get-url` returns after
// url.<base>.insteadOf rewriting: sheaf records and compares configured
// URLs, while git operations (clone, fetch, ls-remote) keep applying the
// rewrites.
func RemoteURL(ctx context.Context, dir, remote string) (string, error) {
	out, err := Run(ctx, dir, "config", "--get-all", "remote."+remote+".url")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.ExitCode == 1 && ctx.Err() == nil {
			return "", fmt.Errorf("gitx: no remote %q (remote.%s.url is not set)", remote, remote)
		}
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first), nil
}

// TopLevel returns the absolute top-level directory of the work tree
// containing dir (symlinks resolved by git). It fails outside a work tree.
func TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", fmt.Errorf("gitx: %s is not inside a work tree", dir)
	}
	return top, nil
}

// IsRepo reports whether dir exists and is inside a git work tree (a
// checkout or a linked worktree). It is false inside a .git directory or a
// bare repository, and on any error.
func IsRepo(ctx context.Context, dir string) bool {
	out, err := Run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// CommonDir returns the absolute path of the git directory shared by all
// worktrees of the repository containing dir (`git rev-parse
// --git-common-dir`): the primary checkout and its linked worktrees return the
// same path, so callers can group work trees by repository, e.g. to run one
// fetch per repository.
func CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("gitx: unexpected git common dir %q in %s", p, dir)
	}
	return filepath.Clean(p), nil
}
