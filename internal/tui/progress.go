package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// RowState is the state of a Progress row.
type RowState int

// Row states.
const (
	// RowRunning means the work of the row is in progress (spinner).
	RowRunning RowState = iota + 1
	// RowDone, like RowSkipped and RowFailed, is a final state.
	RowDone
	RowSkipped
	RowFailed
	// RowCancelled means the user declined the work of the row (e.g. a
	// removal not confirmed).
	RowCancelled
	// RowInterrupted means the work of the row was stopped, or never
	// started, because the command was interrupted: not a failure.
	RowInterrupted
	// RowWaiting means the work of the row waits (for a confirmation, or
	// for its turn): a static marker instead of the spinner; not finished,
	// it counts as running. An update without state makes it run again.
	RowWaiting
)

// pending reports whether the work of a row in state s is not finished.
func (s RowState) pending() bool { return s == RowRunning || s == RowWaiting }

// RowUpdate is a message creating or updating the row Key of a Progress.
// Label is used when the row is created. A zero State keeps the row's state
// (a new row starts running, a waiting one runs again); a non-empty Message replaces the row's
// message, as does any Message with RowInterrupted (the message of the
// running work does not describe the interrupted one). Aside rows are
// displayed faint and not counted with the others in the header (only
// apart, see WithAside) nor by the progress bars (e.g. the fetch preceding
// the fast-forwards of a repository).
type RowUpdate struct {
	Key, Label string
	State      RowState
	Message    string
	Aside      bool
}

// TitleMsg replaces the title of a Progress (e.g. "Checking …" becomes
// "Removing …" once the removal is confirmed).
type TitleMsg struct{ Title string }

// FinishMsg ends a Progress: it quits the program, and its final view is
// the title only, with how it ended and the elapsed time frozen: "<title>:
// finished in 1.2s", "interrupted after", or "cancelled after" when every
// row was cancelled, or a question was answered no before any row (aside
// rows apart) finished. It has no counts: the caller prints the summary of
// the results itself, which they would repeat or contradict. Pending
// questions are answered no.
type FinishMsg struct{}

// AskMsg makes a Progress display a yes/no question (see Confirm) below its
// rows; the answer is sent on Reply, which must have room for one value.
// Questions arriving while one is displayed are queued. ctrl+c answers no.
type AskMsg struct {
	Question string
	Details  []string
	Reply    chan<- bool
}

// PlannedMsg announces Units more rows to a Progress, aside rows excluded
// (see ops.Planner): the progress bars show the finished rows over the
// planned ones. Until the first one, the total is unknown.
type PlannedMsg struct{ Units int }

// AsidePlannedMsg announces Units more aside rows to a Progress (see
// ops.AsidePlanner): the header counts the finished ones over them (see
// WithAside).
type AsidePlannedMsg struct{ Units int }

// barMin and barMax bound the width of the progress bar of the header,
// percentage included.
const barMin, barMax = 12, 30

// InterruptedMsg tells a Progress that the command was interrupted Count
// times in all (signals included); the view shows the corresponding note.
// Counts lower than the presses already seen are ignored.
type InterruptedMsg struct{ Count int }

type progressRow struct {
	key, label string
	state      RowState
	message    string
	aside      bool
}

// Progress is a bubbletea model displaying one row per unit of work (a
// repository, a checkout) fed by RowUpdate messages: a spinner while
// running, then a final marker (✓ done, - skipped or cancelled, ✗ failed,
// ! interrupted) and a short message. The header shows the counts and the
// elapsed time, then, once the rows are planned (PlannedMsg), a progress
// bar.
//
// ctrl+c does not quit: each press runs the interrupt function given to
// WithInterrupt in a command (outside the event loop), and the view tells
// what happens (first press: no new work; second: forced stop). The caller
// ends the program with FinishMsg once the work has returned. Text is
// displayed with control characters replaced by U+FFFD.
type Progress struct {
	title string
	rows  []progressRow
	// index maps keys to indices into rows. Copies of the model share rows'
	// backing array and index, which only grow (an entry beyond len(rows)
	// belongs to a later copy): only the latest model may be updated, as
	// bubbletea does.
	index map[string]int

	asks []AskMsg // asks[0] is displayed
	ask  Confirm

	planned int // rows announced by PlannedMsg; 0: unknown

	asideName    string // see WithAside
	asidePlanned int    // aside rows announced by AsidePlannedMsg

	now          func() time.Time
	start, ended time.Time
	interrupt    func()
	interrupts   int
	declined     bool // a question was answered no (ctrl+c aside)
	finished     bool

	width, height    int
	profile          colorprofile.Profile
	terminalProgress bool

	spin   spinner.Model
	styles Styles
}

