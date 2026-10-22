package ops

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

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
	// Interactive fetches in interactive git mode (see gitx.Net).
	Interactive bool
	// HeadTimes sets CheckoutStatus.HeadTime, with one more git process
	// per repository.
	HeadTimes bool

	// fetchHook, when set by a test, runs while a repository fetch holds
	// its per-repository lock (see fetchRepo).
	fetchHook func(commonDir string)
}

// CheckoutStatus is the status of one checkout.
type CheckoutStatus struct {
	Checkout
	Status *gitx.RepoStatus // nil when Err is set
	Err    error
	// HeadTime is the author date of HEAD (what git log shows), with
	// StatusOptions.HeadTimes; zero when unknown (no commit yet, or the
	// status or the commit could not be read).
	HeadTime time.Time
	// PullRequest is set for a checkout on a branch tracking the head of a
	// pull request of the repository (branch.<name>.merge =
	// refs/pull/<n>/head of the repository's remote, upstream for a clone
	// of a fork, as `sheaf wt pr` sets up for a pull request from a fork,
	// and as browse sees it), for which git has no upstream
	// (Status.Upstream is empty).
	PullRequest *PullRequestHead
}

// PullRequestHead is the pull request whose head a branch tracks (see
// CheckoutStatus.PullRequest).
type PullRequestHead struct {
	Number int
	// Recorded tells whether the head recorded by the last fetch of the
	// pull request (gitx.PullHeadRef, by `wt pr`) exists; Ahead and Behind
	// then count the commits of HEAD not in it and the reverse, else they
	// are 0.
	Recorded      bool
	Ahead, Behind int
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

// serialize wraps rep so that concurrent callers report one at a time.
func serialize(rep Reporter) func(Event) {
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

// fetchSets fetches each repository of sets once (up to jobs in parallel),
// reporting fetch events, and returns the problems of failed fetches.
func fetchSets(ctx context.Context, net gitx.Net, sets []*repoCheckouts, jobs int, report func(Event), hook func(commonDir string)) []Problem {
	probs := make([]*Problem, len(sets))
	forEach(jobs, len(sets), func(i int) {
		s := sets[i]
		if err := ctx.Err(); err != nil {
			probs[i] = &Problem{Repo: s.name, Path: s.primary, Err: fmt.Errorf("fetch not started (interrupted): %w", err)}
			return
		}
		report(Event{Repo: s.name, Phase: PhaseFetch, Kind: KindStart})
		if err := fetchRepo(ctx, net, s.primary, s.remotes, hook); err != nil {
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

func sortProblems(ps []Problem) {
	slices.SortStableFunc(ps, func(a, b Problem) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), naturalCompare(a.Path, b.Path))
	})
}

// CollectStatus returns the status of every selected checkout (gitx.Status,
// which takes no lock and runs in parallel, up to Jobs at a time; stashes are
// repository-wide, so all checkouts of a repository report the same count).
// It reports fetch events only (with Fetch). The error is only set for an
// invalid filter.
func CollectStatus(ctx context.Context, w *Workspace, opts StatusOptions, rep Reporter) (StatusReport, error) {
	ctx = withCommandCache(ctx)
	sets, probs, err := enumerate(ctx, w, opts.Filter, opts.Jobs)
	if err != nil {
		return StatusReport{}, err
	}
	if opts.Fetch {
		probs = append(probs, fetchSets(ctx, gitx.Net{Interactive: opts.Interactive}, sets, opts.Jobs, serialize(rep), opts.fetchHook)...)
	}
	var cs []CheckoutStatus
	for _, s := range sets {
		for _, c := range s.checkouts {
			cs = append(cs, CheckoutStatus{Checkout: c})
		}
	}
	forEach(opts.Jobs, len(cs), func(i int) {
		cs[i].Status, cs[i].Err = gitx.Status(ctx, cs[i].Path)
	})
	pullRequests(ctx, sets, cs, opts.Jobs)
	if opts.HeadTimes {
		headTimes(ctx, sets, cs, opts.Jobs)
	}
	if opts.DirtyOnly {
		cs = slices.DeleteFunc(cs, CheckoutStatus.Clean)
	}
	sortProblems(probs)
	return StatusReport{Checkouts: cs, Problems: probs}, nil
}

// pullRequests sets the PullRequest of cs, the checkouts of sets in order:
// for each repository with a checkout on a branch without upstream, one
// read of its branch configuration, then, for each checkout on a pull
// request branch (see CheckoutStatus.PullRequest), one count of its commits ahead of and behind the recorded
// head (gitx.PullHeadRef), up to jobs repositories at a time. Best effort:
// what cannot be read is left unset (not recorded).
func pullRequests(ctx context.Context, sets []*repoCheckouts, cs []CheckoutStatus, jobs int) {
	starts := make([]int, len(sets))
	n := 0
	for i, s := range sets {
		starts[i] = n
		n += len(s.checkouts)
	}
	forEach(jobs, len(sets), func(i int) {
		repo := cs[starts[i] : starts[i]+len(sets[i].checkouts)]
		untracked := func(c CheckoutStatus) bool {
			return c.Status != nil && c.Status.Branch != "" && !c.Status.Detached && c.Status.Upstream == ""
		}
		if !slices.ContainsFunc(repo, untracked) {
			return
		}
		prs, err := gitx.BranchPullRequests(ctx, sets[i].primary, sets[i].remotes.Reference)
		if err != nil {
			return
		}
		for j := range repo {
			if !untracked(repo[j]) {
				continue
			}
			num, ok := prs[repo[j].Status.Branch]
			if !ok {
				continue
			}
			p := &PullRequestHead{Number: num}
			if !repo[j].Status.Unborn {
				if ahead, behind, err := gitx.AheadBehind(ctx, repo[j].Path, "HEAD", gitx.PullHeadRef(num)); err == nil {
					p.Recorded, p.Ahead, p.Behind = true, ahead, behind
				}
			}
			repo[j].PullRequest = p
		}
	})
}

// headTimes sets the HeadTime of cs, the checkouts of sets in order, with
// one gitx.CommitTimes per repository, up to jobs at a time. The dates are
// best effort: when they cannot be read, they stay unknown. When the call
// of a repository fails (e.g. a HEAD commit pruned since its status was
// read), each commit is asked for alone, so that only the unreadable ones
// lose their date.
func headTimes(ctx context.Context, sets []*repoCheckouts, cs []CheckoutStatus, jobs int) {
	starts := make([]int, len(sets))
	n := 0
	for i, s := range sets {
		starts[i] = n
		n += len(s.checkouts)
	}
	forEach(jobs, len(sets), func(i int) {
		repo := cs[starts[i] : starts[i]+len(sets[i].checkouts)]
		var oids []string
		for _, c := range repo {
			if c.Status != nil && c.Status.Head != "" {
				oids = append(oids, c.Status.Head)
			}
		}
		times, err := gitx.CommitTimes(ctx, sets[i].primary, oids)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			times = map[string]time.Time{}
			for _, id := range oids {
				if one, err := gitx.CommitTimes(ctx, sets[i].primary, []string{id}); err == nil {
					maps.Copy(times, one)
				}
			}
		}
		for j := range repo {
			if repo[j].Status != nil {
				repo[j].HeadTime = times[repo[j].Status.Head]
			}
		}
	})
}
