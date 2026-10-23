package tui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// osc8 matches the hyperlink sequences (OSC 8, ended by BEL or ST); the
// URL is the third group, empty for the sequence that ends a link.
var osc8 = regexp.MustCompile("\x1b\\]8;([^;\x07\x1b]*);([^\x07\x1b]*)(?:\x07|\x1b\\\\)")

// stripOSC8 removes the hyperlink sequences of s.
func stripOSC8(s string) string { return osc8.ReplaceAllString(s, "") }

// linked returns the text of s between the start of a hyperlink to url
// and its end, for each such link.
func linked(s, url string) []string {
	var out []string
	open := ""
	start := 0
	for _, m := range osc8.FindAllStringSubmatchIndex(s, -1) {
		u := s[m[4]:m[5]]
		if open == url && u == "" {
			out = append(out, s[start:m[0]])
		}
		open, start = u, m[1]
	}
	return out
}

// assertLinksClosed fails unless every hyperlink opened on each line of v
// is ended on that line, before the next one starts.
func assertLinksClosed(t *testing.T, v, context string) {
	t.Helper()
	for i, l := range strings.Split(v, "\n") {
		open := false
		for _, m := range osc8.FindAllStringSubmatch(l, -1) {
			if m[2] != "" && open {
				t.Fatalf("%s: line %d opens a hyperlink inside another: %q", context, i, l)
			}
			open = m[2] != ""
		}
		if open {
			t.Fatalf("%s: line %d leaves a hyperlink open: %q", context, i, l)
		}
	}
}

func linkRows() []Row {
	return []Row{
		{Key: "1", Cells: []string{"alpha", "main", "main", "clean"}, Links: []Link{{Cell: 0, Text: "alpha", URL: "https://github.com/o/alpha"}}},
		{Key: "2", Cells: []string{"alpha", "pr-7", "pr/7", "clean, PR #7 ↑1"}, Links: []Link{
			{Cell: 0, Text: "alpha", URL: "https://github.com/o/alpha"},
			{Cell: 3, Text: "PR #7", URL: "https://github.com/o/alpha/pull/7"},
		}},
		{Key: "3", Cells: []string{"bravo", "main", "main", "dirty"}},
	}
}

func newLinkOv(rows []Row) Overview {
	return NewOverview("t", []string{"REPO", "DIR", "BRANCH", "STATE"}, rows, nil).WithGroupFaint()
}

// With hyperlinks, the cells of the links (B46) are OSC 8 hyperlinks to
// their URL: the repository name on every row of its group (faint or not,
// B68), "PR #7" in STATE; the text is unchanged.
// Without, no hyperlink is rendered.
func TestOverviewRendersHyperlinks(t *testing.T) {
	t.Parallel()
	plain := newLinkOv(linkRows())
	if raw := plain.View().Content; osc8.MatchString(raw) {
		t.Fatalf("hyperlinks without WithHyperlinks:\n%q", raw)
	}
	m := plain.WithHyperlinks()
	raw := m.View().Content
	if got := linked(raw, "https://github.com/o/alpha"); len(got) != 2 || sgr.ReplaceAllString(got[0], "") != "alpha" || sgr.ReplaceAllString(got[1], "") != "alpha" {
		t.Errorf("repository links %q, want two on alpha:\n%q", got, raw)
	}
	if got := linked(raw, "https://github.com/o/alpha/pull/7"); len(got) != 1 || sgr.ReplaceAllString(got[0], "") != "PR #7" {
		t.Errorf("pull request links %q, want one on PR #7:\n%q", got, raw)
	}
	if strings.Contains(raw, "bravo\x1b]8") || len(osc8.FindAllString(raw, -1)) != 6 {
		t.Errorf("want 3 hyperlinks (6 sequences):\n%q", raw)
	}
	if stripOSC8(raw) != plain.View().Content {
		t.Errorf("text changed by the hyperlinks:\n%q\nwant\n%q", stripOSC8(raw), plain.View().Content)
	}
	assertLinksClosed(t, raw, "default size")
	// The first row on screen shows its group's name, linked.
	var sm tea.Model = m
	sm, _ = sm.Update(tea.WindowSizeMsg{Width: 80, Height: 5}) // 1 row
	sm, _ = press(sm, "down")
	if raw := sm.View().Content; len(linked(raw, "https://github.com/o/alpha")) != 1 {
		t.Errorf("scrolled to the second row: repository link missing:\n%q", raw)
	}
}

