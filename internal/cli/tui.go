package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

// orgRepoItems maps the repositories of an organization offered by `add
// --org` (ops.SelectionRequiredError.Available) to menu items keyed by
// name: description as detail, fork/archived tags; repositories already in
// the bundle are shown selected, disabled and tagged "present".
func orgRepoItems(avail []hosting.Repo, b *bundle.Bundle) []tui.Item {
	items := make([]tui.Item, 0, len(avail))
	for _, r := range avail {
		it := tui.Item{Key: r.Name, Label: r.Name, Detail: r.Description}
		if r.Fork {
			it.Tags = append(it.Tags, "fork")
		}
		if r.Archived {
			it.Tags = append(it.Tags, "archived")
		}
		if _, ok := ops.InBundle(b, r.Slug()); ok {
			it.Tags = append(it.Tags, "present")
			it.Selected, it.Disabled = true, true
		}
		items = append(items, it)
	}
	return items
}

// proposalItems maps sync proposals to menu items as documented on
// ops.Proposal: nothing preselected; new and gone proposals selectable,
// moved and archived ones informational (disabled), as are gone ones
// refused as a mass removal (the TUI never allows it). Gone proposals are
// warnings: selecting one removes the repository.
func proposalItems(ps []ops.Proposal) []tui.Item {
	items := make([]tui.Item, 0, len(ps))
	for _, p := range ps {
		it := tui.Item{Key: p.Key, Label: p.Name, Detail: p.Slug, Tags: []string{string(p.Kind)}}
		switch {
		case p.Kind == ops.ProposalMoved:
			it.Detail += ", now " + p.Repo.Slug()
		case p.Note != "":
			it.Detail += ": " + p.Note
		case p.Repo.Description != "":
			it.Detail += ": " + p.Repo.Description
		}
		if p.Repo.Fork {
			it.Tags = append(it.Tags, "fork")
		}
		if p.Kind == ops.ProposalNew && p.Repo.Archived {
			it.Tags = append(it.Tags, "archived")
		}
		if p.MassRemoval {
			it.Tags = append(it.Tags, "mass removal: refused")
		}
		if p.Kind == ops.ProposalGone {
			it.Level = tui.LevelWarn
		}
		it.Disabled = (p.Kind != ops.ProposalNew && p.Kind != ops.ProposalGone) || p.MassRemoval
		items = append(items, it)
	}
	return items
}

// candidateItems maps the checkouts found by init to menu items keyed by
// path, all preselected.
func candidateItems(cs []ops.Candidate) []tui.Item {
	items := make([]tui.Item, 0, len(cs))
	for _, c := range cs {
		items = append(items, tui.Item{Key: c.Path, Label: c.Name, Detail: c.Primary, Tags: []string{c.Kind.String()}, Selected: true})
	}
	return items
}

// eventRow maps an ops event to a progress row update: one row per
// repository, or per checkout when Worktree is set. Events of the aside
// phase (e.g. the fetch preceding update's fast-forwards) go to a separate
// row of the repository, counted apart (see progressOpts).
func eventRow(e ops.Event, aside string) tui.RowUpdate {
	who := e.Repo
	if e.Worktree != "" {
		who += "/" + e.Worktree
	}
	u := tui.RowUpdate{Message: e.Message}
	if aside != "" && e.Phase == aside {
		who += " (" + e.Phase + ")"
		u.Aside = true
	}
	u.Key, u.Label = who, who
	switch e.Kind {
	case ops.KindStart:
		u.State, u.Message = tui.RowRunning, e.Phase
	case ops.KindDone:
		u.State = tui.RowDone
	case ops.KindSkipped:
		u.State = tui.RowSkipped
		if errors.Is(e.Err, ops.ErrNotConfirmed) {
			u.State = tui.RowCancelled
		}
	case ops.KindFailed:
		u.State = tui.RowFailed
		switch {
		case interruptedErr(e.Err):
			u.State, u.Message = tui.RowInterrupted, interruptedNote(e.Message)
		case e.Err != nil:
			u.Message = errorText(e.Err)
		}
	default:
		if e.Phase == ops.PhaseWaitFastForward {
			u.Message = "waiting for in-progress fast-forward to finish (Ctrl-C again to force)"
		}
	}
	return u
}

