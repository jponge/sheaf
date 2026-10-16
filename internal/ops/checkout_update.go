package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
)

// UpdateOptions configures Update.
type UpdateOptions struct {
	Filter CheckoutFilter
	// Jobs bounds the repositories updated in parallel; the checkouts of one
	// repository are updated one after the other, after its single fetch.
	Jobs int
	// Force, when closed, kills the fast-forward merges in progress at once
	// (see gitx.FastForwardOptions.Force); the CLI closes it on a second
	// Ctrl-C.
	Force <-chan struct{}
	// MergeTimeout bounds each merge (zero: gitx.DefaultMergeTimeout).
	MergeTimeout time.Duration
	// Interactive fetches in interactive git mode (see gitx.Net); the
	// fast-forward merges never are.
	Interactive bool

	// fetchHook, when set by a test, runs while a repository fetch holds
	// its per-repository lock (see fetchRepo).
	fetchHook func(commonDir string)
}

// CheckoutUpdate is the outcome of Update for one checkout.
type CheckoutUpdate struct {
	Checkout
	Outcome  gitx.PullOutcome
	Old, New string // HEAD before and after (see gitx.PullResult)
	// Reason explains skips and failures for humans: the dirty counts, a
	// missing or gone upstream, divergence, a failed fetch, an interruption.
	Reason string
	Err    error // set iff Outcome is gitx.PullFailed
	// UntrackedInTheWay lists the untracked files that git refused to
	// overwrite; the checkout then counts as skipped (Outcome stays
	// gitx.PullFailed, Err holds git's full message).
	UntrackedInTheWay []string
	// Pinned is set for a worktree pinned at a tag (Checkout.Tag): it is never
	// moved, Reason says "pinned at tag <tag>", and it is neither a failure
	// nor a skip (Skipped returns false).
	Pinned bool
}

// UpdateReport is the result of Update.
type UpdateReport struct {
	Checkouts []CheckoutUpdate // sorted like ListCheckouts
	Problems  []Problem
}

// inflight tracks the checkouts whose fast-forward is running, to report
// PhaseWaitFastForward when the context is cancelled.
type inflight struct {
	mu        sync.Mutex
	cancelled bool
	running   map[Checkout]bool
	report    func(Event)
}

// start registers c, whose merge is starting (it runs to completion even if
// the context is cancelled); if the cancellation was already reported, the
// waiting event for c is reported at once.
func (f *inflight) start(c Checkout) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[c] = true
	if f.cancelled {
		f.reportWait(c)
	}
}

func (f *inflight) reportWait(c Checkout) {
	f.report(Event{Repo: c.Repo, Worktree: c.Dir, Phase: PhaseWaitFastForward, Kind: KindProgress,
		Message: "waiting for the in-progress fast-forward to finish"})
}

func (f *inflight) end(c Checkout) {
	f.mu.Lock()
	delete(f.running, c)
	f.mu.Unlock()
}

func (f *inflight) onCancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = true
	for c := range f.running {
		f.reportWait(c)
	}
}

// Update fetches origin (upstream and origin for a repository with a fork,
// see RemotesOf; when only the fork fetch fails, the checkouts tracking the
// fork fail with that error, the others are updated, and the failure is
// also returned as a Problem) once per selected repository, then fast-forwards
// each of its checkouts (primary, then worktrees) to its upstream with
// gitx.FastForwardWithOptions: checkouts that are dirty (staged, unstaged or
// conflicted changes; untracked files do not count), detached, without
// upstream or whose upstream is gone, or diverged are skipped with a Reason.
// Worktrees pinned at a tag are never touched: they are reported as Pinned
// (KindSkipped event, "pinned at tag <tag>"), which Skipped does not count.
// Up to Jobs repositories are processed in parallel.
//
// Cancellation of ctx stops fetches and prevents any further checkout from
// starting (they are reported as failed with ctx's error, "not started"),
// but a fast-forward merge already running completes (see gitx.FastForward);
// a PhaseWaitFastForward event is reported for each. Closing opts.Force
// kills those merges.
//
// Events: fetch start/done/failed per repository, and one KindDone,
// KindSkipped or KindFailed PhaseUpdate event per checkout (Worktree set to
// its directory name). The error is only set for an invalid filter.
func Update(ctx context.Context, w *Workspace, opts UpdateOptions, rep Reporter) (UpdateReport, error) {
	ctx = withCommandCache(ctx)
	sets, probs, err := enumerate(ctx, w, opts.Filter, opts.Jobs)
	if err != nil {
		return UpdateReport{}, err
	}
	report := serialize(rep)
	fl := &inflight{running: map[Checkout]bool{}, report: report}
	stop := context.AfterFunc(ctx, fl.onCancel)
	defer stop()
	results := make([][]CheckoutUpdate, len(sets))
	fetchProbs := make([]*Problem, len(sets))
	forEach(opts.Jobs, len(sets), func(i int) {
		results[i], fetchProbs[i] = updateRepo(ctx, sets[i], opts, report, fl)
	})
	var out []CheckoutUpdate
	for i, r := range results {
		out = append(out, r...)
		if fetchProbs[i] != nil {
			probs = append(probs, *fetchProbs[i])
		}
	}
	sortProblems(probs)
	return UpdateReport{Checkouts: out, Problems: probs}, nil
}

