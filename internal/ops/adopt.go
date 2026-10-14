package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// adoptAfterMove is a test hook run after the primary checkout was
// relocated and its worktrees repaired; an error (or a panic) triggers the
// rollback.
var adoptAfterMove = func() error { return nil }

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
// primary checkout lives in dir/<normalized default branch>/, whatever branch
// it has checked out (or a detached HEAD); other branches live in sibling
// worktrees. Nothing is ever checked out, switched or created: the checkout
// is only moved, so its state (branch, changes, untracked and ignored files)
// is irrelevant and preserved.
//
//   - A plain clone (dir/.git is a directory) is moved to
//     dir/<normalized default branch>/.
//   - A laid-out repository has exactly one primary checkout dir/<child>/.
//     When <child> is the normalized default branch (case-insensitively) it
//     is only registered; otherwise it is renamed to that directory, which
//     must not exist. To avoid adopting an unrelated clone that merely sits
//     in a directory (docs/somerepo/), <child> must be named after the
//     checkout's current or default branch, or the origin repository must be
//     named like dir (case-insensitively); see laidOutPlausible.
//
// Linked worktrees of the repository are repaired after a move, including
// worktrees nested inside the moved directory.
//
// The owner/name slug and the host are derived from the origin remote URL
// (ParseRemoteURL). Refusals happen before anything is changed. The move
// uses renames only (same filesystem, nothing is copied), and any later
// failure, cancellation or panic, up to and including registration, is
// rolled back.
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
	entry, err := describeCheckout(ctx, checkout, name, opts.DefaultBranch, opts.Host)
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	if !plain {
		if err := laidOutPlausible(ctx, dir, checkout, entry.DefaultBranch); err != nil {
			return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
		}
	}
	if e := existingByKey(b, entry.Host, entry.Slug); e != nil {
		return "", fmt.Errorf("cannot adopt %s: %s is already in the bundle as %s", dir, entry.Slug, e.Name)
	}
	register := func() error {
		return w.updateCtx(ctx, emit, func(b *bundle.Bundle) error { return b.AddRepository(entry) })
	}
	return adoptCheckout(ctx, w.Root, layout{plain: plain, dir: dir, checkout: checkout}, entry, register, emit)
}

// laidOutPlausible accepts checkout, the single primary checkout found in
// dir, as dir's primary when its directory is named after its default
// branch (def, may be empty when unknown) or its current branch, or when its
// origin repository is named like dir (all case-insensitively).
func laidOutPlausible(ctx context.Context, dir, checkout, def string) error {
	child := filepath.Base(checkout)
	matches := func(branch string) bool {
		n, err := bundle.NormalizeBranch(branch)
		return err == nil && strings.EqualFold(child, n)
	}
	if def != "" && matches(def) {
		return nil
	}
	if cur, err := gitx.CurrentBranch(ctx, checkout); err == nil && matches(cur) {
		return nil
	}
	if u, err := gitx.RemoteURL(ctx, checkout, "origin"); err == nil {
		if _, slug, err := ParseRemoteURL(u); err == nil {
			if _, repo, _ := strings.Cut(slug, "/"); strings.EqualFold(repo, filepath.Base(dir)) {
				return nil
			}
		}
	}
	return fmt.Errorf("%s is the only checkout in %s, but neither its branches nor its origin repository name match %q or %q, so it does not look like the primary checkout of %s; adopt the checkout itself (move it directly under the bundle) or rename it to <repo>/<default branch>/",
		checkout, dir, child, filepath.Base(dir), filepath.Base(dir))
}

// layout describes the checkout being adopted.
type layout struct {
	plain    bool   // plain clone at dir (otherwise laid out)
	dir      string // the repository directory under the bundle root
	checkout string // the primary checkout (dir for a plain clone)
}

// adoptCheckout relocates (if needed) and registers (see Adopt).
func adoptCheckout(ctx context.Context, root string, l layout, entry bundle.Repository, register func() error, emit func(Event)) (msg string, err error) {
	defDir, err := bundle.NormalizeBranch(entry.DefaultBranch)
	if err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", l.dir, err)
	}
	primary := filepath.Join(l.dir, defDir)
	if !l.plain {
		if strings.EqualFold(filepath.Base(l.checkout), defDir) {
			if err := register(); err != nil {
				return "", err
			}
			return fmt.Sprintf("registered %s (%s)", entry.Name, entry.Slug), nil
		}
		if _, serr := os.Lstat(primary); serr == nil {
			return "", fmt.Errorf("cannot adopt %s: %s must become %s, which already exists", l.dir, l.checkout, primary)
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
	if err = register(); err != nil {
		return "", err
	}
	return fmt.Sprintf("moved to %s and registered %s (%s)", primary, entry.Name, entry.Slug), nil
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
