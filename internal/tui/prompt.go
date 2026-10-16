package tui

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Prompt is a bubbletea model asking for one line of text, in one of
// several modes (e.g. "branch" or "tag") cycled with tab.
//
// Typed or pasted text (without control characters) is appended;
// backspace deletes a rune, ctrl+w a word and ctrl+u everything (as the
// filter of MultiSelect). enter confirms a value that is not blank; esc
// and ctrl+c cancel. Update returns tea.Quit once answered; later messages
// are ignored. The title is displayed with control characters replaced by
// U+FFFD.
type Prompt struct {
	title string
	modes []string
	mode  int
	value string
	width int // 0 = unknown

	confirmed, cancelled, interrupted bool

	keys   promptKeys
	help   help.Model
	styles Styles
}

type promptKeys struct{ Confirm, Mode, Cancel key.Binding }

// NewPrompt returns a Prompt titled title, starting in the first of modes
// (at least one).
func NewPrompt(title string, modes []string) Prompt {
	h := help.New()
	st := DefaultStyles()
	h.Styles = st.Help
	modes = slices.Clone(modes)
	if len(modes) == 0 {
		modes = []string{"value"}
	}
	k := promptKeys{
		Confirm: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		Mode:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", strings.Join(modes, "/"))),
		Cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
	return Prompt{title: title, modes: modes, keys: k, help: h, styles: st}
}

// Init implements tea.Model.
func (m Prompt) Init() tea.Cmd { return nil }

// Value returns the text entered.
func (m Prompt) Value() string { return m.value }

// Mode returns the current mode.
func (m Prompt) Mode() string { return m.modes[m.mode] }

// Confirmed reports whether the text was confirmed with enter.
func (m Prompt) Confirmed() bool { return m.confirmed }

// Cancelled reports whether the prompt was cancelled (esc, ctrl+c). A model
// whose program ended for another reason (e.g. input closed) is neither
// confirmed nor cancelled.
func (m Prompt) Cancelled() bool { return m.cancelled }

// Interrupted reports whether the prompt was cancelled with ctrl+c, which
// callers treat as an interruption rather than a plain cancellation.
func (m Prompt) Interrupted() bool { return m.interrupted }

// Update implements tea.Model.
func (m Prompt) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.confirmed || m.cancelled {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.help.SetWidth(msg.Width)
	case tea.PasteMsg:
		m.value += dropControl(msg.Content)
	case tea.KeyPressMsg:
		switch s := msg.String(); {
		case s == "ctrl+c":
			m.cancelled, m.interrupted = true, true
			return m, tea.Quit
		case key.Matches(msg, m.keys.Cancel):
			m.cancelled = true
			return m, tea.Quit
		case key.Matches(msg, m.keys.Confirm):
			if strings.TrimSpace(m.value) != "" {
				m.confirmed = true
				return m, tea.Quit
			}
		case key.Matches(msg, m.keys.Mode):
			m.mode = (m.mode + 1) % len(m.modes)
		case s == "ctrl+u":
			m.value = ""
		case s == "ctrl+w":
			m.value = deleteLastWord(m.value)
		case s == "backspace":
			if r := []rune(m.value); len(r) > 0 {
				m.value = string(r[:len(r)-1])
			}
		case typedText(msg) != "":
			m.value += dropControl(typedText(msg))
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m Prompt) View() tea.View {
	st := m.styles
	input := st.FilterPrompt.Render(sanitize(m.Mode())+":") + " " + m.value
	if !m.confirmed && !m.cancelled {
		input += "█"
	}
	lines := []string{st.Title.Render(sanitize(m.title)), input}
	if !m.confirmed && !m.cancelled {
		bindings := []key.Binding{m.keys.Confirm}
		if len(m.modes) > 1 {
			bindings = append(bindings, m.keys.Mode)
		}
		lines = append(lines, m.help.ShortHelpView(append(bindings, m.keys.Cancel)))
	}
	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = trunc.Render(l)
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}
