package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// RowState is the state of a Progress row.
type RowState int

const (
	// RowRunning: the work of the row is in progress (spinner).
	RowRunning RowState = iota + 1
	// RowDone, RowSkipped and RowFailed are final states.
	RowDone
	RowSkipped
	RowFailed
)

// RowUpdate is a message creating or updating the row Key of a Progress.
// Label is used when the row is created. A zero State keeps the row's state
// (a new row starts running); a non-empty Message replaces the row's
// message. Aside rows are displayed faint and not counted in the header
// (e.g. the fetch preceding the fast-forwards of a repository).
type RowUpdate struct {
	Key, Label string
	State      RowState
	Message    string
	Aside      bool
}

// FinishMsg ends a Progress: it quits the program, and its final view is
// the header only, with the elapsed time frozen (the caller prints the
// detailed summary itself). Pending questions are answered no.
type FinishMsg struct{}

// AskMsg makes a Progress display a yes/no question (see Confirm) below its
// rows; the answer is sent on Reply, which must have room for one value.
// Questions arriving while one is displayed are queued. ctrl+c answers no.
type AskMsg struct {
	Question string
	Details  []string
	Reply    chan<- bool
}

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
// running, then a final marker (✓ done, - skipped, ✗ failed) and a short
// message. The header shows the counts and the elapsed time.
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

	now          func() time.Time
	start, ended time.Time
	interrupt    func()
	interrupts   int
	finished     bool

	width, height int

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

// WithStyles returns a copy of m using st.
func (m Progress) WithStyles(st Styles) Progress {
	m.styles = st
	return m
}

// Init implements tea.Model: it starts the spinner.
func (m Progress) Init() tea.Cmd { return m.spin.Tick }

// Counts returns the number of rows, aside rows excluded, in each state.
func (m Progress) Counts() (running, done, skipped, failed int) {
	for _, r := range m.rows {
		if r.aside {
			continue
		}
		switch r.state {
		case RowRunning:
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
	case InterruptedMsg:
		m.interrupts = max(m.interrupts, msg.Count)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case RowUpdate:
		m.apply(msg)
	case AskMsg:
		m.asks = append(m.asks[:len(m.asks):len(m.asks)], msg)
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
	case tea.KeyMsg:
		if len(m.asks) > 0 && msg.Type == tea.KeyCtrlC {
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
		if msg.Type == tea.KeyCtrlC {
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
	if u.State != 0 {
		r.state = u.State
	}
	if u.Message != "" {
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
func (m Progress) answer(k tea.KeyMsg) Progress {
	m.ask, _ = m.updateAsk(k)
	if !m.ask.Done() {
		return m
	}
	reply(m.asks[0], m.ask.Confirmed())
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

func (m Progress) header() string {
	running, done, skipped, failed := m.Counts()
	if m.finished {
		return m.styles.Title.Render(sanitize(m.title)) +
			fmt.Sprintf(": %d done, %d skipped, %d failed in %v", done, skipped, failed, m.elapsed())
	}
	return m.styles.Title.Render(sanitize(m.title)) + m.styles.Status.Render(
		fmt.Sprintf(" · %d running, %d done, %d skipped, %d failed · %v", running, done, skipped, failed, m.elapsed()))
}

// View implements tea.Model.
func (m Progress) View() string {
	lines := []string{m.header()}
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
		footer = append(footer, strings.Split(ask.View(), "\n")...)
	}

	visible := m.visibleRows(len(footer))
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
			if !shown[i] && r.state == RowRunning {
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

// visibleRows returns the indices of the rows to render, in order. When the
// height is known and too small, running rows are kept first, then the most
// recently added finished ones, leaving a line for the hidden-rows note.
func (m Progress) visibleRows(footer int) []int {
	all := make([]int, len(m.rows))
	for i := range all {
		all[i] = i
	}
	room := m.height - 1 - footer // header and footer
	if m.height <= 0 || len(m.rows) <= room {
		return all
	}
	room-- // the "… N more" line
	keep := map[int]bool{}
	for i, r := range m.rows {
		if len(keep) < room && r.state == RowRunning {
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
	return out
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
