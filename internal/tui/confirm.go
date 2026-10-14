package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

	done, confirmed bool
	styles          Styles
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
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			m.done, m.confirmed = true, true
			return m, tea.Quit
		case "n", "N", "enter", "esc", "q", "ctrl+c":
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m Confirm) View() string {
	st := m.styles
	lines := []string{st.Question.Render(sanitize(m.question)) + " " + st.Status.Render("[y/N]")}

	details := m.details
	switch {
	case m.height == 1:
		details = nil
	case m.height > 0 && 1+len(details) > m.height:
		room := m.height - 2 // question + "… and K more"
		hidden := len(details) - room
		details = append(details[:room:room], fmt.Sprintf("… and %d more", hidden))
	}
	for _, d := range details {
		lines = append(lines, "  "+sanitize(d))
	}

	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = trunc.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
