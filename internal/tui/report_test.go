package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// newTestReport returns a Report of n lines "line 1".."line n" on a
// settable clock.
func newTestReport(n int) (Report, *fakeClock) {
	var lines []string
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	c := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	return NewReport("sheaf doctor: /b", lines, "back to the overview").WithClock(c.now), c
}

// shownLines returns the numbers of the report lines "line N" in the view.
func shownLines(m tea.Model) []int {
	var n []int
	for _, l := range strings.Split(view(m), "\n") {
		var i int
		if _, err := fmt.Sscanf(l, "line %d", &i); err == nil {
			n = append(n, i)
		}
	}
	return n
}

func span(from, to int) []int {
	var n []int
	for i := from; i <= to; i++ {
		n = append(n, i)
	}
	return n
}

// With an unknown size (no terminal), the report shows its title, every
// line and the help line, on the alternate screen.
func TestReportShowsTitleLinesAndHelp(t *testing.T) {
	t.Parallel()
	m, _ := newTestReport(30)
	v := m.View()
	if !v.AltScreen {
		t.Error("not on the alternate screen")
	}
	lines := strings.Split(view(m), "\n")
	if lines[0] != "sheaf doctor: /b" {
		t.Errorf("title %q", lines[0])
	}
	if got := shownLines(m); !slices.Equal(got, span(1, 30)) {
		t.Errorf("lines shown %v, want 1-30", got)
	}
	if last := lines[len(lines)-1]; last != "↑/↓ scroll • space/b page • g/G top/bottom • q/enter back to the overview" {
		t.Errorf("help line %q", last)
	}
}

// A report longer than the window scrolls with up/down/k/j, pgup/pgdown and
// space/b (a page), home/end and g/G (to the top, to the bottom), within
// its bounds; a status line tells which lines are shown.
// Scrolling works during the grace period.
func TestReportScrolls(t *testing.T) {
	t.Parallel()
	m0, _ := newTestReport(30)
	var m tea.Model = m0
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 13}) // 7 report lines below the title, blanks, status and help
	for _, step := range []struct {
		keys     []string
		from, to int
	}{
		{nil, 1, 7},
		{[]string{"up"}, 1, 7},
		{[]string{"down"}, 2, 8},
		{[]string{"j", "j"}, 4, 10},
		{[]string{"k"}, 3, 9},
		{[]string{"pgdown"}, 10, 16},
		{[]string{"pgup"}, 3, 9},
		{[]string{"end"}, 24, 30},
		{[]string{"down", "pgdown"}, 24, 30},
		{[]string{"home"}, 1, 7},
		{[]string{"space"}, 8, 14},
		{[]string{"space", "b"}, 8, 14},
		{[]string{"G"}, 24, 30},
		{[]string{"space"}, 24, 30},
		{[]string{"b"}, 17, 23},
		{[]string{"g"}, 1, 7},
		{[]string{"b"}, 1, 7},
		{[]string{"shift+g"}, 24, 30},
	} {
		var cmd tea.Cmd
		if len(step.keys) > 0 {
			m, cmd = press(m, step.keys...)
		}
		if got := shownLines(m); cmd != nil || m.(Report).Done() || !slices.Equal(got, span(step.from, step.to)) {
			t.Fatalf("after %v: lines %v (quit %v), want %d-%d\n%s", step.keys, got, isQuit(cmd), step.from, step.to, view(m))
		}
		if want := fmt.Sprintf("lines %d-%d of 30", step.from, step.to); !strings.Contains(view(m), want) {
			t.Fatalf("after %v: no status %q\n%s", step.keys, want, view(m))
		}
		if n := strings.Count(view(m), "\n") + 1; n > 13 {
			t.Fatalf("after %v: %d lines in a window of 13", step.keys, n)
		}
	}
	// A report that fits has no status line.
	short, _ := newTestReport(3)
	s, _ := short.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	if strings.Contains(view(s), "lines ") || !slices.Equal(shownLines(s), span(1, 3)) {
		t.Errorf("short report:\n%s", view(s))
	}
}

// Lines longer than the window are wrapped, not cut off; a window too small
// for the title and help still shows a line of the report.
func TestReportWrapsAndFitsSmallWindows(t *testing.T) {
	t.Parallel()
	long := "WARN  alpha: " + strings.Repeat("word ", 12) + "end"
	m := NewReport("title", []string{long}, "back")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
	v := view(next)
	for _, l := range strings.Split(v, "\n") {
		if w := lipgloss.Width(l); w > 20 {
			t.Errorf("line of width %d: %q", w, l)
		}
	}
	if got := strings.Join(strings.Fields(v), " "); !strings.Contains(got, strings.Join(strings.Fields(long), " ")) {
		t.Errorf("the long line was cut off:\n%s", v)
	}
	small, _ := newTestReport(30)
	s, _ := small.Update(tea.WindowSizeMsg{Width: 80, Height: 2})
	if got := shownLines(s); len(got) == 0 || strings.Count(view(s), "\n") >= 2 {
		t.Errorf("window of 2 lines:\n%s", view(s))
	}
}

