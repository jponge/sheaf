package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// Test hooks: adoptAfterMove runs after the primary checkout was relocated
// and its worktrees repaired, adoptAfterSwitch after the primary was
// switched to the default branch; an error (or a panic) triggers the
// rollback.
var (
	adoptAfterMove   = func() error { return nil }
	adoptAfterSwitch = func() error { return nil }
)

// Filesystem seams for tests of the move's failure paths.
var (
	osRename = os.Rename
	osMkdir  = os.Mkdir
)

// rollbackTimeout bounds the git calls of a rollback, which run even when
// the caller's context is done.
const rollbackTimeout = 30 * time.Second

// AdoptOptions configures Adopt.
type AdoptOptions struct {
	// DefaultBranch is origin's default branch. When empty it is read from
	// refs/remotes/origin/HEAD or, if that is missing, asked to the remote;
	// Adopt refuses when both fail.
	DefaultBranch string
	// Host overrides the host derived from the origin URL (an SSH
	// configuration alias such as "github_work" is otherwise recorded
	// as-is). It must be a valid host (bundle.ValidateHost).
	Host string
}

// Adopt registers the existing checkout dir, an immediate subdirectory of the
// bundle root, with origin "adopted", and establishes the bundle layout: the
// primary checkout at dir/<normalized default branch>/, every other branch
// in a sibling worktree.
//
//   - A plain clone (dir/.git is a directory) is moved to
//     dir/<normalized default branch>/.
//   - A laid-out repository has exactly one primary checkout dir/<child>/
//     where <child> is the normalized name of its checked-out branch
//     (case-insensitively); any other layout is refused. It is only
//     registered when that branch is the default branch.
//   - When the checkout is on another branch than the default one, it must
//     be clean (no work-tree-local blocking risk; untracked files count):
//     the primary is placed at dir/<normalized default>/, switched to the
//     default branch (a local branch tracking origin/<default> is created if
//     missing; ignored files are never overwritten and stay in the
//     primary), and the previous branch is checked out again in the sibling
//     worktree dir/<normalized branch>/ (dir/<child>/ for a laid-out repo).
//   - A detached HEAD is refused.
//
// Linked worktrees of the repository are repaired after the move, including
// worktrees nested inside the moved directory.
//
// The owner/name slug and the host are derived from the origin remote URL
// (ParseRemoteURL). Refusals happen before anything is changed. The move
// uses renames only (same filesystem, nothing is copied), and any later
// failure or panic, up to and including registration, is rolled back in
// reverse order to the original state.
//
// A directory already registered under its name is skipped (Skip error).
func Adopt(ctx context.Context, w *Workspace, dir string, opts AdoptOptions, emit func(Event)) (string, error) {
	dir, name, err := adoptTarget(w, dir)
	if err != nil {
		return "", err
	}
	if err := bundle.ValidateHost(opts.Host); err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	b := w.Bundle()
	if _, ok := b.Repository(name); ok {
		return "", Skip(name + " is already registered")
	}
	if err := bundle.ValidateRepoName(name); err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	plain := isPrimaryCheckout(ctx, dir)
	checkout := dir
	if !plain {
		if checkout, err = findPrimary(ctx, dir); err != nil {
			return "", fmt.Errorf("cannot adopt %s: neither a plain clone nor a laid-out repository: %w", dir, err)
		}
	}
	cur, err := gitx.CurrentBranch(ctx, checkout)
	if errors.Is(err, gitx.ErrDetachedHead) {
		return "", fmt.Errorf("cannot adopt %s: HEAD of %s is detached; check out a branch first", dir, checkout)
	}
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	if !plain {
		if err := checkLaidOut(checkout, cur); err != nil {
			return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
		}
	}
	entry, err := describeCheckout(ctx, checkout, name, opts.DefaultBranch, opts.Host)
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	if e := existingByKey(b, entry.Host, entry.Slug); e != nil {
		return "", fmt.Errorf("cannot adopt %s: %s is already in the bundle as %s", dir, entry.Slug, e.Name)
	}
	register := func() error {
		return w.updateCtx(ctx, emit, func(b *bundle.Bundle) error { return b.AddRepository(entry) })
	}
	return adoptCheckout(ctx, w.Root, layout{plain: plain, dir: dir, checkout: checkout, branch: cur}, entry, register, emit)
}

// checkLaidOut checks that the primary checkout of a laid-out repository
// lives in the directory named after its branch.
func checkLaidOut(checkout, branch string) error {
	want, err := bundle.NormalizeBranch(branch)
	if err != nil {
		return err
	}
	if child := filepath.Base(checkout); !strings.EqualFold(child, want) {
		return fmt.Errorf("not laid out as <repo>/<branch>/: %s is on branch %s, which belongs in directory %s; move or rename it first", checkout, branch, want)
	}
	return nil
}

