package tui

import (
	"os"
	"slices"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestMain forces lipgloss to emit no ANSI sequences so that assertions on
// View() are independent of the terminal running the tests.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

var namedKeys = map[string]tea.KeyType{
	"up": tea.KeyUp, "down": tea.KeyDown, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
	"home": tea.KeyHome, "end": tea.KeyEnd, "space": tea.KeySpace, "enter": tea.KeyEnter,
	"esc": tea.KeyEsc, "ctrl+c": tea.KeyCtrlC, "backspace": tea.KeyBackspace,
	"ctrl+u": tea.KeyCtrlU, "ctrl+w": tea.KeyCtrlW,
}

// keyMsg builds the KeyMsg bubbletea produces for a named key, or for the
// literal runes of s otherwise.
func keyMsg(s string) tea.KeyMsg {
	if k, ok := namedKeys[s]; ok {
		if k == tea.KeySpace {
			return tea.KeyMsg{Type: k, Runes: []rune{' '}}
		}
		return tea.KeyMsg{Type: k}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press feeds keys one by one and returns the final model and the last command.
func press(m tea.Model, keys ...string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(keyMsg(k))
	}
	return m, cmd
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// paste builds the KeyMsg bubbletea produces for a bracketed paste of s.
func paste(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

// assertNoControlChars fails when v contains a control character other than
// the newlines separating View lines.
func assertNoControlChars(t *testing.T, v string) {
	t.Helper()
	for _, r := range v {
		if r != '\n' && unicode.IsControl(r) {
			t.Fatalf("view contains control character %U:\n%q", r, v)
		}
	}
}

func lineCount(v string) int { return strings.Count(v, "\n") + 1 }

// hasLine reports whether v has a line equal to want.
func hasLine(v, want string) bool {
	return slices.Contains(strings.Split(v, "\n"), want)
}