// errCancelled is returned when the user cancels a TUI menu or declines a
// confirmation: nothing was changed by the command (exit status 1).
var errCancelled = errors.New("cancelled: nothing was changed")

// tuiMode tells whether c runs its TUI (see Deps.UseTUI).
func (d Deps) tuiMode(c *cobra.Command) bool {
	f := d.UseTUI
	if f == nil {
		f = UseTUI
	}
	return f(c.InOrStdin(), d.termOf(c), OptionsFrom(c).NoTUI)
}

// termOf returns the output on which the TUI programs of c render: d.term
// if set, else standard output.
func (d Deps) termOf(c *cobra.Command) io.Writer {
	if d.term != nil {
		return d.term
	}
	return c.OutOrStdout()
}

// tuiSession runs the bubbletea programs of one command: menus and
// confirmations (tui.MultiSelect, tui.Confirm) and progress views. Programs
// render inline (no alternate screen; the overview of the home screen is
// the exception), so their last frame stays in the terminal; bubbletea's
// own signal handler is disabled: signals go through
// execute's interruptCtl, like ctrl+c in a progress view. With controls
// (see terminalControls), progress views drive the terminal's progress
// indicator, and the programs set the window title if there is one
// (Deps.windowTitle).
type tuiSession struct {
	ctx       context.Context
	ctl       *interruptCtl
	in        io.Reader
	out, errw io.Writer
	spin      *spinner // held while a program runs
	controls  bool
	title     string
}

func newTUISession(c *cobra.Command, d Deps) *tuiSession {
	ctx := c.Context()
	ctl := d.ctl
	if ctl == nil { // not run through execute (no signal handling)
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		ctl = newInterruptCtl()
		ctl.bind(cancel, c.ErrOrStderr())
	}
	out := d.termOf(c)
	if p, ok := out.(*pipeWatch); ok {
		// bubbletea needs the terminal itself to know its size.
		out = p.w
	}
	s := &tuiSession{ctx: ctx, ctl: ctl, in: c.InOrStdin(), out: out, errw: d.leveled(c, c.ErrOrStderr()), spin: d.spin}
	if s.controls = d.terminalControls(out); s.controls {
		s.title = d.windowTitle
	}
	return s
}

// terminalControls tells whether the TUI programs rendering on out may send
// it the controls of the terminal window (title, progress indicator): when
// it is a terminal, other than TERM=dumb.
func (d Deps) terminalControls(out io.Writer) bool {
	return isTerminal(out) && d.getenv("TERM") != "dumb"
}

// titled returns m wrapped to set the window title of s, if any.
func (s *tuiSession) titled(m tea.Model) tea.Model {
	if s.title == "" {
		return m
	}
	return tui.Titled{Model: m, Title: s.title}
}

// run runs a menu or confirmation program. An interruption (signal) kills
// it and returns the context's error.
func (s *tuiSession) run(m tea.Model) (tea.Model, error) { return s.runModel(keepLastLine{m}) }

// runModel is run without keepLastLine: the final frame is m's last view
// as is.
func (s *tuiSession) runModel(m tea.Model) (tea.Model, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	flush(s.errw) // the program owns the terminal
	s.spin.hold()
	final, err := tui.Run(s.titled(m), s.in, s.out, tea.WithContext(s.ctx), tea.WithoutSignalHandler())
	s.spin.release()
	if cerr := s.ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if final == nil {
		return nil, err
	}
	m = unwrapModel(final)
	if i, ok := m.(interface{ Interrupted() bool }); ok && i.Interrupted() {
		// ctrl+c in a menu or dialog: like SIGINT (exit 130).
		s.ctl.interrupt()
		return nil, s.ctx.Err()
	}
	return m, err
}

