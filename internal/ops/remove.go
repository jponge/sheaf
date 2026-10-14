package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// ErrNotConfirmed is returned (wrapped) by Remove when the removal was not
// confirmed.
var ErrNotConfirmed = errors.New("removal not confirmed")

// RemoveOptions configures Remove.
type RemoveOptions struct {
	// Force overrides the refusals caused by data at risk (Item.Hard false).
	Force bool
	// Offline skips the fetch.
	Offline bool
	// WorkingDir is the caller's working directory; the repository is not
	// removed while it is inside. Empty means os.Getwd.
	WorkingDir string
	// Confirm is called with the plan once Remove has decided it may
	// proceed; returning false aborts with ErrNotConfirmed. nil never
	// confirms. A TUI asks the user here; the CLI answers with --yes.
	Confirm func(RemovePlan) bool
}

// RemoveItem is one path of a repository directory being removed, with what
// deleting it would lose.
type RemoveItem struct {
	Path   string
	Branch string // checked-out branch, empty for non-checkouts and detached HEADs
	// Risks lists the data at risk. For linked worktrees only the
	// work-tree-local kinds are listed; repository-wide kinds are listed on
	// the primary checkout.
	Risks []gitx.Risk
	// Problem describes an issue other than Risks (e.g. an unknown entry, a
	// failed fetch), empty if none.
	Problem string
	// Hard marks a problem that Force does not override.
	Hard bool
	// Worktree is true for a linked worktree inside the repository
	// directory (removed with git worktree remove).
	Worktree bool
}

// blocking reports whether the item prevents a removal without Force.
func (it RemoveItem) blocking() bool { return it.Problem != "" || !gitx.SafeToDelete(it.Risks) }

// RemovePlan describes what Remove deletes and what would be lost.
type RemovePlan struct {
	Repo  string
	Dir   string // the repository directory; empty when it does not exist
	Items []RemoveItem
	// Primary is the primary checkout, empty if none was found.
	Primary string
}

// Refused reports whether Remove refuses the plan with the given force.
func (p RemovePlan) Refused(force bool) bool {
	for _, it := range p.Items {
		if it.Hard || !force && it.blocking() {
			return true
		}
	}
	return false
}

// RisksError is returned when Remove refuses because of the plan's items.
type RisksError struct {
	Plan  RemovePlan
	Force bool
}

func (e *RisksError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "refusing to remove %s:", e.Plan.Repo)
	hard := false
	for _, it := range e.Plan.Items {
		if !it.Hard && (e.Force || !it.blocking()) {
			continue
		}
		hard = hard || it.Hard
		sb.WriteString("\n  " + it.Path)
		if it.Branch != "" {
			sb.WriteString(" [" + it.Branch + "]")
		}
		var parts []string
		for _, r := range it.Risks {
			if !r.Informational() {
				parts = append(parts, r.String())
			}
		}
		if it.Problem != "" {
			parts = append(parts, it.Problem)
		}
		sb.WriteString(": " + strings.Join(parts, ", "))
	}
	if hard {
		sb.WriteString("\nsome problems cannot be overridden with --force; fix them first")
	} else {
		sb.WriteString("\npass --force to remove anyway (the data listed above will be lost)")
	}
	return sb.String()
}

