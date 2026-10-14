package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// Terminal detection seams (tests replace them).
var (
	stdinIsTerminal  = func(r io.Reader) bool { return isTerminal(r) }
	stdoutIsTerminal = func(w io.Writer) bool { return isTerminal(w) }
)

// gitInteractive decides interactive git mode (see gitx.WithInteractive):
// only in plain CLI mode (tui false), with a single job (prompts of parallel
// invocations would interleave), and with standard input on a terminal
// (someone can answer).
func gitInteractive(tui bool, jobs int, stdinTTY bool) bool { return !tui && jobs == 1 && stdinTTY }

// checkoutContext returns the command's context, marked for interactive git
// mode when gitInteractive says so. The checkout commands always render in
// plain mode for now; the TUI progress view (step 8) must pass its own
// decision instead of false.
func checkoutContext(c *cobra.Command) context.Context {
	ctx := c.Context()
	if gitInteractive(false, OptionsFrom(c).Jobs, stdinIsTerminal(c.InOrStdin())) {
		ctx = gitx.WithInteractive(ctx)
	}
	return ctx
}

// useColor reports whether output may use ANSI colors: only on a terminal,
// and not when NO_COLOR is set to a non-empty value (https://no-color.org).
func useColor(tty bool, noColor string) bool { return tty && noColor == "" }

func coColorFor(w io.Writer) bool { return useColor(stdoutIsTerminal(w), os.Getenv("NO_COLOR")) }

const (
	coAnsiReset  = "\x1b[0m"
	coAnsiRed    = "\x1b[31m"
	coAnsiGreen  = "\x1b[32m"
	coAnsiYellow = "\x1b[33m"
)

func coPaint(on bool, color, s string) string {
	if !on {
		return s
	}
	return color + s + coAnsiReset
}

// coCheckoutRenderer is the plain renderer plus the phases of the checkout
// commands that need their own wording.
type coCheckoutRenderer struct{ plainRenderer }

func (r coCheckoutRenderer) Report(e ops.Event) {
	if e.Phase == ops.PhaseWaitFastForward {
		fmt.Fprintf(r.w, "waiting for in-progress fast-forward in %s/%s to finish (Ctrl-C again to force)\n", e.Repo, e.Worktree)
		return
	}
	if e.Kind == ops.KindFailed && errors.Is(e.Err, context.Canceled) && strings.HasPrefix(e.Message, "not started") {
		who := e.Repo
		if e.Worktree != "" {
			who += "/" + e.Worktree
		}
		fmt.Fprintf(r.w, "%s: %s not started (interrupted)\n", who, e.Phase)
		return
	}
	r.plainRenderer.Report(e)
}

// addCheckoutCommands registers the commands acting on every checkout of the
// bundle (status, update, fetch, run).
func addCheckoutCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newStatusCmd(d), newFetchCmd(d), newUpdateCmd(d), newRunCmd(d))
}

// coAddFilterFlags registers --repo and, when primaryOnly, --primary-only.
func coAddFilterFlags(c *cobra.Command, f *ops.CheckoutFilter, primaryOnly bool) {
	c.Flags().StringArrayVar(&f.Repos, "repo", nil, "only this repository; a name or a glob pattern such as 'smallrye-*' (repeatable)")
	if primaryOnly {
		c.Flags().BoolVar(&f.PrimaryOnly, "primary-only", false, "only the primary checkouts, not the worktrees")
	}
}

// coPlural returns "<n> <word>" with an "s" unless n is 1.
func coPlural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func coProblemsErr(n int) error { return errors.New(coPlural(n, "problem")) }

// coDisplayName is the name of a checkout in summaries and run headers;
// worktrees outside <bundle>/<repo>/ carry their path.
func coDisplayName(c ops.Checkout) string {
	if c.Outside {
		return c.Name() + " (outside: " + c.Path + ")"
	}
	return c.Name()
}

// coDisplayDir is the DIR cell of the status table.
func coDisplayDir(c ops.Checkout) string {
	if c.Outside {
		return c.Dir + " (outside)"
	}
	return c.Dir
}

