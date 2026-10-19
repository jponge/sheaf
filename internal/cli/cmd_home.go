package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

// homeHint follows the help of bare sheaf outside a bundle.
const homeHint = "Not in a bundle: create one with `sheaf create [dir]`, or `sheaf init [dir]` to adopt existing checkouts."

// cdFileVar names the file in which the shell function defined by
// shell-init expects the directory to change to (see runHome).
const cdFileVar = "SHEAF_CD_FILE"

// installHome makes bare sheaf run runHome. A runnable root command would
// show "sheaf [flags]" in the usage of its help: the usage is rendered as
// for a root command that is not runnable, so the help is unchanged.
func installHome(root *cobra.Command, d Deps) {
	root.RunE = func(c *cobra.Command, _ []string) error { return runHome(c, d) }
	usage := root.UsageFunc()
	root.SetUsageFunc(func(c *cobra.Command) error {
		if c == root {
			run := root.RunE
			root.RunE = nil
			defer func() { root.RunE = run }()
		}
		return usage(c)
	})
}

// runHome is bare sheaf. Without the TUI (no terminal, --no-tui) it prints
// the help, as before the home screen existed; outside a bundle, the help
// and a hint. Otherwise it opens the overview of the bundle's checkouts
// (local state only, nothing is fetched) and runs the action chosen there
// with the TUI of its command, then opens the overview again, until the
// user quits. The overview is on the alternate screen, so that each one
// starts on a clear screen; the TUI of an action (menus, dialogs, prompt,
// progress view) runs inline on the normal screen, where what the action
// prints (summaries, notes, errors) also goes as it runs, so that it stays
// there as a log (also on an interruption). That output is also captured
// and, unless the action was cancelled without printing anything or is
// browse (whose outcome is the overview's message), shown in
// a scrollable report view on the alternate screen until a key is pressed
// (keys typed ahead are ignored, see tui.Report); the overview then comes
// back. Enter on a checkout ends sheaf: the path of the checkout is
// written to the file named by $SHEAF_CD_FILE, set by the shell function
// of shell-init, which then changes the directory; without it the path is
// printed. On a terminal, the programs of the home screen and of its
// actions set the window title (see homeTitle); bubbletea clears it when
// each program ends.
func runHome(c *cobra.Command, d Deps) error {
	if !d.tuiMode(c) {
		return c.Help()
	}
	o := OptionsFrom(c)
	wd, err := d.getwd()
	if err != nil {
		return err
	}
	root, err := ops.FindBundle(o.Bundle, wd)
	if errors.Is(err, bundle.ErrNotFound) {
		if err := c.Help(); err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), homeHint)
		return nil
	}
	if err != nil {
		return err
	}
	if o.Jobs < 1 {
		return fmt.Errorf("invalid --jobs %d: must be at least 1", o.Jobs)
	}
	d.windowTitle = homeTitle("", root)
	h := &home{c: c, d: d, s: newTUISession(c, d), root: root, jobs: o.Jobs}
	return h.loop()
}

// homeTitle returns the window title of the home screen on the bundle
// root, "sheaf: <name of root>", or that of its action name, "sheaf
// <name>: <name of root>" (see tuiSession).
func homeTitle(name, root string) string {
	if name != "" {
		name = " " + name
	}
	return "sheaf" + name + ": " + filepath.Base(root)
}

// home is the state of the home screen between two overviews.
type home struct {
	c    *cobra.Command
	d    Deps
	s    *tuiSession
	root string
	jobs int

	cursor, filter, message string
	rows                    map[string]homeRow // by row key, of the last overview
	bundle                  *bundle.Bundle     // as read for the last overview
	// selected are the selected rows of the next overview: those of the
	// last one, unless its action applied to them and succeeded.
	selected []string
	// action is the name of the running action.
	action string
	// succeeded tells whether every run of the last action succeeded, and
	// ran whether one ran, i.e. was not cancelled (see outcome).
	succeeded, ran bool
	// unchanged is the note of the last command that changed nothing (see
	// Deps.unchanged), and output a copy of what the last action printed
	// (see command); both reset before each action.
	unchanged string
	output    bytes.Buffer
}

// homeRow is what a row of the overview shows: a checkout, or a problem.
type homeRow struct {
	checkout *ops.CheckoutStatus
	problem  *ops.Problem
}

func (r homeRow) repo() string {
	if r.checkout != nil {
		return r.checkout.Repo
	}
	return r.problem.Repo
}

