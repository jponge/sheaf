package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// TUI variants of the commands that need it. Each one ends like its CLI
// counterpart: the same summary on stdout and the same returned error (so
// the same exit status); the plain progress lines are replaced by the
// progress view.

// tuiAdd: `add --org <org>` without --all/--repo offers a menu of the
// organization's repositories, then clones with a progress view.
func tuiAdd(c *cobra.Command, w *ops.Workspace, h hosting.Hosting, req ops.AddRequest, jobs int) error {
	s := newTUISession(c)
	out := c.OutOrStdout()
	if req.Org != "" && !req.All && len(req.Repos) == 0 {
		fmt.Fprintf(c.ErrOrStderr(), "listing repositories of %s...\n", req.Org)
		_, err := ops.Add(s.ctx, w, h, req, jobs, ops.Discard)
		var sel *ops.SelectionRequiredError
		if !errors.As(err, &sel) || len(sel.Available) == 0 {
			return err
		}
		items := orgRepoItems(sel.Available, w.Bundle())
		keys, err := s.multiSelect("Add repositories of "+sel.Org, items)
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
	s.progress("Adding repositories", progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		sum, addErr = ops.Add(ctx, w, h, req, jobs, rep)
	})
	if addErr != nil {
		return addErr
	}
	printFailureDetails(c.ErrOrStderr(), sum)
	return printSummary(out, sum)
}

// tuiSummary runs an operation returning a Summary in a progress view and
// prints the summary.
func tuiSummary(c *cobra.Command, title string, run func(ctx context.Context, rep ops.Reporter) ops.Summary) error {
	s := newTUISession(c)
	var sum ops.Summary
	s.progress(title, progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) { sum = run(ctx, rep) })
	printFailureDetails(c.ErrOrStderr(), sum)
	return printSummary(c.OutOrStdout(), sum)
}

// tuiUpdate runs update in a progress view and prints its summary.
func tuiUpdate(c *cobra.Command, w *ops.Workspace, opts ops.UpdateOptions, strict bool) error {
	s := newTUISession(c)
	opts.Force = s.ctl.force
	var r ops.UpdateReport
	var uerr error
	s.progress("Updating", progressOpts{Aside: ops.PhaseFetch}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		r, uerr = ops.Update(ctx, w, opts, rep)
	})
	if uerr != nil {
		return uerr
	}
	return printUpdateSummary(c.OutOrStdout(), r, strict)
}

// tuiInitAdopt offers the checkouts found by init (all preselected) and
// adopts the chosen ones. Declining leaves the new bundle empty and prints
// how to adopt later (init itself succeeded).
func tuiInitAdopt(c *cobra.Command, root string, cands []ops.Candidate, adoptAll bool, jobs int) error {
	s := newTUISession(c)
	out := c.OutOrStdout()
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
	s.progress("Adopting checkouts", progressOpts{}, func(ctx context.Context, rep ops.Reporter, _ askFunc) {
		sum = ops.AdoptAll(ctx, w, paths, ops.AdoptOptions{}, jobs, rep)
	})
	printFailureDetails(c.ErrOrStderr(), sum)
	return printSummary(out, sum)
}

// tuiRemove removes a repository with a progress view; without --yes the
// removal plan is confirmed in a dialog (default no).
func tuiRemove(c *cobra.Command, w *ops.Workspace, name string, opts ops.RemoveOptions, yes bool) error {
	s := newTUISession(c)
	var msg string
	var rerr error
	s.progress("Removing "+name, progressOpts{Single: name}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
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
		emit(ops.Event{Kind: ops.KindSkipped, Message: "cancelled"})
	case err != nil:
		emit(ops.Event{Kind: ops.KindFailed, Err: err})
	default:
		emit(ops.Event{Kind: ops.KindDone, Message: msg})
	}
}

