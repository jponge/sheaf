package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Confirm is a bubbletea model asking a yes/no question, followed by detail
// lines (e.g. what will be removed). y confirms; n, enter (the default, No),
// esc, q and ctrl+c decline. Multi-rune and pasted input is ignored. Update
// returns tea.Quit once answered; later messages are ignored. The question and
// details are displayed with control characters replaced by U+FFFD.
type Confirm struct {
	question string
	details  []string

	width, height int // 0 = unknown

	done, confirmed, interrupted bool
	styles                       Styles
}

// NewConfirm returns a Confirm asking question, followed by details lines.
func NewConfirm(question string, details []string) Confirm {
	return Confirm{
		question: question,
		details:  append([]string(nil), details...),
		styles:   DefaultStyles(),
	}
}

// WithStyles returns a copy of m using st.
func (m Confirm) WithStyles(st Styles) Confirm {
	m.styles = st
	return m
}

// Init implements tea.Model.
func (m Confirm) Init() tea.Cmd { return nil }

// Confirmed reports whether the user answered yes.
func (m Confirm) Confirmed() bool { return m.confirmed }

// Interrupted reports whether the dialog was declined with ctrl+c, which
// callers treat as an interruption of the command rather than a plain no.
func (m Confirm) Interrupted() bool { return m.interrupted }

// Done reports whether the user answered (yes or no).
func (m Confirm) Done() bool { return m.done }

// Update implements tea.Model.
func (m Confirm) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "y", "Y":
			m.done, m.confirmed = true, true
			return m, tea.Quit
		case "ctrl+c":
			m.done, m.interrupted = true, true
			return m, tea.Quit
		case "n", "N", "enter", "esc", "q":
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m Confirm) View() tea.View {
	st := m.styles
	lines := []string{st.Question.Render(sanitize(m.question)) + " " + st.Status.Render("[y/N]")}

	// Details are wrapped to the width (first line indented by 2, the
	// following ones by 4); the elision below counts wrapped lines.
	var details []string
	for _, d := range m.details {
		for i, l := range wrap(sanitize(d), m.width-4) {
			if i == 0 {
				details = append(details, "  "+l)
			} else {
				details = append(details, "    "+l)
			}
		}
	}
	switch {
	case m.height == 1:
		details = nil
	case m.height > 0 && 1+len(details) > m.height:
		room := m.height - 2 // question + "… and K more"
		hidden := len(details) - room
		details = append(details[:room:room], fmt.Sprintf("  … and %d more", hidden))
	}
	lines = append(lines, details...)

	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = trunc.Render(l)
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}
