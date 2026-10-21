package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
	"github.com/spf13/cobra"
)

// TUI variants of the commands that need it. Each one ends like its CLI
// counterpart: the same summary on stdout and the same returned error (so
// the same exit status); the plain progress lines are replaced by the
// progress view.

// tuiAdd: `add --org <org>` without --all/--repo offers a menu of the
// organization's repositories, then clones with a progress view.
func tuiAdd(c *cobra.Command, d Deps, w *ops.Workspace, h hosting.Hosting, req ops.AddRequest, jobs int) error {
	s := newTUISession(c, d)
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	if req.Org != "" && !req.All && len(req.Repos) == 0 {
		fmt.Fprintf(c.ErrOrStderr(), "listing repositories of %s...\n", req.Org)
		stop := d.spin.start("") // the line above names the phase
		_, err := ops.Add(s.ctx, w, h, req, jobs, ops.Discard)
		stop()
		var sel *ops.SelectionRequiredError
		if !errors.As(err, &sel) || len(sel.Available) == 0 {
			return err
		}
		items := orgRepoItems(sel.Available, w.Bundle())
		keys, err := s.pick(tui.NewMultiSelect("Add repositories of "+sel.Org, items).WithLockedLabel("present"))
		if err != nil {
			return err
		}
		// Present repositories (selected, disabled) are kept: Add skips
		// them, and tracks the organization when nothing else is added,
		// as with --repo.
		req.Repos = keys
		if len(req.Repos) == 0 {
			fmt.Fprintln(out, "no repository selected; nothing changed")
			return nil
		}
	}
	var sum ops.Summary
	var addErr error
	before := w.Bundle()
	defer s.ctl.during(noteClone)()
	s.progress("Adding repositories", progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		sum, addErr = ops.Add(ctx, w, h, req, jobs, rep)
	})
	if addErr != nil {
		return addErr
	}
	printFailureDetails(c.ErrOrStderr(), sum)
	err := printSummary(out, sum)
	printNewSources(out, before, w.Bundle())
	return err
}

// printNewSources tells which organizations are tracked in after (the
// bundle after an add) and were not in before.
func printNewSources(w io.Writer, before, after *bundle.Bundle) {
	for _, src := range after.Sources {
		if !slices.ContainsFunc(before.Sources, func(b bundle.Source) bool { return strings.EqualFold(b.Org, src.Org) }) {
			fmt.Fprintf(w, "now tracking %s: sync will propose its new repositories\n", src.Org)
		}
	}
}

// tuiSummary runs an operation returning a Summary in a progress view and
// prints the summary.
func tuiSummary(c *cobra.Command, d Deps, title string, run func(ctx context.Context, rep ops.Reporter) ops.Summary) error {
	s := newTUISession(c, d)
	var sum ops.Summary
	s.progress(title, progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) { sum = run(ctx, rep) })
	printFailureDetails(c.ErrOrStderr(), sum)
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	return printSummary(out, sum)
}

// tuiUpdate runs update in a progress view and prints its summary.
func tuiUpdate(c *cobra.Command, d Deps, w *ops.Workspace, opts ops.UpdateOptions, strict bool) error {
	s := newTUISession(c, d)
	opts.Force = s.ctl.force
	var r ops.UpdateReport
	var uerr error
	defer s.ctl.during(noteFastForward)()
	s.progress("Updating", progressOpts{Aside: ops.PhaseFetch}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		r, uerr = ops.Update(ctx, w, opts, rep)
	})
	if uerr != nil {
		return uerr
	}
	d.reportCounts(updateCounts(r))
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	return printUpdateSummary(out, r, strict)
}

// tuiInitAdopt offers the checkouts found by init (all preselected) and
// adopts the chosen ones. Declining leaves the new bundle empty and prints
// how to adopt later (init itself succeeded).
func tuiInitAdopt(c *cobra.Command, d Deps, root string, cands []ops.Candidate, adoptAll bool, jobs int) error {
	s := newTUISession(c, d)
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	var paths, names []string
	for _, cand := range cands {
		paths = append(paths, cand.Path)
		names = append(names, cand.Name)
	}
	if !adoptAll {
		keys, err := s.multiSelect("Adopt the checkouts found in "+root, candidateItems(cands))
		if err != nil && !errors.Is(err, errCancelled) {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintf(out, "nothing adopted; adopt them with: sheaf adopt %s\n", strings.Join(names, " "))
			return nil
		}
		paths = keys
	}
	w, err := ops.OpenWorkspace(root)
	if err != nil {
		return err
	}
	var sum ops.Summary
	defer s.ctl.during(noteAdopt)()
	s.progress("Adopting checkouts", progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		sum = ops.AdoptAll(ctx, w, paths, ops.AdoptOptions{Hosting: d.hosting()}, jobs, rep)
	})
	printFailureDetails(c.ErrOrStderr(), sum)
	err = printSummary(out, sum)
	var left []string
	for _, cand := range cands {
		if !slices.Contains(paths, cand.Path) {
			left = append(left, cand.Name)
		}
	}
	if len(left) > 0 {
		fmt.Fprintf(out, "not adopted: %s; adopt later with: sheaf adopt %s\n", strings.Join(left, ", "), strings.Join(left, " "))
	}
	return err
}

