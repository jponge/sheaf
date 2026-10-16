package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// PhasePrune is the phase of prune events.
const PhasePrune = "prune"

// PruneOptions configures PlanPrune and ApplyPrune.
type PruneOptions struct {
	// Repos selects repositories (see CheckoutFilter.Repos).
	Repos []string
	// Offline skips the fetches: gone branches are detected as of the last
	// fetch.
	Offline bool
	// IncludeBranches also proposes local branches without worktree.
	IncludeBranches bool
	// DeleteBranches also deletes the branches of removed worktrees, when
	// merged (git branch -d semantics; never -D).
	DeleteBranches bool
	// Jobs bounds the repositories fetched and inspected in parallel.
	Jobs int
	// WorkingDir is the caller's working directory; empty means os.Getwd.
	WorkingDir string
}

// PruneCandidate is a worktree, or a local branch without worktree, whose
// branch is gone or merged.
type PruneCandidate struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Worktree is false for a branch without worktree (IncludeBranches).
	Worktree bool   `json:"worktree"`
	Dir      string `json:"dir,omitempty"`
	Path     string `json:"path,omitempty"`
	Head     string `json:"head"`
	// Gone: the upstream was deleted on the remote. Merged: the tip was
	// merged into origin/<default branch>.
	Gone   bool   `json:"gone"`
	Merged bool   `json:"merged"`
	Reason string `json:"reason"`
	// Notes lists what does not prevent the removal but goes with it or is
	// worth knowing (non-blocking risks, e.g. "3 ignored files (deleted with
	// the worktree)"), and the blocking risks found when planning, which
	// will keep the worktree.
	Notes []string `json:"notes,omitempty"`
}