// q, esc and enter end a Report after the grace period, ctrl+c as an
// interruption; its view is then empty, still on the alternate screen.
// During the grace period they are ignored, except ctrl+c.
func TestReportEndsOnBackKeysAfterGrace(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"enter", "esc", "q", "ctrl+c"} {
		m, c := newTestReport(3)
		if k != "ctrl+c" {
			next, cmd := press(m, k)
			if cmd != nil || next.(Report).Done() {
				t.Fatalf("%s during the grace period ended the report", k)
			}
		}
		c.t = c.t.Add(ReportGrace)
		next, cmd := press(m, k)
		r := next.(Report)
		if !isQuit(cmd) || !r.Done() || r.Interrupted() != (k == "ctrl+c") {
			t.Errorf("%s: quit %v, done %v, interrupted %v", k, isQuit(cmd), r.Done(), r.Interrupted())
		}
		if v := r.View(); v.Content != "" || !v.AltScreen {
			t.Errorf("%s: final view %q, alternate screen %v", k, v.Content, v.AltScreen)
		}
		if again, cmd := press(r, "ctrl+c"); cmd != nil || again.(Report).Interrupted() != (k == "ctrl+c") {
			t.Errorf("%s: a later ctrl+c changed the model", k)
		}
	}
	m, _ := newTestReport(3)
	if next, cmd := press(m, "ctrl+c"); !isQuit(cmd) || !next.(Report).Interrupted() {
		t.Errorf("ctrl+c during the grace period: quit %v, interrupted %v", isQuit(cmd), next.(Report).Interrupted())
	}
}

// Keys other than the scroll keys and q, esc, enter and ctrl+c are ignored,
// after the grace period too (D1: G, space, g and b used to end it).
func TestReportIgnoresOtherKeys(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"x", "a", "Q", "1", "?", "/", "tab", "backspace", "space", "b", "g", "G"} {
		m, c := newTestReport(3)
		c.t = c.t.Add(ReportGrace)
		next, cmd := press(m, k)
		if cmd != nil || next.(Report).Done() {
			t.Errorf("%s ended the report", k)
		}
		if got := shownLines(next); !slices.Equal(got, span(1, 3)) {
			t.Errorf("%s: lines %v", k, got)
		}
	}
}

// Pastes and key releases do not end a Report; title and lines are
// sanitized.
func TestReportIgnoresOtherMessagesAndSanitizes(t *testing.T) {
	t.Parallel()
	c := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	var m tea.Model = NewReport("t\x1b[2J", []string{"FAIL  a\x1b]0;x\x07", "b\tc"}, "back").WithClock(c.now)
	c.t = c.t.Add(ReportGrace)
	for _, msg := range []tea.Msg{paste("x"), tea.KeyReleaseMsg{Code: 'x', Text: "x"}} {
		next, cmd := m.Update(msg)
		if cmd != nil || next.(Report).Done() {
			t.Fatalf("%T ended the report", msg)
		}
		m = next
	}
	assertNoControlChars(t, view(m))
	if !strings.Contains(view(m), "FAIL  a") {
		t.Errorf("view:\n%s", view(m))
	}
}

// Tabs (e.g. in git's file lists) are expanded to the next multiple of 8
// columns rather than shown as U+FFFD.
func TestReportExpandsTabs(t *testing.T) {
	t.Parallel()
	m := NewReport("t", []string{"\tnew.txt", "M\tfile.txt", "ab\tc\td"}, "back")
	lines := strings.Split(view(m), "\n")
	want := []string{"        new.txt", "M       file.txt", "ab      c       d"}
	if !slices.Equal(lines[2:5], want) {
		t.Errorf("lines %q, want %q", lines[2:5], want)
	}
}

// reportShape classifies the lines of a report view from newTestReport:
// T title, L report line, S status line, K key help, ? other.
func reportShape(m tea.Model) string {
	status := regexp.MustCompile(`^lines \d+-\d+ of \d+$`)
	return shape(view(m), func(l string) string {
		switch {
		case l == "sheaf doctor: /b":
			return "T"
		case strings.HasPrefix(l, "line "):
			return "L"
		case status.MatchString(l):
			return "S"
		case strings.Contains(l, "back to the overview"):
			return "K"
		}
		return "?"
	})
}