// Remove deletes the repository name from the bundle: its linked worktrees
// (git worktree remove), its directory <root>/<name> and its sheaf.yaml
// entry, in that order.
//
// Before deleting anything it fetches origin (so that commits whose remote
// branch was deleted count as unpushed), then gathers gitx.DeletionRisks for
// the primary checkout and every worktree, fetching every nested repository
// that has a remote (all its remotes, at every nesting level) before it is
// assessed (remotes inside the repository directory do not count; with
// Offline nothing is fetched), and lists the entries of the repository
// directory that belong to no checkout. Data at risk, unknown
// entries, a failed fetch, a missing primary checkout or a locked worktree
// make Remove refuse with a *RisksError unless opts.Force is set. Worktrees
// outside the repository directory and a repository directory that is a
// symbolic link are always refused: Remove only ever deletes inside
// <root>/<name>, a real directory directly under the bundle root (symbolic
// links inside it are removed, never followed).
//
// The risk check and the deletion are not atomic: changes made in between
// are lost.
func Remove(ctx context.Context, w *Workspace, name string, opts RemoveOptions, emit func(Event)) (string, error) {
	if _, ok := w.Bundle().Repository(name); !ok {
		return "", fmt.Errorf("no repository named %q in the bundle", name)
	}
	// The fetch happens once; the re-check after confirmation reuses its
	// outcome.
	fetched, fetchMsg := false, ""
	fetch := func(primary string) (string, error) {
		if fetched {
			return fetchMsg, nil
		}
		fetched = true
		if opts.Offline {
			emit(Event{Phase: PhaseFetch, Message: "offline: not fetching the repository nor its nested repositories; the remote state may be stale (commits whose remote branch was deleted are not detected)"})
			return "", nil
		}
		emit(Event{Phase: PhaseFetch, Message: "fetching origin"})
		if err := gitx.Fetch(ctx, primary); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fetchMsg = fetchProblem(err)
		}
		return fetchMsg, nil
	}
	wd := opts.WorkingDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	// Nested repositories are fetched once each (unless offline), when the
	// risk check reaches them; the re-check reuses the outcome.
	var fetchNested func(context.Context, string) error
	if !opts.Offline {
		fetchedNested := map[string]error{}
		fetchNested = func(ctx context.Context, dir string) error {
			if err, ok := fetchedNested[dir]; ok {
				return err
			}
			emit(Event{Phase: PhaseFetch, Message: "fetching nested repository " + dir})
			err := gitx.FetchAll(ctx, dir)
			if ctx.Err() == nil {
				fetchedNested[dir] = err
			}
			return err
		}
	}
	risksOf := func(ctx context.Context, dir, deletedRoot string) ([]gitx.Risk, error) {
		return gitx.DeletionRisksWithOptions(ctx, dir, gitx.DeletionRisksOptions{BeforeNested: fetchNested, DeletedRoot: deletedRoot})
	}
	plan, err := planRemoval(ctx, w, name, wd, fetch, risksOf, emit)
	if err != nil {
		return "", err
	}
	if plan.Refused(opts.Force) {
		return "", &RisksError{Plan: plan, Force: opts.Force}
	}
	if opts.Confirm == nil || !opts.Confirm(plan) {
		return "", fmt.Errorf("%w: %s", ErrNotConfirmed, name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Changes made while Confirm was pending (e.g. a TUI dialog) would be
	// deleted unchecked: check again, without fetching, and abort if
	// anything differs.
	again, err := planRemoval(ctx, w, name, wd, fetch, risksOf, func(Event) {})
	if err != nil {
		return "", err
	}
	if again.Dir != plan.Dir || again.Primary != plan.Primary || !reflect.DeepEqual(blockingView(again), blockingView(plan)) {
		return "", fmt.Errorf("%s changed while waiting for confirmation; nothing was removed, run remove again", name)
	}
	if plan.Dir != "" {
		if err := deleteRepoDir(ctx, w.Root, plan, emit); err != nil {
			return "", err
		}
	}
	unregister := func(b *bundle.Bundle) error { return b.RemoveRepository(name) }
	if err := w.updateCtx(ctx, emit, unregister); err != nil {
		if ctx.Err() == nil || plan.Dir == "" {
			return "", err
		}
		// The directory is gone: do not leave its entry behind because of a
		// cancellation; retry once, briefly, ignoring it.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registrationRetryTimeout)
		defer cancel()
		if rerr := w.updateCtx(rctx, emit, unregister); rerr != nil {
			return "", fmt.Errorf("%s was deleted but removing its entry from %s failed: %w (run remove again)", plan.Dir, bundle.FileName, rerr)
		}
	}
	if plan.Dir == "" {
		return "removed " + name + " from the bundle (its directory did not exist)", nil
	}
	return "removed " + plan.Dir, nil
}

// repoDirOf returns <root>/<name> if it is a real directory directly under
// root, "" if it does not exist, and an error otherwise.
func repoDirOf(root, name string) (string, error) {
	if err := bundle.ValidateRepoName(name); err != nil {
		return "", err
	}
	dir := bundle.RepoDir(root, name)
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing to remove %s: it is a symbolic link", dir)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("refusing to remove %s: not a directory", dir)
	}
	if r, err := filepath.EvalSymlinks(dir); err != nil || r != dir {
		return "", fmt.Errorf("refusing to remove %s: it does not resolve to itself (%s, %v)", dir, r, err)
	}
	return dir, nil
}

// within reports whether p is dir or below it (both clean and absolute).
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// blockingView keeps what decides a removal: paths, problems and blocking
// risks; informational risks (ignored files, tag-only commits, clean nested
// repositories) may change meanwhile without consequence.
func blockingView(p RemovePlan) []RemoveItem {
	out := make([]RemoveItem, len(p.Items))
	for i, it := range p.Items {
		it.Risks = slices.DeleteFunc(slices.Clone(it.Risks), func(r gitx.Risk) bool { return r.Informational() })
		if len(it.Risks) == 0 {
			it.Risks = nil
		}
		out[i] = it
	}
	return out
}