func (h *home) loop() error {
	for {
		ov, err := h.overview()
		if err != nil {
			return err
		}
		final, err := h.s.runModel(ov)
		if err != nil {
			return err
		}
		m, ok := final.(tui.Overview)
		if !ok {
			return nil
		}
		name, targets := m.Chosen()
		h.cursor, h.filter, h.selected = m.CurrentKey(), m.Filter(), m.SelectedKeys()
		switch name {
		case "": // quit, or input closed
			return nil
		case "cd":
			return h.cd(h.rows[targets[0]])
		}
		h.message = h.act(name, targets)
		if err := h.s.ctx.Err(); err != nil {
			return err // interrupted during the action: exit 130
		}
		if (h.ran || h.output.Len() > 0) && name != "browse" {
			// What the action printed, in a view until a key is pressed
			// (ctrl+c interrupts, as in the action's dialogs; input
			// ending returns to the overview); none after an action
			// cancelled before printing anything, nor after browse,
			// whose outcome (the page opened, or the error) fits in the
			// message.
			report := homeReport(name, h.root, h.output.String(), h.message)
			if _, err := h.s.runModel(report); err != nil {
				return err
			}
		}
		if h.succeeded && h.scope(name) == tui.ScopeRows {
			h.selected = nil // the selection was acted upon
		}
	}
}

// overview reads the bundle and the local state of its checkouts. When the
// table is wider than the window, BRANCH, then DIR, are truncated before
// STATE is cut.
func (h *home) overview() (tui.Overview, error) {
	w, err := ops.OpenWorkspace(h.root)
	if err != nil {
		return tui.Overview{}, err
	}
	r, err := ops.CollectStatus(h.s.ctx, w, ops.StatusOptions{Jobs: h.jobs, HeadTimes: true}, ops.Discard)
	if err != nil {
		return tui.Overview{}, err
	}
	if err := h.s.ctx.Err(); err != nil {
		return tui.Overview{}, err
	}
	h.rows, h.bundle = map[string]homeRow{}, w.Bundle()
	now := h.d.clock()
	var rows []tui.Row
	for i := range r.Checkouts {
		cs := &r.Checkouts[i]
		h.rows[cs.Path] = homeRow{checkout: cs}
		rows = append(rows, tui.Row{Key: cs.Path, Cells: []string{cs.Repo, displayDir(cs.Checkout), homeBranch(*cs), homeAge(now, cs.HeadTime), homeState(*cs)}, Alert: cs.Err != nil, Level: homeLevel(*cs)})
	}
	for i := range r.Problems {
		p := &r.Problems[i]
		key := "problem:" + p.Path
		h.rows[key] = homeRow{problem: p}
		rows = append(rows, tui.Row{Key: key, Cells: []string{p.Repo, filepath.Base(p.Path), "-", "", "problem: " + firstLine(p.Err.Error())}, Alert: true})
	}
	// Checkouts and problems are each sorted by repository: the problem
	// rows of a repository go after its checkouts, so that the rows of a
	// repository are contiguous for the grouping.
	slices.SortStableFunc(rows, func(a, b tui.Row) int { return strings.Compare(a.Cells[0], b.Cells[0]) })
	ov := tui.NewOverview("sheaf: "+w.Root, []string{"REPO", "DIR", "BRANCH", "AGE", "STATE"}, rows, h.actions())
	return ov.WithFilter(h.filter).WithCursor(h.cursor).WithMessage(h.message).WithSelected(h.selected).WithAltScreen().WithGrouping().WithShrinkable(2, 1), nil
}

// scope returns the scope of the action name.
func (h *home) scope(name string) tui.Scope {
	for _, a := range h.actions() {
		if a.Name == name {
			return a.Scope
		}
	}
	return tui.ScopeNone
}

func homeBranch(c ops.CheckoutStatus) string {
	switch {
	case c.Tag != "":
		return tagLabel(c.Tag, c.TagMovedTo)
	case c.Detached && c.Status != nil:
		return "(detached " + shortHead(c.Status.Head) + ")"
	case c.Detached:
		return "(detached)"
	case c.Status != nil && c.Status.Unborn:
		return c.Branch + " (unborn)"
	}
	return c.Branch
}