// PruneSkip is a gone or merged branch that is not proposed, and why.
type PruneSkip struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Tag is set (and Branch empty) for a worktree pinned at a tag, which is
	// never pruned.
	Tag    string `json:"tag,omitempty"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// PrunePlan is the result of PlanPrune.
type PrunePlan struct {
	Candidates []PruneCandidate
	Skipped    []PruneSkip
	// Problems are repositories that could not be inspected (missing
	// primary checkout, failed fetch), which get no candidates, or only
	// partially (origin/<default branch> missing: merged branches are not
	// detected).
	Problems []Problem

	// fetched holds the repositories whose origin was fetched by PlanPrune:
	// ApplyPrune does not fetch them again.
	fetched map[string]bool
	// distrust holds the repositories whose fork refs are not trusted (fork
	// gone, or --offline), see forkDistrust.
	distrust map[string]forkDistrust
}

// PlanPrune finds, in the selected repositories, the linked worktrees whose
// branch is gone (its upstream was deleted on the remote and pruned) or
// merged, and with opts.IncludeBranches the local branches without worktree
// in that state. Unless opts.Offline, origin is fetched first (once per
// repository, up to opts.Jobs repositories in parallel); a repository whose
// fetch fails gets no candidates, except a forked repository whose fork
// appears to be gone: its fork refs are then not trusted (see forkDistrust;
// also with opts.Offline), branches tracking the fork count as gone, and
// the branch deletions of ApplyPrune do not rely on those refs.
//
// A branch is merged when its tip is reachable from origin/<default branch>
// but is not on the first-parent history of origin/<default branch>, i.e.
// it was merged with a merge commit. A branch whose tip is on that
// first-parent line (a new branch without commits of its own, or one
// fast-forwarded into the default branch) does not count: otherwise every
// freshly created branch would be proposed. Squash and rebase merges create
// new commits and are not detected either; they are found as gone once the
// remote branch is deleted.
//
// Never proposed: the default branch, the branch of the primary checkout
// (whatever it is), detached worktrees, worktrees outside the repository
// directory, locked or missing ones, and pull request branches set up by
// AddPRWorktree (upstream refs/pull/<n>/head); gone or merged ones are listed
// in PrunePlan.Skipped with the reason. Worktrees pinned at a tag
// (AddTagWorktree) are never proposed and always listed in Skipped
// (PruneSkip.Tag). The error is only set for an invalid repository filter.
func PlanPrune(ctx context.Context, w *Workspace, opts PruneOptions, rep Reporter) (PrunePlan, error) {
	ctx = withCommandCache(ctx)
	repos, err := coSelectRepos(w.Bundle(), opts.Repos)
	if err != nil {
		return PrunePlan{}, err
	}
	report := coSerialize(rep)
	type repoPlan struct {
		cands    []PruneCandidate
		skips    []PruneSkip
		probs    []Problem
		distrust forkDistrust
	}
	plans := make([]repoPlan, len(repos))
	coForEach(opts.Jobs, len(repos), func(i int) {
		p := &plans[i]
		p.cands, p.skips, p.probs, p.distrust = planPruneRepo(ctx, w.Root, repos[i], opts, report)
	})
	out := PrunePlan{fetched: map[string]bool{}, distrust: map[string]forkDistrust{}}
	for i, p := range plans {
		if p.distrust.set() {
			out.distrust[repos[i].Name] = p.distrust
		}
		if !opts.Offline && len(p.cands) > 0 {
			out.fetched[repos[i].Name] = true // candidates exist only after a successful fetch
		}
		out.Candidates = append(out.Candidates, p.cands...)
		out.Skipped = append(out.Skipped, p.skips...)
		out.Problems = append(out.Problems, p.probs...)
	}
	return out, nil
}

func planPruneRepo(ctx context.Context, root string, r bundle.Repository, opts PruneOptions, report func(Event)) ([]PruneCandidate, []PruneSkip, []Problem, forkDistrust) {
	var distrust forkDistrust
	primary, common, prob := coCheckPrimary(ctx, root, r)
	if prob != nil {
		return nil, nil, []Problem{*prob}, distrust
	}
	fail := func(err error) ([]PruneCandidate, []PruneSkip, []Problem, forkDistrust) {
		return nil, nil, []Problem{{Repo: r.Name, Path: primary, Err: err}}, distrust
	}
	rs := RemotesOf(r)
	if !opts.Offline {
		report(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindStart})
		if err := coFetchRepo(ctx, primary, rs); err != nil {
			var rf *remoteFetchError
			if ctx.Err() != nil || !onlyForkFetchFailed(err, rs) || !errors.As(err, &rf) || !forkGoneError(rf.Err, rs.Fork) {
				report(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindFailed, Err: err})
				return fail(fmt.Errorf("fetch failed, nothing proposed: %w", err))
			}
			// The fork is gone: go on without trusting its refs.
			distrust = goneForkDistrust(rs.Fork)
			report(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + rs.Reference + "; " + distrust.why + ": its remote-tracking refs are not trusted"})
		} else {
			report(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindDone, Message: "fetched " + rs.String()})
		}
	} else if rs.Forked() {
		distrust = offlineForkDistrust(rs.Fork)
	}
	infos, repoDir, _, err := listCheckedRepo(ctx, r, primary, common, true)
	if err != nil {
		return fail(err)
	}
	branches, err := gitx.LocalBranches(ctx, primary)
	if err != nil {
		return fail(err)
	}
	inUse, err := gitx.BranchesInUse(ctx, primary)
	if err != nil {
		return fail(err)
	}
	var probs []Problem
	target := "refs/remotes/" + RemotesOf(r).Reference + "/" + r.DefaultBranch
	merged := map[string]bool{}
	if ok, err := refExists(ctx, primary, target); err != nil {
		return fail(err)
	} else if ok {
		names, err := gitx.MergedBranches(ctx, primary, target)
		if err != nil {
			return fail(err)
		}
		for _, n := range names {
			merged[n] = true
		}
	} else {
		probs = append(probs, Problem{Repo: r.Name, Path: primary, Err: fmt.Errorf("%s does not exist: merged branches are not detected", strings.TrimPrefix(target, "refs/remotes/"))})
	}
	checkedOut := map[string]WorktreeInfo{}
	for _, in := range infos {
		if in.Branch != "" {
			if _, dup := checkedOut[in.Branch]; !dup {
				checkedOut[in.Branch] = in
			}
		}
	}
	var (
		cands []PruneCandidate
		skips []PruneSkip
	)
	for _, b := range branches {
		if b.Name == r.DefaultBranch {
			continue
		}
		gone, goneNote := b.Gone, ""
		if distrust.gone && distrust.covers(b.Upstream) {
			gone, goneNote = true, " ("+distrust.why+")"
		}
		isMerged := false
		if merged[b.Name] {
			fp, err := onFirstParentLine(ctx, primary, b.Head, target)
			if err != nil {
				return fail(err)
			}
			isMerged = !fp
		}
		if !gone && !isMerged {
			continue
		}
		var why []string
		if gone {
			why = append(why, "upstream "+strings.TrimPrefix(b.Upstream, "refs/remotes/")+" is gone"+goneNote)
		}
		if isMerged {
			why = append(why, "merged into "+strings.TrimPrefix(target, "refs/remotes/"))
		}
		reason := strings.Join(why, ", ")
		skip := func(path, msg string) {
			skips = append(skips, PruneSkip{Repo: r.Name, Branch: b.Name, Path: path, Reason: reason + ", but " + msg})
		}
		if n, ok := b.PullRequest(); ok {
			skip("", fmt.Sprintf("it is the branch of pull request #%d (never pruned; remove it with `sheaf wt rm`)", n))
			continue
		}
		if u, busy := busyUse(inUse[b.Name]); busy {
			skip(u.Path, "it is "+describeUse(u))
			continue
		}
		c := PruneCandidate{Repo: r.Name, Branch: b.Name, Head: b.Head, Gone: gone, Merged: isMerged, Reason: reason}
		in, ok := checkedOut[b.Name]
		switch {
		case ok && in.Primary:
			skip(in.Path, "it is checked out in the primary checkout, which is never pruned")
		case ok && in.unusable != nil:
			skip(in.Path, "its worktree cannot be used: "+in.unusable.Error())
		case ok && in.Outside:
			skip(in.Path, "its worktree is outside "+repoDir)
		case ok && in.Prunable:
			skip(in.Path, "its worktree directory is missing (clean it up with `git worktree prune`)")
		case ok && in.Locked:
			skip(in.Path, "its worktree is locked")
		case ok:
			c.Worktree, c.Dir, c.Path = true, in.Dir, in.Path
			if risks, err := gitx.DeletionRisksWithOptions(ctx, in.Path, gitx.DeletionRisksOptions{UntrustedRemotes: distrust.untrusted()}); err != nil {
				if ctx.Err() != nil {
					return fail(ctx.Err())
				}
				c.Notes = []string{"data at risk not assessed: " + firstLineOf(err.Error())}
			} else {
				c.Notes = riskNotes(risks)
			}
			cands = append(cands, c)
		case opts.IncludeBranches:
			cands = append(cands, c)
		default:
			skip("", "it has no worktree (pass --include-branches to delete such branches)")
		}
	}
	for _, in := range infos {
		if in.Tag != "" {
			skips = append(skips, PruneSkip{Repo: r.Name, Tag: in.Tag, Path: in.Path,
				Reason: "pinned at tag " + in.Tag + " (tag worktrees are never pruned; remove it with `sheaf wt rm`)"})
		}
	}
	return cands, skips, probs, distrust
}

// busyUse returns a use of a branch other than being checked out (a rebase
// or bisect in progress), which makes it untouchable.
func busyUse(uses []gitx.BranchUse) (gitx.BranchUse, bool) {
	for _, u := range uses {
		if u.How != "checked out" {
			return u, true
		}
	}
	return gitx.BranchUse{}, false
}

// riskNotes describes the risks of a worktree for prune listings.
func riskNotes(risks []gitx.Risk) []string {
	var out []string
	for _, r := range risks {
		switch {
		case r.Kind.RepositoryWide():
			out = append(out, r.String()+" (not affected: they belong to the repository)")
		case r.Informational():
			out = append(out, r.String()+" (deleted with the worktree)")
		default:
			out = append(out, r.String()+" (keeps the worktree)")
		}
	}
	return out
}

// onFirstParentLine reports whether commit is on the first-parent history of
// target: rev-list walks target's first parents down to the ancestors of
// commit's parents, so commit is listed iff it is on that line.
func onFirstParentLine(ctx context.Context, dir, commit, target string) (bool, error) {
	out, err := gitx.Run(ctx, dir, "rev-list", "--first-parent", target, "--not", commit+"^@")
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Fields(out), commit), nil
}

// PruneOutcome classifies a PruneResult.
type PruneOutcome int

const (
	// PruneRemoved: the worktree (and, if requested, its branch) or the
	// branch was removed.
	PruneRemoved PruneOutcome = iota + 1
	// PruneKept: left alone for safety (data at risk, unmerged branch,
	// changed since the plan); Message says why.
	PruneKept
	// PruneFailed: an error; Err says why.
	PruneFailed
)

// PruneResult is the outcome of ApplyPrune for one candidate.
type PruneResult struct {
	PruneCandidate
	Outcome PruneOutcome
	Message string
	Err     error
}

// ApplyPrune removes the candidates of plan, one after the other, and
// reports one PhasePrune event per candidate (KindDone, KindSkipped for
// PruneKept, KindFailed).
//
// A worktree is removed with RemoveWorktree and all its safety checks
// (never forced): work-tree-local data at risk, a lock, the working
// directory inside it, or a failed fetch keep it. It must still be the same
// worktree on the same commit as in the plan. With opts.DeleteBranches its
// branch is deleted when merged into its upstream or origin/<default
// branch>, or contained in another ref (git branch -d semantics); otherwise
// the worktree is removed and the branch kept. A branch without worktree is
// deleted under the same condition, only if it still points at the planned
// commit and is not checked out (compare-and-swap).
func ApplyPrune(ctx context.Context, w *Workspace, plan PrunePlan, opts PruneOptions, rep Reporter) []PruneResult {
	// Every read re-checks the plan: never answered from a cache the caller
	// may share with the planning.
	ctx = gitx.WithFreshReads(ctx)
	report := coSerialize(rep)
	wd := opts.WorkingDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	out := make([]PruneResult, 0, len(plan.Candidates))
	for _, c := range plan.Candidates {
		var res PruneResult
		switch {
		case ctx.Err() != nil:
			res = PruneResult{PruneCandidate: c, Outcome: PruneFailed, Message: "not started (interrupted)", Err: ctx.Err()}
		case c.Worktree:
			res = pruneWorktree(ctx, w, c, opts, plan.fetched[c.Repo], plan.distrust[c.Repo], wd, report)
		default:
			res = pruneBranch(ctx, w, c, plan.distrust[c.Repo])
		}
		e := Event{Repo: c.Repo, Worktree: c.Dir, Phase: PhasePrune, Message: res.Message}
		switch res.Outcome {
		case PruneRemoved:
			e.Kind = KindDone
		case PruneKept:
			e.Kind = KindSkipped
		default:
			e.Kind, e.Err = KindFailed, res.Err
		}
		report(e)
		out = append(out, res)
	}
	return out
}

func pruneWorktree(ctx context.Context, w *Workspace, c PruneCandidate, opts PruneOptions, fetched bool, distrust forkDistrust, wd string, report func(Event)) PruneResult {
	res := PruneResult{PruneCandidate: c}
	changed := ""
	ro := WorktreeRemoveOptions{DeleteBranch: opts.DeleteBranches, Offline: opts.Offline, WorkingDir: wd, originFetched: fetched, forkDistrust: distrust,
		Confirm: func(p WorktreeRemovePlan) bool {
			res.Notes = riskNotes(p.Risks)
			switch {
			case p.Worktree.Path != c.Path:
				changed = fmt.Sprintf("worktree %s is now at %s", c.Dir, p.Worktree.Path)
			case p.Worktree.Branch != c.Branch:
				changed = fmt.Sprintf("worktree %s is now on %s, not %s", c.Dir, branchOrDetached(p.Worktree), c.Branch)
			case p.Worktree.Head != c.Head:
				if isAncestor(ctx, c.Path, c.Head, p.Worktree.Head) {
					changed = fmt.Sprintf("branch %s has new commits since it was proposed (now %s)", c.Branch, coShortID(p.Worktree.Head))
				} else {
					changed = fmt.Sprintf("branch %s moved from %s to %s since it was proposed", c.Branch, coShortID(c.Head), coShortID(p.Worktree.Head))
				}
			default:
				return true
			}
			return false
		}}
	// By directory: the branch may have moved elsewhere (even to the
	// primary checkout) since the plan; Confirm checks branch and commit.
	tgt := WorktreeTarget{Name: c.Dir, Kind: TargetDir}
	msg, err := RemoveWorktree(ctx, w, c.Repo, tgt, ro, report)
	var refused *WorktreeRefusedError
	if errors.As(err, &refused) && ro.DeleteBranch && len(refused.Plan.Hard) == 0 && len(refused.Plan.Blocking()) == 0 && refused.Plan.Unmerged != "" {
		// Only the branch deletion was refused: remove the worktree alone.
		unmerged := refused.Plan.Unmerged
		ro.DeleteBranch = false
		msg, err = RemoveWorktree(ctx, w, c.Repo, tgt, ro, report)
		if err == nil {
			msg += "; kept branch " + c.Branch + ": " + unmerged
		}
	}
	switch {
	case err == nil:
		res.Outcome, res.Message = PruneRemoved, msg
	case errors.As(err, &refused):
		var why []string
		why = append(why, refused.Plan.Hard...)
		for _, r := range refused.Plan.Blocking() {
			why = append(why, r.String())
		}
		res.Notes = riskNotes(refused.Plan.Risks)
		res.Outcome = PruneKept
		res.Message = "kept worktree " + describe(refused.Plan.Worktree) + ": " + strings.Join(why, "; ") + " (review it, then use `sheaf wt rm`)"
	case errors.Is(err, ErrNotConfirmed):
		res.Outcome, res.Message = PruneKept, "kept: "+changed+"; run prune again"
	case errors.Is(err, errNoWorktree):
		res.Outcome, res.Message = PruneKept, "kept: worktree "+c.Dir+" no longer exists; run prune again"
	default:
		res.Outcome, res.Message, res.Err = PruneFailed, err.Error(), err
	}
	return res
}

func pruneBranch(ctx context.Context, w *Workspace, c PruneCandidate, distrust forkDistrust) PruneResult {
	res := PruneResult{PruneCandidate: c}
	failed := func(err error) PruneResult {
		res.Outcome, res.Message, res.Err = PruneFailed, err.Error(), err
		return res
	}
	kept := func(msg string) PruneResult {
		res.Outcome, res.Message = PruneKept, "kept branch "+c.Branch+": "+msg
		return res
	}
	r, ok := lookupRepo(w.Bundle(), c.Repo)
	if !ok {
		return failed(fmt.Errorf("no repository named %q in the bundle", c.Repo))
	}
	primary, _, prob := coCheckPrimary(ctx, w.Root, r)
	if prob != nil {
		return failed(prob)
	}
	// ApplyPrune already reads fresh; lockBundleFresh keeps this correct on
	// its own (defence in depth).
	ctx, unlock, err := lockBundleFresh(ctx, w.Root, func() {})
	if err != nil {
		return failed(fmt.Errorf("locking %s: %w", bundle.FileName, err))
	}
	defer unlock()
	if ok, err := refExists(ctx, primary, "refs/heads/"+c.Branch); err != nil {
		return failed(err)
	} else if !ok {
		return kept("it no longer exists")
	}
	var plan WorktreeRemovePlan
	if err := checkMerged(ctx, primary, r, c.Branch, &plan, distrust); err != nil {
		return failed(err)
	}
	if plan.branchOID != c.Head {
		return kept(fmt.Sprintf("it moved from %s to %s since it was proposed; run prune again", coShortID(c.Head), coShortID(plan.branchOID)))
	}
	if plan.Unmerged != "" {
		return kept(plan.Unmerged + " (delete it with `git branch -D " + c.Branch + "` if you are sure)")
	}
	if reason := deleteBranchIfUnchanged(ctx, primary, c.Branch, c.Head); reason != "" {
		return kept(reason)
	}
	res.Outcome = PruneRemoved
	if plan.MergedInto != "" {
		res.Message = fmt.Sprintf("deleted branch %s (merged into %s)", c.Branch, plan.MergedInto)
	} else {
		res.Message = fmt.Sprintf("deleted branch %s (contained in %s)", c.Branch, plan.ContainedIn)
	}
	return res
}
