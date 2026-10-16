package ops

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jponge/sheaf/internal/gitx"
)

// StatusOptions configures CollectStatus.
type StatusOptions struct {
	Filter CheckoutFilter
	// DirtyOnly keeps only the checkouts that are not clean (see
	// CheckoutStatus.Clean) or whose status failed.
	DirtyOnly bool
	// Fetch fetches origin into each selected repository first (once per
	// repository); otherwise status never accesses the network.
	Fetch bool
	Jobs  int
}

// CheckoutStatus is the status of one checkout.
type CheckoutStatus struct {
	Checkout
	Status *gitx.RepoStatus // nil when Err is set
	Err    error
}

// Clean reports whether the status is known and the work tree has no
// staged, unstaged, untracked or conflicted entries. Unlike
// gitx.RepoStatus.Dirty (what update checks), untracked files make a
// checkout not clean here: status shows everything not committed. Stashes
// and unpushed commits do not count.
func (c CheckoutStatus) Clean() bool {
	s := c.Status
	return c.Err == nil && s != nil && s.Staged == 0 && s.Unstaged == 0 && s.Untracked == 0 && s.Conflicted == 0
}

// StatusReport is the result of CollectStatus.
type StatusReport struct {
	// Checkouts are sorted like ListCheckouts.
	Checkouts []CheckoutStatus
	// Problems lists the checkouts that could not be listed and, with
	// StatusOptions.Fetch, the repositories whose fetch failed (their
	// checkouts are still reported, as of the last successful fetch).
	Problems []Problem
}

// coSerialize wraps rep so that concurrent callers report one at a time.
func coSerialize(rep Reporter) func(Event) {
	if rep == nil {
		rep = Discard
	}
	var mu sync.Mutex
	return func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		rep.Report(e)
	}
}

// coFetchSets fetches each repository of sets once (up to jobs in parallel),
// reporting fetch events, and returns the problems of failed fetches.
func coFetchSets(ctx context.Context, sets []*coRepoCheckouts, jobs int, report func(Event)) []Problem {
	probs := make([]*Problem, len(sets))
	coForEach(jobs, len(sets), func(i int) {
		s := sets[i]
		if err := ctx.Err(); err != nil {
			probs[i] = &Problem{Repo: s.name, Path: s.primary, Err: fmt.Errorf("fetch not started (interrupted): %w", err)}
			return
		}
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindStart})
		if err := coFetchRepo(ctx, s.primary, s.remotes); err != nil {
			probs[i] = &Problem{Repo: s.name, Path: s.primary, Err: fmt.Errorf("fetch failed: %w", err)}
			report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindFailed, Err: err})
			return
		}
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + s.remotes.String()})
	})
	var out []Problem
	for _, p := range probs {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func coSortProblems(ps []Problem) {
	slices.SortStableFunc(ps, func(a, b Problem) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), strings.Compare(a.Path, b.Path))
	})
}

// CollectStatus returns the status of every selected checkout (gitx.Status,
// which takes no lock and runs in parallel, up to Jobs at a time; stashes are
// repository-wide, so all checkouts of a repository report the same count).
// It reports fetch events only (with Fetch). The error is only set for an
// invalid filter.
func CollectStatus(ctx context.Context, w *Workspace, opts StatusOptions, rep Reporter) (StatusReport, error) {
	sets, probs, err := coEnumerate(ctx, w, opts.Filter, opts.Jobs)
	if err != nil {
		return StatusReport{}, err
	}
	if opts.Fetch {
		probs = append(probs, coFetchSets(ctx, sets, opts.Jobs, coSerialize(rep))...)
	}
	var cs []CheckoutStatus
	for _, s := range sets {
		for _, c := range s.checkouts {
			cs = append(cs, CheckoutStatus{Checkout: c})
		}
	}
	coForEach(opts.Jobs, len(cs), func(i int) {
		cs[i].Status, cs[i].Err = gitx.Status(ctx, cs[i].Path)
	})
	if opts.DirtyOnly {
		cs = slices.DeleteFunc(cs, CheckoutStatus.Clean)
	}
	coSortProblems(probs)
	return StatusReport{Checkouts: cs, Problems: probs}, nil
}
