package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Worktree is one entry of `git worktree list --porcelain -z`.
type Worktree struct {
	// Path is the absolute path of the worktree as recorded by git.
	Path string
	// Head is the checked-out commit id (empty for a bare repository or an
	// unborn branch).
	Head string
	// Branch is the short branch name (refs/heads/ stripped), empty when
	// Detached or Bare.
	Branch   string
	Detached bool
	Bare     bool
	Locked   bool
	// LockReason is the optional reason given to `git worktree lock`.
	LockReason string
	// Prunable is true when git considers the worktree stale (e.g. its
	// directory was moved or deleted); PrunableReason says why.
	Prunable       bool
	PrunableReason string
}

// WorktreeList lists the worktrees of the repository containing repoDir; the
// main worktree (or the bare repository) comes first.
func WorktreeList(ctx context.Context, repoDir string) ([]Worktree, error) {
	out, err := Run(ctx, repoDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	wts, err := parseWorktrees(out)
	if err != nil {
		return nil, fmt.Errorf("gitx: worktree list in %s: %w", repoDir, err)
	}
	return wts, nil
}

// parseWorktrees parses NUL-separated porcelain output: one "key[ value]"
// attribute per field, records terminated by an empty field.
func parseWorktrees(out string) ([]Worktree, error) {
	var (
		wts []Worktree
		cur *Worktree
	)
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			cur = nil
			continue
		}
		key, val, _ := strings.Cut(f, " ")
		if key == "worktree" {
			wts = append(wts, Worktree{Path: val})
			cur = &wts[len(wts)-1]
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("attribute %q outside a worktree record", f)
		}
		switch key {
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked, cur.LockReason = true, val
		case "prunable":
			cur.Prunable, cur.PrunableReason = true, val
		}
		// Unknown attributes are ignored for forward compatibility.
	}
	return wts, nil
}

// WorktreeAddMode tells how WorktreeAdd obtained the branch.
type WorktreeAddMode int

const (
	// WorktreeLocalBranch: the local branch existed and was checked out.
	WorktreeLocalBranch WorktreeAddMode = iota + 1
	// WorktreeRemoteBranch: origin/<branch> existed; a local branch tracking
	// it was created.
	WorktreeRemoteBranch
	// WorktreeNewBranch: neither existed; a new branch without upstream was
	// created from WorktreeAddOpts.Base.
	WorktreeNewBranch
)

func (m WorktreeAddMode) String() string {
	switch m {
	case WorktreeLocalBranch:
		return "local-branch"
	case WorktreeRemoteBranch:
		return "remote-branch"
	case WorktreeNewBranch:
		return "new-branch"
	}
	return fmt.Sprintf("WorktreeAddMode(%d)", int(m))
}

// WorktreeAddOpts configures WorktreeAdd.
type WorktreeAddOpts struct {
	// Base is the start point of a new branch (WorktreeNewBranch only).
	// Default: "origin/<default branch>" read from the local
	// refs/remotes/origin/HEAD; WorktreeAdd fails with ErrNoDefaultBranch when
	// that ref is missing (it never asks the remote).
	Base string
}

// WorktreeAddResult is the result of WorktreeAdd.
type WorktreeAddResult struct {
	Mode WorktreeAddMode
	// Upstream is the short name of the upstream of the checked-out branch
	// (e.g. "origin/feat-x"), empty when it has none. With
	// WorktreeLocalBranch it is whatever the existing branch tracks, possibly
	// not origin/<branch>: callers may want to warn about it.
	Upstream string
}

// ErrWorktreePathOccupied is returned by WorktreeAdd when the target path
// exists and is not an empty directory.
var ErrWorktreePathOccupied = errors.New("gitx: worktree path exists and is not an empty directory")