// tuiRemove removes a repository with a progress view; without --yes the
// removal plan is confirmed in a dialog (default no).
func tuiRemove(c *cobra.Command, d Deps, w *ops.Workspace, name string, opts ops.RemoveOptions, yes bool) error {
	s := newTUISession(c, d)
	var msg string
	var rerr error
	title, removing := "Checking "+name+" for removal", "Removing "+name
	if yes {
		title = removing
	}
	s.progress(title, progressOpts{Single: name, Confirmed: removing}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
		emit := func(e ops.Event) { // like CLI mode's
			if e.Repo == "" {
				e.Repo = name
			}
			if e.Phase == "" {
				e.Phase = ops.PhaseRemove
			}
			if e.Kind == 0 {
				e.Kind = ops.KindProgress
			}
			rep.Report(e)
		}
		opts.Confirm = func(p ops.RemovePlan) bool {
			lines := removePlanLines(p)
			if yes {
				for _, l := range lines {
					emit(ops.Event{Message: l})
				}
				return true
			}
			return ask("Remove "+name+"?", lines)
		}
		msg, rerr = ops.Remove(ctx, w, name, opts, emit)
		finalEvent(emit, msg, rerr)
	})
	if errors.Is(rerr, ops.ErrNotConfirmed) {
		return errCancelled
	}
	if rerr != nil {
		return rerr
	}
	fmt.Fprintln(c.OutOrStdout(), msg)
	return nil
}

// finalEvent reports the outcome of a single operation run outside
// ops.Execute (no final event of its own).
func finalEvent(emit func(ops.Event), msg string, err error) {
	switch {
	case errors.Is(err, ops.ErrNotConfirmed):
		emit(ops.Event{Kind: ops.KindSkipped, Message: "cancelled", Err: err})
	case err != nil:
		emit(ops.Event{Kind: ops.KindFailed, Err: err})
	default:
		emit(ops.Event{Kind: ops.KindDone, Message: msg})
	}
}

// tuiWorktreeRemove removes a worktree with a progress view; without --yes
// the plan (risks, branch deletion, commits lost with -D) is confirmed in
// a dialog (default no).
func tuiWorktreeRemove(c *cobra.Command, d Deps, w *ops.Workspace, repo string, target ops.WorktreeTarget, opts ops.WorktreeRemoveOptions, yes bool) error {
	s := newTUISession(c, d)
	var msg string
	var rerr error
	removing := "Removing worktree " + target.Name + " of " + repo
	title := "Checking worktree " + target.Name + " of " + repo + " for removal"
	if yes {
		title = removing
	}
	s.progress(title, progressOpts{Single: repo + "/" + target.Name, Confirmed: removing}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
		emit := func(e ops.Event) { // like CLI mode's eventSink
			if e.Repo == "" {
				e.Repo = repo
			}
			if e.Phase == "" {
				e.Phase = ops.PhaseWorktree
			}
			if e.Kind == 0 {
				e.Kind = ops.KindProgress
			}
			rep.Report(e)
		}
		opts.Confirm = func(p ops.WorktreeRemovePlan) bool {
			lines := worktreeRemovePlanLines(p, opts.Force)
			if yes {
				for _, l := range lines {
					emit(ops.Event{Worktree: p.Worktree.Dir, Message: l})
				}
				return true
			}
			return ask(fmt.Sprintf("Remove worktree %s of %s?", p.Worktree.Dir, repo), lines)
		}
		msg, rerr = ops.RemoveWorktree(ctx, w, repo, target, opts, emit)
		finalEvent(emit, msg, rerr)
	})
	if errors.Is(rerr, ops.ErrNotConfirmed) {
		return errCancelled
	}
	if rerr != nil {
		return rerr
	}
	d.reportCounts(countParts(count{1, "removed"}))
	fmt.Fprintln(c.OutOrStdout(), msg)
	return nil
}