// tuiWorktreeRemove removes a worktree with a progress view; without --yes
// the plan (risks, branch deletion, commits lost with -D) is confirmed in
// a dialog (default no).
func tuiWorktreeRemove(c *cobra.Command, w *ops.Workspace, repo string, target ops.WorktreeTarget, opts ops.WorktreeRemoveOptions, yes bool) error {
	s := newTUISession(c)
	var msg string
	var rerr error
	s.progress("Removing worktree "+target.Name+" of "+repo, progressOpts{Single: repo + "/" + target.Name}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
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
	fmt.Fprintln(c.OutOrStdout(), msg)
	return nil
}

// tuiPrune plans and, after confirmation (or with --yes), applies a prune
// in one progress view.
func tuiPrune(c *cobra.Command, w *ops.Workspace, opts ops.PruneOptions, yes bool) error {
	s := newTUISession(c)
	var plan ops.PrunePlan
	var results []ops.PruneResult
	var perr error
	wasDeclined := false
	s.progress("Pruning", progressOpts{Aside: ops.PhaseFetch}, func(ctx context.Context, rep ops.Reporter, ask askFunc) {
		plan, perr = ops.PlanPrune(ctx, w, opts, rep)
		if perr != nil || len(plan.Candidates) == 0 {
			return
		}
		if !yes {
			var lines []string
			for _, cand := range plan.Candidates {
				lines = append(lines, pruneCandidateLines(cand)...)
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
	printPrune(c.OutOrStdout(), plan, results, false)
	return pruneErr(s.ctx, plan, results)
}

// tuiSync offers the proposals of sync in a menu (new and gone ones
// selectable, nothing preselected), then which unselected new repositories
// to exclude from future syncs, confirms removals, and applies the choices
// like --exclude, --add-new and --remove-gone --yes (never --force nor
// --allow-mass-removal: such refusals are displayed as failures).
func tuiSync(c *cobra.Command, d Deps, w *ops.Workspace, rep ops.SyncReport, noFork bool) error {
	s := newTUISession(c)
	out := c.OutOrStdout()
	if len(rep.Proposals) == 0 {
		syWriteProposals(out, rep, false)
		return rep.Err()
	}
	keys, err := s.multiSelect("Sync: choose the changes to apply", proposalItems(rep.Proposals))
	if err != nil {
		return err
	}
	var add, gone, unselected []ops.Proposal
	for _, p := range rep.Proposals {
		chosen := slices.Contains(keys, p.Key)
		switch {
		case p.Kind == ops.ProposalNew && chosen:
			add = append(add, p)
		case p.Kind == ops.ProposalNew:
			unselected = append(unselected, p)
		case p.Kind == ops.ProposalGone && chosen:
			gone = append(gone, p)
		}
	}
	var excluded []ops.Proposal
	if len(unselected) > 0 {
		ekeys, err := s.multiSelect("Exclude the unselected new repositories from future syncs?", excludeItems(unselected))
		if err != nil {
			return err
		}
		for _, p := range unselected {
			if slices.Contains(ekeys, p.Key) {
				excluded = append(excluded, p)
			}
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
	if len(add) == 0 && len(gone) == 0 && len(excluded) == 0 {
		fmt.Fprintln(out, "nothing selected; nothing changed")
		return rep.Err()
	}
	errs := []error{rep.Err()}
	if len(excluded) > 0 {
		if err := ops.SyncExclude(s.ctx, w, excluded); err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, p := range excluded {
			fmt.Fprintf(out, "excluded %s (recorded in sheaf.yaml, no longer proposed)\n", p.Slug)
		}
	}
	if len(add) == 0 && len(gone) == 0 {
		return errors.Join(errs...)
	}
	wd, err := d.getwd()
	if err != nil {
		return err
	}
	jobs := OptionsFrom(c).Jobs
	var addSum, rmSum ops.Summary
	s.progress("Syncing", progressOpts{}, func(ctx context.Context, r ops.Reporter, _ askFunc) {
		if len(add) > 0 {
			addSum = ops.SyncAddNewWithOptions(ctx, w, add, ops.CloneOptions{Hosting: d.hosting(), NoFork: noFork}, jobs, r)
		}
		if len(gone) > 0 {
			opts := ops.SyncRemoveOptions{Remove: ops.RemoveOptions{WorkingDir: wd, Confirm: func(ops.RemovePlan) bool { return true }}}
			rmSum = ops.SyncRemoveGone(ctx, w, gone, opts, jobs, r)
		}
	})
	if len(add) > 0 {
		printFailureDetails(c.ErrOrStderr(), addSum)
		errs = append(errs, printSummary(out, addSum))
	}
	if len(gone) > 0 {
		printFailureDetails(c.ErrOrStderr(), rmSum)
		errs = append(errs, printSummary(out, rmSum))
	}
	return errors.Join(errs...)
}
