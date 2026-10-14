package gitx

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Fetch runs `git fetch --prune` in dir: it fetches the remote of the
// current branch's upstream, or origin when there is none, and deletes
// remote-tracking refs whose remote branch no longer exists.
//
// Remote-tracking refs are shared by all worktrees of a repository, and two
// concurrent fetches into the same repository fail with "cannot lock ref".
// Callers must therefore serialize Fetch per repository (group worktrees by
// CommonDir) and fetch once per repository, then call FastForward on each
// worktree, which may run in parallel.
func Fetch(ctx context.Context, dir string) error {
	_, err := Run(ctx, dir, "fetch", "--quiet", "--prune")
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

// PullFFOnly is Fetch followed by FastForward, for a single checkout. The
// cheap preconditions (detached, dirty, no or gone upstream) are checked
// first, so a work tree that would be skipped is not fetched either.
//
// It is not safe to run concurrently with other fetches into the same
// repository (see Fetch); to update several worktrees of one repository, call
// Fetch once and FastForward on each.
func PullFFOnly(ctx context.Context, dir string) PullResult {
	if r, ok := preCheck(ctx, dir, false); !ok {
		return r
	}
	if err := Fetch(ctx, dir); err != nil {
		return PullResult{Outcome: PullFailed, Err: err}
	}
	return FastForward(ctx, dir)
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
// Once the merge has been started, every error re-reads HEAD (with a fresh,
// short-lived context when ctx is done) so that New reflects the observed
// HEAD: in particular, when ctx was cancelled or timed out the outcome is
// PullFailed with an Err matching ctx.Err() under errors.Is, and New may
// differ from Old if git had already moved HEAD (e.g. a post-merge hook was
// interrupted). New is empty only if HEAD could not be read. A merge failure
// due to divergence that appeared after the precheck (a concurrent Fetch) is
// reported as PullSkippedDiverged.
func FastForward(ctx context.Context, dir string) PullResult {
	r, ok := preCheck(ctx, dir, true)
	if !ok {
		return r
	}
	old := r.Old
	_, mergeErr := Run(ctx, dir, "merge", "--quiet", "--ff-only", "--no-autostash", "@{upstream}")
	if mergeErr != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rereadTimeout)
		defer cancel()
		after, err := Status(rctx, dir)
		if err != nil {
			return PullResult{Outcome: PullFailed, Old: old, Err: mergeErr}
		}
		if ctx.Err() == nil && after.Head == old && after.Ahead > 0 && after.Behind > 0 {
			return PullResult{Outcome: PullSkippedDiverged, Old: old, New: old}
		}
		return PullResult{Outcome: PullFailed, Old: old, New: after.Head, Err: mergeErr}
	}

	out, err := Run(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return PullResult{Outcome: PullFailed, Old: old, Err: err}
	}
	newHead := strings.TrimSpace(out)
	if newHead == old {
		return PullResult{Outcome: PullUpToDate, Old: old, New: newHead}
	}
	if old != "" {
		if _, err := Run(ctx, dir, "merge-base", "--is-ancestor", old, newHead); err != nil {
			return PullResult{Outcome: PullFailed, Old: old, New: newHead,
				Err: fmt.Errorf("gitx: merge in %s moved HEAD from %s to %s, which is not a fast-forward: %w", dir, old, newHead, err)}
		}
	}
	return PullResult{Outcome: PullFastForwarded, Old: old, New: newHead}
}

// rereadTimeout bounds the HEAD re-read after a failed or cancelled merge.
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
