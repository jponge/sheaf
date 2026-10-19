package tui

import (
	"strings"
	"testing"

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

// A line of a view wider than the window is cut at its width and ends with
// "…" (B52 E2: the title, the overview message and STATE were cut without
// it), at every width from 20 to 200 columns, in every view.
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
				cut["STATE"], cut["message"] = state, msg
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
