package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fetch runs `git fetch --prune origin` in dir: it updates the
// remote-tracking refs of origin and deletes those whose remote branch no
// longer exists. It always fetches origin, whatever the upstream of the
// current branch (which may be a local branch or another remote): branches
// tracking another remote are not refreshed and FastForward sees them as of
// their last fetch.
//
// Remote-tracking refs are shared by all worktrees of a repository, and two
// concurrent fetches into the same repository fail with "cannot lock ref".
// Callers must therefore serialize Fetch per repository (group worktrees by
// CommonDir) and fetch once per repository, then call FastForward on each
// worktree, which may run in parallel. Fetch is cancellable at any point
// through ctx; a fetch killed with SIGKILL may leave a ref lock behind (see
// StaleLockPath). Under a context made by WithInteractive, git keeps the
// caller's controlling terminal.
func Fetch(ctx context.Context, dir string) error {
	_, err := run(ctx, netOpts(ctx), dir, "fetch", "--quiet", "--prune", "origin")
	return err
}

// PullOutcome classifies the result of FastForward and PullFFOnly.
type PullOutcome int

const (
	// PullUpToDate: the pull succeeded and HEAD did not move (includes a
	// branch that is only ahead of its upstream).
	PullUpToDate PullOutcome = iota + 1
	// PullFastForwarded: HEAD moved forward from PullResult.Old to New.
	PullFastForwarded
	// PullSkippedDirty: staged, unstaged or conflicted changes; nothing was
	// run (PullFFOnly does not fetch either).
	PullSkippedDirty
	// PullSkippedNoUpstream: no upstream configured, or its remote-tracking
	// ref is gone; nothing was run (PullFFOnly does not fetch either).
	PullSkippedNoUpstream
	// PullSkippedDiverged: local and upstream both have commits the other
	// lacks according to the remote-tracking ref; HEAD was not moved.
	PullSkippedDiverged
	// PullSkippedDetached: HEAD is detached; nothing was run.
	PullSkippedDetached
	// PullFailed: any other failure; PullResult.Err says why.
	PullFailed
)

func (o PullOutcome) String() string {
	switch o {
	case PullUpToDate:
		return "up-to-date"
	case PullFastForwarded:
		return "fast-forwarded"
	case PullSkippedDirty:
		return "skipped-dirty"
	case PullSkippedNoUpstream:
		return "skipped-no-upstream"
	case PullSkippedDiverged:
		return "skipped-diverged"
	case PullSkippedDetached:
		return "skipped-detached"
	case PullFailed:
		return "failed"
	}
	return fmt.Sprintf("PullOutcome(%d)", int(o))
}

// PullResult is the result of FastForward and PullFFOnly.
type PullResult struct {
	Outcome PullOutcome
	// Old and New are HEAD before and after the operation. They are equal
	// unless FastForwarded or Failed after the merge started (see
	// FastForward); both are empty when Status itself failed, and New is
	// empty when HEAD could not be re-read.
	Old, New string
	// Err is set iff Outcome is PullFailed.
	Err error
}

// FastForwardOptions tunes FastForwardWithOptions and PullFFOnlyWithOptions.
// The zero value gives the behaviour of FastForward and PullFFOnly.
type FastForwardOptions struct {
	// MergeTimeout bounds the merge (see FastForward). Zero means
	// DefaultMergeTimeout; a negative value is treated like zero.
	MergeTimeout time.Duration
	// Force, when non-nil and closed while the merge runs (or before it
	// starts), kills the merge's whole process group with SIGKILL immediately
	// (on platforms without process groups, only the git process). The result
	// is PullFailed with an Err matching ErrForceKilled and New set to the
	// observed HEAD. Unlike the timeout, which sends SIGTERM first, SIGKILL
	// does not let git remove its lock files: .git/index.lock may be left
	// behind (see StaleLockPath), and the work tree may be half-written.
	// Closing Force after the merge has finished has no effect.
	Force <-chan struct{}
	// OnMergeStart, when non-nil, is called (synchronously, in the calling
	// goroutine) right before the merge starts, i.e. once the pre-checks
	// passed and the work tree is about to be updated; it is not called when
	// FastForward returns without merging.
	OnMergeStart func()
}

// DefaultMergeTimeout is the bound on the merge of FastForward and
// PullFFOnly.
const DefaultMergeTimeout = 10 * time.Minute