// NewProgress returns a Progress titled title; the elapsed time counts from
// now.
func NewProgress(title string) Progress {
	m := Progress{
		title:  title,
		index:  map[string]int{},
		now:    time.Now,
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		styles: DefaultStyles(),
	}
	m.start = m.now()
	return m
}

// WithClock returns a copy of m using now as its clock; the elapsed time
// counts from now().
func (m Progress) WithClock(now func() time.Time) Progress {
	m.now = now
	m.start = now()
	return m
}

// WithInterrupt returns a copy of m running f (in a command) on each ctrl+c
// pressed outside a question.
func (m Progress) WithInterrupt(f func()) Progress {
	m.interrupt = f
	return m
}

// WithTerminalProgress returns a copy of m driving the progress indicator
// of the terminal (tab, taskbar; see tea.View.ProgressBar) too: the ratio
// of the header bar, in the error state once a row failed, indeterminate
// until rows are planned. The final view has none, which makes bubbletea
// reset it, as it does when the program is killed.
func (m Progress) WithTerminalProgress() Progress {
	m.terminalProgress = true
	return m
}

// WithAside returns a copy of m whose header counts the aside rows apart,
// as name (their phase) followed by the finished ones over the planned ones
// (AsidePlannedMsg), before the counts of the other rows: "fetch 2/5";
// "fetch 2 done" when none were planned. The progress bars still count the
// other rows only (see PlannedMsg).
func (m Progress) WithAside(name string) Progress {
	m.asideName = name
	return m
}

// WithStyles returns a copy of m using st.
func (m Progress) WithStyles(st Styles) Progress {
	m.styles = st
	return m
}

// Init implements tea.Model: it starts the spinner.
func (m Progress) Init() tea.Cmd { return m.spin.Tick }

// Counts returns the number of rows, aside rows excluded, in each state
// but RowCancelled and RowInterrupted (which the header counts only when
// there are some).
func (m Progress) Counts() (running, done, skipped, failed int) {
	for _, r := range m.rows {
		if r.aside {
			continue
		}
		switch r.state {
		case RowRunning, RowWaiting:
			running++
		case RowDone:
			done++
		case RowSkipped:
			skipped++
		case RowFailed:
			failed++
		}
	}
	return
}

// Finished reports whether FinishMsg was received.
func (m Progress) Finished() bool { return m.finished }

// Interrupts returns the number of ctrl+c presses outside questions.
func (m Progress) Interrupts() int { return m.interrupts }

// Update implements tea.Model.
func (m Progress) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.finished {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.ColorProfileMsg:
		m.profile = msg.Profile
	case PlannedMsg:
		m.planned += msg.Units
	case AsidePlannedMsg:
		m.asidePlanned += msg.Units
	case InterruptedMsg:
		m.interrupts = max(m.interrupts, msg.Count)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case RowUpdate:
		m.apply(msg)
	case TitleMsg:
		m.title = msg.Title
	case AskMsg:
		m.asks = append(slices.Clip(m.asks), msg)
		if len(m.asks) == 1 {
			m.showAsk()
		}
	case FinishMsg:
		m.finished, m.ended = true, m.now()
		for _, a := range m.asks {
			reply(a, false)
		}
		m.asks = nil
		return m, tea.Quit
	case tea.KeyPressMsg:
		if len(m.asks) > 0 && msg.String() == "ctrl+c" {
			// No, sent after the interrupt function ran: the work sees the
			// cancellation together with the answer.
			a := m.asks[0]
			m.asks = m.asks[1:]
			if len(m.asks) > 0 {
				m.showAsk()
			}
			m.interrupts++
			f := m.interrupt
			return m, func() tea.Msg {
				if f != nil {
					f()
				}
				reply(a, false)
				return nil
			}
		}
		if len(m.asks) > 0 {
			return m.answer(msg), nil
		}
		if msg.String() == "ctrl+c" {
			m.interrupts++
			if f := m.interrupt; f != nil {
				return m, func() tea.Msg { f(); return nil }
			}
		}
	}
	return m, nil
}