// multiSelect runs a menu and returns the selected keys; cancelling (esc,
// q, ctrl+c) or input ending returns errCancelled.
func (s *tuiSession) multiSelect(title string, items []tui.Item) ([]string, error) {
	return s.pick(tui.NewMultiSelect(title, items))
}

// pick is multiSelect with a menu configured by the caller.
func (s *tuiSession) pick(menu tui.MultiSelect) ([]string, error) {
	final, err := s.run(menu)
	if err != nil {
		return nil, err
	}
	ms, ok := final.(tui.MultiSelect)
	if !ok || !ms.Confirmed() {
		return nil, errCancelled
	}
	return ms.SelectedKeys(), nil
}

// confirm asks a yes/no question (default no); declining returns
// errCancelled.
func (s *tuiSession) confirm(question string, details []string) error {
	final, err := s.run(tui.NewConfirm(question, details))
	if err != nil {
		return err
	}
	if c, ok := final.(tui.Confirm); !ok || !c.Confirmed() {
		return errCancelled
	}
	return nil
}

// askFunc asks a yes/no question from inside a progress view and returns
// the answer (no when the view is gone or the command interrupted).
type askFunc func(question string, details []string) bool

// progressOpts configures progress: events of the Aside phase go to rows
// that are counted apart (see eventRow, tui.Progress.WithAside); with
// Single set, every event goes to
// one row labelled Single (an operation on one repository), which is all
// the planned work; Confirmed, if
// set, replaces the title once a question is answered yes (e.g. "Checking
// alpha for removal" becomes "Removing alpha").
type progressOpts struct {
	Aside, Single, Confirmed string
}

// progress runs work in a goroutine while a tui.Progress view displays the
// events it reports, and returns once work has returned. The reporter is
// an ops.Planner: the units of work it announces are the rows the view
// expects (see tui.PlannedMsg). Notices (see
// isNotice) are then printed on stderr in CLI format, followed by a hint
// when git failed for lack of a terminal to prompt in (see
// looksLikeCredentialFailure). Reporting never blocks
// work: events are queued and forwarded to the program by another
// goroutine. ctrl+c in the view goes to interruptCtl.interrupt. If the
// program ends first (input closed), the remaining events are printed on
// stderr like in CLI mode, and questions are answered no.
func (s *tuiSession) progress(title string, o progressOpts, work func(ctx context.Context, rep ops.Reporter, ask askFunc)) {
	plain := checkoutRenderer{plainRenderer{s.errw}}
	model := tui.NewProgress(title).WithInterrupt(s.ctl.interrupt)
	if o.Aside != "" {
		model = model.WithAside(o.Aside)
	}
	if s.controls {
		model = model.WithTerminalProgress()
	}
	p, err := tui.NewProgram(s.titled(model), s.in, s.out, tea.WithoutSignalHandler())
	if err != nil {
		fmt.Fprintf(s.errw, "sheaf: cannot start the progress view (%v); plain output follows\n", err)
		work(s.ctx, plain, func(string, []string) bool { return false })
		return
	}
	flush(s.errw) // the program owns the terminal
	s.spin.hold() // the work may start a phase (see spinHosting)
	q := &msgQueue{wake: make(chan struct{}, 1)}
	if o.Single != "" {
		q.push(tui.PlannedMsg{Units: 1})
	}
	programDone := make(chan struct{})
	ask := func(question string, details []string) bool {
		reply := make(chan bool, 1)
		q.push(tui.AskMsg{Question: question, Details: details, Reply: reply})
		select {
		case v := <-reply:
			if v && o.Confirmed != "" {
				q.push(tui.TitleMsg{Title: o.Confirmed})
			}
			return v
		case <-programDone:
			return false
		case <-s.ctx.Done():
			return false
		}
	}
	workDone := make(chan struct{})
	go func() {
		defer close(workDone)
		defer q.close()
		work(s.ctx, queueReporter{q, o.Single != ""}, ask)
	}()
	pumpDone := make(chan struct{})
	var notices []ops.Event // written by the pump, read after pumpDone
	credentials := false
	go func() {
		defer close(pumpDone)
		for {
			items, closed := q.take()
			for _, it := range items {
				select {
				case <-programDone:
					if e, ok := it.(ops.Event); ok {
						plain.Report(e)
					}
					continue // questions: ask returns no on programDone
				default:
				}
				switch v := it.(type) {
				case ops.Event:
					if isNotice(v) {
						notices = append(notices, v)
					}
					if v.Kind == ops.KindFailed && v.Err != nil && looksLikeCredentialFailure(v.Err.Error()) {
						credentials = true
					}
					u := eventRow(v, o.Aside)
					if o.Single != "" {
						u.Key, u.Label = o.Single, o.Single
					}
					p.Send(u)
				case tui.AskMsg, tui.TitleMsg, tui.PlannedMsg:
					p.Send(v)
				}
			}
			if closed {
				p.Send(tui.FinishMsg{}) // no-op if the program has ended
				return
			}
		}
	}()
	runDone := make(chan struct{})
	s.ctl.setNotify(func(n int) { go p.Send(tui.InterruptedMsg{Count: n}) })
	s.ctl.setRelease(func() {
		p.Kill()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
		}
	})
	_, _ = p.Run()
	s.spin.release()
	s.ctl.setRelease(nil)
	s.ctl.setNotify(nil)
	close(runDone)
	close(programDone)
	<-workDone
	<-pumpDone
	for _, e := range notices {
		plain.Report(e)
	}
	if credentials {
		fmt.Fprintln(s.errw, credentialHint)
	}
	flush(s.errw)
}