// updateRepo updates the checkouts of s. The problem is set when only the
// fork of a forked repository could not be fetched (see Update).
func updateRepo(ctx context.Context, s *repoCheckouts, opts UpdateOptions, report func(Event), fl *inflight) ([]CheckoutUpdate, *Problem) {
	out := make([]CheckoutUpdate, 0, len(s.checkouts))
	emit := func(u CheckoutUpdate) {
		e := Event{Repo: u.Repo, Worktree: u.Dir, Phase: PhaseUpdate, Message: u.Reason}
		switch {
		case u.Outcome == gitx.PullFailed:
			e.Kind, e.Err = KindFailed, u.Err
		case u.Skipped() || u.Pinned:
			e.Kind = KindSkipped
		default:
			e.Kind = KindDone
			if e.Message == "" {
				e.Message = u.Outcome.String()
			}
		}
		report(e)
		out = append(out, u)
	}
	notStarted := func(c Checkout, err error) CheckoutUpdate {
		return CheckoutUpdate{Checkout: c, Outcome: gitx.PullFailed, Reason: "not started (interrupted)", Err: err}
	}
	// A worktree pinned at a tag is never moved, so neither a failed fetch
	// nor an interruption concerns it. Once its HEAD moved away from the
	// tag's commit, it is reported like any detached worktree (skipped).
	pinned := func(c Checkout) bool {
		switch {
		case c.Tag == "":
			return false
		case c.TagMovedTo != "":
			emit(CheckoutUpdate{Checkout: c, Outcome: gitx.PullSkippedDetached,
				Reason: "detached (was pinned at tag " + c.Tag + "; moved to " + shortID(c.TagMovedTo) + ")"})
		default:
			emit(CheckoutUpdate{Checkout: c, Outcome: gitx.PullSkippedDetached, Pinned: true, Reason: "pinned at tag " + c.Tag})
		}
		return true
	}
	if err := ctx.Err(); err != nil {
		for _, c := range s.checkouts {
			if !pinned(c) {
				emit(notStarted(c, err))
			}
		}
		return out, nil
	}
	report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindStart})
	fetchErr := fetchRepo(ctx, gitx.Net{Interactive: opts.Interactive}, s.primary, s.remotes, opts.fetchHook)
	// Only the fork could not be fetched: the checkouts tracking it fail,
	// the others are updated from the reference as usual.
	forkFailed := fetchErr != nil && ctx.Err() == nil && onlyForkFetchFailed(fetchErr, s.remotes)
	if err := fetchErr; err != nil && !forkFailed {
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindFailed, Err: err})
		reason := "fetch failed"
		if ctx.Err() != nil {
			reason = "fetch interrupted"
		}
		for _, c := range s.checkouts {
			if !pinned(c) {
				emit(CheckoutUpdate{Checkout: c, Outcome: gitx.PullFailed, Reason: reason, Err: err})
			}
		}
		return out, nil
	}
	var prob *Problem
	if forkFailed {
		prob = &Problem{Repo: s.name, Path: s.primary, Err: fmt.Errorf("fetch of your fork failed (checkouts tracking it were not updated): %w", fetchErr)}
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindFailed, Err: fetchErr,
			Message: "fetched " + s.remotes.Reference + " only; checkouts tracking " + s.remotes.Fork + " are not updated"})
	} else {
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + s.remotes.String()})
	}
	ffo := gitx.FastForwardOptions{MergeTimeout: opts.MergeTimeout, Force: opts.Force}
	for _, c := range s.checkouts {
		if pinned(c) {
			continue
		}
		if err := ctx.Err(); err != nil {
			emit(notStarted(c, err))
			continue
		}
		if forkFailed && c.Branch != "" && branchRemote(ctx, c.Path, c.Branch) == s.remotes.Fork {
			emit(CheckoutUpdate{Checkout: c, Outcome: gitx.PullFailed, Reason: "fetch of " + s.remotes.Fork + " (your fork) failed", Err: fetchErr})
			continue
		}
		o := ffo
		started := false
		o.OnMergeStart = func() { started = true; fl.start(c) }
		r := gitx.FastForwardWithOptions(ctx, c.Path, o)
		fl.end(c)
		u := classifyUpdate(ctx, c, r)
		if !started && u.Outcome == gitx.PullFailed && ctx.Err() != nil && errors.Is(r.Err, ctx.Err()) {
			u.Reason = "not started (interrupted)"
		}
		emit(u)
	}
	return out, prob
}

