package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

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
// user quits. Enter on a checkout ends sheaf: the path of the checkout is
// written to the file named by $SHEAF_CD_FILE, set by the shell function
// of shell-init, which then changes the directory; without it the path is
// printed.
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
	h := &home{c: c, d: d, s: newTUISession(c, d), root: root, jobs: o.Jobs}
	return h.loop()
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
	// selected are the selected rows of the next overview: those of the
	// last one, unless its action applied to them and succeeded.
	selected []string
	// succeeded tells whether every run of the last action succeeded (see
	// outcome).
	succeeded bool
	// unchanged is the note of the last command that changed nothing (see
	// Deps.unchanged), reset before each action.
	unchanged string
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
		if h.succeeded && h.scope(name) == tui.ScopeRows {
			h.selected = nil // the selection was acted upon
		}
	}
}

// overview reads the bundle and the local state of its checkouts.
func (h *home) overview() (tui.Overview, error) {
	w, err := ops.OpenWorkspace(h.root)
	if err != nil {
		return tui.Overview{}, err
	}
	r, err := ops.CollectStatus(h.s.ctx, w, ops.StatusOptions{Jobs: h.jobs}, ops.Discard)
	if err != nil {
		return tui.Overview{}, err
	}
	if err := h.s.ctx.Err(); err != nil {
		return tui.Overview{}, err
	}
	h.rows = map[string]homeRow{}
	var rows []tui.Row
	for i := range r.Checkouts {
		cs := &r.Checkouts[i]
		h.rows[cs.Path] = homeRow{checkout: cs}
		rows = append(rows, tui.Row{Key: cs.Path, Cells: []string{cs.Repo, displayDir(cs.Checkout), homeBranch(*cs), homeState(*cs)}, Alert: cs.Err != nil})
	}
	for i := range r.Problems {
		p := &r.Problems[i]
		key := "problem:" + p.Path
		h.rows[key] = homeRow{problem: p}
		rows = append(rows, tui.Row{Key: key, Cells: []string{p.Repo, filepath.Base(p.Path), "-", "problem: " + firstLine(p.Err.Error())}, Alert: true})
	}
	ov := tui.NewOverview("sheaf: "+w.Root, []string{"REPO", "DIR", "BRANCH", "STATE"}, rows, h.actions())
	return ov.WithFilter(h.filter).WithCursor(h.cursor).WithMessage(h.message).WithSelected(h.selected), nil
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
		if s.Ahead > 0 {
			parts = append(parts, "ahead "+strconv.Itoa(s.Ahead))
		}
		if s.Behind > 0 {
			parts = append(parts, "behind "+strconv.Itoa(s.Behind))
		}
	case !c.Detached:
		parts = append(parts, "no upstream")
	}
	return strings.Join(parts, ", ")
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
	return []tui.OverviewAction{
		{Key: "enter", Name: "cd", Help: "cd", Scope: tui.ScopeCurrent, Check: noProblem},
		{Key: "u", Name: "update", Help: "update", Scope: tui.ScopeRows, Check: noProblem},
		{Key: "f", Name: "fetch", Help: "fetch", Scope: tui.ScopeRows},
		{Key: "w", Name: "add", Help: "add worktree", Scope: tui.ScopeCurrent},
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
	h.unchanged = ""
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
			opts := ops.UpdateOptions{Filter: ops.CheckoutFilter{Repos: repos, Paths: paths}, Jobs: h.jobs}
			err = tuiUpdate(h.c, h.d, w, opts, false)
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
	final, err := h.s.run(prompt)
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

// command runs `sheaf <args>` (global options of this run, the bundle of
// the home screen) as a command line would, TUI included, in this process
// and with its context, so that interruptions are handled as usual.
func (h *home) command(args ...string) error {
	d := h.d
	d.ctl = h.s.ctl // the home screen's controller, with its context
	d.unchanged = func(note string) { h.unchanged = note }
	root := NewRootCmdWithDeps(Streams{In: h.c.InOrStdin(), Out: h.c.OutOrStdout(), Err: h.c.ErrOrStderr()}, d)
	root.SetArgs(append([]string{"--bundle=" + h.root, "--jobs=" + strconv.Itoa(h.jobs)}, args...))
	return root.ExecuteContext(h.s.ctx)
}

// outcome prints the errors of an action like execute does (cancellations
// excepted) and returns the message for the overview: "<name>: done",
// "<name>: <note>" when the command changed nothing (e.g. "prune: nothing
// to prune"), "<name>: cancelled", "<name> failed: <error>", or the counts
// when the action ran several times.
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
				fmt.Fprintln(h.c.ErrOrStderr(), "sheaf:", err)
			}
		}
	}
	h.succeeded = cancelled == 0 && failed == 0
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