const coStatusLong = `Show the state of every checkout of the bundle: the primary checkout of each
repository and its worktrees, ordered by repository, primary first, then worktrees
by directory name. Status does not access the network: ahead/behind counts are
relative to the remote-tracking refs of the last fetch, unless --fetch is given.

A checkout is "clean" when it has no staged, unstaged, untracked or conflicted
entries. Stashes belong to the repository and are shown on its first row whose
status is known. Worktrees outside <bundle>/<repo>/ are marked "(outside)".
Checkouts that cannot be inspected (e.g. a missing directory) are listed as
problems after the table, and the exit code is then 1.

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
		Short: "Show branch, upstream and changes of every checkout and worktree",
		Long:  coStatusLong,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			r, err := ops.CollectStatus(checkoutContext(c), w, opts, coCheckoutRenderer{plainRenderer{c.ErrOrStderr()}})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				if err := coWriteStatusJSON(out, w.Root, r); err != nil {
					return err
				}
			} else {
				coWriteStatusTable(out, r, coColorFor(out))
			}
			n := len(r.Problems)
			for _, s := range r.Checkouts {
				if s.Err != nil {
					n++
				}
			}
			if n > 0 {
				return coProblemsErr(n)
			}
			return nil
		},
	}
	coAddFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&opts.DirtyOnly, "dirty-only", false, "only checkouts that are not clean")
	cmd.Flags().BoolVar(&opts.Fetch, "fetch", false, "fetch origin into each repository first")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a JSON document (schema in --help)")
	return cmd
}

func coShortHead(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

func coWriteStatusTable(out io.Writer, r ops.StatusReport, color bool) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tDIR\tBRANCH\tUPSTREAM\tAHEAD\tBEHIND\tSTAGED\tUNSTAGED\tUNTRACKED\tCONFLICTS\tSTASHES\tSTATE")
	stashShown := ""
	for _, c := range r.Checkouts {
		branch := c.Branch
		if c.Detached {
			branch = "(detached)"
		}
		stashes := ""
		if c.Err != nil {
			// The state (last column) carries the error; the colored part is
			// last so escapes do not disturb the alignment.
			fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", c.Repo, coDisplayDir(c.Checkout), branch,
				coPaint(color, coAnsiRed, "error: "+firstLine(c.Err.Error())))
			continue
		}
		s := c.Status
		if c.Detached {
			branch = "(detached " + coShortHead(s.Head) + ")"
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
		state := coPaint(color, coAnsiGreen, "clean")
		if !c.Clean() {
			state = coPaint(color, coAnsiYellow, "dirty")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n", c.Repo, coDisplayDir(c.Checkout), branch, upstream, ahead, behind,
			s.Staged, s.Unstaged, s.Untracked, s.Conflicted, stashes, state)
	}
	_ = tw.Flush()
	coWriteProblems(out, r.Problems, color)
}

func coWriteProblems(out io.Writer, ps []ops.Problem, color bool) {
	for _, p := range ps {
		fmt.Fprintf(out, "%s %s\n", coPaint(color, coAnsiRed, "problem:"), p.Error())
	}
}

type coJsonCheckout struct {
	Dir          string `json:"dir"`
	Path         string `json:"path"`
	Primary      bool   `json:"primary"`
	Outside      bool   `json:"outside"`
	Branch       string `json:"branch"`
	Detached     bool   `json:"detached"`
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

type coJsonRepo struct {
	Name      string           `json:"name"`
	Stashes   int              `json:"stashes"`
	Checkouts []coJsonCheckout `json:"checkouts"`
}

type coJsonProblem struct {
	Repo  string `json:"repo"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type coJsonStatus struct {
	Version      int             `json:"version"`
	Bundle       string          `json:"bundle"`
	Repositories []coJsonRepo    `json:"repositories"`
	Problems     []coJsonProblem `json:"problems"`
}

func coWriteStatusJSON(out io.Writer, root string, r ops.StatusReport) error {
	doc := coJsonStatus{Version: 1, Bundle: root, Repositories: []coJsonRepo{}, Problems: []coJsonProblem{}}
	stashKnown := false
	for _, c := range r.Checkouts {
		if n := len(doc.Repositories); n == 0 || doc.Repositories[n-1].Name != c.Repo {
			doc.Repositories = append(doc.Repositories, coJsonRepo{Name: c.Repo, Checkouts: []coJsonCheckout{}})
			stashKnown = false
		}
		repo := &doc.Repositories[len(doc.Repositories)-1]
		jc := coJsonCheckout{Dir: c.Dir, Path: c.Path, Primary: c.Primary, Outside: c.Outside, Branch: c.Branch, Detached: c.Detached}
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
		doc.Problems = append(doc.Problems, coJsonProblem{Repo: p.Repo, Path: p.Path, Error: p.Err.Error()})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func newFetchCmd(d Deps) *cobra.Command {
	var opts ops.FetchOptions
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch origin into every repository (once per repository)",
		Long: "Run `git fetch --prune origin` once per repository of the bundle (worktrees share\n" +
			"the remote-tracking refs), several repositories in parallel (-j). Only origin is\n" +
			"fetched, as update only fast-forwards to origin's branches.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			opts.Jobs = OptionsFrom(c).Jobs
			s, err := ops.FetchRepos(checkoutContext(c), w, opts, coCheckoutRenderer{plainRenderer{c.ErrOrStderr()}})
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
		Long: "Fetch origin once per repository, then fast-forward each checkout (primary first,\n" +
			"then worktrees) to its upstream with `git merge --ff-only`. Checkouts are skipped,\n" +
			"with the reason, when they have staged or unstaged changes (untracked files do not\n" +
			"count), are detached, have no upstream or a deleted one, or have diverged.\n" +
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
			opts.Force = forceFrom(c.Context())
			r, err := ops.Update(checkoutContext(c), w, opts, coCheckoutRenderer{plainRenderer{c.ErrOrStderr()}})
			if err != nil {
				return err
			}
			return coPrintUpdateSummary(c.OutOrStdout(), r, strict)
		},
	}
	coAddFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&strict, "strict", false, "exit with 1 when a checkout was skipped")
	return cmd
}