// resolveExisting resolves symlinks in the longest existing prefix of p
// (p may not exist, e.g. a working directory that was deleted).
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, rest...)...)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// planRemoval builds the removal plan. fetch is called with the primary
// checkout before the risk checks and returns a problem description (empty
// if none).
func planRemoval(ctx context.Context, w *Workspace, name, wd string, fetch func(primary string) (string, error),
	risksOf func(ctx context.Context, dir, deletedRoot string) ([]gitx.Risk, error), emit func(Event)) (RemovePlan, error) {
	plan := RemovePlan{Repo: name}
	dir, err := repoDirOf(w.Root, name)
	if err != nil {
		return plan, err
	}
	if dir == "" {
		return plan, nil
	}
	plan.Dir = dir
	if wd != "" {
		if r := resolveExisting(wd); within(dir, r) {
			plan.Items = append(plan.Items, RemoveItem{Path: dir, Hard: true,
				Problem: "it contains the current working directory " + r + "; change to another directory first"})
		}
	}
	found, err := primaries(ctx, dir)
	if err != nil {
		return plan, err
	}
	switch len(found) {
	case 0:
		entries, err := significantEntries(dir)
		if err != nil {
			return plan, err
		}
		if len(entries) > 0 {
			plan.Items = append(plan.Items, RemoveItem{Path: dir, Hard: true, Problem: fmt.Sprintf(
				"no primary checkout found (entries: %s); inspect it and delete it manually if nothing is needed", strings.Join(entries, ", "))})
		}
		return plan, nil
	case 1:
	default:
		plan.Items = append(plan.Items, RemoveItem{Path: dir, Hard: true, Problem: fmt.Sprintf(
			"several primary checkouts found (%s); sheaf cannot tell which repository this is, sort it out manually", strings.Join(found, ", "))})
		return plan, nil
	}
	primary := found[0]
	plan.Primary = primary

	fetchMsg, err := fetch(primary)
	if err != nil {
		return plan, err
	}
	if fetchMsg != "" {
		plan.Items = append(plan.Items, RemoveItem{Path: primary, Problem: fetchMsg, Hard: true})
	}
	emit(Event{Phase: PhaseCheck, Message: "checking for data at risk"})
	wts, err := gitx.WorktreeList(ctx, primary)
	if err != nil {
		return plan, err
	}
	known := []string{primary}
	for i, wt := range wts {
		if wt.Bare {
			continue
		}
		it := RemoveItem{Path: filepath.Clean(wt.Path), Branch: wt.Branch}
		if wt.Prunable {
			// Its directory is gone: nothing to lose there, git prunes it.
			continue
		}
		resolved, rerr := filepath.EvalSymlinks(it.Path)
		if rerr != nil || !within(dir, it.Path) || !within(dir, resolved) {
			it.Problem, it.Hard = "worktree outside "+dir+"; remove or move it first", true
			plan.Items = append(plan.Items, it)
			continue
		}
		known = append(known, it.Path)
		risks, err := risksOf(ctx, it.Path, dir)
		if err != nil {
			return plan, err
		}
		if i > 0 {
			risks = slices.DeleteFunc(risks, func(r gitx.Risk) bool { return r.Kind.RepositoryWide() })
		}
		it.Risks = risks
		it.Worktree = i > 0
		if wt.Locked {
			it.Problem = "locked"
			if wt.LockReason != "" {
				it.Problem += " (" + wt.LockReason + ")"
			}
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, addUnknownEntries(dir, known, &plan)
}

// fetchProblem describes a failed fetch. A lock file left behind (or held
// by a concurrent git process) gets a short message with its path and what
// to do, instead of git's long stderr.
func fetchProblem(err error) string {
	if lock, ok := gitx.StaleLockPath(err); ok {
		return "fetch failed, the remote state is unknown: stale lock file " + lock +
			" (if no other git process is running in this repository, delete it and retry), or pass --offline to check local data only"
	}
	return "fetch failed, the remote state is unknown: " + err.Error() +
		"; retry when the remote is reachable, or pass --offline to check local data only"
}

func joinProblem(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// registrationRetryTimeout bounds the non-cancellable retry of the final
// sheaf.yaml update after the repository directory was deleted.
const registrationRetryTimeout = 5 * time.Second

// osJunk lists files that operating systems create on their own in any
// directory; they are not unknown entries.
var osJunk = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true}

// significantEntries lists the names in dir other than OS junk.
func significantEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !osJunk[e.Name()] {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// addUnknownEntries adds an item for each entry of dir that is none of the
// known checkouts.
func addUnknownEntries(dir string, known []string, plan *RemovePlan) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !slices.Contains(known, p) && !osJunk[e.Name()] {
			plan.Items = append(plan.Items, RemoveItem{Path: p, Problem: "not a checkout of this repository"})
		}
	}
	return nil
}

func deleteRepoDir(ctx context.Context, root string, plan RemovePlan, emit func(Event)) error {
	// Re-check: the directory must still be a real directory under root.
	dir, err := repoDirOf(root, plan.Repo)
	if err != nil {
		return err
	}
	if dir == "" || dir != plan.Dir || filepath.Dir(dir) != root {
		return fmt.Errorf("refusing to remove %s: it changed since it was checked", plan.Dir)
	}
	if plan.Primary != "" {
		for _, it := range plan.Items {
			if !it.Worktree || !within(dir, it.Path) {
				continue
			}
			emit(Event{Phase: PhaseRemove, Worktree: filepath.Base(it.Path), Message: "removing worktree " + it.Path})
			if err := gitx.WorktreeRemove(ctx, plan.Primary, it.Path, true); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				emit(Event{Phase: PhaseRemove, Worktree: filepath.Base(it.Path), Message: "git worktree remove failed, deleting the directory instead: " + err.Error()})
			}
		}
	}
	emit(Event{Phase: PhaseRemove, Message: "deleting " + dir})
	return os.RemoveAll(dir)
}