// layout describes the checkout being adopted.
type layout struct {
	plain    bool   // plain clone at dir (otherwise laid out)
	dir      string // the repository directory under the bundle root
	checkout string // the primary checkout (dir for a plain clone)
	branch   string // its checked-out branch
}

// adoptCheckout relocates, switches and registers (see Adopt).
func adoptCheckout(ctx context.Context, root string, l layout, entry bundle.Repository, register func() error, emit func(Event)) (msg string, err error) {
	def := entry.DefaultBranch
	defDir, err := bundle.NormalizeBranch(def)
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", l.dir, err)
	}
	switching := l.branch != def
	if !l.plain && !switching {
		if err := register(); err != nil {
			return "", err
		}
		return fmt.Sprintf("registered %s (%s)", entry.Name, entry.Slug), nil
	}
	primary := filepath.Join(l.dir, defDir)
	var wtDir string
	if switching {
		wtDir, err = branchDirName(l, defDir)
		if err == nil {
			err = checkSwitchable(ctx, l.checkout, def, l.branch)
		}
		if err == nil && !l.plain {
			if _, serr := os.Lstat(primary); serr == nil {
				err = fmt.Errorf("%s already exists", primary)
			}
		}
		if err != nil {
			return "", fmt.Errorf("cannot adopt %s: %w", l.dir, err)
		}
	}
	nested, err := nestedWorktrees(ctx, l.checkout)
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", l.dir, err)
	}

	// From here on every change pushes its inverse; the deferred function
	// runs them in reverse order on error or panic.
	var rollback []func(rctx context.Context) error
	defer func() {
		r := recover()
		if err == nil && r == nil {
			return
		}
		cause := err
		if r != nil {
			cause = fmt.Errorf("panic: %v", r)
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		var rerr error
		for i := len(rollback) - 1; i >= 0 && rerr == nil; i-- {
			rerr = rollback[i](rctx)
		}
		if r != nil {
			panic(r) // restored as far as possible; the executor reports it
		}
		if rerr != nil {
			err = fmt.Errorf("adopting %s failed: %w; rolling back also failed, fix manually: %v", l.dir, cause, rerr)
			return
		}
		err = fmt.Errorf("adopting %s failed (restored): %w", l.dir, cause)
	}()

	emit(Event{Message: "moving " + l.checkout + " to " + primary})
	undo, err := relocate(root, l, defDir)
	if err != nil {
		return "", err
	}
	rollback = append(rollback, func(rctx context.Context) error {
		if err := undo(); err != nil {
			return fmt.Errorf("moving %s back to %s failed: %v", primary, l.checkout, err)
		}
		if err := gitx.WorktreeRepair(rctx, l.checkout, joinAll(l.checkout, nested)...); err != nil {
			return fmt.Errorf("%s was moved back but repairing its worktrees failed: %v", l.checkout, err)
		}
		return nil
	})
	if err = gitx.WorktreeRepair(ctx, primary, joinAll(primary, nested)...); err != nil {
		return "", err
	}
	if err = adoptAfterMove(); err != nil {
		return "", err
	}
	if !switching {
		if err = register(); err != nil {
			return "", err
		}
		return fmt.Sprintf("moved to %s and registered %s (%s)", primary, entry.Name, entry.Slug), nil
	}

	created, err := localBranchMissing(ctx, primary, def)
	if err != nil {
		return "", err
	}
	rollback = append(rollback, func(rctx context.Context) error { return restoreBranch(rctx, primary, l.branch, def, created) })
	emit(Event{Message: "checking out " + def + " in " + primary})
	if err = checkoutDefault(ctx, primary, def, created); err != nil {
		return "", err
	}
	if err = adoptAfterSwitch(); err != nil {
		return "", err
	}
	wt := filepath.Join(l.dir, wtDir)
	emit(Event{Worktree: wtDir, Message: "creating worktree " + wt + " for " + l.branch})
	if _, err = gitx.WorktreeAdd(ctx, primary, wt, l.branch, gitx.WorktreeAddOpts{}); err != nil {
		return "", err
	}
	rollback = append(rollback, func(rctx context.Context) error {
		if err := gitx.WorktreeRemove(rctx, primary, wt, false); err != nil {
			return fmt.Errorf("removing worktree %s failed: %v", wt, err)
		}
		return nil
	})
	if err = register(); err != nil {
		return "", err
	}
	if ignored, ierr := hasIgnoredFiles(ctx, primary); ierr == nil && ignored {
		emit(Event{Message: "ignored files (e.g. build output, local configuration) stay in " + primary + "; they were not copied to " + wt})
	}
	return fmt.Sprintf("primary %s on %s, %s checked out in %s, registered %s (%s)", primary, def, l.branch, wt, entry.Name, entry.Slug), nil
}