// Skipped reports whether the checkout was left alone on purpose (dirty,
// detached, no or gone upstream, diverged, untracked files in the way).
func (u CheckoutUpdate) Skipped() bool {
	if u.Pinned {
		return false
	}
	if len(u.UntrackedInTheWay) > 0 {
		return true
	}
	switch u.Outcome {
	case gitx.PullSkippedDirty, gitx.PullSkippedNoUpstream, gitx.PullSkippedDiverged, gitx.PullSkippedDetached:
		return true
	}
	return false
}

// untrackedInTheWay extracts the files of git's "untracked working tree
// files would be overwritten by merge" error (one tab-indented path per
// line), or returns nil.
func untrackedInTheWay(err error) []string {
	var ge *gitx.GitError
	if !errors.As(err, &ge) {
		return nil
	}
	_, rest, ok := strings.Cut(ge.Stderr, "untracked working tree files would be overwritten by merge:")
	if !ok {
		return nil
	}
	var files []string
	for l := range strings.SplitSeq(rest, "\n") {
		if strings.HasPrefix(l, "\t") {
			files = append(files, strings.TrimSpace(l))
		} else if len(files) > 0 {
			break
		}
	}
	return files
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// classifyUpdate turns a pull result into a CheckoutUpdate with a Reason. For
// skips it re-reads the status (ignoring ctx's cancellation, bounded) to give
// the counts and the upstream.
func classifyUpdate(ctx context.Context, c Checkout, r gitx.PullResult) CheckoutUpdate {
	u := CheckoutUpdate{Checkout: c, Outcome: r.Outcome, Old: r.Old, New: r.New, Err: r.Err}
	st := func() *gitx.RepoStatus {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		s, err := gitx.Status(sctx, c.Path)
		if err != nil {
			return nil
		}
		return s
	}
	switch r.Outcome {
	case gitx.PullFastForwarded:
		u.Reason = shortID(r.Old) + ".." + shortID(r.New)
	case gitx.PullUpToDate:
		u.Reason = "up to date"
	case gitx.PullSkippedDetached:
		u.Reason = "detached HEAD"
	case gitx.PullSkippedDirty:
		u.Reason = "uncommitted changes"
		if s := st(); s != nil {
			u.Reason = fmt.Sprintf("uncommitted changes (%d staged, %d unstaged", s.Staged, s.Unstaged)
			if s.Conflicted > 0 {
				u.Reason += fmt.Sprintf(", %d conflicted", s.Conflicted)
			}
			u.Reason += ")"
		}
		u.Reason += ": commit or stash them, then update again"
	case gitx.PullSkippedNoUpstream:
		u.Reason = "no upstream branch configured"
		if s := st(); s != nil && s.Upstream != "" && s.UpstreamGone {
			u.Reason = "upstream " + s.Upstream + " is gone (deleted on the remote)"
		} else if why := pullRequestReason(ctx, c); why != "" {
			u.Reason = why
		}
	case gitx.PullSkippedDiverged:
		u.Reason = "diverged from its upstream: merge or rebase manually"
		if s := st(); s != nil && s.Upstream != "" {
			u.Reason = fmt.Sprintf("diverged from %s (%d ahead, %d behind): merge or rebase manually", s.Upstream, s.Ahead, s.Behind)
		}
	case gitx.PullFailed:
		if files := untrackedInTheWay(r.Err); len(files) > 0 {
			u.UntrackedInTheWay = files
			u.Reason = "untracked files in the way: " + strings.Join(files, ", ") + " (move or remove them, then update again)"
			break
		}
		switch {
		case errors.Is(r.Err, gitx.ErrForceKilled):
			u.Reason = "fast-forward killed on request; check the work tree (a .git/index.lock may be left behind)"
		case ctx.Err() != nil && errors.Is(r.Err, ctx.Err()):
			u.Reason = "interrupted"
		}
	}
	return u
}

// branchRemote returns branch.<branch>.remote, "" when unset or unreadable.
func branchRemote(ctx context.Context, dir, branch string) string {
	out, err := gitx.Run(ctx, dir, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
