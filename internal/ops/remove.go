package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

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
	// ExpectedGone, when set to a slug, tells that the hosting service no
	// longer knows the repository: a fetch that succeeds then contradicts
	// it (typically a token that cannot see the repository) and is a
	// refusal that only Force overrides.
	ExpectedGone string
	// Interactive fetches in interactive git mode (see gitx.Net).
	Interactive bool
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
// For a repository with a fork (see RemotesOf), upstream is fetched like
// origin below, then the fork: a fork that appears to be gone is a data
// risk only when commits of local branches exist on it alone (see
// fetchFork); commits on any remote-tracking ref count as pushed.
//
// Before deleting anything it fetches origin (so that commits whose remote
// branch was deleted count as unpushed), then gathers gitx.DeletionRisks for
// the primary checkout and every worktree, fetching every nested repository
// that has a remote (all its remotes, at every nesting level) before it is
// assessed (remotes inside the repository directory do not count; with
// Offline nothing is fetched), and lists the entries of the repository
// directory that belong to no checkout. Data at risk, unknown entries or a
// gone remote make Remove refuse with a *RisksError unless opts.Force is
// set. Other failed fetches, a missing primary checkout, locked worktrees,
// worktrees outside the repository directory and a repository directory
// that is a symbolic link are always refused: Remove only ever deletes inside
// <root>/<name>, a real directory directly under the bundle root (symbolic
// links inside it are removed, never followed).
//
// Once confirmed, the plan is checked again under the bundle lock, which
// is held until the deletion and the unregistration are done (other sheaf
// processes wait); changes made by other programs between that re-check and
// the deletion are lost.
func Remove(ctx context.Context, w *Workspace, name string, opts RemoveOptions, emit func(Event)) (string, error) {
	ctx = withCommandCache(ctx)
	entry, err := repoNamed(w.Bundle(), name)
	if err != nil {
		return "", err
	}
	rs := RemotesOf(entry)
	// The fetch happens once; the re-check after confirmation reuses its
	// outcome.
	// fetchForceable: the fetch problem is one that Force overrides (a gone
	// remote, or a remote that answers although ExpectedGone says it is gone).
	fetched, fetchMsg, fetchForceable := false, "", false
	// published: the recorded heads of pull request branches (see
	// publishedPullHeads), set by fetch.
	var published []string
	net := gitx.Net{Interactive: opts.Interactive}
	fetch := func(primary string) (string, error) {
		if fetched {
			return fetchMsg, nil
		}
		fetched = true
		if opts.Offline {
			emit(Event{Phase: PhaseFetch, Message: "offline: not fetching the repository nor its nested repositories; the remote state may be stale (commits whose remote branch was deleted are not detected)"})
			var err error
			published, err = publishedPullHeads(ctx, net, primary, rs.Reference, false, emit)
			return "", err
		}
		emit(Event{Phase: PhaseFetch, Message: "fetching " + rs.Reference})
		if err := net.FetchRemote(ctx, primary, rs.Reference); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fetchMsg = fetchProblem(err)
			var ge *gitx.GitError
			if errors.As(err, &ge) && remoteRepoGoneFor(ge.Stderr, rs.Reference) {
				fetchMsg, fetchForceable = goneRemoteProblem, true
			}
		} else {
			if published, err = publishedPullHeads(ctx, net, primary, rs.Reference, true, emit); err != nil {
				return "", err
			}
			if opts.ExpectedGone != "" {
				fetchMsg, fetchForceable = "the GitHub API cannot see "+opts.ExpectedGone+" but its remote still answers: check gh authentication scope (not removed)", true
			}
		}
		if !rs.Forked() {
			return fetchMsg, nil
		}
		msg, forceable, err := fetchFork(ctx, net, primary, rs.Fork, emit)
		if err != nil {
			return "", err
		}
		if msg != "" {
			// Force overrides the combination only if it overrides both.
			fetchForceable = forceable && (fetchMsg == "" || fetchForceable)
			fetchMsg = joinProblem(fetchMsg, msg)
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
			err := net.FetchAll(ctx, dir)
			if ctx.Err() == nil {
				fetchedNested[dir] = err
			}
			return err
		}
	}
	risksOf := func(ctx context.Context, dir, deletedRoot string) ([]gitx.Risk, error) {
		return gitx.DeletionRisksWithOptions(ctx, dir, gitx.DeletionRisksOptions{BeforeNested: fetchNested, DeletedRoot: deletedRoot, PublishedRefs: published, ForeignWorktrees: true})
	}
	plan, err := planRemoval(ctx, w, name, wd, fetch, func() bool { return fetchForceable }, risksOf, emit)
	if err != nil {
		return "", err
	}
	if plan.Refused(opts.Force) {
		if err := nameUnpushedBranches(ctx, &plan, published); err != nil {
			return "", err
		}
		return "", &RisksError{Plan: plan, Force: opts.Force}
	}
	if opts.Confirm == nil || !opts.Confirm(plan) {
		return "", fmt.Errorf("%w: %s", ErrNotConfirmed, name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The re-check, the deletion and the unregistration run under the
	// bundle lock, serialized with other sheaf processes and tasks (like wt
	// rm and prune), and git is read afresh: nothing cached before or while
	// Confirm was pending is trusted.
	ctx, unlock, err := lockBundleFresh(ctx, w.Root, func() {
		emit(Event{Phase: PhaseRemove, Message: "waiting for " + lockFileName + " (another sheaf process is working on the bundle)"})
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("locking %s: %w", bundle.FileName, err)
	}
	defer unlock()
	// Changes made while Confirm was pending (e.g. a TUI dialog) would be
	// deleted unchecked: check again, without fetching, and abort if
	// anything differs.
	again, err := planRemoval(ctx, w, name, wd, fetch, func() bool { return fetchForceable }, risksOf, func(Event) {})
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
	// Under the lock already held, the unregistration cannot be cancelled:
	// a deleted directory never leaves its entry behind because of a
	// cancellation.
	if err := w.updateLocked(func(b *bundle.Bundle) error { return b.RemoveRepository(name) }); err != nil {
		if plan.Dir == "" {
			return "", err
		}
		return "", fmt.Errorf("%s was deleted but removing its entry from %s failed: %w (run remove again)", plan.Dir, bundle.FileName, err)
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
	if err := refuseRemovalUnlessRealDir(dir, fi); err != nil {
		return "", err
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
// if none); forceable, called after it, tells whether that problem is one
// that --force overrides (see Remove).
func planRemoval(ctx context.Context, w *Workspace, name, wd string, fetch func(primary string) (string, error), forceable func() bool,
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
		// A gone remote, or a remote answering although the hosting
		// service says the repository is gone, is overridden by --force;
		// any other failed fetch leaves the remote state unknown.
		plan.Items = append(plan.Items, RemoveItem{Path: primary, Problem: fetchMsg, Hard: !forceable()})
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
		if slices.ContainsFunc(risks, func(r gitx.Risk) bool { return r.Kind == gitx.RiskForeignWorktree }) {
			// Like a worktree outside the directory: whatever --force.
			it.Problem, it.Hard = "remove or move the worktrees of other repositories first", true
		}
		if wt.Locked {
			// Like wt rm: whatever --force, git keeps it for a reason.
			p := "locked"
			if wt.LockReason != "" {
				p += " (" + wt.LockReason + ")"
			}
			it.Problem, it.Hard = joinProblem(it.Problem, p+"; unlock it with `git worktree unlock "+it.Path+"`"), true
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, addUnknownEntries(dir, known, &plan)
}

// publishedPullHeads returns the recorded heads (gitx.PullHeadRef) of the
// pull requests whose head a local branch of primary tracks from remote
// (the branches `sheaf wt pr` sets up for pull requests from forks), for
// gitx.DeletionRisksOptions.PublishedRefs: their commits count as published,
// only the commits beyond them are at risk.
//
// With fetch, the heads are fetched first (one fetch); when that fails (or
// the branches cannot be read), none is returned and their commits count as
// unpushed. Without fetch (offline), the recorded heads are used as of their
// last fetch, like remote-tracking refs; a branch whose head was never
// recorded (made before sheaf recorded heads) gets none: its commits count
// as unpushed until remove fetches online or `sheaf wt pr` refreshes it.
// Only a cancellation of ctx is returned as an error.
func publishedPullHeads(ctx context.Context, net gitx.Net, primary, remote string, fetch bool, emit func(Event)) ([]string, error) {
	ns, err := gitx.PullRequestBranches(ctx, primary, remote)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		emit(Event{Phase: PhaseFetch, Message: "cannot read the pull request branches: " + err.Error() + "; their commits count as unpushed"})
		return nil, nil
	}
	if len(ns) == 0 {
		return nil, nil
	}
	var refs, specs []string
	for _, n := range ns {
		refs = append(refs, gitx.PullHeadRef(n))
		specs = append(specs, fmt.Sprintf("+refs/pull/%d/head:%s", n, gitx.PullHeadRef(n)))
	}
	if !fetch {
		return refs, nil
	}
	emit(Event{Phase: PhaseFetch, Message: "fetching the pull request heads of " + strings.Join(refs, ", ")})
	if err := net.FetchRefspecs(ctx, primary, remote, specs...); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		emit(Event{Phase: PhaseFetch, Message: "could not fetch the pull request heads (" + failureLine(err) + "); the commits of their branches count as unpushed"})
		return nil, nil
	}
	return refs, nil
}

// failureLine condenses an error to its first line.
func failureLine(err error) string {
	s, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return s
}

// nameUnpushedBranches sets the Detail of the unpushed-commits risk of the
// primary checkout's item to the branches carrying them (gitx.UnpushedBranches
// with the published refs Remove used), for the refusal message.
func nameUnpushedBranches(ctx context.Context, plan *RemovePlan, published []string) error {
	for i, it := range plan.Items {
		for j, r := range it.Risks {
			if r.Kind != gitx.RiskUnpushedBranchCommits || it.Path != plan.Primary {
				continue
			}
			names, err := gitx.UnpushedBranches(ctx, plan.Primary, gitx.DeletionRisksOptions{PublishedRefs: published})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue // the count alone
			}
			const maxListed = 5
			if len(names) > maxListed {
				names = append(names[:maxListed:maxListed], fmt.Sprintf("and %d more", len(names)-maxListed))
			}
			plan.Items[i].Risks[j].Detail = strings.Join(names, ", ")
		}
	}
	return nil
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

// goneRemoteProblem is the refusal reason when the fetch shows that origin
// no longer exists. It is a data risk (RemoveItem.Hard false): --force
// overrides it, and --offline is deliberately not suggested since skipping
// the fetch would only hide it.
const goneRemoteProblem = "the remote repository appears to be gone: this clone may hold the only copy of its history " +
	"(or you no longer have access (wrong SSH key/account))"

// quotedNotFound matches git's "repository '<url>' not found" (HTTP 404).
var quotedNotFound = regexp.MustCompile(`repository '[^']*' not found`)

// remoteRepoGoneFor reports whether the standard error of a failed fetch of
// the named remote says that the remote repository does not exist. git runs
// with LC_ALL=C, so the messages are the untranslated ones. These are
// heuristics:
//
//   - "Repository not found": GitHub over SSH ("ERROR: Repository not
//     found.") and over HTTPS ("remote: Repository not found."); GitHub
//     answers the same for a private repository the user cannot see, which
//     for this user also means the clone may be the only copy;
//   - "repository '<url>' not found": git over HTTP(S) on a 404;
//   - "'<path>' does not appear to be a git repository": a local or SSH
//     path that no longer holds a repository, except "'<remote>' does not
//     appear ..." which means that no such remote is configured (left to
//     the generic fetch failure).
//
// "Could not read from remote repository" alone is not enough: git prints
// it for authentication and network failures too.
func remoteRepoGoneFor(stderr, remote string) bool {
	if strings.Contains(stderr, "Repository not found") || quotedNotFound.MatchString(stderr) {
		return true
	}
	return strings.Contains(stderr, "does not appear to be a git repository") &&
		!strings.Contains(stderr, "'"+remote+"' does not appear to be a git repository")
}

// fetchFork fetches the fork remote of a forked repository for Remove and
// returns a problem (empty if none) and whether Force overrides it:
//
//   - fetch succeeds: no problem;
//   - the fork appears to be gone: a data risk (forceable) when commits are
//     reachable from the fork's (now stale) remote-tracking refs but from
//     no other remote's (see forkOnlyCommits); otherwise only a progress
//     note, since everything the clone holds is also on the reference
//     repository or reported as unpushed;
//   - any other failure: the remote state is unknown (not forceable), as
//     for the reference repository.
func fetchFork(ctx context.Context, net gitx.Net, primary, fork string, emit func(Event)) (string, bool, error) {
	emit(Event{Phase: PhaseFetch, Message: "fetching " + fork + " (your fork)"})
	gone, err := fetchForkRemote(ctx, net, primary, fork)
	switch {
	case ctx.Err() != nil:
		return "", false, ctx.Err()
	case err == nil:
		return "", false, nil
	case !gone:
		return "fetch of your fork (" + fork + ") failed: " + fetchProblem(err), false, nil
	}
	n, cerr := forkOnlyCommits(ctx, primary, fork)
	if cerr != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "your fork (" + fork + ") appears to be gone and its commits cannot be checked: " + cerr.Error(), false, nil
	}
	if n == 0 {
		emit(Event{Phase: PhaseFetch, Message: "your fork (" + fork + ") appears to be gone; no commit exists only on it"})
		return "", false, nil
	}
	verb := "exist"
	if n == 1 {
		verb = "exists"
	}
	return fmt.Sprintf("your fork (%s) appears to be gone and %s %s only on it (its remote-tracking refs %s/*): this clone holds the only copy "+
		"(or you no longer have access (wrong SSH key/account))", fork, commits(n), verb, fork), true, nil
}

// forkOnlyCommits counts the commits reachable from the remote-tracking
// refs of fork (stale once the fork is gone) and from no other remote's
// remote-tracking ref: once the fork is gone, the clone holds their only
// copy, whether a local branch still points at them or not. Commits on no
// remote-tracking ref at all are not included: the risk checks report them
// as unpushed.
func forkOnlyCommits(ctx context.Context, primary, fork string) (int, error) {
	return revCount(ctx, primary, "--remotes="+fork+"/*", "--not", "--exclude="+fork+"/*", "--remotes")
}

func joinProblem(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

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
// known checkouts, and a hard one for each worktree of another repository
// found in them (see foreignWorktrees).
func addUnknownEntries(dir string, known []string, plan *RemovePlan) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	root := resolveExisting(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if slices.Contains(known, p) || osJunk[e.Name()] {
			continue
		}
		foreign := foreignWorktrees(p, root)
		if len(foreign) == 0 || foreign[0].Path != p {
			plan.Items = append(plan.Items, RemoveItem{Path: p, Problem: "not a checkout of this repository"})
		}
		plan.Items = append(plan.Items, foreign...)
	}
	return nil
}

// foreignWorktrees walks p (without following symbolic links, and below
// embedded repositories too) for work trees whose .git file names a git
// directory outside root, the resolved repository directory being removed:
// worktrees of other repositories (or .git files that cannot be read).
// Deleting one would lose its changes unchecked and leave its repository's
// administrative files dangling, so each is a hard item, like a worktree
// outside the repository directory. A work tree whose git directory is
// inside root (e.g. of a repository embedded there) is not one: it is
// deleted with that repository. A directory that cannot be read cannot be
// checked: it is a hard item too.
func foreignWorktrees(p, root string) []RemoveItem {
	var out []RemoveItem
	_ = filepath.WalkDir(p, func(d string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // vanished meanwhile
			}
			out = append(out, RemoveItem{Path: d, Hard: true, Problem: "cannot be checked for worktrees of other repositories (" + err.Error() + "); fix its permissions, or inspect it and delete it manually"})
			if de != nil && de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !de.IsDir() {
			return nil
		}
		if de.Name() == ".git" {
			return filepath.SkipDir // a repository's git directory
		}
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		gitdir, rerr := gitx.GitFileDir(d)
		switch {
		case rerr != nil:
			out = append(out, RemoveItem{Path: d, Hard: true, Problem: "unreadable .git file (" + rerr.Error() + "); remove or move it first"})
		case !within(root, resolveExisting(gitdir)):
			out = append(out, RemoveItem{Path: d, Hard: true, Problem: "worktree of another repository (git directory " + gitdir + "); remove or move it first"})
		}
		return nil
	})
	return out
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
				return fmt.Errorf("removing worktree %s: %w; %s was not deleted, run remove again", it.Path, err, dir)
			}
		}
	}
	emit(Event{Phase: PhaseRemove, Message: "deleting " + dir})
	return os.RemoveAll(dir)
}