// PullFFOnly is Fetch followed by FastForward, for a single checkout. The
// cheap preconditions (detached, dirty, no or gone upstream) are checked
// first, so a work tree that would be skipped is not fetched either.
//
// It is not safe to run concurrently with other fetches into the same
// repository (see Fetch); to update several worktrees of one repository, call
// Fetch once and FastForward on each.
//
// Cancellation: ctx interrupts the pre-checks and the fetch (outcome
// PullFailed, Err matching ctx.Err()), but not the merge once FastForward has
// started it: the merge runs to completion or for up to DefaultMergeTimeout
// (10 minutes), whatever ctx does. Callers that must be able to abort sooner
// need a hard-abort path: use PullFFOnlyWithOptions with a Force channel. See
// FastForward.
func PullFFOnly(ctx context.Context, dir string) PullResult {
	return PullFFOnlyWithOptions(ctx, dir, FastForwardOptions{})
}

// PullFFOnlyWithOptions is PullFFOnly with the options of FastForwardWithOptions
// (the fetch is not affected by them; it is cancelled through ctx).
func PullFFOnlyWithOptions(ctx context.Context, dir string, opts FastForwardOptions) PullResult {
	if r, ok := preCheck(ctx, dir, false); !ok {
		return r
	}
	if err := Fetch(ctx, dir); err != nil {
		return PullResult{Outcome: PullFailed, Err: err}
	}
	return FastForwardWithOptions(ctx, dir, opts)
}

// FastForward fast-forwards the branch checked out in dir to the local
// remote-tracking ref of its upstream, as of the last Fetch. It never
// accesses the network, so FastForward calls on different worktrees of the
// same repository may run concurrently.
//
// Preconditions are checked with Status before anything else runs, so a
// skipped work tree is not touched at all and the classification never
// depends on parsing git's human-readable messages. The order of checks is:
// detached, dirty (see RepoStatus.Dirty; untracked files do not count), no or
// gone upstream, diverged, not behind (PullUpToDate, nothing run). Then
// `git merge --ff-only --no-autostash @{upstream}` runs; command-line options
// override pull.*, merge.ff and branch.<name>.mergeOptions, so no merge commit
// or rebase can result. Git refuses to overwrite untracked files, which yields
// PullFailed with the files untouched.
//
// Cancellation: ctx is honoured only before the merge starts (the Status
// pre-check, and a last check right before the merge, which then returns
// PullFailed with Err matching ctx.Err() and HEAD untouched). Once started, the merge runs to completion on a context
// detached from ctx's cancellation and bounded by DefaultMergeTimeout (10
// minutes) instead, because interrupting the checkout of a fast-forward
// leaves the work tree and index half-updated, which then looks like local
// modifications. (HEAD is not moved by an interrupted merge: git updates the
// branch ref only after the index and work tree were updated, so an
// interruption leaves HEAD where it was, with a possibly half-written work
// tree; HEAD can only have moved if the merge got further, e.g. when a
// post-merge hook hangs.) A merge that completes is reported as
// PullFastForwarded (or PullUpToDate) with a nil Err even if ctx was
// cancelled meanwhile: the update did happen, and callers can consult
// ctx.Err() themselves. The re-reads after the merge also ignore ctx's
// cancellation. Hooks run by the merge (post-merge, post-checkout) are part
// of it.
//
// Because the merge outlives ctx, callers must provide a hard-abort path
// instead of waiting up to ten minutes: FastForwardWithOptions with a Force
// channel kills the merge immediately (sheaf's CLI does so on a second
// Ctrl-C).
//
// When the merge fails, HEAD is re-read so that New reflects the observed
// HEAD; New is empty only if HEAD could not be read. If the merge exceeded
// its timeout, its process group is killed as described in Run, the outcome
// is PullFailed with an Err matching context.DeadlineExceeded, and New may
// differ from Old if git had already moved HEAD (e.g. a hook hung). A merge
// failure due to divergence that appeared after the precheck (a concurrent
// Fetch) is reported as PullSkippedDiverged.
func FastForward(ctx context.Context, dir string) PullResult {
	return FastForwardWithOptions(ctx, dir, FastForwardOptions{})
}