// WorktreeAdd creates a worktree at path for branch, from the repository
// containing repoDir. A relative path is relative to repoDir. path must not
// exist or must be an empty directory (ErrWorktreePathOccupied otherwise,
// checked before anything is created). It never accesses the network:
// remote-tracking refs are used as they are, so callers wanting a fresh
// remote state should Fetch first. Cases, in order:
//
//   - refs/heads/<branch> exists: it is checked out (git refuses if it is
//     already checked out in another worktree), whatever its upstream;
//   - refs/remotes/origin/<branch> exists: a local branch tracking it is
//     created;
//   - otherwise a new branch is created from opts.Base with no upstream
//     (--no-track), so a later pull never merges the base into it.
//
// If git fails after this call created the branch, the branch is deleted
// again (best effort, with a fresh short-lived context if ctx is done). The
// result is only meaningful when err is nil.
func WorktreeAdd(ctx context.Context, repoDir, path, branch string, opts WorktreeAddOpts) (WorktreeAddResult, error) {
	if branch == "" {
		return WorktreeAddResult{}, errors.New("gitx: worktree add: empty branch name")
	}
	if path == "" {
		return WorktreeAddResult{}, errors.New("gitx: worktree add: empty path")
	}
	if err := checkWorktreeTarget(repoDir, path); err != nil {
		return WorktreeAddResult{}, err
	}
	local, err := refExists(ctx, repoDir, "refs/heads/"+branch)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	mode, args, err := worktreeAddArgs(ctx, repoDir, path, branch, opts.Base, local)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	if _, err := Run(ctx, repoDir, append([]string{"worktree", "add", "--quiet"}, args...)...); err != nil {
		if !local {
			deleteCreatedBranch(ctx, repoDir, branch)
		}
		return WorktreeAddResult{}, err
	}
	up, err := Run(ctx, repoDir, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+branch)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	return WorktreeAddResult{Mode: mode, Upstream: strings.TrimSpace(up)}, nil
}

// worktreeAddArgs chooses the mode and the `git worktree add` arguments.
func worktreeAddArgs(ctx context.Context, repoDir, path, branch, base string, local bool) (WorktreeAddMode, []string, error) {
	if local {
		return WorktreeLocalBranch, []string{"--", path, branch}, nil
	}
	remote, err := refExists(ctx, repoDir, "refs/remotes/origin/"+branch)
	if err != nil {
		return 0, nil, err
	}
	if remote {
		return WorktreeRemoteBranch, []string{"--track", "-b", branch, "--", path, "origin/" + branch}, nil
	}
	if base == "" {
		def, err := localDefaultBranch(ctx, repoDir)
		if err != nil {
			return 0, nil, err
		}
		base = "origin/" + def
	}
	return WorktreeNewBranch, []string{"--no-track", "-b", branch, "--", path, base}, nil
}

// checkWorktreeTarget fails unless path (relative to repoDir) is absent or an
// empty directory.
func checkWorktreeTarget(repoDir, path string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoDir, path)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitx: worktree add: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s", ErrWorktreePathOccupied, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("gitx: worktree add: %w", err)
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("gitx: worktree add: %w", err)
		}
		return fmt.Errorf("%w: %s", ErrWorktreePathOccupied, path)
	}
	return nil
}

// deleteCreatedBranch removes a branch that a failed `git worktree add -b`
// created. git branch -D refuses a branch checked out in some worktree, so a
// branch that is in use is never removed.
func deleteCreatedBranch(ctx context.Context, repoDir, branch string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rereadTimeout)
	defer cancel()
	if ok, err := refExists(cctx, repoDir, "refs/heads/"+branch); err == nil && ok {
		_, _ = Run(cctx, repoDir, "branch", "--quiet", "-D", "--", branch)
	}
}

// refExists reports whether the fully qualified ref exists.
func refExists(ctx context.Context, dir, ref string) (bool, error) {
	_, err := Run(ctx, dir, "show-ref", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	var ge *GitError
	if errors.As(err, &ge) && ge.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// WorktreeRemove removes the worktree at path. Without force git refuses
// when the worktree has modified tracked files or untracked files; with
// force those are discarded. A locked worktree is refused either way.
func WorktreeRemove(ctx context.Context, repoDir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	_, err := Run(ctx, repoDir, append(args, "--", path)...)
	return err
}

// WorktreeRepair runs `git worktree repair` from repoDir, fixing the links
// between the repository and its worktrees after either was moved. paths
// lists moved worktrees whose new location git cannot discover by itself.
func WorktreeRepair(ctx context.Context, repoDir string, paths ...string) error {
	args := []string{"worktree", "repair"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	_, err := Run(ctx, repoDir, args...)
	return err
}
