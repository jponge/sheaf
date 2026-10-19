package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// ghProbeTimeout bounds `gh auth status`.
const ghProbeTimeout = 20 * time.Second

// defaultGHProbe is the production Deps.GHProbe: gh must be on PATH and
// `gh auth status --hostname github.com` must succeed (gh exits non-zero
// when not logged in to that host).
func defaultGHProbe(ctx context.Context) error {
	path, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("%w: %w", hosting.ErrGHNotFound, err)
	}
	return ghAuthStatus(ctx, path, ghProbeTimeout)
}

// ghAuthStatus runs `<gh> auth status --hostname github.com`, bounded by
// timeout; a gh that does not answer in time is reported as such, not as an
// authentication failure.
func ghAuthStatus(ctx context.Context, gh string, timeout time.Duration) error {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, gh, "auth", "status", "--hostname", "github.com")
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_PAGER=cat")
	// Bound the wait for grandchildren that keep the output pipe open.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if tctx.Err() != nil {
		return fmt.Errorf("%w within %v (gh auth status --hostname github.com)", ops.ErrGHProbeTimeout, timeout)
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("gh auth status: %w: %s", err, msg)
	}
	return fmt.Errorf("gh auth status: %w", err)
}

// addMaintenanceCommands registers prune and doctor.
func addMaintenanceCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newPruneCmd(d), newDoctorCmd(d))
}

// pruneJSON is the --json document of prune.
type pruneJSON struct {
	Version    int                  `json:"version"`
	DryRun     bool                 `json:"dry_run"`
	Candidates []ops.PruneCandidate `json:"candidates"`
	Skipped    []ops.PruneSkip      `json:"skipped"`
	Results    []pruneResultJSON    `json:"results"`
	Errors     []repoErrorJSON      `json:"errors"`
}

type pruneResultJSON struct {
	ops.PruneCandidate
	Outcome string `json:"outcome"` // removed, kept, failed
	Message string `json:"message"`
}

func pruneOutcome(o ops.PruneOutcome) string {
	switch o {
	case ops.PruneRemoved:
		return "removed"
	case ops.PruneKept:
		return "kept"
	}
	return "failed"
}

func candidateLabel(c ops.PruneCandidate) string {
	if c.Worktree {
		return fmt.Sprintf("worktree %s/%s [%s] at %s", c.Repo, c.Dir, c.Branch, c.Path)
	}
	return fmt.Sprintf("branch %s/%s", c.Repo, c.Branch)
}

func newPruneCmd(d Deps) *cobra.Command {
	var opts ops.PruneOptions
	var yes, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove worktrees whose branch is gone or merged",
		Long: "Fetch origin (unless --offline), then find the linked worktrees whose branch is\n" +
			"gone (its upstream was deleted on the remote) or merged into origin/<default branch>\n" +
			"with a merge commit. For a repository cloned from your fork, upstream and origin are\n" +
			"fetched and merged means merged into upstream/<default branch>; when your fork is gone\n" +
			"(or with --offline) its remote-tracking refs are not trusted: branches tracking a gone\n" +
			"fork count as gone, and a branch whose commits are only on its refs is kept. Squash-\n" +
			"and rebase-merged branches are only found once their remote branch is deleted (gone);\n" +
			"a branch without commits of its own is never considered merged. The primary checkout,\n" +
			"worktrees outside the repository directory, locked ones and pull request branches of\n" +
			"`sheaf wt pr` are never pruned.\n\n" +
			"Without --yes nothing is changed (dry run): the candidates are listed. With --yes\n" +
			"each worktree is removed like `sheaf wt rm` (uncommitted changes or untracked files\n" +
			"keep it; there is no --force). --delete-branches also deletes the branches of the\n" +
			"removed worktrees when merged or contained in another ref (like -d, never -D).\n" +
			"--include-branches also deletes, under the same condition, gone or merged local\n" +
			"branches that have no worktree. sheaf never prompts in CLI mode. In TUI mode (without\n" +
			"--dry-run or --json) the candidates are listed in a dialog asking for confirmation,\n" +
			"unless --yes is given.\n\n" +
			"--json prints {\"version\": 1, \"dry_run\": bool, \"candidates\": [...], \"skipped\":\n" +
			"[...], \"results\": [{..., \"outcome\": \"removed\"|\"kept\"|\"failed\", \"message\"}],\n" +
			"\"errors\": [{\"repo\", \"error\"}]}. --json never opens the TUI: with --yes it removes\n" +
			"the candidates in plain mode (no dialog) and reports them in \"results\".",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if yes && dryRun {
				return errors.New("--dry-run and --yes are mutually exclusive")
			}
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			if opts.WorkingDir, err = d.getwd(); err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			if !dryRun && !asJSON && d.tuiMode(c) {
				return tuiPrune(c, d, w, opts, yes)
			}
			ctx := c.Context()
			opts.Interactive = d.interactiveGit(c)
			rep := plainRenderer{c.ErrOrStderr()}
			plan, err := ops.PlanPrune(ctx, w, opts, rep)
			if err != nil {
				return err
			}
			var results []ops.PruneResult
			if yes {
				results = ops.ApplyPrune(ctx, w, plan, opts, rep)
			}
			out := c.OutOrStdout()
			if asJSON {
				err = printPruneJSON(out, plan, results, !yes)
			} else {
				printPrune(out, plan, results, !yes)
			}
			if err != nil {
				return err
			}
			return pruneErr(ctx, plan, results)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&opts.Repos, "repo", nil, "only this repository; a name or a glob pattern such as 'smallrye-*' (repeatable)")
	f.BoolVarP(&yes, "yes", "y", false, "remove the candidates (without it, prune only lists them)")
	f.BoolVar(&dryRun, "dry-run", false, "only list the candidates (the default without --yes)")
	f.BoolVar(&opts.Offline, "offline", false, "do not fetch first (gone branches as of the last fetch)")
	f.BoolVar(&opts.DeleteBranches, "delete-branches", false, "also delete the branches of removed worktrees when merged (never unmerged ones)")
	f.BoolVar(&opts.IncludeBranches, "include-branches", false, "also delete gone or merged local branches without worktree, when merged")
	f.BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// pruneErr is the error of prune: the interruption, or the problems of the