func (m *Progress) apply(u RowUpdate) {
	i, ok := m.index[u.Key]
	if !ok || i >= len(m.rows) || m.rows[i].key != u.Key {
		state := u.State
		if state == 0 {
			state = RowRunning
		}
		m.index[u.Key] = len(m.rows)
		m.rows = append(m.rows, progressRow{key: u.Key, label: u.Label, state: state, message: u.Message, aside: u.Aside})
		return
	}
	r := &m.rows[i]
	switch {
	case u.State != 0:
		r.state = u.State
	case r.state == RowWaiting:
		r.state = RowRunning
	}
	if u.Message != "" || u.State == RowInterrupted {
		r.message = u.Message
	}
}

func (m *Progress) showAsk() {
	a := m.asks[0]
	m.ask = NewConfirm(a.Question, a.Details).WithStyles(m.styles)
}

func (m Progress) updateAsk(msg tea.Msg) (Confirm, tea.Cmd) {
	next, cmd := m.ask.Update(msg)
	return next.(Confirm), cmd
}

// answer routes a key to the displayed question.
func (m Progress) answer(k tea.KeyPressMsg) Progress {
	m.ask, _ = m.updateAsk(k)
	if !m.ask.Done() {
		return m
	}
	reply(m.asks[0], m.ask.Confirmed())
	m.declined = m.declined || !m.ask.Confirmed()
	m.asks = m.asks[1:]
	if len(m.asks) > 0 {
		m.showAsk()
	}
	return m
}

func reply(a AskMsg, yes bool) {
	select {
	case a.Reply <- yes:
	default:
	}
}

func (m Progress) elapsed() time.Duration {
	end := m.now()
	if m.finished {
		end = m.ended
	}
	return roundElapsed(end.Sub(m.start))
}

