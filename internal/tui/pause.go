package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// PauseGrace is the time during which a Pause ignores key presses (ctrl+c
// excepted), counted from NewPause.
const PauseGrace = 250 * time.Millisecond

// Pause is a bubbletea model displaying a line of text (e.g. "press any key
// to continue") until a key is pressed: any key ends it, ctrl+c as an
// interruption. Keys pressed during the first PauseGrace are ignored
// (ctrl+c excepted), so that a key typed ahead, e.g. while the work that
// precedes the pause ran, does not end it before it can be read. Pastes
// and other messages are ignored. Update returns tea.Quit once a key is
// pressed; later messages are ignored and the view is then empty, so that
// the program leaves nothing in the terminal. The text is displayed with
// control characters replaced by U+FFFD.
type Pause struct {
	text  string
	now   func() time.Time
	start time.Time

	done, interrupted bool
	styles            Styles
}

// NewPause returns a Pause displaying text.
func NewPause(text string) Pause {
	m := Pause{text: text, now: time.Now, styles: DefaultStyles()}
	m.start = m.now()
	return m
}

// WithClock returns a copy of m using now as its clock; the grace period
// counts from now().
func (m Pause) WithClock(now func() time.Time) Pause {
	m.now = now
	m.start = now()
	return m
}

// Init implements tea.Model.
func (m Pause) Init() tea.Cmd { return nil }

// Done reports whether a key was pressed. A model whose program ended for
// another reason (e.g. input closed) is not done.
func (m Pause) Done() bool { return m.done }

// Interrupted reports whether the key was ctrl+c, which callers treat as an
// interruption.
func (m Pause) Interrupted() bool { return m.interrupted }

// Update implements tea.Model.
func (m Pause) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		ctrlC := k.String() == "ctrl+c"
		if !ctrlC && m.now().Sub(m.start) < PauseGrace {
			return m, nil
		}
		m.done, m.interrupted = true, ctrlC
		return m, tea.Quit
	}
	return m, nil
}

// View implements tea.Model.
func (m Pause) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	return tea.NewView(m.styles.Status.Render(sanitize(m.text)))
}
