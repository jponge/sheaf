package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// Recheck re-examines the remotes of repositories that are already
	// registered instead of adopting directories (see Adopt).
	Recheck bool
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
// A directory already registered under its name is skipped (Skip error),
// unless opts.Recheck is set: see Recheck.
func Adopt(ctx context.Context, w *Workspace, dir string, opts AdoptOptions, emit func(Event)) (string, error) {
	if opts.Recheck {
		// A registered repository is named by its directory: a missing one
		// is looked up by name (an unknown name gets the usual message).
		if abs, err := filepath.Abs(dir); err == nil && resolveExisting(filepath.Dir(abs)) == w.Root {
			if _, err := os.Lstat(abs); errors.Is(err, fs.ErrNotExist) {
				name, b := filepath.Base(abs), w.Bundle()
				_, registered := lookupRepo(b, name)
				if _, near := caseVariant(b, name); !registered && !near {
					return "", fmt.Errorf("cannot re-examine %s: %w, and no directory %s", name, repoNotFound(b, name), abs)
				}
				return recheckEntry(ctx, w, name, opts, emit)
			}
		}
	}
	dir, name, err := adoptTarget(w, dir)
	if err != nil {
		return "", err
	}
	if err := bundle.ValidateHost(opts.Host); err != nil {
		return "", fmt.Errorf("cannot adopt %s: %w", dir, err)
	}
	if opts.Recheck {
		return recheckEntry(ctx, w, name, opts, emit)
	}
	b := w.Bundle()
	if _, ok := b.Repository(name); ok {
		return "", Skip(name + " is already registered; use --recheck to re-examine its remotes")
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
	// The hints of the warnings are run after the move: they name the final
	// path of the primary checkout.
	finalPath := func(def string) string {
		if n, err := bundle.NormalizeBranch(def); err == nil {
			return filepath.Join(dir, n)
		}
		return checkout
	}
	entry, warnings, err := describeCheckout(ctx, checkout, finalPath, name, opts.DefaultBranch, opts.Host)
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
	for _, m := range warnings {
		emit(Event{Message: "warning: " + m})
	}
	msg, err := adoptCheckout(ctx, w.Root, layout{plain: plain, dir: dir, checkout: checkout}, entry, register, emit)
	if err != nil || !entry.Forked() {
		return msg, err
	}
	if defDir, nerr := bundle.NormalizeBranch(entry.DefaultBranch); nerr == nil {
		checkDefaultTracking(ctx, filepath.Join(dir, defDir), entry, gitx.ForkRemotes, func(m, hint string) {
			emit(Event{Message: "warning: " + m + "; fix: " + hint})
		})
	}
	return msg + fmt.Sprintf("; recorded as a fork of %s (origin = your fork)", entry.Slug), nil
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
	for _, remote := range gitx.ForkRemotes.All() { // upstream, origin
		if u, err := gitx.RemoteURL(ctx, checkout, remote); err == nil {
			if _, slug, err := ParseRemoteURL(u); err == nil {
				if _, repo, _ := strings.Cut(slug, "/"); strings.EqualFold(repo, filepath.Base(dir)) {
					return nil
				}
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
		return moveIntoSubdir(root, l.dir, defDir)
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
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("cannot adopt %s: no such directory", abs)
	}
	if err != nil {
		return "", "", fmt.Errorf("cannot adopt %s: %w", abs, err)
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

// forkUpstream returns the URL, host and slug of the remote "upstream" of
// checkout when it may be the repository origin was forked from: upstream
// exists, its URL parses (ParseRemoteURL) and has another owner/name than
// origin's (originSlug, case-insensitively). ok is false otherwise.
func forkUpstream(ctx context.Context, checkout, originURL, originSlug string) (url, host, slug string, ok bool) {
	u, err := gitx.RemoteURL(ctx, checkout, gitx.ForkRemotes.Reference)
	if err != nil || u == originURL {
		return "", "", "", false
	}
	h, s, err := ParseRemoteURL(u)
	if err != nil || strings.EqualFold(s, originSlug) {
		return "", "", "", false // the same repository (e.g. another URL form, or a mirror)
	}
	return u, h, s, true
}

// sharedHistory checks that origin/<def> and upstream/<def> both exist in
// checkout and have a merge base, i.e. that origin plausibly is a fork of
// upstream. It returns why not ("" when they share history).
//
// primary is the path of the primary checkout once the repository is adopted
// (the one named in hints), name the repository's directory name.
func sharedHistory(ctx context.Context, checkout, primary, name, def, upstreamSlug string) (string, error) {
	rs := gitx.ForkRemotes
	for _, remote := range []string{rs.Reference, rs.Fork} {
		ok, err := refExists(ctx, checkout, "refs/remotes/"+remote+"/"+def)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("%s/%s is missing locally: cannot check that origin is a fork of upstream (%s); recorded as a single-remote repository "+
				"(to record the fork, run `git -C %s fetch %s`, then `sheaf adopt --recheck %s`)", remote, def, upstreamSlug, primary, remote, name), nil
		}
	}
	_, err := gitx.Run(ctx, checkout, "merge-base", rs.Fork+"/"+def, rs.Reference+"/"+def)
	var ge *gitx.GitError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &ge) && ge.ExitCode == 1 && ctx.Err() == nil:
		return fmt.Sprintf("upstream (%s) shares no history with origin (no merge base between %s/%s and %s/%s): not recorded as the repository of your fork; recorded as a single-remote repository",
			upstreamSlug, rs.Fork, def, rs.Reference, def), nil
	}
	return "", err
}

// describeCheckout builds the bundle entry of the checkout from its origin
// remote, or, when the checkout is set up for a fork (a remote "upstream"
// naming another repository, see forkUpstream, whose default branch shares
// history with origin's, see sharedHistory), from upstream with origin
// recorded as the fork. No hosting service is asked. The warnings explain
// why an upstream remote was not recorded. finalPath maps the default branch
// to the path of the primary checkout after adoption (for hints).
func describeCheckout(ctx context.Context, checkout string, finalPath func(def string) string, name, defaultBranch, hostOverride string) (bundle.Repository, []string, error) {
	url, err := gitx.RemoteURL(ctx, checkout, "origin")
	if err != nil {
		return bundle.Repository{}, nil, fmt.Errorf("no usable origin remote: %w", err)
	}
	host, slug, err := ParseRemoteURL(url)
	if err != nil {
		return bundle.Repository{}, nil, err
	}
	var warnings []string
	defaultOf := func(remote string) (string, error) {
		if defaultBranch != "" {
			return defaultBranch, nil
		}
		def, err := gitx.DefaultBranchOf(ctx, checkout, remote)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("cannot determine the default branch of %s; pass --default-branch <name> (%v)", remote, err)
		}
		return def, nil
	}
	if uu, uh, us, ok := forkUpstream(ctx, checkout, url, slug); ok {
		def, err := defaultOf(gitx.ForkRemotes.Reference)
		if err != nil {
			return bundle.Repository{}, nil, err
		}
		why, err := sharedHistory(ctx, checkout, finalPath(def), name, def, us)
		if err != nil {
			return bundle.Repository{}, nil, err
		}
		if why == "" {
			if hostOverride != "" {
				uh = hostOverride
			}
			if strings.EqualFold(uh, bundle.HostGitHub) {
				uh = ""
			}
			return bundle.Repository{Name: name, Slug: us, URL: uu, DefaultBranch: def, Origin: bundle.OriginAdopted, Host: uh,
				Fork: bundle.Fork{Slug: slug, URL: url}}, nil, nil
		}
		warnings = append(warnings, why)
	}
	if hostOverride != "" {
		host = hostOverride
	}
	if strings.EqualFold(host, bundle.HostGitHub) {
		host = "" // the default, as stored in sheaf.yaml
	}
	def, err := defaultOf(gitx.OriginRemotes.Reference)
	if err != nil {
		return bundle.Repository{}, nil, err
	}
	return bundle.Repository{Name: name, Slug: slug, URL: url, DefaultBranch: def, Origin: bundle.OriginAdopted, Host: host}, warnings, nil
}

// moveIntoSubdir moves dir to dir/sub through a temporary directory in root
// and returns a function that moves it back.
func moveIntoSubdir(root, dir, sub string) (func() error, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dir, sub)
	tmp, err := os.MkdirTemp(root, ".sheaf-adopt-"+filepath.Base(dir)+"-")
	if err != nil {
		return nil, err
	}
	staged := filepath.Join(tmp, "repo")
	if err := osRename(dir, staged); err != nil {
		os.Remove(tmp)
		return nil, err
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
		return nil, restore(err)
	}
	if err := osRename(staged, target); err != nil {
		if rerr := os.Remove(dir); rerr != nil {
			return nil, fmt.Errorf("%w; the clone is still in %s (removing %s failed: %v)", err, staged, dir, rerr)
		}
		return nil, restore(err)
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
	return undo, nil
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

// recheckEntry re-examines the remotes of the registered repository name with the
// recognition rules of Adopt (describeCheckout) and records the fork when
// they now qualify. It never moves a directory and never changes a branch or
// its tracking (a default branch not tracking upstream is reported, with the
// fix). The default branch is the entry's (the primary checkout is named after
// it, and no remote is asked). An entry that already has a fork is never
// changed, even when its remotes no longer qualify: that is warned about.
func recheckEntry(ctx context.Context, w *Workspace, name string, opts AdoptOptions, emit func(Event)) (string, error) {
	b := w.Bundle()
	cur, ok := lookupRepo(b, name)
	if !ok {
		if _, ok := caseVariant(b, name); ok {
			return "", fmt.Errorf("cannot re-examine %s: %w", name, repoNotFound(b, name))
		}
		return "", fmt.Errorf("cannot re-examine %s: %w; adopt it first: sheaf adopt %s", name, repoNotFound(b, name), name)
	}
	primary, _, prob := coCheckPrimary(ctx, w.Root, cur)
	if prob != nil {
		return "", fmt.Errorf("cannot re-examine %s: %s: %w", name, prob.Path, prob.Err)
	}
	found, warnings, err := describeCheckout(ctx, primary, func(string) string { return primary }, name, cur.DefaultBranch, opts.Host)
	if err != nil {
		return "", fmt.Errorf("cannot re-examine %s: %w", name, err)
	}
	for _, m := range warnings {
		emit(Event{Message: "warning: " + m})
	}
	final, msg := cur, "unchanged"
	switch {
	case cur.Forked() && !found.Forked():
		emit(Event{Message: fmt.Sprintf("warning: the remotes of %s no longer qualify as a fork of %s (see above); the recorded fork %s is kept", primary, cur.Slug, cur.Fork.Slug)})
	case cur.Forked() && (found.Slug != cur.Slug || found.URL != cur.URL || found.Fork != cur.Fork || found.HostName() != cur.HostName()):
		emit(Event{Message: fmt.Sprintf("warning: the remotes of %s describe another fork (%s of %s) than the recorded one (%s of %s); the entry is kept", primary, found.Fork.Slug, found.Slug, cur.Fork.Slug, cur.Slug)})
	case !cur.Forked() && found.Forked():
		if e := existingByKey(w.Bundle(), found.Host, found.Slug); e != nil && e.Name != name {
			return "", fmt.Errorf("cannot record %s as a fork of %s: %s is already in the bundle as %s", name, found.Slug, found.Slug, e.Name)
		}
		final = cur
		final.Slug, final.URL, final.Host, final.Fork = found.Slug, found.URL, found.Host, found.Fork
		msg = fmt.Sprintf("recorded as a fork of %s (origin = your fork)", final.Slug)
	}
	if final != cur {
		err := w.updateCtx(ctx, emit, func(b *bundle.Bundle) error {
			e, ok := b.Repository(name)
			if !ok || *e != cur {
				return fmt.Errorf("%s changed in %s meanwhile; rerun the command", name, bundle.FileName)
			}
			if o := existingByKey(b, final.Host, final.Slug); o != nil && o.Name != name {
				return fmt.Errorf("cannot record %s as a fork of %s: %s is already in the bundle as %s", name, final.Slug, final.Slug, o.Name)
			}
			*e = final
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	if final.Forked() {
		checkDefaultTracking(ctx, primary, final, gitx.ForkRemotes, func(m, hint string) {
			emit(Event{Message: "warning: " + m + "; fix: " + hint})
		})
	}
	return msg, nil
}
