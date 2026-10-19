package tui

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// sgr matches the SGR sequences (colors, text attributes) of lipgloss styles.
var sgr = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// view returns the content of m's view without the SGR sequences of its
// styles, so that assertions are independent of the styles, like the
// Ascii color profile of lipgloss v1 (which rendered no SGR) made them.
// Other escape sequences and control characters are kept.
func view(m tea.Model) string { return sgr.ReplaceAllString(m.View().Content, "") }

var namedKeys = map[string]tea.Key{
	"up": {Code: tea.KeyUp}, "down": {Code: tea.KeyDown}, "pgup": {Code: tea.KeyPgUp},
	"pgdown": {Code: tea.KeyPgDown}, "home": {Code: tea.KeyHome}, "end": {Code: tea.KeyEnd},
	"space": {Code: tea.KeySpace, Text: " "}, "enter": {Code: tea.KeyEnter},
	"esc": {Code: tea.KeyEscape}, "ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
	"backspace": {Code: tea.KeyBackspace}, "tab": {Code: tea.KeyTab},
	"shift+g": {Code: 'g', Mod: tea.ModShift},
	"ctrl+u":  {Code: 'u', Mod: tea.ModCtrl}, "ctrl+w": {Code: 'w', Mod: tea.ModCtrl},
}

// keyMsg builds the KeyPressMsg bubbletea produces for a named key, or a
// key event typing the literal runes of s otherwise (bubbletea produces one
// per rune, or per grapheme of several runes).
func keyMsg(s string) tea.KeyPressMsg {
	if k, ok := namedKeys[s]; ok {
		return tea.KeyPressMsg(k)
	}
	if r := []rune(s); len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: s}
	}
	return tea.KeyPressMsg{Code: tea.KeyExtended, Text: s}
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

// paste builds the message bubbletea produces for a bracketed paste of s.
func paste(s string) tea.PasteMsg { return tea.PasteMsg{Content: s} }

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

// injectedSGR is an SGR sequence that the styles of this package never
// produce (256-color foreground): view strips SGR sequences, so the tests of
// sanitization inject it into caller text and check the raw view content.
const injectedSGR = "\x1b[38;5;201m"

// assertNoInjectedSGR fails when the raw content of m's view contains
// injectedSGR.
func assertNoInjectedSGR(t *testing.T, m tea.Model) {
	t.Helper()
	if raw := m.View().Content; strings.Contains(raw, injectedSGR) {
		t.Fatalf("raw view contains the injected SGR sequence:\n%q", raw)
	}
}

func lineCount(v string) int { return strings.Count(v, "\n") + 1 }

// hasLine reports whether v has a line equal to want.
// tableLine matches the header and the rows of the table of newOv: cut
// with "…" when wider than the window (B52 E2), unlike the key help.
var tableLine = regexp.MustCompile(`^(      REPO|[> ] \[[ x]\] )`)

func isTableLine(l string) bool { return tableLine.MatchString(l) }

func hasLine(v, want string) bool {
	return slices.Contains(strings.Split(v, "\n"), want)
}

// shape returns the classes of the lines of v (see classify), separated by
// spaces, e.g. "T R R _ S _ K"; blank lines are "_".
func shape(v string, classify func(line string) string) string {
	var c []string
	for _, l := range strings.Split(v, "\n") {
		if l == "" {
			c = append(c, "_")
		} else {
			c = append(c, classify(l))
		}
	}
	return strings.Join(c, " ")
}