// queueReporter is the reporter of progress: events and announced units of
// work (tui.PlannedMsg) go to q; with single, the units are ignored (the
// one row is all the work).
type queueReporter struct {
	q      *msgQueue
	single bool
}

func (r queueReporter) Report(e ops.Event) { r.q.push(e) }

func (r queueReporter) Plan(units int) {
	if !r.single {
		r.q.push(tui.PlannedMsg{Units: units})
	}
}

// msgQueue is an unbounded FIFO between the goroutines of progress.
type msgQueue struct {
	mu     sync.Mutex
	items  []any
	closed bool
	wake   chan struct{}
}

func (q *msgQueue) push(it any) {
	q.mu.Lock()
	q.items = append(q.items, it)
	q.mu.Unlock()
	q.signal()
}

func (q *msgQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *msgQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take waits for queued items or the closing, and returns the items and
// whether the queue is closed (no item follows).
func (q *msgQueue) take() ([]any, bool) {
	for {
		q.mu.Lock()
		items, closed := q.items, q.closed
		q.items = nil
		q.mu.Unlock()
		if len(items) > 0 || closed {
			return items, closed
		}
		<-q.wake
	}
}

// printFailureDetails writes to w the full error of the failed results
// whose message has several lines: the summary only shows the first one,
// and in TUI mode the events (which carry it in CLI mode) are not printed.
func printFailureDetails(w io.Writer, s ops.Summary) {
	for _, r := range s.Results {
		if r.Status == ops.StatusFailed && r.Err != nil {
			if msg := r.Err.Error(); msg != firstLine(msg) {
				fmt.Fprintf(w, "sheaf: %s: %s\n", r.Repo, msg)
			}
		}
	}
}

// keepLastLine wraps a menu or dialog so that its view ends with an empty
// line: bubbletea erases the last line of the final frame when the program
// exits, and the whole final state should stay in the terminal.
type keepLastLine struct{ tea.Model }

func (k keepLastLine) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := k.Model.Update(msg)
	return keepLastLine{m}, cmd
}

func (k keepLastLine) View() tea.View {
	v := k.Model.View()
	v.Content += "\n"
	return v
}