// A blank line separates the title from the report lines (B41), another
// one the report lines from the status line (shown when they do not fit),
// and another one it from the key help; the one below the title goes
// before the one after the report lines when that lets them fit.
func TestReportSpacesTheFooter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n    int
		size tea.Msg
		want string
	}{
		{3, nil, "T _ L L L _ K"},
		{3, tea.WindowSizeMsg{Width: 80, Height: 10}, "T _ L L L _ K"},
		{7, tea.WindowSizeMsg{Width: 80, Height: 11}, "T _ L L L L L L L _ K"},
		{7, tea.WindowSizeMsg{Width: 80, Height: 10}, "T L L L L L L L _ K"},
		{30, tea.WindowSizeMsg{Width: 80, Height: 12}, "T _ L L L L L L _ S _ K"},
	} {
		var m tea.Model
		m, _ = newTestReport(c.n)
		if c.size != nil {
			m, _ = m.Update(c.size)
		}
		if got := reportShape(m); got != c.want {
			t.Errorf("%d lines, size %v: shape %q, want %q:\n%s", c.n, c.size, got, c.want, view(m))
		}
	}
}

// On a short terminal the blank lines go first (the one above the help
// first, then the one below the title), then the help, title and status
// lines as before.
func TestReportShortTerminalDropsSpacingFirst(t *testing.T) {
	t.Parallel()
	want := map[int]string{
		8: "T _ L L _ S _ K",
		7: "T _ L _ S _ K",
		6: "T _ L _ S K",
		5: "T L _ S K",
		4: "T L S K",
		3: "T L S",
		2: "L S",
		1: "L",
	}
	for h := 1; h <= 8; h++ {
		r, _ := newTestReport(30)
		m, _ := r.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		if got := reportShape(m); got != want[h] || lineCount(view(m)) > h {
			t.Errorf("height %d: shape %q, want %q:\n%s", h, got, want[h], view(m))
		}
	}
}

// A report that fits without some of the blank lines drops them (the one
// below the title first) rather than scroll, and then needs no status
// line; one that scrolls anyway keeps them.
func TestReportDropsSpacingToFitWithoutScrolling(t *testing.T) {
	t.Parallel()
	want := map[int]string{
		7: "T _ L L L _ K",
		6: "T L L L _ K",
		5: "T L L L K",
		4: "T L S K",
	}
	for h := 4; h <= 7; h++ {
		r, _ := newTestReport(3)
		m, _ := r.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		if got := reportShape(m); got != want[h] || lineCount(view(m)) > h {
			t.Errorf("3 lines, height %d: shape %q, want %q:\n%s", h, got, want[h], view(m))
		}
	}
}

// An empty report has no blank line below the title next to the one above
// the key help: a single blank line separates them, as before B41.
func TestReportEmptyHasNoAdjacentBlankLines(t *testing.T) {
	t.Parallel()
	for _, size := range []tea.Msg{nil, tea.WindowSizeMsg{Width: 80, Height: 4}, tea.WindowSizeMsg{Width: 80, Height: 5}, tea.WindowSizeMsg{Width: 80, Height: 24}} {
		var m tea.Model
		m, _ = newTestReport(0)
		if size != nil {
			m, _ = m.Update(size)
		}
		if got := reportShape(m); got != "T _ K" {
			t.Errorf("size %v: shape %q, want %q:\n%s", size, got, "T _ K", view(m))
		}
	}
}

// WithMouse turns mouse reporting on (cell motion: clicks and wheel, not
// bare motion) while the report is displayed, and off in its final view;
// the wheel scrolls by wheelStep lines within bounds, during the grace
// period too, and neither the wheel nor clicks end the report (B49).
func TestReportMouseWheelScrolls(t *testing.T) {
	t.Parallel()
	if m, _ := newTestReport(30); m.View().MouseMode != tea.MouseModeNone {
		t.Error("mouse mode on without WithMouse")
	}
	m0, c := newTestReport(30)
	var m tea.Model = m0.WithMouse()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 13}) // 7 report lines
	if mode := m.View().MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode %v, want cell motion", mode)
	}
	down := tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 3, Y: 4}
	up := tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 3, Y: 4}
	for _, step := range []struct {
		msg      tea.Msg
		from, to int
	}{
		{down, 4, 10},
		{down, 7, 13},
		{up, 4, 10},
		{up, 1, 7},
		{up, 1, 7},
		{tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: 4}, 1, 7},
		{tea.MouseWheelMsg{Button: tea.MouseWheelLeft}, 1, 7},
	} {
		var cmd tea.Cmd
		m, cmd = m.Update(step.msg)
		if got := shownLines(m); cmd != nil || m.(Report).Done() || !slices.Equal(got, span(step.from, step.to)) {
			t.Fatalf("after %v: lines %v (done %v), want %d-%d\n%s", step.msg, got, m.(Report).Done(), step.from, step.to, view(m))
		}
	}
	c.t = c.t.Add(ReportGrace)
	for range 10 {
		m, _ = m.Update(down)
	}
	m, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	if got := shownLines(m); m.(Report).Done() || !slices.Equal(got, span(24, 30)) {
		t.Fatalf("after the grace period: lines %v, done %v", got, m.(Report).Done())
	}
	m, _ = press(m, "q")
	if v := m.View(); !m.(Report).Done() || v.MouseMode != tea.MouseModeNone {
		t.Errorf("final view: done %v, mouse mode %v", m.(Report).Done(), v.MouseMode)
	}
}
