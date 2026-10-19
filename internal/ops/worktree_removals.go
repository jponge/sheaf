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
// rep is told the units of work, one per worktree (see Planner), and
// receives the events of each worktree as RemoveWorktree emits them (those
// of the fetch of its repository included), then its final event: KindDone
// with its message, KindFailed, or KindSkipped with ErrNotConfirmed when
// the confirmation was declined (also for the worktrees refused or that
// could not be planned, the Err of their result being unchanged: nothing
// was attempted).
func RemoveWorktrees(ctx context.Context, w *Workspace, items []WorktreeRemoval, opts WorktreeRemoveOptions, confirm func([]WorktreeRemovalResult) bool, rep Reporter) []WorktreeRemovalResult {
	ctx = withCommandCache(ctx)
	report := serialize(rep)
	plan(rep, len(items))
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
	// One fetch per repository, before its worktrees are planned.
	for _, name := range repos {
		var those []int
		for i, x := range xs {
			if x != nil && x.r.Name == name {
				those = append(those, i)
			}
		}
		first := xs[those[0]]
		ev := func(phase, msg string) {
			for _, i := range those {
				xs[i].ev(phase, msg)
			}
		}
		var f removalFetch
		var err error
		if opts.Offline {
			f = offlineRemovalFetch(first.r, ev)
		} else {
			f, err = fetchForRemoval(ctx, gitx.Net{Interactive: opts.Interactive}, first.primary, first.r, ev)
		}
		for _, i := range those {
			x := xs[i]
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
		if out[i].Err == nil {
			confirmed = append(confirmed, i)
		}
	}
	declined := false
	if len(confirmed) > 0 && (confirm == nil || !confirm(append([]WorktreeRemovalResult(nil), out...))) {
		declined = true
		for _, i := range confirmed {
			out[i].Err = fmt.Errorf("%w: worktree %s", ErrNotConfirmed, xs[i].wt.Path)
		}
	}
	final := func(i int) {
		e := Event{Repo: items[i].Repo, Worktree: items[i].Target.Name, Phase: PhaseWorktree, Message: out[i].Message}
		if x := xs[i]; x != nil {
			e.Repo, e.Worktree = x.r.Name, x.wt.Dir
		}
		switch {
		case out[i].Err == nil:
			e.Kind = KindDone
		case errors.Is(out[i].Err, ErrNotConfirmed):
			e.Kind, e.Err = KindSkipped, out[i].Err
		case declined:
			e.Kind, e.Err = KindSkipped, errors.Join(ErrNotConfirmed, out[i].Err)
		default:
			e.Kind, e.Err = KindFailed, out[i].Err
		}
		report(e)
	}
	for i := range items {
		if !declined && slices.Contains(confirmed, i) {
			out[i].Message, out[i].Err = xs[i].apply(ctx, w, opts)
		}
		final(i)
	}
	return out
}