// tuiWorktreeRemoveMany removes several worktrees with one progress view
// and one dialog (default no) listing what will be removed, with the fate
// of each branch, and the worktrees refused (see ops.RemoveWorktrees). It
// prints the message of each removed worktree and returns the error of
// each worktree that was not removed, or errCancelled alone when the
// dialog was declined.
func tuiWorktreeRemoveMany(c *cobra.Command, d Deps, w *ops.Workspace, items []ops.WorktreeRemoval, opts ops.WorktreeRemoveOptions) []error {
	s := newTUISession(c, d)
	var results []ops.WorktreeRemovalResult
	declined := false
	n := plural(len(items), "worktree")
	s.progress("Checking "+n+" for removal", progressOpts{Confirmed: "Removing " + n}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
		results = ops.RemoveWorktrees(ctx, w, items, opts, func(rs []ops.WorktreeRemovalResult) bool {
			lines, removable := worktreeRemovalsLines(rs, opts.Force)
			if !ask(fmt.Sprintf("Remove %s?", plural(removable, "worktree")), lines) {
				declined = true
				return false
			}
			return true
		}, rep)
	})
	if declined {
		return []error{errCancelled}
	}
	var errs []error
	var removed, failed, interrupted int
	for _, r := range results {
		switch {
		case r.Err == nil:
			removed++
			fmt.Fprintln(c.OutOrStdout(), r.Message)
		case interruptedErr(r.Err):
			interrupted++
		default:
			failed++
		}
		errs = append(errs, r.Err)
	}
	d.reportCounts(countParts(count{removed, "removed"}, count{failed, "failed"}, count{interrupted, "interrupted"}))
	return errs
}

// worktreeRemovalsLines describes the plans of several worktree removals
// (see ops.RemoveWorktrees): for each worktree to remove, a line saying
// so, then the lines of its plan (see worktreeRemovePlanLines), indented;
// for each worktree refused or that cannot be removed, why. It also
// returns the number of worktrees to remove.
func worktreeRemovalsLines(rs []ops.WorktreeRemovalResult, force bool) (lines []string, removable int) {
	for _, r := range rs {
		var refused *ops.WorktreeRefusedError
		switch {
		case r.Err == nil:
			removable++
			p := *r.Plan
			lines = append(lines, fmt.Sprintf("will remove worktree %s/%s [%s] at %s", p.Repo, p.Worktree.Dir, branchLabel(p.Worktree), p.Worktree.Path))
			for _, l := range worktreeRemovePlanLines(p, force) {
				if l != worktreeRemoveLine(p) {
					lines = append(lines, "  "+l)
				}
			}
		case errors.As(r.Err, &refused):
			lines = append(lines, refused.Summary())
		default:
			msg, _ := errorLine(r.Err)
			lines = append(lines, "cannot remove "+r.Repo+"/"+r.Target.Name+": "+msg)
		}
	}
	return lines, removable
}

// tuiPrune plans and, after confirmation (or with --yes), applies a prune
// in one progress view.
func tuiPrune(c *cobra.Command, d Deps, w *ops.Workspace, opts ops.PruneOptions, yes bool) error {
	s := newTUISession(c, d)
	var plan ops.PrunePlan
	var results []ops.PruneResult
	var perr error
	wasDeclined := false
	title := "Checking prune candidates"
	if yes {
		title = "Pruning"
	}
	s.progress(title, progressOpts{Aside: ops.PhaseFetch, Confirmed: "Pruning"}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
		plan, perr = ops.PlanPrune(ctx, w, opts, rep)
		if perr != nil || len(plan.Candidates) == 0 {
			return
		}
		if !yes {
			var lines []string
			for _, cand := range plan.Candidates {
				lines = append(lines, pruneCandidateLines(cand, true, opts.DeleteBranches)...)
			}
			if !ask(fmt.Sprintf("Prune %s?", plural(len(plan.Candidates), "candidate")), lines) {
				wasDeclined = true
				return
			}
		}
		results = ops.ApplyPrune(ctx, w, plan, opts, rep)
	})
	if perr != nil {
		return perr
	}
	if wasDeclined {
		return errCancelled
	}
	if len(plan.Candidates) == 0 {
		d.reportUnchanged("nothing to prune")
	}
	d.reportCounts(pruneCounts(results))
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	printPrune(out, plan, results, false, "")
	return pruneErr(s.ctx, plan, results)
}