// homeLevel returns the level of the STATE cell of c (see homeState), the
// worst of: failed for an error, an upstream gone or a divergence (ahead
// and behind); warning for local changes or commits to push or pull (ahead,
// behind); info for no upstream; ok otherwise (clean, in sync or
// detached).
func homeLevel(c ops.CheckoutStatus) tui.Level {
	if c.Err != nil {
		return tui.LevelFail
	}
	s := c.Status
	switch {
	case s.Upstream != "" && (s.UpstreamGone || s.Ahead > 0 && s.Behind > 0):
		return tui.LevelFail
	case !c.Clean() || s.Upstream != "" && (s.Ahead > 0 || s.Behind > 0):
		return tui.LevelWarn
	case s.Upstream == "" && !c.Detached:
		return tui.LevelInfo
	}
	return tui.LevelOK
}

func homeState(c ops.CheckoutStatus) string {
	if c.Err != nil {
		return "error: " + firstLine(c.Err.Error())
	}
	s := c.Status
	parts := []string{"clean"}
	if !c.Clean() {
		parts[0] = "dirty"
	}
	switch {
	case s.Upstream != "" && s.UpstreamGone:
		parts = append(parts, "upstream gone")
	case s.Upstream != "":
		var ab []string
		if s.Ahead > 0 {
			ab = append(ab, "↑"+strconv.Itoa(s.Ahead))
		}
		if s.Behind > 0 {
			ab = append(ab, "↓"+strconv.Itoa(s.Behind))
		}
		if len(ab) > 0 {
			parts = append(parts, strings.Join(ab, " "))
		}
	case !c.Detached:
		parts = append(parts, "no upstream")
	}
	return strings.Join(parts, ", ")
}

// homeAge returns the age at now of a commit dated t, for the AGE column:
// "now" under a minute (and for a date in the future, e.g. clock skew),
// then the whole number of minutes ("5m") under an hour, hours ("3h")
// under a day, days ("2d") under a week, weeks ("4w") under 30 days,
// months of 30 days ("11mo") under 12 such months (360 days), then years
// of 365 days ("2y"), at least 1 (360 to 364 days are "1y");
// empty for a zero t (unknown).
func homeAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	const day = 24 * time.Hour
	d := now.Sub(t)
	if d < time.Minute {
		return "now"
	}
	for _, u := range []struct {
		unit, below time.Duration
		suffix      string
	}{
		{time.Minute, time.Hour, "m"},
		{time.Hour, day, "h"},
		{day, 7 * day, "d"},
		{7 * day, 30 * day, "w"},
		{30 * day, 360 * day, "mo"},
	} {
		if d < u.below {
			return strconv.Itoa(int(d/u.unit)) + u.suffix
		}
	}
	return strconv.Itoa(max(1, int(d/(365*day)))) + "y"
}

// actions are the actions of the overview.
func (h *home) actions() []tui.OverviewAction {
	noProblem := func(targets []string) string {
		for _, k := range targets {
			if h.rows[k].problem != nil {
				return "not available on a row with a problem (see `sheaf doctor`)"
			}
		}
		return ""
	}
	removable := func(targets []string) string {
		if why := noProblem(targets); why != "" {
			return why
		}
		for _, k := range targets {
			switch c := h.rows[k].checkout; {
			case c.Primary:
				return "the primary checkout cannot be removed (use `sheaf remove " + c.Repo + "`)"
			case c.Outside:
				return "the worktree " + c.Path + " is outside its repository directory: remove it with git"
			}
		}
		return ""
	}
	onGitHub := func(targets []string) string {
		if why := noProblem(targets); why != "" {
			return why
		}
		for _, k := range targets {
			if r, ok := h.bundle.Repository(h.rows[k].repo()); ok && !ops.OnGitHub(*r) {
				return r.Name + " is not a GitHub repository (hosted on " + r.HostName() + ")"
			}
		}
		return ""
	}
	// browse resolves the repository from the checkout's directory (see
	// act), which must be in its repository directory.
	browsable := func(targets []string) string {
		if why := onGitHub(targets); why != "" {
			return why
		}
		if c := h.rows[targets[0]].checkout; c.Outside {
			return "the worktree " + c.Path + " is outside its repository directory: use `sheaf browse " + c.Repo + "`"
		}
		return ""
	}
	return []tui.OverviewAction{
		{Key: "enter", Name: "cd", Help: "cd", Scope: tui.ScopeCurrent, Check: noProblem},
		{Key: "u", Name: "update", Help: "update", Scope: tui.ScopeRows, Check: noProblem},
		{Key: "f", Name: "fetch", Help: "fetch", Scope: tui.ScopeRows},
		{Key: "w", Name: "add", Help: "add worktree", Scope: tui.ScopeCurrent},
		{Key: "#", Name: "pr", Help: "check out PR", Scope: tui.ScopeCurrent, Check: onGitHub},
		{Key: "b", Name: "browse", Help: "browse", Scope: tui.ScopeCurrent, Check: browsable},
		{Key: "r", Name: "remove", Help: "remove worktree", Scope: tui.ScopeRows, Check: removable},
		{Key: "p", Name: "prune", Help: "prune", Scope: tui.ScopeNone},
		{Key: "d", Name: "doctor", Help: "doctor", Scope: tui.ScopeNone},
		{Key: "s", Name: "sync", Help: "sync", Scope: tui.ScopeNone},
	}
}

