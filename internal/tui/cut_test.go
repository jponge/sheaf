package tui

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// longText returns a text of n cells of words, without runs of a repeated
// character.
func longText(prefix string, n int) string {
	s := prefix
	for i := 0; lipgloss.Width(s) < n; i++ {
		s += " w" + string(rune('a'+i%26)) + "x"
	}
	return s
}

// wrap never makes a line wider than the width, wide characters included:
// the indentation of the first line is kept before a broken word only if
// the word's first character fits after it (V3 review: "   日" at width
// 4), and the text is kept, in order, as a prefix-preserving split.
func TestWrapFitsWideCharactersAfterTheIndentation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s     string
		width int
	}{
		{"   日本語のテキスト", 4},
		{"      " + strings.Repeat("長", 10), 7},
		{"  " + strings.Repeat("長", 10) + " x", 3},
		{"  abcdefghij", 3},
	} {
		lines := wrap(c.s, c.width)
		for _, l := range lines {
			if lipgloss.Width(l) > c.width {
				t.Errorf("wrap(%q, %d): line %q of width %d", c.s, c.width, l, lipgloss.Width(l))
			}
		}
		if got := strings.Join(strings.Fields(strings.Join(lines, " ")), ""); got != strings.Join(strings.Fields(c.s), "") {
			t.Errorf("wrap(%q, %d) = %q: text not kept", c.s, c.width, lines)
		}
		if !strings.HasPrefix(c.s, lines[0]) {
			t.Errorf("wrap(%q, %d): first line %q is not a prefix", c.s, c.width, lines[0])
		}
	}
}

// A line of a view wider than the window is cut at its width and ends with
// "…" (B52 E2: the title, the overview message and STATE were cut without
// it), at every width from 20 to 200 columns, in every view. The overview
// message wraps to a second line first (B62 M6), which is the one cut.
func TestViewsEndCutLinesWithEllipsis(t *testing.T) {
	t.Parallel()
	title := longText("sheaf: /bundle", 260)
	state := longText("error: boom", 260)
	msg := longText("wt add failed:", 260)
	views := map[string]tea.Model{
		"overview": NewOverview(title, []string{"REPO", "STATE"}, []Row{{Key: "a", Cells: []string{"alpha", state}, Alert: true}}, nil).WithMessage(msg),
		"report":   NewReport(title, []string{"line"}, "back"),
		"progress": NewProgress(title),
		"menu":     NewMultiSelect(title, []Item{{Key: "a", Label: "alpha"}}),
		"confirm":  NewConfirm(title, nil),
		"prompt":   NewPrompt(title, []string{"branch"}),
	}
	for name, m0 := range views {
	widths:
		for w := 20; w <= 200; w++ {
			m, _ := m0.Update(tea.WindowSizeMsg{Width: w, Height: 40})
			lines := strings.Split(view(m), "\n")
			for _, l := range lines {
				if lipgloss.Width(l) > w {
					t.Errorf("%s, width %d: line of width %d: %q", name, w, lipgloss.Width(l), l)
					break widths
				}
			}
			cut := map[string]string{"title": title}
			if name == "overview" {
				cut["STATE"] = state
				// The message: on the status line and the next one, whole
				// or cut at the end of the second line.
				for i, l := range lines {
					if !strings.HasPrefix(l, "0/1 selected · wt") || i+1 >= len(lines) {
						continue
					}
					next := lines[i+1]
					if strings.Join(strings.Fields(l+" "+next), " ") == "0/1 selected · "+msg {
						continue // whole
					}
					if strings.HasSuffix(l, "…") || lipgloss.Width(next) != w || !strings.HasSuffix(next, "…") {
						t.Errorf("%s, width %d: the message is not cut at the end of its second line: %q, %q", name, w, l, next)
						break widths
					}
				}
			}
			for what, text := range cut {
				// The line of text: the one containing its start (words
				// of 3 cells: at least the first word fits at 20).
				start := strings.SplitN(text, " ", 2)[0]
				found := false
				for _, l := range lines {
					if !strings.Contains(l, start) {
						continue
					}
					found = true
					if lipgloss.Width(l) != w || !strings.HasSuffix(l, "…") {
						t.Errorf("%s, width %d: the %s is cut without an ellipsis: %q", name, w, what, l)
						break widths
					}
				}
				if !found {
					t.Errorf("%s, width %d: no %s line:\n%s", name, w, what, view(m))
					break widths
				}
			}
		}
	}
}

