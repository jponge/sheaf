package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jponge/sheaf/internal/gitx"
)

// WorktreeRemoval names a linked worktree for RemoveWorktrees.
type WorktreeRemoval struct {
	Repo   string
	Target WorktreeTarget
}

// WorktreeRemovalResult is the outcome of RemoveWorktrees for one worktree.
type WorktreeRemovalResult struct {
	WorktreeRemoval
	// Plan is the removal plan; nil when the worktree could not be
	// planned (Err says why).
	Plan *WorktreeRemovePlan
	// Message is what RemoveWorktree returns once the worktree is removed.
	Message string
	// Err is nil once the worktree is removed; otherwise, as RemoveWorktree
	// returns it, the refusal (*WorktreeRefusedError), ErrNotConfirmed or
	// the failure.
	Err error
}

// RemoveWorktrees removes several linked worktrees, possibly of several
// repositories, after one confirmation: each worktree is located, checked
// and planned exactly as by RemoveWorktree with opts (opts.Confirm is not
// used), except that each repository is fetched once (unless
// opts.Offline), before its worktrees are planned. confirm is then called
// once with the results so far, in the order of items: a worktree that may
// be removed has its Plan and no Err; one refused (*WorktreeRefusedError,
// with its Plan) or that could not be planned has its Err. confirm is not
// called when no worktree may be removed; a nil confirm, or one that
// returns false, removes nothing (ErrNotConfirmed). The confirmed
// worktrees are then removed one after the other as by RemoveWorktree:
// each is checked again under the bundle lock, with fresh reads, and kept
// if it changed since its plan.
//
// rep is told the units of work (a row per repository, for its fetch, and
// per worktree, see Planner) and receives the progress events, then one
// final event per worktree (KindDone with its message, KindSkipped with
// ErrNotConfirmed, KindFailed); a refusal is reported before confirm is
// called.
func RemoveWorktrees(ctx context.Context, w *Workspace, items []WorktreeRemoval, opts WorktreeRemoveOptions, confirm func([]WorktreeRemovalResult) bool, rep Reporter) []WorktreeRemovalResult {
	ctx = withCommandCache(ctx)
	report := serialize(rep)
	opts.Confirm, opts.originFetched, opts.forkDistrust = nil, false, forkDistrust{}
	out := make([]WorktreeRemovalResult, len(items))
	xs := make([]*worktreeRemoval, len(items))
	var repos []string // located, in order of first appearance
	for i, it := range items {
		out[i].WorktreeRemoval = it
		x, err := locateWorktreeRemoval(ctx, w, it.Repo, it.Target, opts, report)
		if err != nil {
			out[i].Err = err
			continue
		}
		xs[i] = x
		if !slices.Contains(repos, x.r.Name) {
			repos = append(repos, x.r.Name)
		}
	}
	plan(rep, len(repos)+len(items))
	final := func(i int) {
		e := Event{Repo: items[i].Repo, Phase: PhaseWorktree, Message: out[i].Message}
		if x := xs[i]; x != nil {
			e.Repo, e.Worktree = x.r.Name, x.wt.Dir
		} else {
			e.Worktree = items[i].Target.Name
		}
		switch {
		case out[i].Err == nil:
			e.Kind = KindDone
		case errors.Is(out[i].Err, ErrNotConfirmed):
			e.Kind, e.Err = KindSkipped, out[i].Err
		default:
			e.Kind, e.Err = KindFailed, out[i].Err
		}
		report(e)
	}
	for i := range items {
		if xs[i] == nil {
			final(i)
		}
	}
	// One fetch per repository, before its worktrees are planned.
	for _, name := range repos {
		var first *worktreeRemoval
		for _, x := range xs {
			if x != nil && x.r.Name == name {
				first = x
				break
			}
		}
		ev := func(kind Kind) func(phase, msg string) {
			return func(phase, msg string) { report(Event{Repo: name, Phase: phase, Kind: kind, Message: msg}) }
		}
		var f removalFetch
		var err error
		if opts.Offline {
			f = offlineRemovalFetch(first.r, ev(KindDone))
		} else {
			report(Event{Repo: name, Phase: PhaseFetch, Kind: KindStart})
			f, err = fetchForRemoval(ctx, gitx.Net{Interactive: opts.Interactive}, first.primary, first.r, ev(KindProgress))
			switch {
			case err != nil:
				report(Event{Repo: name, Phase: PhaseFetch, Kind: KindFailed, Err: err})
			case f.msg != "":
				report(Event{Repo: name, Phase: PhaseFetch, Kind: KindFailed, Err: errors.New(f.msg)})
			case f.distrust.gone:
				report(Event{Repo: name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + RemotesOf(first.r).Reference + "; " + f.distrust.why + ": its remote-tracking refs are not trusted"})
			default:
				report(Event{Repo: name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + RemotesOf(first.r).String()})
			}
		}
		for i, x := range xs {
			if x == nil || x.r.Name != name {
				continue
			}
			if err != nil {
				out[i].Err = err
				continue
			}
			x.useFetch(f, opts)
			if err := x.planFirst(ctx); err != nil {
				out[i].Err = err
				continue
			}
			p := x.plan
			out[i].Plan = &p
			if x.plan.Refused(opts) {
				out[i].Err = x.refusal(ctx, opts)
			}
		}
	}
	var confirmed []int
	for i := range items {
		switch {
		case xs[i] == nil:
		case out[i].Err != nil:
			final(i)
		default:
			confirmed = append(confirmed, i)
		}
	}
	if len(confirmed) == 0 {
		return out
	}
	if confirm == nil || !confirm(append([]WorktreeRemovalResult(nil), out...)) {
		for _, i := range confirmed {
			out[i].Err = fmt.Errorf("%w: worktree %s", ErrNotConfirmed, xs[i].wt.Path)
			final(i)
		}
		return out
	}
	for _, i := range confirmed {
		out[i].Message, out[i].Err = xs[i].apply(ctx, w, opts)
		final(i)
	}
	return out
}
