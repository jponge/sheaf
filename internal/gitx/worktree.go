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
	// Tag is the tag a detached worktree created by WorktreeAddTag is pinned
	// to (read from its TagMarkerFile), empty otherwise.
	Tag string
	// TagMoved is set when HEAD is no longer the commit recorded when the
	// worktree was pinned at Tag (a commit on the detached HEAD, or a checkout
	// of another commit); Tag is kept.
	TagMoved bool
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
	fillWorktreeTags(wts)
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
	// Remotes are the remotes searched, in order, for an existing
	// refs/remotes/<remote>/<branch> (WorktreeRemoteBranch); default
	// ["origin"].
	Remotes []string
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

// ErrBaseNotFound is returned by WorktreeAdd when the start point of a new
// branch (WorktreeAddOpts.Base) does not resolve to an object.
var ErrBaseNotFound = errors.New("gitx: base not found")

// WorktreeAdd creates a worktree at path for branch, from the repository
// containing repoDir. A relative path is relative to repoDir. path must not
// exist or must be an empty directory (ErrWorktreePathOccupied otherwise,
// checked before anything is created). It never accesses the network:
// remote-tracking refs are used as they are, so callers wanting a fresh
// remote state should fetch first. Cases, in order:
//
//   - refs/heads/<branch> exists: it is checked out (git refuses if it is
//     already checked out in another worktree), whatever its upstream;
//   - refs/remotes/<remote>/<branch> exists for a remote of opts.Remotes
//     (default origin), in that order: a local branch tracking it is
//     created;
//   - otherwise a new branch is created from opts.Base with no upstream
//     (--no-track), so a later pull never merges the base into it.
//
// If git fails after this call created the branch, the branch is deleted
// again if it still points at its start point and is not checked out (see
// cleanupCreatedBranch). That cleanup cannot lose commits: it only removes a
// branch whose tip is still the start point, through a compare-and-swap, and
// it never removes one that git reported as already existing. A residual
// window remains in which a branch of the same name that someone else created
// concurrently, exactly at the same start point, after this call's failed add
// and before the cleanup, is removed as well; it then held no commit beyond
// the start point. The result is only meaningful when err is nil.
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
	mode, args, err := worktreeAddArgs(ctx, repoDir, path, branch, opts.Base, opts.Remotes, local)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	startOID := ""
	if mode == WorktreeNewBranch {
		start := args[len(args)-1]
		if _, err := Run(ctx, repoDir, "rev-parse", "--verify", "--quiet", "--end-of-options", start); err != nil {
			var ge *GitError
			if errors.As(err, &ge) && ge.ExitCode == 1 && ctx.Err() == nil {
				return WorktreeAddResult{}, fmt.Errorf("%w: %s", ErrBaseNotFound, start)
			}
			return WorktreeAddResult{}, err
		}
	}
	if !local {
		// The start point is the last argument; resolve it now so that the
		// cleanup below only removes a branch still pointing at it.
		out, err := Run(ctx, repoDir, "rev-parse", "--verify", "--end-of-options", args[len(args)-1]+"^{commit}")
		if err != nil {
			return WorktreeAddResult{}, err
		}
		startOID = strings.TrimSpace(out)
		if mode == WorktreeNewBranch {
			// Create the branch exactly at the commit resolved above, so that a
			// base that moves between the two commands cannot leak into it. (A
			// tracking branch must name its remote-tracking ref as start point
			// for git to set up the upstream.)
			args[len(args)-1] = startOID
		}
	}
	if _, err := Run(ctx, repoDir, append([]string{"worktree", "add", "--quiet"}, args...)...); err != nil {
		if !local {
			cleanupCreatedBranch(ctx, repoDir, branch, startOID, err)
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
func worktreeAddArgs(ctx context.Context, repoDir, path, branch, base string, remotes []string, local bool) (WorktreeAddMode, []string, error) {
	if local {
		return WorktreeLocalBranch, []string{"--", path, branch}, nil
	}
	remote, err := remoteBranchRef(ctx, repoDir, branch, remotes)
	if err != nil {
		return 0, nil, err
	}
	if remote != "" {
		return WorktreeRemoteBranch, []string{"--track", "-b", branch, "--", path, remote + "/" + branch}, nil
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

// cleanupCreatedBranch removes the branch that a failed `git worktree add -b`
// created from startOID (best effort, with a fresh short-lived context if ctx
// is done). It removes nothing when:
//
//   - addErr says the branch already existed (it was created by someone else
//     after WorktreeAdd checked for it);
//   - the branch is checked out in some worktree;
//   - the branch no longer points at startOID (it was moved since, so it may
//     hold someone's commits); the deletion is a compare-and-swap
//     (`git update-ref -d <ref> <startOID>`), so this holds even against a
//     concurrent update.
func cleanupCreatedBranch(ctx context.Context, repoDir, branch, startOID string, addErr error) {
	if startOID == "" {
		return
	}
	var ge *GitError
	if errors.As(addErr, &ge) && strings.Contains(ge.Stderr, "a branch named '"+branch+"' already exists") {
		return
	}
	// Fresh reads: this check guards a deletion (see WithFreshReads). Defence
	// in depth: the failed `worktree add` before it is a mutation, which has
	// already invalidated this repository's cached answers.
	cctx, cancel := context.WithTimeout(WithFreshReads(context.WithoutCancel(ctx)), rereadTimeout)
	defer cancel()
	wts, err := WorktreeList(cctx, repoDir)
	if err != nil {
		return
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			return
		}
	}
	_, _ = Run(cctx, repoDir, "update-ref", "-d", "refs/heads/"+branch, startOID)
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
