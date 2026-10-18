package tui

import (
	"fmt"
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
	if last := lines[len(lines)-1]; !strings.Contains(last, "↑/↓") || !strings.Contains(last, "scroll") || !strings.Contains(last, "any other key") || !strings.Contains(last, "back to the overview") {
		t.Errorf("help line %q", last)
	}
}

// A report longer than the window scrolls with up/down/k/j, pgup/pgdown and
// home/end, within its bounds; a status line tells which lines are shown.
// Scrolling works during the grace period.
func TestReportScrolls(t *testing.T) {
	t.Parallel()
	m0, _ := newTestReport(30)
	var m tea.Model = m0
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10}) // title, status, help: 7 lines
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
		if n := strings.Count(view(m), "\n") + 1; n > 10 {
			t.Fatalf("after %v: %d lines in a window of 10", step.keys, n)
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

// Any key other than the scroll keys ends a Report after the grace period,
// ctrl+c as an interruption; its view is then empty, still on the
// alternate screen. During the grace period such keys are ignored, except
// ctrl+c.
func TestReportEndsOnOtherKeysAfterGrace(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"x", "enter", "esc", "space", "q", "ctrl+c"} {
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
	if !slices.Equal(lines[1:4], want) {
		t.Errorf("lines %q, want %q", lines[1:4], want)
	}
}