// branchDirName returns the worktree directory for the checked-out branch:
// the existing child for a laid-out repository, its normalized name for a
// plain clone. It must differ from the default branch's directory.
func branchDirName(l layout, defDir string) (string, error) {
	name := filepath.Base(l.checkout)
	if l.plain {
		n, err := bundle.NormalizeBranch(l.branch)
		if err != nil {
			return "", err
		}
		name = n
	}
	if strings.EqualFold(name, defDir) {
		return "", fmt.Errorf("branch %q and the default branch both map to directory %q", l.branch, defDir)
	}
	return name, nil
}

// relocate moves the primary checkout to dir/defDir and returns the inverse
// operation.
func relocate(root string, l layout, defDir string) (func() error, error) {
	if l.plain {
		_, undo, err := moveIntoSubdir(root, l.dir, defDir)
		return undo, err
	}
	target := filepath.Join(l.dir, defDir)
	if err := osRename(l.checkout, target); err != nil {
		return nil, err
	}
	return func() error { return osRename(target, l.checkout) }, nil
}

// nestedWorktrees returns the linked worktrees located inside checkout,
// relative to it: they move with it and must be passed to `git worktree
// repair`, which cannot find them by itself.
func nestedWorktrees(ctx context.Context, checkout string) ([]string, error) {
	wts, err := gitx.WorktreeList(ctx, checkout)
	if err != nil {
		return nil, err
	}
	var rel []string
	for _, wt := range wts[1:] {
		p := filepath.Clean(wt.Path)
		if within(checkout, p) {
			r, err := filepath.Rel(checkout, p)
			if err != nil {
				return nil, err
			}
			rel = append(rel, r)
		}
	}
	return rel, nil
}

func joinAll(base string, rel []string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = filepath.Join(base, r)
	}
	return out
}

// hasIgnoredFiles reports whether the work tree in dir has ignored entries.
func hasIgnoredFiles(ctx context.Context, dir string) (bool, error) {
	out, err := gitx.Run(ctx, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	return strings.Trim(out, "\x00") != "", err
}

// adoptTarget checks that dir is a real directory (not a symlink) directly
// under the bundle root and returns its absolute path and its name.
func adoptTarget(w *Workspace, dir string) (string, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", "", err
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("cannot adopt %s: not a directory (symbolic links are not followed)", abs)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", err
	}
	if parent != w.Root {
		return "", "", fmt.Errorf("cannot adopt %s: it must be directly inside the bundle %s", abs, w.Root)
	}
	name := filepath.Base(abs)
	return filepath.Join(w.Root, name), name, nil
}

// describeCheckout builds the bundle entry of the checkout from its origin.
func describeCheckout(ctx context.Context, checkout, name, defaultBranch, hostOverride string) (bundle.Repository, error) {
	url, err := gitx.RemoteURL(ctx, checkout, "origin")
	if err != nil {
		return bundle.Repository{}, fmt.Errorf("no usable origin remote: %w", err)
	}
	host, slug, err := ParseRemoteURL(url)
	if err != nil {
		return bundle.Repository{}, err
	}
	if hostOverride != "" {
		host = hostOverride
	}
	if strings.EqualFold(host, bundle.HostGitHub) {
		host = "" // the default, as stored in sheaf.yaml
	}
	def := defaultBranch
	if def == "" {
		if def, err = gitx.DefaultBranch(ctx, checkout); err != nil {
			if ctx.Err() != nil {
				return bundle.Repository{}, ctx.Err()
			}
			return bundle.Repository{}, fmt.Errorf("cannot determine the default branch of origin; pass --default-branch <name> (%v)", err)
		}
	}
	return bundle.Repository{Name: name, Slug: slug, URL: url, DefaultBranch: def, Origin: bundle.OriginAdopted, Host: host}, nil
}

// checkSwitchable checks, before anything is moved, that the checkout (on
// branch cur) can switch to def: a clean work tree, and def not checked out
// in a linked worktree.
func checkSwitchable(ctx context.Context, checkout, def, cur string) error {
	risks, err := gitx.DeletionRisks(ctx, checkout)
	if err != nil {
		return err
	}
	if !gitx.SafeToRemoveWorktree(risks) {
		var parts []string
		for _, r := range risks {
			if !r.Informational() && !r.Kind.RepositoryWide() {
				parts = append(parts, r.String())
			}
		}
		return fmt.Errorf("it is on branch %s, not on the default branch %s, and its work tree is not clean (%s): commit or stash first, or check out %s", cur, def, strings.Join(parts, ", "), def)
	}
	wts, err := gitx.WorktreeList(ctx, checkout)
	if err != nil {
		return err
	}
	for _, wt := range wts[1:] {
		if wt.Branch == def {
			return fmt.Errorf("the default branch %s is already checked out in the worktree %s", def, wt.Path)
		}
	}
	return nil
}