// plan and the failed removals.
func pruneErr(ctx context.Context, plan ops.PrunePlan, results []ops.PruneResult) error {
	var problems []string
	for _, p := range plan.Problems {
		problems = append(problems, p.Error())
	}
	for _, r := range results {
		if r.Outcome == ops.PruneFailed {
			problems = append(problems, candidateLabel(r.PruneCandidate)+": "+r.Message)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

func printPrune(out io.Writer, plan ops.PrunePlan, results []ops.PruneResult, dryRun bool) {
	for _, s := range plan.Skipped {
		if s.Tag != "" {
			fmt.Fprintf(out, "skipped %s (tag %s): %s\n", s.Repo, s.Tag, s.Reason)
			continue
		}
		fmt.Fprintf(out, "skipped %s/%s: %s\n", s.Repo, s.Branch, s.Reason)
	}
	if len(plan.Candidates) == 0 {
		fmt.Fprintln(out, "nothing to prune")
		return
	}
	if dryRun {
		for _, c := range plan.Candidates {
			for _, l := range pruneCandidateLines(c, false, false) {
				fmt.Fprintln(out, l)
			}
		}
		fmt.Fprintf(out, "dry run: pass --yes to prune %d candidate(s)\n", len(plan.Candidates))
		return
	}
	n := map[ops.PruneOutcome]int{}
	for _, r := range results {
		n[r.Outcome]++
		switch r.Outcome {
		case ops.PruneRemoved:
			fmt.Fprintf(out, "%s/%s: %s\n", r.Repo, r.Branch, r.Message)
		default:
			fmt.Fprintf(out, "%s/%s: %s: %s\n", r.Repo, r.Branch, pruneOutcome(r.Outcome), r.Message)
		}
		printNotes(out, r.Notes)
	}
	fmt.Fprintf(out, "%d removed, %d kept, %d failed\n", n[ops.PruneRemoved], n[ops.PruneKept], n[ops.PruneFailed])
}

// pruneCandidateLines describes what pruning c would do (a dry run), or
// will do (a confirmation), with its notes. A branch without worktree, or
// the branch of a worktree with deleteBranches (--delete-branches), is
// only deleted when merged (git branch -d rules, see ops.ApplyPrune): the
// confirmation states that condition, unless the branch is known to be
// merged, and says when the branch of a worktree is kept.
func pruneCandidateLines(c ops.PruneCandidate, confirm, deleteBranches bool) []string {
	var what string
	switch {
	case c.Worktree && confirm:
		what = "will remove worktree " + c.Repo + "/" + c.Dir + " [" + c.Branch + "] at " + c.Path
	case c.Worktree:
		what = "would remove worktree " + c.Repo + "/" + c.Dir + " [" + c.Branch + "] at " + c.Path
	case !confirm:
		what = "would delete branch " + c.Repo + "/" + c.Branch + " if merged"
	case c.Merged:
		what = "will delete branch " + c.Repo + "/" + c.Branch
	default:
		what = "will delete branch " + c.Repo + "/" + c.Branch + " only if merged (git branch -d rules), otherwise keep it"
	}
	lines := []string{what + ": " + c.Reason}
	switch {
	case !c.Worktree || !confirm:
	case !deleteBranches:
		lines = append(lines, "branch "+c.Branch+" is kept")
	case c.Merged:
		lines = append(lines, "will delete branch "+c.Branch)
	default:
		lines = append(lines, "will delete branch "+c.Branch+" if merged (git branch -d rules), otherwise keep it")
	}
	for _, n := range c.Notes {
		lines = append(lines, "  note: "+n)
	}
	return lines
}

func printNotes(out io.Writer, notes []string) {
	for _, n := range notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
}

func printPruneJSON(out io.Writer, plan ops.PrunePlan, results []ops.PruneResult, dryRun bool) error {
	doc := pruneJSON{Version: 1, DryRun: dryRun, Candidates: plan.Candidates, Skipped: plan.Skipped,
		Results: []pruneResultJSON{}, Errors: []repoErrorJSON{}}
	if doc.Candidates == nil {
		doc.Candidates = []ops.PruneCandidate{}
	}
	if doc.Skipped == nil {
		doc.Skipped = []ops.PruneSkip{}
	}
	for _, r := range results {
		doc.Results = append(doc.Results, pruneResultJSON{PruneCandidate: r.PruneCandidate, Outcome: pruneOutcome(r.Outcome), Message: r.Message})
	}
	for _, p := range plan.Problems {
		doc.Errors = append(doc.Errors, repoErrorJSON{Repo: p.Repo, Error: p.Err.Error()})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// doctorJSON is the --json document of doctor.
type doctorJSON struct {
	Version int         `json:"version"`
	Failed  bool        `json:"failed"`
	Checks  []ops.Check `json:"checks"`
}

// errDoctorFailed makes doctor exit with status 1 after printing its report.
var errDoctorFailed = errors.New("doctor found problems (FAIL)")

func newDoctorCmd(d Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment and the bundle",
		Long: "Check that git is recent enough (" + gitx.MinGitVersion.String() + " or later), that gh is installed and\n" +
			"authenticated for github.com, that sheaf.yaml is valid, and for each repository: its\n" +
			"directory and primary checkout (<repo>/<default branch>/, a work tree of its own),\n" +
			"origin's URL against sheaf.yaml (for a clone of your fork: origin, upstream, and the\n" +
			"default branch tracking upstream), the gh default repository of GitHub repositories\n" +
			"(missing or several: WARN; another repository: INFO), missing worktrees or worktrees\n" +
			"outside the repository directory, and lock files left in its git directory. Directories of the\n" +
			"bundle that are not repositories of it are listed (with `sheaf adopt` when they hold\n" +
			"a checkout). Nothing is fetched or changed. One line per check: OK, INFO, WARN or\n" +
			"FAIL, with hints; the exit status is 1 when a check FAILs. --json prints\n" +
			"{\"version\": 1, \"failed\": bool, \"checks\": [{level, check, repo, message, hint}]}.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			wd, err := d.getwd()
			if err != nil {
				return err
			}
			probe := d.GHProbe
			if probe == nil {
				probe = defaultGHProbe
			}
			opts := ops.DoctorOptions{GH: probe}
			opts.Root, opts.RootErr = ops.FindBundle(OptionsFrom(c).Bundle, wd)
			checks := ops.Doctor(c.Context(), opts)
			failed := ops.DoctorFailed(checks)
			out := c.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(doctorJSON{Version: 1, Failed: failed, Checks: checks}); err != nil {
					return err
				}
			} else {
				out := d.leveled(c, out)
				for _, ch := range checks {
					msg := ch.Message
					if ch.Repo != "" {
						msg = ch.Repo + ": " + msg
					}
					fmt.Fprintf(out, "%-5s %s\n", strings.ToUpper(ch.Level.String()), msg)
					if ch.Hint != "" {
						fmt.Fprintf(out, "      hint: %s\n", ch.Hint)
					}
				}
			}
			if c.Context().Err() != nil {
				return c.Context().Err()
			}
			if failed {
				return errDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
