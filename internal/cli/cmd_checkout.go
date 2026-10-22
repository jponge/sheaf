package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/x/ansi"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
	"github.com/spf13/cobra"
)

func (d Deps) stdinIsTerminal(r io.Reader) bool {
	if d.stdinTTY != nil {
		return d.stdinTTY(r)
	}
	return isTerminal(r)
}

// gitInteractive decides interactive git mode (see gitx.Net): only in plain
// CLI mode (tui false), with a single job (prompts of parallel invocations
// would interleave), and with standard input on a terminal (someone can
// answer).
func gitInteractive(tui bool, jobs int, stdinTTY bool) bool { return !tui && jobs == 1 && stdinTTY }

// interactiveGit tells whether the plain CLI variant of c runs git's network
// operations in interactive git mode, as gitInteractive says (the Interactive
// field of the ops options). The TUI variants (cmd_tui.go) never do: git
// never gets the terminal while a bubbletea program owns it.
func (d Deps) interactiveGit(c *cobra.Command) bool {
	return gitInteractive(false, OptionsFrom(c).Jobs, d.stdinIsTerminal(c.InOrStdin()))
}

// paintWith renders s in the style of l with paint (see Deps.painter);
// unstyled when paint is nil.
func paintWith(paint func(tui.Level, string) string, l tui.Level, s string) string {
	if paint == nil {
		return s
	}
	return paint(l, s)
}

// checkoutRenderer is the plain renderer plus the phases of the checkout
// commands that need their own wording.
type checkoutRenderer struct{ plainRenderer }

func (r checkoutRenderer) Report(e ops.Event) {
	if e.Phase == ops.PhaseWaitFastForward {
		fmt.Fprintf(r.w, "waiting for in-progress fast-forward in %s/%s to finish (Ctrl-C again to force)\n", e.Repo, e.Worktree)
		return
	}
	r.plainRenderer.Report(e)
}

// addCheckoutCommands registers the commands acting on every checkout of the
// bundle (status, update, fetch, run).
func addCheckoutCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newStatusCmd(d), newFetchCmd(d), newUpdateCmd(d), newRunCmd(d))
}

// addFilterFlags registers --repo and, when primaryOnly, --primary-only.
func addFilterFlags(c *cobra.Command, f *ops.CheckoutFilter, primaryOnly bool) {
	c.Flags().StringArrayVar(&f.Repos, "repo", nil, "only this repository; a name or a glob pattern such as 'smallrye-*' (repeatable)")
	if primaryOnly {
		c.Flags().BoolVar(&f.PrimaryOnly, "primary-only", false, "only the primary checkouts, not the worktrees")
	}
}

// plural returns "<n> <word>" with an "s" unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func problemsErr(n int) error { return errors.New(plural(n, "problem")) }

// displayName is the name of a checkout in summaries and run headers;
// worktrees outside <bundle>/<repo>/ carry their path.
func displayName(c ops.Checkout) string {
	if c.Outside {
		return c.Name() + " (outside: " + c.Path + ")"
	}
	return c.Name()
}

// displayDir is the DIR cell of the status table.
func displayDir(c ops.Checkout) string {
	if c.Outside {
		return c.Dir + " (outside)"
	}
	return c.Dir
}