func coPrintUpdateSummary(out io.Writer, r ops.UpdateReport, strict bool) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	var ff, upToDate, skipped, failed int
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
		case u.Skipped():
			label = "skipped"
			skipped++
		default:
			label = "failed"
			failed++
			if u.Err != nil {
				if detail != "" {
					detail += ": "
				}
				detail += firstLine(u.Err.Error())
				if msg := u.Err.Error(); msg != firstLine(msg) {
					details = append(details, coDisplayName(u.Checkout)+":\n    "+strings.ReplaceAll(msg, "\n", "\n    "))
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", label, coDisplayName(u.Checkout), detail)
	}
	_ = tw.Flush()
	for _, d := range details {
		fmt.Fprintln(out, d)
	}
	coWriteProblems(out, r.Problems, false)
	fmt.Fprintf(out, "%d fast-forwarded, %d up to date, %d skipped, %d failed", ff, upToDate, skipped, failed)
	if n := len(r.Problems); n > 0 {
		fmt.Fprintf(out, ", %s", coPlural(n, "problem"))
	}
	fmt.Fprintln(out)
	switch {
	case failed > 0 || len(r.Problems) > 0:
		return fmt.Errorf("update: %d failed, %s", failed, coPlural(len(r.Problems), "problem"))
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
			sink := &coBlockSink{w: out}
			opts.Sink = sink
			r, err := ops.RunCommand(c.Context(), w, opts)
			if err != nil {
				return err
			}
			return coPrintRunSummary(out, r)
		},
	}
	cmd.Flags().SetInterspersed(false)
	coAddFilterFlags(cmd, &opts.Filter, true)
	cmd.Flags().BoolVar(&opts.DirtyOnly, "dirty-only", false, "only checkouts that are not clean")
	cmd.Flags().BoolVar(&opts.Shell, "shell", false, "run the first argument as a script with sh -c")
	return cmd
}

// coBlockSink prints each checkout's output as a block: a header, the output
// (a missing final newline is added), the failure if any, and a blank line.
type coBlockSink struct {
	w    io.Writer
	last byte // last byte written by the command, 0 if none
}

func (s *coBlockSink) Begin(c ops.Checkout) io.Writer {
	fmt.Fprintf(s.w, "==> %s <==\n", coDisplayName(c))
	s.last = 0
	return s
}

// Write is the output of the current block.
func (s *coBlockSink) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.last = p[len(p)-1]
	}
	return s.w.Write(p)
}

func (s *coBlockSink) End(r ops.RunResult) {
	if s.last != 0 && s.last != '\n' {
		fmt.Fprintln(s.w)
	}
	if r.Failed() {
		fmt.Fprintf(s.w, "[%s]\n", coRunFailure(r))
	}
	if r.Note != "" {
		fmt.Fprintf(s.w, "[note: %s]\n", r.Note)
	}
	fmt.Fprintln(s.w)
}

func coRunFailure(r ops.RunResult) string {
	switch {
	case !r.Started && r.Err != nil && errors.Is(r.Err, context.Canceled):
		return "not started (interrupted)"
	case r.Err != nil:
		return firstLine(r.Err.Error())
	default:
		return fmt.Sprintf("exit status %d", r.ExitCode)
	}
}

func coPrintRunSummary(out io.Writer, r ops.RunReport) error {
	var failed []ops.RunResult
	for _, res := range r.Results {
		if res.Failed() {
			failed = append(failed, res)
		}
	}
	fmt.Fprintf(out, "%d succeeded, %d failed\n", len(r.Results)-len(failed), len(failed))
	for _, res := range failed {
		fmt.Fprintf(out, "failed: %s (%s)\n", coDisplayName(res.Checkout), coRunFailure(res))
	}
	coWriteProblems(out, r.Problems, false)
	switch {
	case len(failed) > 0:
		return fmt.Errorf("%d of %d commands failed", len(failed), len(r.Results))
	case len(r.Problems) > 0:
		return coProblemsErr(len(r.Problems))
	}
	return nil
}
