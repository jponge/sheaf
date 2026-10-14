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
func Clone(ctx context.Context, url, dest, branch string) error {
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
	args = append(args, "--", url, abs)
	_, err = Run(ctx, parent, args...)
	return err
}

// ErrDetachedHead is returned by CurrentBranch when HEAD is detached.
var ErrDetachedHead = errors.New("gitx: HEAD is detached")

// ErrNoDefaultBranch is returned by DefaultBranch when neither
// refs/remotes/origin/HEAD nor the remote itself names a default branch.
var ErrNoDefaultBranch = errors.New("gitx: cannot determine the default branch of origin")

// DefaultBranch returns the short name of origin's default branch (e.g.
// "main"). It reads the local refs/remotes/origin/HEAD symbolic ref and, when
// that is missing, asks the remote with `git ls-remote --symref origin HEAD`
// (network access; nothing is written locally).
func DefaultBranch(ctx context.Context, repoDir string) (string, error) {
	if b, err := localDefaultBranch(ctx, repoDir); err == nil {
		return b, nil
	}
	out, err := Run(ctx, repoDir, "ls-remote", "--symref", "origin", "HEAD")
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
	out, err := Run(ctx, repoDir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: refs/remotes/origin/HEAD: %w", ErrNoDefaultBranch, err)
	}
	if b, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/origin/"); ok && b != "" {
		return b, nil
	}
	return "", fmt.Errorf("%w: refs/remotes/origin/HEAD points to %q", ErrNoDefaultBranch, strings.TrimSpace(out))
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

// RemoteURL returns the (fetch) URL configured for remote in dir.
func RemoteURL(ctx context.Context, dir, remote string) (string, error) {
	out, err := Run(ctx, dir, "remote", "get-url", "--", remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
// Fetch per repository.
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