const statusLong = `Show the state of every checkout of the bundle: the primary checkout of each
repository and its worktrees, ordered by repository, primary first, then worktrees
by directory name (those outside <bundle>/<repo>/ last). Status does not access
the network: ahead/behind counts are relative to the remote-tracking refs of the
last fetch, unless --fetch is given.

A checkout is "clean" when it has no staged, unstaged, untracked or conflicted
entries. Stashes belong to the repository and are shown on its first row whose
status is known. Worktrees outside <bundle>/<repo>/ are marked "(outside)".
Checkouts that cannot be inspected (e.g. a missing directory) are listed as
problems after the table, and the exit code is then 1.

When standard input and output are terminals and the terminal is
narrower than the table, status prints the compact table of the home
screen, cut to the terminal's width: REPO, DIR, BRANCH, CHANGES and STATE
("clean, ↑1 ↓2", "dirty, no upstream", "clean, PR #7"...). CHANGES holds
the non-zero counts: +staged ~unstaged ?untracked !conflicted $stashes.
--no-tui and --json always print the full output.

--json prints one document instead of the table:

  {
    "version": 1,
    "bundle": "/abs/path",
    "repositories": [
      {
        "name": "repo",
        "stashes": 0,
        "checkouts": [
          {
            "dir": "main", "path": "/abs/path/repo/main", "primary": true,
            "outside": false,        // linked worktree not in <bundle>/<repo>/
            "branch": "main",        // "" when detached
            "tag": "",               // the tag a worktree made by wt add --tag is pinned to
            "tag_moved": false,      // its HEAD is no longer the tag's commit
            "detached": false, "unborn": false,
            "head": "<commit id>",   // "" when unborn
            "upstream": "origin/main", "upstream_gone": false,
            "ahead": 0, "behind": 0, // null without (existing) upstream
            "staged": 0, "unstaged": 0, "untracked": 0, "conflicted": 0,
            "clean": true,
            "error": "..."           // only when the status failed; counts are then 0
          }
        ]
      }
    ],
    "problems": [ { "repo": "repo", "path": "/abs/path", "error": "..." } ]
  }

Repositories without selected checkouts are omitted; "problems" is always an
array. New fields may be added within version 1.`

func newStatusCmd(d Deps) *cobra.Command {
	var opts ops.StatusOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show branch, upstream and changes of checkouts and worktrees",
		Long:  statusLong,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			opts.Interactive = d.interactiveGit(c)
			r, err := ops.CollectStatus(c.Context(), w, opts, checkoutRenderer{plainRenderer{c.ErrOrStderr()}})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				if err := writeStatusJSON(out, w.Root, r); err != nil {
					return err
				}
			} else {
				writeStatus(out, r, d.painter(c, out), d.outputWidth(c, out))
			}
			n := len(r.Problems)
			for _, s := range r.Checkouts {
				if s.Err != nil {
					n++
				}
			}
			if n > 0 {
				return problemsErr(n)
			}
			return nil
		},
	}
	addFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&opts.DirtyOnly, "dirty-only", false, "only checkouts that are not clean")
	cmd.Flags().BoolVar(&opts.Fetch, "fetch", false, "fetch origin (upstream and origin for a clone of your fork) into each repository first")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a JSON document (schema in --help)")
	return cmd
}

// writeStatus writes the status table of r for a terminal of width
// columns (0 = unknown, or not a terminal): the full table (see
// writeStatusTable), or the compact one (see writeStatusCompact) when the
// full one is wider than the terminal.
func writeStatus(out io.Writer, r ops.StatusReport, paint func(tui.Level, string) string, width int) {
	var full bytes.Buffer
	writeStatusTable(&full, ops.StatusReport{Checkouts: r.Checkouts}, paint)
	widest := 0
	for l := range strings.Lines(full.String()) {
		widest = max(widest, ansi.StringWidth(strings.TrimSuffix(l, "\n")))
	}
	if width <= 0 || widest <= width {
		_, _ = full.WriteTo(out)
		writeProblems(out, r.Problems, paint)
		return
	}
	writeStatusCompact(out, r, paint, width)
}