// cd ends sheaf on the checkout of r (see runHome).
func (h *home) cd(r homeRow) error {
	path := r.checkout.Path
	if f := h.d.getenv(cdFileVar); f != "" {
		return os.WriteFile(f, []byte(path), 0o600)
	}
	_, err := fmt.Fprintln(h.c.OutOrStdout(), path)
	return err
}

// act runs the action name on targets and returns the message the next
// overview displays.
func (h *home) act(name string, targets []string) string {
	h.action = name
	h.unchanged = ""
	h.output.Reset()
	var repos []string
	for _, k := range targets {
		if r := h.rows[k].repo(); !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	switch name {
	case "update":
		var paths []string
		for _, k := range targets {
			paths = append(paths, h.rows[k].checkout.Path)
		}
		w, err := ops.OpenWorkspace(h.root)
		if err == nil {
			// As in command: what update prints is also captured, its TUI
			// renders on the terminal only.
			c := &cobra.Command{}
			c.SetIn(h.c.InOrStdin())
			c.SetOut(teeWriter{io.MultiWriter(h.c.OutOrStdout(), &h.output), h.c.OutOrStdout()})
			c.SetErr(teeWriter{io.MultiWriter(h.c.ErrOrStderr(), &h.output), h.c.ErrOrStderr()})
			c.SetContext(h.s.ctx)
			d := h.d
			d.ctl, d.term, d.windowTitle = h.s.ctl, h.s.out, homeTitle(name, h.root)
			opts := ops.UpdateOptions{Filter: ops.CheckoutFilter{Repos: repos, Paths: paths}, Jobs: h.jobs}
			err = tuiUpdate(c, d, w, opts, false)
		}
		return h.outcome(name, []error{err})
	case "fetch":
		args := []string{"fetch"}
		for _, r := range repos {
			args = append(args, "--repo="+r)
		}
		return h.outcome(name, []error{h.command(args...)})
	case "add":
		return h.add(repos[0])
	case "pr":
		return h.pr(repos[0])
	case "browse":
		// As browse run in the checkout's directory.
		return h.outcome(name, []error{h.commandIn(h.rows[targets[0]].checkout.Path, "browse")})
	case "remove":
		var errs []error
		for _, k := range targets {
			c := h.rows[k].checkout
			errs = append(errs, h.command("wt", "rm", "--dir="+c.Dir, "--", c.Repo))
			if h.s.ctx.Err() != nil {
				break
			}
		}
		return h.outcome(name, errs)
	default: // prune, doctor, sync
		return h.outcome(name, []error{h.command(name)})
	}
}

// add asks for a branch or a tag and adds the worktree with `wt add`.
func (h *home) add(repo string) string {
	// The hints follow wt add: an existing branch (local, then origin's),
	// else a new one from the default branch; --tag pins a detached
	// worktree at the tag.
	prompt := tui.NewPrompt("New worktree of "+repo, []string{"branch", "tag"}).WithHints(
		"an existing local or remote branch, else a new branch from the default branch",
		"a tag: a detached worktree pinned at it")
	final, err := h.session().run(prompt)
	if err != nil {
		return h.outcome("add", []error{err})
	}
	p, ok := final.(tui.Prompt)
	if !ok || !p.Confirmed() {
		return h.outcome("add", []error{errCancelled})
	}
	value := strings.TrimSpace(p.Value())
	if p.Mode() == "tag" {
		return h.outcome("add", []error{h.command("wt", "add", "--tag="+value, "--", repo)})
	}
	return h.outcome("add", []error{h.command("wt", "add", "--", repo, value)})
}

// pr asks for the number of a pull request of repo and checks it out with
// `wt pr`.
func (h *home) pr(repo string) string {
	prompt := tui.NewPrompt("Pull request of "+repo, []string{"number"}).WithHints(
		"checked out in a worktree, as `sheaf wt pr` does (#123 also accepted)").WithCheck(func(v string) string {
		if _, err := parsePRNumber(strings.TrimSpace(v)); err != nil {
			return err.Error()
		}
		return ""
	})
	final, err := h.session().run(prompt)
	if err != nil {
		return h.outcome("pr", []error{err})
	}
	p, ok := final.(tui.Prompt)
	if !ok || !p.Confirmed() {
		return h.outcome("pr", []error{errCancelled})
	}
	return h.outcome("pr", []error{h.command("wt", "pr", "--", repo, strings.TrimSpace(p.Value()))})
}

// session returns the TUI session of the home screen with the window title
// of the running action.
func (h *home) session() *tuiSession {
	s := *h.s
	if s.title != "" {
		s.title = homeTitle(h.action, h.root)
	}
	return &s
}

// command runs `sheaf <args>` (global options of this run, the bundle of
// the home screen) as a command line would (see commandIn).
func (h *home) command(args ...string) error { return h.commandIn("", args...) }

// commandIn runs `sheaf <args>` (global options of this run, the bundle of
// the home screen) as a command line would, with dir as its working
// directory when not empty, in this process and with its
// context, so that interruptions are handled as usual. Its TUI renders on
// the terminal; what it prints (standard output and error) goes to the
// terminal as it runs, as for the command alone (so that it stays there,
// e.g. after an interruption), and to h.output, for the report view. A
// command whose user chose nothing (see Deps.cancelled) returns
// errCancelled.
func (h *home) commandIn(dir string, args ...string) error {
	d := h.d
	if dir != "" {
		d.Getwd = func() (string, error) { return dir, nil }
	}
	d.ctl = h.s.ctl // the home screen's controller, with its context
	d.term = h.s.out
	d.windowTitle = homeTitle(h.action, h.root)
	d.unchanged = func(note string) { h.unchanged = note }
	cancelled := false
	d.cancelled = func() { cancelled = true }
	out := teeWriter{io.MultiWriter(h.c.OutOrStdout(), &h.output), h.c.OutOrStdout()}
	errw := teeWriter{io.MultiWriter(h.c.ErrOrStderr(), &h.output), h.c.ErrOrStderr()}
	root := NewRootCmdWithDeps(Streams{In: h.c.InOrStdin(), Out: out, Err: errw}, d)
	root.SetArgs(append([]string{"--bundle=" + h.root, "--jobs=" + strconv.Itoa(h.jobs)}, args...))
	err := root.ExecuteContext(h.s.ctx)
	if err == nil && cancelled {
		return errCancelled
	}
	return err
}

// outcome prints the errors of an action to standard error and h.output
// like execute does (cancellations excepted) and returns the message for
// the overview: "<name>: done", "<name>: <note>" when the command changed
// nothing (e.g. "prune: nothing to prune"), "<name>: cancelled", "<name>
// failed: <error>", or the counts when the action ran several times.
func (h *home) outcome(name string, errs []error) string {
	var done, cancelled, failed int
	var last error
	for _, err := range errs {
		switch {
		case err == nil:
			done++
		case errors.Is(err, errCancelled):
			cancelled++
		default:
			failed++
			last = err
			if h.s.ctx.Err() == nil {
				fmt.Fprintln(io.MultiWriter(h.c.ErrOrStderr(), &h.output), "sheaf:", err)
			}
		}
	}
	h.succeeded, h.ran = cancelled == 0 && failed == 0, done+failed > 0
	switch {
	case len(errs) > 1:
		return fmt.Sprintf("%s: %d done, %d cancelled, %d failed", name, done, cancelled, failed)
	case failed > 0:
		return name + " failed: " + firstLine(last.Error())
	case cancelled > 0:
		return name + ": cancelled"
	case h.unchanged != "":
		return name + ": " + h.unchanged
	}
	return name + ": done"
}

// homeReport returns the report view of the action name on the bundle
// root, which printed output (see homeReportLines), with the level words
// of its lines colored (see levelMark): the output is captured without
// escape sequences.
func homeReport(name, root, output, message string) tui.Report {
	return tui.NewReport("sheaf "+name+": "+root, homeReportLines(output, message), "back to the overview").WithLevels(levelMark)
}

// homeReportLines returns the lines of the report view of an action: those
// of output, what it printed, without escape sequences; message, its
// outcome, when it printed nothing.
func homeReportLines(output, message string) []string {
	output = strings.TrimSuffix(ansi.Strip(output), "\n")
	if output == "" {
		return []string{message}
	}
	return strings.Split(output, "\n")
}