// roundElapsed rounds d for display: tenths of seconds under a
// minute, seconds above.
func roundElapsed(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// header returns the header: the title, then while running the aside
// counts (see WithAside) if aside, the counts of the other rows if counts,
// and the elapsed time; once finished, how it ended (see FinishMsg).
func (m Progress) header(aside, counted bool) string {
	running, done, skipped, failed := m.Counts()
	counts := fmt.Sprintf("%d done, %d skipped, %d failed", done, skipped, failed)
	cancelled, interrupted := 0, 0
	for _, r := range m.rows {
		switch {
		case r.aside:
		case r.state == RowCancelled:
			cancelled++
		case r.state == RowInterrupted:
			interrupted++
		}
	}
	if cancelled > 0 {
		counts += fmt.Sprintf(", %d cancelled", cancelled)
	}
	if interrupted > 0 {
		counts += fmt.Sprintf(", %d interrupted", interrupted)
	}
	if m.finished {
		end := "finished in"
		switch {
		case m.interrupts > 0:
			end = "interrupted after"
		case (cancelled > 0 || m.declined) && running+done+skipped+failed+interrupted == 0:
			end = "cancelled after"
		}
		return m.styles.Title.Render(sanitize(m.title)) + fmt.Sprintf(": %s %v", end, m.elapsed())
	}
	status := " · "
	if aside {
		status += m.asideCounts()
	}
	if counted {
		status += fmt.Sprintf("%d running, %s · ", running, counts)
	}
	return m.styles.Title.Render(sanitize(m.title)) + m.styles.Status.Render(status+fmt.Sprint(m.elapsed()))
}

// asideCounts returns the aside rows counted apart in the header, followed
// by its separator: "fetch 2/5 · " (see WithAside); "" without a name for
// them or without such rows, planned or not.
func (m Progress) asideCounts() string {
	finished, all := 0, 0
	for _, r := range m.rows {
		if r.aside {
			all++
			if !r.state.pending() {
				finished++
			}
		}
	}
	switch {
	case m.asideName == "" || all == 0 && m.asidePlanned <= 0:
		return ""
	case m.asidePlanned <= 0:
		return fmt.Sprintf("%s %d done · ", sanitize(m.asideName), finished)
	}
	return fmt.Sprintf("%s %d/%d · ", sanitize(m.asideName), finished, max(all, m.asidePlanned))
}

// ratio returns the finished rows (aside rows excluded) over the planned
// ones, at most 1, and false when no row was planned.
func (m Progress) ratio() (float64, bool) {
	if m.planned <= 0 {
		return 0, false
	}
	finished := 0
	for _, r := range m.rows {
		if !r.aside && !r.state.pending() {
			finished++
		}
	}
	return min(1, float64(finished)/float64(m.planned)), true
}

// headerLine returns the header followed, while running with planned rows,
// by a progress bar of the ratio, in the room left on the line (at most
// barMax columns); the bar is dropped when less than barMin are left. It
// is a gradient with 256 colors or more, else solid (full blocks). When
// the header itself is wider than the terminal, there is no bar, and its
// aside counts are dropped, then its other counts: the title and the
// elapsed time stay (the title is cut last, see truncate).
func (m Progress) headerLine() string {
	h := m.header(true, true)
	if m.width > 0 && lipgloss.Width(h) > m.width {
		// The bar went first.
		if h = m.header(false, true); lipgloss.Width(h) > m.width {
			h = m.header(false, false)
		}
		return h
	}
	r, ok := m.ratio()
	if !ok || m.finished {
		return h
	}
	w := barMax
	if m.width > 0 {
		w = min(w, m.width-lipgloss.Width(h)-2)
	}
	if w < barMin {
		return h
	}
	return h + "  " + m.bar(w, r)
}

// Colors of the progress bar of the header: those of bubbles' progress bar
// (its default blend and empty color).
var (
	barBlendStart = lipgloss.Color("#5A56E0")
	barBlendEnd   = lipgloss.Color("#EE6FF8")
	barEmpty      = lipgloss.Color("#606060")
)

// bar renders a progress bar of w columns at ratio r, the percentage
// included, as bubbles' progress.Model.ViewAs does (that package needs a
// module sheaf does not depend on): a gradient of half blocks with 256
// colors or more, else solid full blocks of a basic ANSI color.
func (m Progress) bar(w int, r float64) string {
	pct := fmt.Sprintf(" %3.0f%%", r*100)
	cells := max(0, w-len(pct))
	fill := min(cells, int(math.Round(float64(cells)*r)))
	var b strings.Builder
	if m.profile >= colorprofile.ANSI256 {
		blend := lipgloss.Blend1D(cells*2, barBlendStart, barBlendEnd)
		for i := range fill {
			b.WriteString(lipgloss.NewStyle().Foreground(blend[2*i]).Background(blend[2*i+1]).Render("▌"))
		}
	} else {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(strings.Repeat("█", fill)))
	}
	b.WriteString(lipgloss.NewStyle().Foreground(barEmpty).Render(strings.Repeat("░", cells-fill)))
	return b.String() + pct
}

// View implements tea.Model.
func (m Progress) View() tea.View {
	v := tea.NewView(m.render())
	if m.terminalProgress && !m.finished {
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, 0)
		if r, ok := m.ratio(); ok {
			state := tea.ProgressBarDefault
			if _, _, _, failed := m.Counts(); failed > 0 {
				state = tea.ProgressBarError
			}
			v.ProgressBar = tea.NewProgressBar(state, int(math.Round(r*100)))
		}
	}
	return v
}