// writeStatusCompact writes the status of r as the home screen's overview
// shows it (see tui.Table), cut to width: REPO (blank when it repeats),
// DIR, BRANCH, CHANGES (statusChanges) and STATE (homeState, colored
// alike), then the problems, cut to width.
func writeStatusCompact(out io.Writer, r ops.StatusReport, paint func(tui.Level, string) string, width int) {
	var rows []tui.Row
	stashShown := ""
	for _, c := range r.Checkouts {
		changes := ""
		if c.Err == nil {
			stashes := 0
			if stashShown != c.Repo {
				stashes, stashShown = c.Status.Stashes, c.Repo
			}
			changes = statusChanges(*c.Status, stashes)
		}
		rows = append(rows, tui.Row{Cells: []string{c.Repo, displayDir(c.Checkout), homeBranch(c), changes, homeState(c)}, Alert: c.Err != nil, Level: homeLevel(c)})
	}
	for _, l := range tui.Table([]string{"REPO", "DIR", "BRANCH", "CHANGES", "STATE"}, rows, width, []int{2, 1}, paint) {
		fmt.Fprintln(out, l)
	}
	var probs bytes.Buffer
	writeProblems(&probs, r.Problems, paint)
	for l := range strings.Lines(probs.String()) {
		fmt.Fprintln(out, ansi.Truncate(strings.TrimSuffix(l, "\n"), width, "…"))
	}
}

// statusChanges returns the CHANGES cell of the compact status table: the
// non-zero counts of s, as "+<staged> ~<unstaged> ?<untracked>
// !<conflicts>", then "$<stashes>"; "" when all are zero.
func statusChanges(s gitx.RepoStatus, stashes int) string {
	var parts []string
	for _, c := range []struct {
		sym string
		n   int
	}{{"+", s.Staged}, {"~", s.Unstaged}, {"?", s.Untracked}, {"!", s.Conflicted}, {"$", stashes}} {
		if c.n > 0 {
			parts = append(parts, c.sym+strconv.Itoa(c.n))
		}
	}
	return strings.Join(parts, " ")
}

func writeStatusTable(out io.Writer, r ops.StatusReport, paint func(tui.Level, string) string) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tDIR\tBRANCH\tUPSTREAM\tAHEAD\tBEHIND\tSTAGED\tUNSTAGED\tUNTRACKED\tCONFLICTS\tSTASHES\tSTATE")
	stashShown := ""
	for _, c := range r.Checkouts {
		branch := c.Branch
		if c.Detached {
			branch = "(detached)"
		}
		if c.Tag != "" {
			branch = tagLabel(c.Tag, c.TagMovedTo)
		}
		stashes := ""
		if c.Err != nil {
			// The state (last column) carries the error; the colored part is
			// last so escapes do not disturb the alignment.
			fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", c.Repo, displayDir(c.Checkout), branch,
				paintWith(paint, tui.LevelFail, "error: "+errorText(c.Err)))
			continue
		}
		s := c.Status
		if c.Tag != "" {
			branch = tagLabel(c.Tag, c.TagMovedTo)
		} else if c.Detached {
			branch = "(detached " + shortHead(s.Head) + ")"
		} else if s.Unborn {
			branch += " (unborn)"
		}
		upstream, ahead, behind := "-", "-", "-"
		switch {
		case s.Upstream != "" && s.UpstreamGone:
			upstream = s.Upstream + " (gone)"
		case s.Upstream != "":
			upstream, ahead, behind = s.Upstream, strconv.Itoa(s.Ahead), strconv.Itoa(s.Behind)
		}
		if stashShown != c.Repo {
			stashes, stashShown = strconv.Itoa(s.Stashes), c.Repo
		}
		// Colored with the level of the overview's STATE (see homeLevel).
		state := "clean"
		if !c.Clean() {
			state = "dirty"
		}
		state = paintWith(paint, homeLevel(c), state)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n", c.Repo, displayDir(c.Checkout), branch, upstream, ahead, behind,
			s.Staged, s.Unstaged, s.Untracked, s.Conflicted, stashes, state)
	}
	_ = tw.Flush()
	writeProblems(out, r.Problems, paint)
}

func writeProblems(out io.Writer, ps []ops.Problem, paint func(tui.Level, string) string) {
	for _, p := range ps {
		fmt.Fprintf(out, "%s %s\n", paintWith(paint, tui.LevelFail, "problem:"), p.Error())
	}
}