// unwrapModel returns the model wrapped by tui.Titled and keepLastLine, or
// m itself.
func unwrapModel(m tea.Model) tea.Model {
	if t, ok := m.(tui.Titled); ok {
		m = t.Model
	}
	if k, ok := m.(keepLastLine); ok {
		return k.Model
	}
	return m
}

// credentialFailures are fragments of git, ssh and credential helper
// messages showing that a prompt (password, passphrase, host key) could not
// be shown: in TUI mode git runs without the terminal.
var credentialFailures = []string{
	"permission denied",
	"host key verification failed",
	"could not read username",
	"could not read password",
	"enter passphrase",
	"terminal prompts disabled",
}

// looksLikeCredentialFailure reports whether msg looks like one of
// credentialFailures (case-insensitively).
func looksLikeCredentialFailure(msg string) bool {
	m := strings.ToLower(msg)
	for _, f := range credentialFailures {
		if strings.Contains(m, f) {
			return true
		}
	}
	return false
}

// credentialHint is printed after a TUI run in which git failed like that.
const credentialHint = "sheaf: hint: git could not ask for credentials or confirm a host key in TUI mode; run with --no-tui -j 1 to let git prompt in the terminal"

// isNotice tells the progress events that CLI mode prints and that matter
// after the run (warnings, notes, informational risks, stale state, fallbacks):
// the progress view only keeps a row's latest message, so they are printed
// again on stderr once the view is closed.
func isNotice(e ops.Event) bool {
	if e.Kind != ops.KindProgress {
		return false
	}
	m := e.Message
	return strings.HasPrefix(m, "warning: ") || strings.HasPrefix(m, "note: ") || strings.HasPrefix(m, "offline: ") ||
		strings.Contains(m, "(informational, not blocking)") || strings.Contains(m, "will be lost (--force)") ||
		strings.HasPrefix(m, "git worktree remove failed, deleting the directory instead")
}

// leveled returns w, standard output or error of c, for the summaries and
// notes that c prints: in TUI mode, with their level words colored (see
// levelWriter) as the terminal behind w allows (see tui.Painter); else,
// and always with --json (stdout and stderr alike), w itself.
func (d Deps) leveled(c *cobra.Command, w io.Writer) io.Writer {
	paint := d.painter(c, w)
	if paint == nil {
		return w
	}
	return &levelWriter{w: w, paint: paint}
}

// painter returns how c renders text in the style of a level on w: in TUI
// mode and without --json, as the terminal behind w allows (see
// tui.Painter); else nil, for unstyled output.
func (d Deps) painter(c *cobra.Command, w io.Writer) func(tui.Level, string) string {
	if asJSON, err := c.Flags().GetBool("json"); !d.tuiMode(c) || err == nil && asJSON {
		return nil
	}
	environ := os.Environ
	if d.environ != nil {
		environ = d.environ
	}
	return tui.Painter(terminalOf(w), environ())
}

// teeWriter writes to Writer, a copy of term (the terminal) and of
// something else, and tells term as the terminal behind it (see
// terminalOf): the home screen captures what its actions print.
type teeWriter struct {
	io.Writer
	term io.Writer
}

// terminalOf returns the stream behind w (standard output watched for
// EPIPE, the terminal of a teeWriter), which may be a terminal.
func terminalOf(w io.Writer) io.Writer {
	switch v := w.(type) {
	case *pipeWatch:
		return terminalOf(v.w)
	case teeWriter:
		return terminalOf(v.term)
	case *spinWriter:
		return terminalOf(v.w)
	}
	return w
}

// spinnerOf returns the spinner whose writer w writes through, if any.
func spinnerOf(w io.Writer) *spinner {
	switch v := w.(type) {
	case *pipeWatch:
		return spinnerOf(v.w)
	case teeWriter:
		return spinnerOf(v.term)
	case *spinWriter:
		return v.s
	}
	return nil
}