// FastForwardWithOptions is FastForward with a configurable merge timeout and
// a hard-abort channel; see FastForwardOptions.
func FastForwardWithOptions(ctx context.Context, dir string, opts FastForwardOptions) PullResult {
	r, ok := preCheck(ctx, dir, true)
	if !ok {
		return r
	}
	if afterPreCheckHook != nil {
		afterPreCheckHook()
	}
	old := r.Old
	if err := ctx.Err(); err != nil {
		// Cancelled after the pre-checks: the merge is not started.
		return PullResult{Outcome: PullFailed, Old: old, New: old, Err: err}
	}
	timeout := opts.MergeTimeout
	if timeout <= 0 {
		timeout = mergeTimeout
	}
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if opts.OnMergeStart != nil {
		opts.OnMergeStart()
	}
	_, mergeErr := run(mctx, runOpts{force: opts.Force}, dir, "merge", "--quiet", "--ff-only", "--no-autostash", "@{upstream}")
	// From here on, ctx's cancellation is ignored (see above).
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), rereadTimeout)
	defer rcancel()
	if mergeErr != nil {
		after, err := Status(rctx, dir)
		if err != nil {
			return PullResult{Outcome: PullFailed, Old: old, Err: mergeErr}
		}
		if mctx.Err() == nil && !errors.Is(mergeErr, ErrForceKilled) && after.Head == old && after.Ahead > 0 && after.Behind > 0 {
			return PullResult{Outcome: PullSkippedDiverged, Old: old, New: old}
		}
		return PullResult{Outcome: PullFailed, Old: old, New: after.Head, Err: mergeErr}
	}
	if afterMergeHook != nil {
		afterMergeHook()
	}

	out, err := Run(rctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return PullResult{Outcome: PullFailed, Old: old, Err: err}
	}
	newHead := strings.TrimSpace(out)
	if newHead == old {
		return PullResult{Outcome: PullUpToDate, Old: old, New: newHead}
	}
	if old != "" {
		if _, err := Run(rctx, dir, "merge-base", "--is-ancestor", old, newHead); err != nil {
			return PullResult{Outcome: PullFailed, Old: old, New: newHead,
				Err: fmt.Errorf("gitx: merge in %s moved HEAD from %s to %s, which is not a fast-forward: %w", dir, old, newHead, err)}
		}
	}
	return PullResult{Outcome: PullFastForwarded, Old: old, New: newHead}
}

// mergeTimeout is the default bound on the merge of FastForward, which ignores
// the caller's cancellation. It is a variable only so that tests can shorten
// it; it must not be changed while FastForward runs.
var mergeTimeout = DefaultMergeTimeout

// rereadTimeout bounds the HEAD re-reads after the merge.
const rereadTimeout = 5 * time.Second

// preCheck classifies dir from a fresh Status. It returns ok=false with the
// final result when the work tree must be skipped (or Status failed), and
// ok=true with Old set otherwise. withAheadBehind adds the diverged and
// up-to-date checks, which are only meaningful after a fetch; an up-to-date
// tree returns ok=false with PullUpToDate.
func preCheck(ctx context.Context, dir string, withAheadBehind bool) (PullResult, bool) {
	st, err := Status(ctx, dir)
	if err != nil {
		return PullResult{Outcome: PullFailed, Err: err}, false
	}
	old := st.Head
	skip := func(o PullOutcome) (PullResult, bool) { return PullResult{Outcome: o, Old: old, New: old}, false }
	switch {
	case st.Detached:
		return skip(PullSkippedDetached)
	case st.Dirty():
		return skip(PullSkippedDirty)
	case st.Upstream == "" || st.UpstreamGone:
		return skip(PullSkippedNoUpstream)
	case withAheadBehind && st.Ahead > 0 && st.Behind > 0:
		return skip(PullSkippedDiverged)
	case withAheadBehind && st.Behind == 0:
		return skip(PullUpToDate)
	}
	return PullResult{Old: old}, true
}

// afterPreCheckHook, when set by a test, runs in FastForward right after the
// pre-checks passed.
var afterPreCheckHook func()

// afterMergeHook, when set by a test, runs right after a successful merge in
// FastForward, before HEAD is re-read.
var afterMergeHook func()

// FetchAll runs `git fetch --prune --all` in dir: it fetches every remote
// of the repository (work tree or bare) and deletes remote-tracking refs
// whose remote branch no longer exists. Like Fetch, callers serialize it per
// repository. It is cancellable through ctx.
func FetchAll(ctx context.Context, dir string) error {
	_, err := run(ctx, netOpts(ctx), dir, "fetch", "--quiet", "--prune", "--all")
	return err
}