// A URL that is not printable ASCII without spaces (e.g. a slug with an
// escape sequence) is not linked; nor is a link whose text is not in its
// cell, or whose cell does not exist.
func TestOverviewRefusesUnsafeHyperlinks(t *testing.T) {
	t.Parallel()
	for _, l := range []Link{
		{Cell: 0, Text: "alpha", URL: "https://github.com/o/\x1b[31malpha"},
		{Cell: 0, Text: "alpha", URL: "https://github.com/o/al\apha"},
		{Cell: 0, Text: "alpha", URL: "https://github.com/o/al pha"},
		{Cell: 0, Text: "alpha", URL: "https://github.com/o/alphà"},
		{Cell: 0, Text: "alpha", URL: ""},
		{Cell: 0, Text: "", URL: "https://github.com/o/alpha"},
		{Cell: 0, Text: "beta", URL: "https://github.com/o/alpha"},
		{Cell: 9, Text: "alpha", URL: "https://github.com/o/alpha"},
		{Cell: -1, Text: "alpha", URL: "https://github.com/o/alpha"},
	} {
		rows := []Row{{Key: "1", Cells: []string{"alpha", "main"}, Links: []Link{l}}}
		if raw := NewOverview("t", []string{"REPO", "DIR"}, rows, nil).WithHyperlinks().View().Content; osc8.MatchString(raw) || strings.Contains(raw, "\x1b[31m") || strings.ContainsRune(raw, '\a') {
			t.Errorf("link %#v rendered:\n%q", l, raw)
		}
	}
}

// Truncated cells and cut lines keep every hyperlink ended on its line, at
// every width, with the cursor and the selection on linked rows; links do
// not change the measured width.
func TestOverviewHyperlinksSurviveTruncation(t *testing.T) {
	t.Parallel()
	rows := linkRows()
	rows[1].Cells[1] = "a-very-long-directory-name"
	rows[1].Cells[2] = "a-very-long-branch-name"
	const dirURL = "https://example.com/dir"
	rows[1].Links = append(rows[1].Links, Link{Cell: 1, Text: "a-very-long-directory-name", URL: dirURL})
	for width := 10; width <= 200; width++ {
		for _, cursor := range []string{"1", "2"} {
			base := NewOverview("t", []string{"REPO", "DIR", "BRANCH", "STATE"}, rows, nil).WithGroupFaint().WithShrinkable(2, 1).WithCursor(cursor).WithSelected([]string{"2"})
			var plain, m tea.Model = base, base.WithHyperlinks()
			plain, _ = plain.Update(tea.WindowSizeMsg{Width: width, Height: 30})
			m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
			raw := m.View().Content
			assertLinksClosed(t, raw, "width "+strconv.Itoa(width))
			if stripOSC8(raw) != plain.View().Content {
				t.Fatalf("width %d: text changed by the hyperlinks:\n%q\nwant\n%q", width, stripOSC8(raw), plain.View().Content)
			}
			for _, l := range strings.Split(raw, "\n") {
				if w := lipgloss.Width(l); w > width {
					t.Fatalf("width %d: line of width %d: %q", width, w, l)
				}
				// A truncated cell keeps its link, on the text shown.
				if text := sgr.ReplaceAllString(stripOSC8(l), ""); strings.Contains(text, "a-very") {
					if got := linked(l, dirURL); len(got) != 1 || !strings.Contains(text, sgr.ReplaceAllString(got[0], "")) || !strings.HasPrefix(sgr.ReplaceAllString(got[0], ""), "a-very") {
						t.Fatalf("width %d: links of the truncated directory %q in %q", width, got, l)
					}
				}
			}
		}
	}
}

// A cell has one hyperlink, the first Link of the cell: a second one would
// find its text in the sequence of the first.
func TestOverviewLinksOnePerCell(t *testing.T) {
	t.Parallel()
	rows := []Row{{Key: "1", Cells: []string{"alpha", "main"}, Links: []Link{
		{Cell: 0, Text: "alpha", URL: "https://github.com/o/alpha"},
		{Cell: 0, Text: "alp", URL: "https://example.com/x"},
	}}}
	raw := NewOverview("t", []string{"REPO", "DIR"}, rows, nil).WithHyperlinks().View().Content
	if got := osc8.FindAllStringSubmatch(raw, -1); len(got) != 2 || got[0][2] != "https://github.com/o/alpha" || got[1][2] != "" {
		t.Errorf("hyperlink sequences %q, want one link to github.com/o/alpha:\n%q", got, raw)
	}
}