func (m Progress) render() string {
	lines := []string{m.headerLine()}
	if m.finished {
		// bubbletea erases the last line of the final frame on exit: keep
		// the header above it.
		return m.truncate(lines) + "\n"
	}
	var footer []string
	switch {
	case m.interrupts == 1:
		footer = append(footer, m.styles.Question.Render("Interrupted: no new work is started; waiting for the running operations (Ctrl-C again to force)"))
	case m.interrupts > 1:
		footer = append(footer, m.styles.Question.Render("Interrupted again: forcing stop…"))
	}
	if len(m.asks) > 0 {
		// The dialog gets the height left by the header, the interruption
		// note and a line for the rows (or the hidden-rows note).
		h := 0
		if m.height > 0 {
			h = max(1, m.height-2-len(footer))
		}
		ask, _ := m.ask.Update(tea.WindowSizeMsg{Width: m.width, Height: h})
		footer = append(footer, strings.Split(ask.View().Content, "\n")...)
	}

	visible, gap := m.visibleRows(len(footer))
	if gap {
		lines = append(lines, "")
	}
	width := 0
	for _, i := range visible {
		width = max(width, lipgloss.Width(sanitize(m.rows[i].label)))
	}
	for _, i := range visible {
		lines = append(lines, m.renderRow(m.rows[i], width))
	}
	if hidden := len(m.rows) - len(visible); hidden > 0 {
		running := 0
		shown := map[int]bool{}
		for _, i := range visible {
			shown[i] = true
		}
		for i, r := range m.rows {
			if !shown[i] && r.state.pending() {
				running++
			}
		}
		note := fmt.Sprintf("… %d more finished", hidden)
		if running > 0 {
			note = fmt.Sprintf("… %d more (%d running)", hidden, running)
		}
		lines = append(lines, m.styles.Status.Render(note))
	}
	lines = append(lines, footer...)
	return m.truncate(lines)
}

// visibleRows returns the indices of the rows to render, in order, and
// whether a blank line separates them from the header (B41). When the
// height is known and too small, running rows are kept first, then the most
// recently added finished ones, leaving a line for the hidden-rows note.
// The blank line is kept unless dropping it lets every row fit, and dropped
// when no row would fit with it, like the gaps of the other views (see
// fitGaps); there is none without rows.
func (m Progress) visibleRows(footer int) ([]int, bool) {
	all := make([]int, len(m.rows))
	for i := range all {
		all[i] = i
	}
	gap := len(m.rows) > 0
	room := m.height - 1 - footer // header and footer
	switch {
	case m.height <= 0 || len(m.rows) <= room-1:
		return all, gap
	case len(m.rows) <= room:
		return all, false
	}
	room-- // the "… N more" line
	if gap = room > 1; gap {
		room--
	}
	keep := map[int]bool{}
	for i, r := range m.rows {
		if len(keep) < room && r.state.pending() {
			keep[i] = true
		}
	}
	for i := len(m.rows) - 1; i >= 0 && len(keep) < room; i-- {
		keep[i] = true
	}
	var out []int
	for _, i := range all {
		if keep[i] {
			out = append(out, i)
		}
	}
	return out, gap
}

func (m Progress) renderRow(r progressRow, width int) string {
	st := m.styles
	var marker, msg string
	switch r.state {
	case RowDone:
		marker, msg = st.Checked.Render("✓"), r.message
	case RowSkipped:
		marker, msg = st.Tag.Render("-"), "skipped"
		if r.message != "" {
			msg += ": " + r.message
		}
	case RowFailed:
		marker, msg = st.Failed.Render("✗"), "failed"
		if r.message != "" {
			msg += ": " + r.message
		}
	case RowCancelled:
		marker, msg = st.Tag.Render("-"), "cancelled"
	case RowWaiting:
		marker, msg = st.Tag.Render("·"), r.message
	case RowInterrupted:
		marker, msg = st.Warn.Render("!"), "interrupted"
		if r.message != "" {
			msg += ": " + r.message
		}
	default:
		marker, msg = m.spin.View(), r.message
	}
	label := sanitize(r.label)
	line := label + strings.Repeat(" ", max(0, width-lipgloss.Width(label)))
	if msg != "" {
		line += "  " + st.Detail.Render(sanitize(msg))
	}
	if r.aside {
		return st.Disabled.Render(marker + " " + line)
	}
	return marker + " " + line
}

func (m Progress) truncate(lines []string) string {
	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = trunc.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