type jsonCheckout struct {
	Dir          string `json:"dir"`
	Path         string `json:"path"`
	Primary      bool   `json:"primary"`
	Outside      bool   `json:"outside"`
	Branch       string `json:"branch"`
	Detached     bool   `json:"detached"`
	Tag          string `json:"tag"`
	TagMoved     bool   `json:"tag_moved"`
	Unborn       bool   `json:"unborn"`
	Head         string `json:"head"`
	Upstream     string `json:"upstream"`
	UpstreamGone bool   `json:"upstream_gone"`
	Ahead        *int   `json:"ahead"`
	Behind       *int   `json:"behind"`
	Staged       int    `json:"staged"`
	Unstaged     int    `json:"unstaged"`
	Untracked    int    `json:"untracked"`
	Conflicted   int    `json:"conflicted"`
	Clean        bool   `json:"clean"`
	Error        string `json:"error,omitempty"`
}

type jsonRepo struct {
	Name      string         `json:"name"`
	Stashes   int            `json:"stashes"`
	Checkouts []jsonCheckout `json:"checkouts"`
}

type jsonProblem struct {
	Repo  string `json:"repo"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type jsonStatus struct {
	Version      int           `json:"version"`
	Bundle       string        `json:"bundle"`
	Repositories []jsonRepo    `json:"repositories"`
	Problems     []jsonProblem `json:"problems"`
}

func writeStatusJSON(out io.Writer, root string, r ops.StatusReport) error {
	doc := jsonStatus{Version: 1, Bundle: root, Repositories: []jsonRepo{}, Problems: []jsonProblem{}}
	stashKnown := false
	for _, c := range r.Checkouts {
		if n := len(doc.Repositories); n == 0 || doc.Repositories[n-1].Name != c.Repo {
			doc.Repositories = append(doc.Repositories, jsonRepo{Name: c.Repo, Checkouts: []jsonCheckout{}})
			stashKnown = false
		}
		repo := &doc.Repositories[len(doc.Repositories)-1]
		jc := jsonCheckout{Dir: c.Dir, Path: c.Path, Primary: c.Primary, Outside: c.Outside, Branch: c.Branch, Detached: c.Detached, Tag: c.Tag, TagMoved: c.TagMovedTo != ""}
		if c.Err != nil {
			jc.Error = c.Err.Error()
		} else {
			s := c.Status
			jc.Unborn, jc.Head, jc.Upstream, jc.UpstreamGone = s.Unborn, s.Head, s.Upstream, s.UpstreamGone
			if s.Upstream != "" && !s.UpstreamGone {
				a, b := s.Ahead, s.Behind
				jc.Ahead, jc.Behind = &a, &b
			}
			jc.Staged, jc.Unstaged, jc.Untracked, jc.Conflicted, jc.Clean = s.Staged, s.Unstaged, s.Untracked, s.Conflicted, c.Clean()
			if !stashKnown {
				repo.Stashes, stashKnown = s.Stashes, true
			}
		}
		repo.Checkouts = append(repo.Checkouts, jc)
	}
	for _, p := range r.Problems {
		doc.Problems = append(doc.Problems, jsonProblem{Repo: p.Repo, Path: p.Path, Error: p.Err.Error()})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func newFetchCmd(d Deps) *cobra.Command {
	var opts ops.FetchOptions
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch the remotes of every repository (once per repository)",
		Long: "Run `git fetch --prune origin` once per repository of the bundle (worktrees share\n" +
			"the remote-tracking refs), several repositories in parallel (-j); for a repository\n" +
			"cloned from your fork, upstream (the repository) then origin (your fork). Only these\n" +
			"remotes are fetched, as update only fast-forwards to their branches.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			if d.tuiMode(c) {
				var ferr error
				err := tuiSummary(c, d, "Fetching", func(ctx context.Context, rep ops.Reporter) ops.Summary {
					var s ops.Summary
					s, ferr = ops.FetchRepos(ctx, w, opts, rep)
					return s
				})
				if ferr != nil {
					return ferr
				}
				return err
			}
			opts.Interactive = d.interactiveGit(c)
			s, err := ops.FetchRepos(c.Context(), w, opts, checkoutRenderer{plainRenderer{c.ErrOrStderr()}})
			if err != nil {
				return err
			}
			return printSummary(c.OutOrStdout(), s)
		},
	}
	cmd.Flags().StringArrayVar(&opts.Repos, "repo", nil, "only this repository; a name or a glob pattern such as 'smallrye-*' (repeatable)")
	return cmd
}

func newUpdateCmd(d Deps) *cobra.Command {
	var opts ops.UpdateOptions
	var strict bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Fetch, then fast-forward every clean checkout and worktree",
		Long: "Fetch origin once per repository (upstream and origin for a clone of your fork; if\n" +
			"only your fork cannot be fetched, the checkouts tracking it fail and the others are\n" +
			"updated), then fast-forward each checkout (primary first, then worktrees) to its\n" +
			"upstream with `git merge --ff-only`. Checkouts are skipped, with the reason, when\n" +
			"they have staged or unstaged changes (untracked files do not count), are detached,\n" +
			"have no upstream or a deleted one, or have diverged.\n" +
			"Worktrees pinned at a tag (wt add --tag) are never moved: they are reported as\n" +
			"pinned, which is not a skip; once their HEAD left the tag's commit, they are\n" +
			"skipped as detached.\n" +
			"\n" +
			"Exit code: 1 if any checkout failed or could not be inspected; skips are not failures\n" +
			"unless --strict is given.\n" +
			"\n" +
			"Ctrl-C stops fetching and starts no new fast-forward, but lets a running one finish\n" +
			"(interrupting it would leave a half-updated work tree); a second Ctrl-C kills it.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			if d.tuiMode(c) {
				return tuiUpdate(c, d, w, opts, strict)
			}
			opts.Force = d.force()
			opts.Interactive = d.interactiveGit(c)
			defer d.emergencyNote(noteFastForward)()
			r, err := ops.Update(c.Context(), w, opts, checkoutRenderer{plainRenderer{c.ErrOrStderr()}})
			if err != nil {
				return err
			}
			return printUpdateSummary(c.OutOrStdout(), r, strict)
		},
	}
	addFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&strict, "strict", false, "exit with 1 when a checkout was skipped")
	return cmd
}

// updateCounts returns the counts of r as printUpdateSummary counts them,
// the zero ones left out (see countParts), the skips followed by their
// reasons in brief (see skipReason), each once, in the order of the
// checkouts: "1 fast-forwarded", "2 skipped (diverged, dirty)".
func updateCounts(r ops.UpdateReport) []string {
	var ff, upToDate, pinned, skipped, failed, interrupted int
	var reasons []string
	for _, u := range r.Checkouts {
		switch {
		case u.Outcome == gitx.PullFastForwarded:
			ff++
		case u.Outcome == gitx.PullUpToDate:
			upToDate++
		case u.Pinned:
			pinned++
		case u.Skipped():
			skipped++
			if why := skipReason(u); !slices.Contains(reasons, why) {
				reasons = append(reasons, why)
			}
		case interruptedErr(u.Err):
			interrupted++
		default:
			failed++
		}
	}
	skips := "skipped"
	if len(reasons) > 0 {
		skips += " (" + strings.Join(reasons, ", ") + ")"
	}
	return countParts(count{ff, "fast-forwarded"}, count{upToDate, "up to date"}, count{pinned, "pinned"},
		count{skipped, skips}, count{failed, "failed"}, count{interrupted, "interrupted"})
}

// skipReason returns why update skipped u, in brief, in the words of the
// overview's STATE where they exist ("dirty", "upstream gone", "PR #7").
func skipReason(u ops.CheckoutUpdate) string {
	switch {
	case len(u.UntrackedInTheWay) > 0:
		return "untracked files"
	case u.Outcome == gitx.PullSkippedDirty:
		return "dirty"
	case u.Outcome == gitx.PullSkippedDetached:
		return "detached"
	case u.Outcome == gitx.PullSkippedDiverged:
		return "diverged"
	case strings.HasPrefix(u.Reason, "upstream "): // see ops.classifyUpdate
		return "upstream gone"
	}
	var n int // a pull request branch, see ops.pullRequestReason
	if _, err := fmt.Sscanf(u.Reason, "pull request branch (refs/pull/%d/head", &n); err == nil {
		return fmt.Sprintf("PR #%d", n)
	}
	return "no upstream"
}

func printUpdateSummary(out io.Writer, r ops.UpdateReport, strict bool) error {
	// The table goes through a buffer: its lines without detail lose the
	// padding of the empty last cell.
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	var ff, upToDate, pinned, skipped, failed, interrupted int
	var details []string
	for _, u := range r.Checkouts {
		var label string
		detail := u.Reason
		switch {
		case u.Outcome == gitx.PullFastForwarded:
			label = "fast-forwarded"
			ff++
		case u.Outcome == gitx.PullUpToDate:
			label, detail = "up to date", ""
			upToDate++
		case u.Pinned:
			label = "pinned"
			pinned++
		case u.Skipped():
			label = "skipped"
			skipped++
		case interruptedErr(u.Err):
			label, detail = "interrupted", interruptedNote(u.Reason)
			interrupted++
		default:
			label = "failed"
			failed++
			if u.Err != nil {
				if detail != "" {
					detail += ": "
				}
				detail += errorText(u.Err)
				if msg := u.Err.Error(); msg != firstLine(msg) {
					details = append(details, displayName(u.Checkout)+":\n    "+strings.ReplaceAll(msg, "\n", "\n    "))
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", label, displayName(u.Checkout), detail)
	}
	_ = tw.Flush()
	for line := range strings.Lines(table.String()) {
		fmt.Fprintln(out, strings.TrimRight(line, " \n"))
	}
	for _, d := range details {
		fmt.Fprintln(out, d)
	}
	writeProblems(out, r.Problems, nil)
	// As summaryLine, with the problems (plural forms).
	parts := countParts(count{ff, "fast-forwarded"}, count{upToDate, "up to date"}, count{pinned, "pinned"},
		count{skipped, "skipped"}, count{failed, "failed"}, count{interrupted, "interrupted"})
	if n := len(r.Problems); n > 0 {
		parts = append(parts, plural(n, "problem"))
	}
	if len(parts) == 0 {
		parts = []string{"0 fast-forwarded"}
	}
	fmt.Fprintln(out, strings.Join(parts, ", "))
	switch {
	case failed > 0 || len(r.Problems) > 0:
		what := countParts(count{failed, "failed"})
		if n := len(r.Problems); n > 0 {
			what = append(what, plural(n, "problem"))
		}
		return errors.New("update: " + strings.Join(what, ", "))
	case interrupted > 0:
		return fmt.Errorf("update: %d interrupted", interrupted)
	case strict && skipped > 0:
		return fmt.Errorf("update: %d skipped (--strict)", skipped)
	}
	return nil
}

func newRunCmd(d Deps) *cobra.Command {
	var opts ops.RunOptions
	cmd := &cobra.Command{
		Use:   "run [flags] [--] <command> [args...]",
		Short: "Run a command in every checkout and worktree",
		Long: "Run a command in the directory of each checkout (primary checkouts and worktrees),\n" +
			"several at a time (-j); the combined output of each is printed as a block headed\n" +
			"\"==> repo/dir <==\", in checkout order. With -j 1 the output is streamed live.\n" +
			"The command is executed directly, without a shell, unless --shell is given: the\n" +
			"first argument is then a script for `sh -c` and the others its $1, $2, ...\n" +
			"Flags after the command name belong to the command; \"--\" is optional.\n" +
			"\n" +
			"The command gets no standard input and no controlling terminal; SHEAF_REPO,\n" +
			"SHEAF_DIR and SHEAF_BRANCH describe the checkout. Exit code: 1 if any command\n" +
			"failed (the summary lists them). Ctrl-C sends SIGTERM to the running commands and\n" +
			"their children, then SIGKILL after 5 seconds.",
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("no command given: sheaf run [flags] -- <command> [args...]")
			}
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			opts.Args = args
			opts.Jobs = OptionsFrom(c).Jobs
			opts.Stream = opts.Jobs == 1
			sink := &blockSink{w: out}
			opts.Sink = sink
			r, err := ops.RunCommand(c.Context(), w, opts)
			if err != nil {
				return err
			}
			return printRunSummary(out, r)
		},
	}
	cmd.Flags().SetInterspersed(false)
	addFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&opts.DirtyOnly, "dirty-only", false, "only checkouts that are not clean")
	cmd.Flags().BoolVar(&opts.Shell, "shell", false, "run the first argument as a script with sh -c")
	return cmd
}

// blockSink prints each checkout's output as a block: a header, the output
// (a missing final newline is added), the failure if any, and a blank line.
type blockSink struct {
	w    io.Writer
	last byte // last byte written by the command, 0 if none
}

func (s *blockSink) Begin(c ops.Checkout) io.Writer {
	fmt.Fprintf(s.w, "==> %s <==\n", displayName(c))
	s.last = 0
	return s
}

// Write is the output of the current block.
func (s *blockSink) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.last = p[len(p)-1]
	}
	return s.w.Write(p)
}

func (s *blockSink) End(r ops.RunResult) {
	if s.last != 0 && s.last != '\n' {
		fmt.Fprintln(s.w)
	}
	if r.Failed() {
		fmt.Fprintf(s.w, "[%s]\n", runFailure(r))
	}
	if r.Note != "" {
		fmt.Fprintf(s.w, "[note: %s]\n", r.Note)
	}
	fmt.Fprintln(s.w)
}

// runFailure says why the command of r failed: an interruption (see
// interruptedErr) without its error, else the error or the exit status.
func runFailure(r ops.RunResult) string {
	switch {
	case !r.Started && interruptedErr(r.Err):
		return "not started (interrupted)"
	case interruptedErr(r.Err):
		return "interrupted"
	case r.Err != nil:
		return errorText(r.Err)
	default:
		return fmt.Sprintf("exit status %d", r.ExitCode)
	}
}

// printRunSummary prints the counts and the failed commands of r, those
// stopped or not started by an interruption (see interruptedErr) apart.
func printRunSummary(out io.Writer, r ops.RunReport) error {
	var failed, interrupted []ops.RunResult
	for _, res := range r.Results {
		switch {
		case interruptedErr(res.Err):
			interrupted = append(interrupted, res)
		case res.Failed():
			failed = append(failed, res)
		}
	}
	fmt.Fprintln(out, summaryLine(count{len(r.Results) - len(failed) - len(interrupted), "succeeded"}, count{len(failed), "failed"}, count{len(interrupted), "interrupted"}))
	for _, res := range failed {
		fmt.Fprintf(out, "failed: %s (%s)\n", displayName(res.Checkout), runFailure(res))
	}
	for _, res := range interrupted {
		fmt.Fprintf(out, "interrupted: %s", displayName(res.Checkout))
		if !res.Started {
			fmt.Fprint(out, " (not started)")
		}
		fmt.Fprintln(out)
	}
	writeProblems(out, r.Problems, nil)
	switch {
	case len(failed) > 0:
		return fmt.Errorf("%d of %d commands failed", len(failed), len(r.Results))
	case len(interrupted) > 0:
		return fmt.Errorf("%d of %d commands interrupted", len(interrupted), len(r.Results))
	case len(r.Problems) > 0:
		return problemsErr(len(r.Problems))
	}
	return nil
}