// A title too wide for the window loses the start of its path (see
// WithTitlePath), not its end, so that the bundle name stays shown (B62
// L3): at a separator when one fits ("sheaf: …/bundle/work"), else within
// the last component; a title that fits is unchanged.
func TestTitlesCutTheirPathFromTheLeft(t *testing.T) {
	t.Parallel()
	const title = "sheaf: /tmp/claude/long/bundle/work"
	for _, c := range []struct {
		width int
		want  string
	}{
		{80, title},
		{len(title), title},
		{len(title) - 1, "sheaf: …/claude/long/bundle/work"},
		{20, "sheaf: …/bundle/work"},
		{19, "sheaf: …/work"},
		{13, "sheaf: …/work"},
		{11, "sheaf: …ork"},
	} {
		for name, m := range titleViews(title, "/tmp/claude/long/bundle/work") {
			m, _ = m.Update(tea.WindowSizeMsg{Width: c.width, Height: 20})
			if got := strings.Split(view(m), "\n")[0]; got != c.want {
				t.Errorf("%s, width %d: title %q, want %q", name, c.width, got, c.want)
			}
		}
	}
}

// titleViews returns an overview and a report titled title, ending with
// path (see WithTitlePath).
func titleViews(title, path string) map[string]tea.Model {
	return map[string]tea.Model{
		"overview": NewOverview(title, []string{"REPO"}, []Row{{Key: "a", Cells: []string{"a"}}}, nil).WithTitlePath(path),
		"report":   NewReport(title, []string{"x"}, "back").WithTitlePath(path),
	}
}

// The cut of a title path counts the width of grapheme clusters, as the
// terminal does, and never splits one: an emoji with its variation
// selector, a letter with its combining accent (V4 review: the title was
// up to 13 columns too wide, then cut at its end, and could start with a
// lone selector or accent). It splits the title before the path it was
// given, even when the path contains ": ".
func TestTitlePathCutKeepsGraphemeClusters(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ head, path string }{
		{"sheaf: ", "/tmp/❤️❤️❤️❤️/❤️❤️❤️"},
		{"sheaf: ", "/tmp/ééééé/éééé"},
		{"sheaf wt rm: ", "/tmp/a: b/c: d/work"},
	} {
		title := c.head + c.path
		for w := 1; w <= lipgloss.Width(title)+2; w++ {
			for name, m := range titleViews(title, c.path) {
				m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 20})
				got := strings.Split(view(m), "\n")[0]
				switch {
				case lipgloss.Width(got) > w:
					t.Errorf("%s, %q, width %d: title %q of width %d", name, title, w, got, lipgloss.Width(got))
				case lipgloss.Width(title) <= w:
					if got != title {
						t.Errorf("%s, %q, width %d: title %q, want it whole", name, title, w, got)
					}
				case w >= lipgloss.Width(c.head)+3: // room for "…" and a cluster of 2 columns
					tail, ok := strings.CutPrefix(got, c.head+"…")
					first, _ := utf8.DecodeRuneInString(tail)
					if !ok || tail == "" || !strings.HasSuffix(c.path, tail) || unicode.Is(unicode.Mn, first) {
						t.Errorf("%s, %q, width %d: title %q, want %q, \"…\" and the end of the path, whole graphemes", name, title, w, got, c.head)
					}
				}
			}
		}
	}
}