// syncApply adds the new repositories add and removes the gone ones (see
// tuiSync), reporting to r, and returns their summaries. A planning r is
// told the units of both batches (one per proposal) up front rather than
// batch by batch, so that the progress ratio never goes backwards.
func syncApply(ctx context.Context, d Deps, w *ops.Workspace, add, gone []ops.Proposal, noFork bool, wd string, jobs int, r ops.Reporter) (addSum, rmSum ops.Summary) {
	if p, ok := r.(ops.Planner); ok {
		p.Plan(len(add) + len(gone))
		r = ops.ReporterFunc(r.Report) // not a Planner: the batches plan again
	}
	if len(add) > 0 {
		addSum = ops.SyncAddNewWithOptions(ctx, w, add, ops.CloneOptions{Hosting: d.hosting(), NoFork: noFork}, jobs, r)
	}
	if len(gone) > 0 {
		opts := ops.SyncRemoveOptions{Remove: ops.RemoveOptions{WorkingDir: wd, Confirm: func(ops.RemovePlan) bool { return true }}}
		rmSum = ops.SyncRemoveGone(ctx, w, gone, opts, jobs, r)
	}
	return addSum, rmSum
}

// tuiSync offers the proposals of sync in a menu (new and gone ones
// selectable, nothing preselected), confirms removals, and applies the
// choices like --add-new and --remove-gone --yes (never --force nor
// --allow-mass-removal: such refusals are displayed as failures). New
// repositories left unselected are neither added nor excluded: the next
// sync proposes them again (--exclude declines them for good).
func tuiSync(c *cobra.Command, d Deps, w *ops.Workspace, rep ops.SyncReport, noFork bool) error {
	s := newTUISession(c, d)
	out := d.leveled(c, c.OutOrStdout())
	defer flush(out)
	if len(rep.Proposals) == 0 {
		syWriteProposals(c.Context(), out, rep, w.Root, nil, false)
		d.reportUnchanged("nothing to sync")
		return syncErr(rep)
	}
	keys, err := s.multiSelect("Sync: choose the changes to apply", proposalItems(rep.Proposals))
	if err != nil {
		return err
	}
	var add, gone []ops.Proposal
	for _, p := range rep.Proposals {
		chosen := slices.Contains(keys, p.Key)
		switch {
		case p.Kind == ops.ProposalNew && chosen:
			add = append(add, p)
		case p.Kind == ops.ProposalGone && chosen:
			gone = append(gone, p)
		}
	}
	if len(gone) > 0 {
		lines := []string{"each one is checked like `sheaf remove`: refused when data would be lost (--force is never implied)"}
		for _, p := range gone {
			lines = append(lines, p.Name+" ("+p.Slug+")")
		}
		what := syPlural(len(gone), "repository", "repositories") + " gone from their organization"
		if len(gone) == 1 {
			what = "1 repository gone from its organization"
		}
		if err := s.confirm("Remove "+what+"?", lines); err != nil {
			return err
		}
	}
	if len(add) == 0 && len(gone) == 0 {
		fmt.Fprintln(out, "nothing selected; nothing changed")
		d.reportUnchanged("nothing selected; nothing changed")
		d.reportCancelled()
		return syncErr(rep)
	}
	errs := []error{syncErr(rep)}
	wd, err := d.getwd()
	if err != nil {
		return err
	}
	jobs := OptionsFrom(c).Jobs
	var addSum, rmSum ops.Summary
	if len(add) > 0 {
		defer s.ctl.during(noteClone)()
	}
	s.progress("Syncing", progressOpts{}, func(ctx context.Context, r ops.Reporter, _ askFunc) {
		addSum, rmSum = syncApply(ctx, d, w, add, gone, noFork, wd, jobs, r)
	})
	if len(add) > 0 {
		printFailureDetails(c.ErrOrStderr(), addSum)
		errs = append(errs, printSummary(out, addSum))
	}
	if len(gone) > 0 {
		printFailureDetails(c.ErrOrStderr(), rmSum)
		errs = append(errs, printSummary(out, rmSum))
	}
	var skipped, failed, interrupted int
	for _, r := range slices.Concat(addSum.Results, rmSum.Results) {
		switch {
		case r.Status == ops.StatusSkipped:
			skipped++
		case r.Status == ops.StatusFailed && interruptedErr(r.Err):
			interrupted++
		case r.Status == ops.StatusFailed:
			failed++
		}
	}
	d.reportCounts(countParts(count{addSum.Count(ops.StatusDone), "added"}, count{rmSum.Count(ops.StatusDone), "removed"},
		count{skipped, "skipped"}, count{failed, "failed"}, count{interrupted, "interrupted"}))
	return errors.Join(errs...)
}