func localBranchMissing(ctx context.Context, dir, branch string) (bool, error) {
	_, err := gitx.Run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	var ge *gitx.GitError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &ge) && ge.ExitCode == 1:
		return true, nil
	}
	return false, err
}

// checkoutDefault checks out def in dir, creating it from origin/<def> with
// tracking when create is set. Ignored files are never overwritten (git then
// refuses the checkout).
func checkoutDefault(ctx context.Context, dir, def string, create bool) error {
	if create {
		_, err := gitx.Run(ctx, dir, "checkout", "--quiet", "--no-overwrite-ignore", "--track", "-b", def, "origin/"+def)
		return err
	}
	_, err := gitx.Run(ctx, dir, "checkout", "--quiet", "--no-overwrite-ignore", def, "--")
	return err
}

// restoreBranch checks cur out again in dir and deletes def if adopt
// created it.
func restoreBranch(ctx context.Context, dir, cur, def string, created bool) error {
	if b, err := gitx.CurrentBranch(ctx, dir); err != nil || b != cur {
		if _, err := gitx.Run(ctx, dir, "checkout", "--quiet", "--no-overwrite-ignore", cur, "--"); err != nil {
			return fmt.Errorf("checking out %s again failed: %v", cur, err)
		}
	}
	if created {
		if missing, err := localBranchMissing(ctx, dir, def); err == nil && !missing {
			if _, err := gitx.Run(ctx, dir, "branch", "--quiet", "-D", "--", def); err != nil {
				return fmt.Errorf("deleting branch %s created by adopt failed: %v", def, err)
			}
		}
	}
	return nil
}

// moveIntoSubdir moves dir to dir/sub through a temporary directory in root
// and returns the new path and a function that moves it back.
func moveIntoSubdir(root, dir, sub string) (string, func() error, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", nil, err
	}
	target := filepath.Join(dir, sub)
	tmp, err := os.MkdirTemp(root, ".sheaf-adopt-"+filepath.Base(dir)+"-")
	if err != nil {
		return "", nil, err
	}
	staged := filepath.Join(tmp, "repo")
	if err := osRename(dir, staged); err != nil {
		os.Remove(tmp)
		return "", nil, err
	}
	// From here on dir is missing: every failure must put it back.
	restore := func(cause error) error {
		if err := osRename(staged, dir); err != nil {
			return fmt.Errorf("%w; restoring %s from %s failed: %v", cause, dir, staged, err)
		}
		os.Remove(tmp)
		return cause
	}
	if err := osMkdir(dir, fi.Mode().Perm()); err != nil {
		return "", nil, restore(err)
	}
	if err := osRename(staged, target); err != nil {
		if rerr := os.Remove(dir); rerr != nil {
			return "", nil, fmt.Errorf("%w; the clone is still in %s (removing %s failed: %v)", err, staged, dir, rerr)
		}
		return "", nil, restore(err)
	}
	os.Remove(tmp)
	undo := func() error {
		back, err := os.MkdirTemp(root, ".sheaf-adopt-"+filepath.Base(dir)+"-")
		if err != nil {
			return err
		}
		staged := filepath.Join(back, "repo")
		if err := osRename(target, staged); err != nil {
			os.Remove(back)
			return err
		}
		if err := os.Remove(dir); err != nil {
			if rerr := osRename(staged, target); rerr != nil {
				return fmt.Errorf("%w; the clone is in %s", err, staged)
			}
			os.Remove(back)
			return err
		}
		if err := osRename(staged, dir); err != nil {
			return fmt.Errorf("%w; the clone is in %s", err, staged)
		}
		os.Remove(back)
		return nil
	}
	return target, undo, nil
}

// AdoptAll adopts each of dirs (see Adopt) with at most jobs in parallel.
// Directories given more than once (after making them absolute and clean)
// are adopted once. Results are named after the directory base names.
func AdoptAll(ctx context.Context, w *Workspace, dirs []string, opts AdoptOptions, jobs int, rep Reporter) Summary {
	var tasks []Task
	seen := map[string]bool{}
	for _, d := range dirs {
		key := filepath.Clean(d)
		if abs, err := filepath.Abs(d); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		tasks = append(tasks, Task{Repo: filepath.Base(key), Phase: PhaseAdopt, Run: func(ctx context.Context, emit func(Event)) (string, error) {
			return Adopt(ctx, w, d, opts, emit)
		}})
	}
	return Execute(ctx, jobs, tasks, rep)
}
