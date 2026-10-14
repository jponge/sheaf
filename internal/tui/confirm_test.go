package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func asConfirm(t *testing.T, m tea.Model) Confirm {
	t.Helper()
	c, ok := m.(Confirm)
	if !ok {
		t.Fatalf("model is %T, want Confirm", m)
	}
	return c
}

func TestConfirmKeys(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"y", true}, {"Y", true}, {"n", false}, {"N", false},
		{"enter", false}, // default is No
		{"esc", false}, {"q", false}, {"ctrl+c", false},
	}
	for _, tt := range tests {
		m, cmd := press(NewConfirm("Remove?", nil), tt.key)
		c := asConfirm(t, m)
		if !c.Done() || c.Confirmed() != tt.want || !isQuit(cmd) {
			t.Errorf("%s: Done=%v Confirmed=%v quit=%v, want true/%v/true", tt.key, c.Done(), c.Confirmed(), isQuit(cmd), tt.want)
		}
	}
}

func TestConfirmIgnoresOtherKeys(t *testing.T) {
	m, cmd := press(NewConfirm("Remove?", nil), "x", "space", "down")
	if c := asConfirm(t, m); c.Done() || c.Confirmed() || cmd != nil {
		t.Fatalf("Done=%v Confirmed=%v cmd=%v after unrelated keys", c.Done(), c.Confirmed(), cmd)
	}
}

func TestConfirmIgnoresKeysOnceDone(t *testing.T) {
	m, _ := press(NewConfirm("Remove?", nil), "n", "y")
	if asConfirm(t, m).Confirmed() {
		t.Fatal("y after n changed the answer")
	}
}

func TestConfirmView(t *testing.T) {
	v := NewConfirm("Remove 2 repositories?", []string{"boiler-vroom", "smallrye-config"}).View()
	for _, s := range []string{"Remove 2 repositories?", "boiler-vroom", "smallrye-config", "[y/N]"} {
		if !strings.Contains(v, s) {
			t.Fatalf("view lacks %q:\n%s", s, v)
		}
	}
	if strings.Index(v, "boiler-vroom") < strings.Index(v, "Remove 2") {
		t.Fatalf("details should follow the question:\n%s", v)
	}
}

func TestConfirmViewElidesDetailsToFitHeight(t *testing.T) {
	details := make([]string, 30)
	for i := range details {
		details[i] = fmt.Sprintf("repo%02d", i)
	}
	m, _ := NewConfirm("Remove?", details).Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 10 {
		t.Fatalf("view has %d lines, window height is 10:\n%s", n, v)
	}
	if !strings.Contains(v, "repo00") || strings.Contains(v, "repo29") || !strings.Contains(v, "more") {
		t.Fatalf("details not elided:\n%s", v)
	}
	if !strings.Contains(v, "[y/N]") {
		t.Fatalf("prompt lost when eliding:\n%s", v)
	}
}

func TestConfirmViewTruncatesToWidth(t *testing.T) {
	m, _ := NewConfirm(strings.Repeat("question ", 10), []string{strings.Repeat("detail ", 10)}).Update(tea.WindowSizeMsg{Width: 25, Height: 10})
	for _, l := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(l); w > 25 {
			t.Fatalf("line %q is %d cells wide, window is 25", l, w)
		}
	}
}

func TestConfirmViewStripsControlCharacters(t *testing.T) {
	v := NewConfirm("Remove\x1b[2J?\n", []string{"a\nb", "\x1b]8;;http://x\x07link\u009b"}).View()
	assertNoControlChars(t, v)
	if n := lineCount(v); n != 3 {
		t.Fatalf("view has %d lines, want 3:\n%s", n, v)
	}
}

func TestConfirmIgnoresMultiRuneAndPastedInput(t *testing.T) {
	for _, msg := range []tea.KeyMsg{keyMsg("yes"), keyMsg("yn"), paste("y")} {
		m, cmd := NewConfirm("Remove?", nil).Update(msg)
		if c := asConfirm(t, m); c.Done() || cmd != nil {
			t.Fatalf("%q: Done=%v cmd=%v, want ignored", msg.String(), c.Done(), cmd)
		}
	}
}

func TestConfirmHeightOneRendersOneLine(t *testing.T) {
	m, _ := NewConfirm("Remove?", []string{"a", "b"}).Update(tea.WindowSizeMsg{Width: 80, Height: 1})
	v := m.View()
	if n := lineCount(v); n != 1 || !strings.Contains(v, "[y/N]") {
		t.Fatalf("view has %d lines, want just the prompt:\n%s", n, v)
	}
}
