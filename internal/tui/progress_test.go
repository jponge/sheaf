package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// fakeClock is a settable clock for elapsed-time assertions.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestProgress(title string) (Progress, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	return NewProgress(title).WithClock(c.now), c
}

// feed sends msgs one by one and returns the final model and last command.
func feed(m tea.Model, msgs ...tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, msg := range msgs {
		m, cmd = m.Update(msg)
	}
	return m, cmd
}

func asProgress(t *testing.T, m tea.Model) Progress {
	t.Helper()
	p, ok := m.(Progress)
	if !ok {
		t.Fatalf("model is %T, want Progress", m)
	}
	return p
}

// rowLine returns the first view line containing label, or "".
func rowLine(v, label string) string {
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, label) {
			return l
		}
	}
	return ""
}

func TestProgressRowsFollowEventsInOrder(t *testing.T) {
	p, _ := newTestProgress("Updating")
	m, _ := feed(p,
		RowUpdate{Key: "b", Label: "beta", State: RowRunning, Message: "fetch"},
		RowUpdate{Key: "a", Label: "alpha", State: RowRunning, Message: "clone"},
		RowUpdate{Key: "b", Message: "fetching origin"},
		RowUpdate{Key: "a", State: RowDone, Message: "cloned"},
	)
	v := view(m)
	lines := strings.Split(v, "\n")
	bi, ai := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "beta") {
			bi = i
		}
		if strings.Contains(l, "alpha") {
			ai = i
		}
	}
	if bi < 0 || ai < 0 || bi > ai {
		t.Fatalf("rows not in insertion order (beta first):\n%s", v)
	}
	if l := rowLine(v, "beta"); !strings.Contains(l, "fetching origin") || strings.Contains(l, "✓") {
		t.Errorf("beta row = %q, want running with its latest message", l)
	}
	if l := rowLine(v, "alpha"); !strings.Contains(l, "✓") || !strings.Contains(l, "cloned") {
		t.Errorf("alpha row = %q, want done marker and message", l)
	}
	running, done, skipped, failed := asProgress(t, m).Counts()
	if running != 1 || done != 1 || skipped != 0 || failed != 0 {
		t.Errorf("counts = %d running, %d done, %d skipped, %d failed", running, done, skipped, failed)
	}
}

func TestProgressHeaderCountsAndElapsed(t *testing.T) {
	p, clock := newTestProgress("Updating")
	m, _ := feed(p,
		RowUpdate{Key: "r1", Label: "r1", State: RowRunning},
		RowUpdate{Key: "r2", Label: "r2", State: RowDone, Message: "fast-forwarded"},
		RowUpdate{Key: "r3", Label: "r3", State: RowSkipped, Message: "dirty"},
		RowUpdate{Key: "r4", Label: "r4", State: RowFailed, Message: "boom"},
		// Aside rows (e.g. the fetch of update) are shown but not counted.
		RowUpdate{Key: "r5", Label: "r5 (fetch)", State: RowDone, Aside: true},
	)
	clock.t = clock.t.Add(2500 * time.Millisecond)
	header := strings.Split(view(m), "\n")[0]
	for _, want := range []string{"Updating", "1 running", "1 done", "1 skipped", "1 failed", "2.5s"} {
		if !strings.Contains(header, want) {
			t.Errorf("header %q lacks %q", header, want)
		}
	}
	if l := rowLine(view(m), "r3"); !strings.Contains(l, "skipped: dirty") {
		t.Errorf("skipped row = %q", l)
	}
	if l := rowLine(view(m), "r4"); !strings.Contains(l, "✗") || !strings.Contains(l, "failed: boom") {
		t.Errorf("failed row = %q", l)
	}
	if !strings.Contains(view(m), "r5 (fetch)") {
		t.Errorf("aside row not shown:\n%s", view(m))
	}
}

func TestProgressMessageOnlyUpdateKeepsFinalState(t *testing.T) {
	p, _ := newTestProgress("t")
	m, _ := feed(p,
		RowUpdate{Key: "a", Label: "a", State: RowDone, Message: "done"},
		RowUpdate{Key: "a", Message: "late note"},
		// A message-only update of an unknown row creates a running row.
		RowUpdate{Key: "b", Label: "b", Message: "fetching origin"},
	)
	running, done, _, _ := asProgress(t, m).Counts()
	if done != 1 || running != 1 {
		t.Fatalf("running=%d done=%d, want 1 and 1:\n%s", running, done, view(m))
	}
	if l := rowLine(view(m), "late note"); !strings.Contains(l, "✓") {
		t.Errorf("row = %q, want still done", l)
	}
}

func TestProgressShowsWaitMessages(t *testing.T) {
	p, _ := newTestProgress("Updating")
	m, _ := feed(p,
		RowUpdate{Key: "alpha/main", Label: "alpha/main", State: RowRunning, Message: "fast-forward"},
		RowUpdate{Key: "alpha/main", Message: "waiting for in-progress fast-forward to finish (Ctrl-C again to force)"},
	)
	if l := rowLine(view(m), "alpha/main"); !strings.Contains(l, "waiting for in-progress fast-forward to finish") {
		t.Fatalf("row = %q", l)
	}
}

func TestProgressFinishQuitsAndRendersSummaryOnly(t *testing.T) {
	p, clock := newTestProgress("Updating")
	m, _ := feed(p,
		RowUpdate{Key: "a", Label: "alpha", State: RowDone},
		RowUpdate{Key: "b", Label: "beta", State: RowSkipped},
		RowUpdate{Key: "c", Label: "gamma", State: RowFailed},
	)
	clock.t = clock.t.Add(3 * time.Second)
	m, cmd := m.Update(FinishMsg{})
	if !isQuit(cmd) {
		t.Fatal("FinishMsg did not quit")
	}
	if !asProgress(t, m).Finished() {
		t.Fatal("Finished() = false")
	}
	clock.t = clock.t.Add(time.Hour) // the elapsed time is frozen at the finish
	v := view(m)
	// The header and a newline: bubbletea erases the last line of the final
	// frame when the program exits, which must not be the header.
	if !strings.HasSuffix(v, "\n") || lineCount(strings.TrimSuffix(v, "\n")) != 1 {
		t.Fatalf("final view = %q, want the header only, followed by a newline", v)
	}
	for _, want := range []string{"Updating", "1 done", "1 skipped", "1 failed", "3s"} {
		if !strings.Contains(v, want) {
			t.Errorf("final view %q lacks %q", v, want)
		}
	}
	if m2, cmd := m.Update(RowUpdate{Key: "d", Label: "delta", State: RowRunning}); cmd != nil || strings.Contains(view(m2), "delta") {
		t.Error("updates after the finish are not ignored")
	}
}

func TestProgressCtrlCStages(t *testing.T) {
	calls := 0
	p, _ := newTestProgress("Updating")
	p = p.WithInterrupt(func() { calls++ })
	m, _ := feed(p, RowUpdate{Key: "a", Label: "alpha", State: RowRunning})

	m, cmd := m.Update(keyMsg("ctrl+c"))
	if cmd == nil {
		t.Fatal("first ctrl+c returned no command")
	}
	if isQuit(cmd) {
		t.Fatal("first ctrl+c quit the progress view")
	}
	if calls != 1 {
		t.Fatalf("interrupt called %d times after the first ctrl+c, want 1", calls)
	}
	if v := view(m); !strings.Contains(v, "Ctrl-C again to force") {
		t.Errorf("view after the first ctrl+c lacks the force hint:\n%s", v)
	}
	if asProgress(t, m).Interrupts() != 1 {
		t.Errorf("Interrupts() = %d", asProgress(t, m).Interrupts())
	}

	m, cmd = m.Update(keyMsg("ctrl+c"))
	if cmd == nil {
		t.Fatal("second ctrl+c returned no command")
	}
	_ = cmd()
	if calls != 2 {
		t.Fatalf("interrupt called %d times after the second ctrl+c, want 2", calls)
	}
	if v := view(m); !strings.Contains(v, "forcing") {
		t.Errorf("view after the second ctrl+c lacks the forcing note:\n%s", v)
	}
}

func TestProgressAskConfirmsThroughReplyChannel(t *testing.T) {
	calls := 0
	p, _ := newTestProgress("Removing")
	p = p.WithInterrupt(func() { calls++ })
	r1, r2 := make(chan bool, 1), make(chan bool, 1)
	m, _ := feed(p,
		RowUpdate{Key: "alpha", Label: "alpha", State: RowRunning, Message: "fetching origin"},
		AskMsg{Question: "Remove alpha?", Details: []string{"will delete /b/alpha"}, Reply: r1},
		AskMsg{Question: "Remove beta?", Reply: r2},
	)
	v := view(m)
	for _, want := range []string{"Remove alpha?", "[y/N]", "will delete /b/alpha", "alpha"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Remove beta?") {
		t.Errorf("second question shown before the first is answered:\n%s", v)
	}
	m, _ = m.Update(keyMsg("y"))
	select {
	case got := <-r1:
		if !got {
			t.Fatal("reply = false after y")
		}
	default:
		t.Fatal("no reply after y")
	}
	if v := view(m); !strings.Contains(v, "Remove beta?") {
		t.Fatalf("queued question not shown:\n%s", v)
	}
	// ctrl+c answers the dialog with no and interrupts, like in menus. The
	// answer is only sent by the command, after the interrupt function ran,
	// so that the work sees the cancellation along with the no.
	m, cmd := m.Update(keyMsg("ctrl+c"))
	select {
	case <-r2:
		t.Fatal("ctrl+c answered before the interrupt function ran")
	default:
	}
	if strings.Contains(view(m), "Remove beta?") {
		t.Fatalf("dialog still shown after ctrl+c:\n%s", view(m))
	}
	if cmd == nil {
		t.Fatal("ctrl+c in a dialog returned no command")
	}
	_ = cmd()
	select {
	case got := <-r2:
		if got {
			t.Fatal("reply = true after ctrl+c")
		}
	default:
		t.Fatal("no reply after ctrl+c")
	}
	if calls != 1 || asProgress(t, m).Interrupts() != 1 {
		t.Fatalf("ctrl+c in a dialog: interrupt called %d times, want 1", calls)
	}
	if strings.Contains(view(m), "[y/N]") {
		t.Fatalf("dialog still shown:\n%s", view(m))
	}
}

func TestProgressFinishAnswersPendingQuestionsNo(t *testing.T) {
	p, _ := newTestProgress("t")
	r := make(chan bool, 1)
	m, _ := feed(p, AskMsg{Question: "Q?", Reply: r}, FinishMsg{})
	select {
	case got := <-r:
		if got {
			t.Fatal("pending question answered yes at the finish")
		}
	default:
		t.Fatal("pending question not answered at the finish")
	}
	_ = m
}

func TestProgressSanitizesText(t *testing.T) {
	p, _ := newTestProgress("t\x1b[2J" + injectedSGR)
	m, _ := feed(p, RowUpdate{Key: "a", Label: "al\x1bpha" + injectedSGR, State: RowFailed, Message: "line1\nline2\x07" + injectedSGR})
	assertNoControlChars(t, view(m))
	assertNoInjectedSGR(t, m)
}

func TestProgressFitsHeight(t *testing.T) {
	p, _ := newTestProgress("t")
	var msgs []tea.Msg
	for i := range 20 {
		msgs = append(msgs, RowUpdate{Key: fmt.Sprint(i), Label: fmt.Sprintf("repo%02d", i), State: RowDone})
	}
	msgs = append(msgs, RowUpdate{Key: "run", Label: "busy", State: RowRunning}, tea.WindowSizeMsg{Width: 80, Height: 6})
	m, _ := feed(p, msgs...)
	v := view(m)
	if n := lineCount(v); n > 6 {
		t.Fatalf("view has %d lines for a height of 6:\n%s", n, v)
	}
	if !strings.Contains(v, "busy") {
		t.Errorf("running row hidden:\n%s", v)
	}
	if !strings.Contains(v, "more") {
		t.Errorf("no hidden-rows note:\n%s", v)
	}
	if !strings.Contains(v, "repo19") {
		t.Errorf("most recent finished row hidden:\n%s", v)
	}
}

// A row whose work the user declined is marked cancelled, not skipped, and
// counted as such; TitleMsg replaces the title, also in the final view.
func TestProgressCancelledRowAndTitleChange(t *testing.T) {
	t.Parallel()
	p, _ := newTestProgress("Checking alpha")
	m, _ := feed(p, RowUpdate{Key: "a", Label: "alpha", State: RowRunning, Message: "checking"}, TitleMsg{Title: "Removing alpha"})
	if v := view(m); !strings.Contains(v, "Removing alpha · 1 running, 0 done, 0 skipped, 0 failed ·") {
		t.Fatalf("title not replaced:\n%s", v)
	}
	m, _ = feed(m, RowUpdate{Key: "a", State: RowCancelled})
	v := view(m)
	if l := strings.Split(v, "\n")[2]; !strings.HasPrefix(l, "- alpha") || !strings.Contains(l, "cancelled") || strings.Contains(l, "skipped") {
		t.Errorf("alpha row = %q, want a cancelled row", l)
	}
	if !strings.Contains(v, "0 running, 0 done, 0 skipped, 0 failed, 1 cancelled ·") {
		t.Errorf("header does not count the cancelled row:\n%s", v)
	}
	m, _ = feed(m, FinishMsg{})
	if v := view(m); !strings.HasPrefix(v, "Removing alpha: 0 done, 0 skipped, 0 failed, 1 cancelled in ") {
		t.Errorf("final view:\n%s", v)
	}
}

// plannedProgress returns a progress view of 4 planned rows (B48): one
// running, then one done, one skipped and one failed.
func plannedProgress(t *testing.T, msgs ...tea.Msg) tea.Model {
	t.Helper()
	p, _ := newTestProgress("Updating")
	m, _ := feed(p, append([]tea.Msg{
		PlannedMsg{Units: 3}, PlannedMsg{Units: 1},
		RowUpdate{Key: "a", Label: "alpha", State: RowRunning},
		RowUpdate{Key: "b", Label: "beta", State: RowDone},
		RowUpdate{Key: "c", Label: "gamma", State: RowSkipped},
		RowUpdate{Key: "d", Label: "delta", State: RowFailed},
		RowUpdate{Key: "e", Label: "e (fetch)", State: RowDone, Aside: true},
	}, msgs...)...)
	return m
}

// barCells matches the cells of the progress bar of the header.
var barCells = regexp.MustCompile(`[█▌░]+`)

// Once the number of rows is planned (PlannedMsg, cumulative), the header
// shows a progress bar of the finished rows (done, skipped, failed; aside
// rows not counted) over the planned ones, with the percentage, after the
// status line; none before (unknown total) nor in the final view.
func TestProgressHeaderBar(t *testing.T) {
	t.Parallel()
	p, _ := newTestProgress("Updating")
	m, _ := feed(p, RowUpdate{Key: "a", Label: "alpha", State: RowDone}, tea.WindowSizeMsg{Width: 120, Height: 20})
	if h := strings.Split(view(m), "\n")[0]; strings.Contains(h, "%") || barCells.MatchString(h) {
		t.Fatalf("header %q has a bar before the total is known", h)
	}
	m = plannedProgress(t, tea.WindowSizeMsg{Width: 120, Height: 20})
	h := strings.Split(view(m), "\n")[0]
	if !strings.HasPrefix(h, "Updating · 1 running, 1 done, 1 skipped, 1 failed · 0s  ") || !strings.HasSuffix(h, " 75%") {
		t.Fatalf("header %q, want the status line, then a bar at 75%%", h)
	}
	if cells := barCells.FindString(h); lipgloss.Width(cells)+len("  75%") != barMax {
		t.Errorf("bar %q: %d cells, want %d with the percentage", cells, lipgloss.Width(cells), barMax)
	}
	m, _ = m.Update(FinishMsg{})
	if v := view(m); strings.Contains(v, "%") || barCells.MatchString(v) {
		t.Errorf("final view %q has a bar", v)
	}
	// More finished rows than planned (rows of unplanned work) is 100%.
	m, _ = feed(p, PlannedMsg{Units: 1}, RowUpdate{Key: "a", Label: "alpha", State: RowDone}, RowUpdate{Key: "b", Label: "beta", State: RowDone})
	if h := strings.Split(view(m), "\n")[0]; !strings.HasSuffix(h, " 100%") || lipgloss.Width(barCells.FindString(h))+len(" 100%") != barMax {
		t.Errorf("header %q, want a full bar at 100%%", h)
	}
}

// The bar narrows to the room left on the header line, and is dropped
// first, before the status line is cut, when less than barMin columns are
// left.
func TestProgressHeaderBarFitsWidth(t *testing.T) {
	t.Parallel()
	status := "Updating · 1 running, 1 done, 1 skipped, 1 failed · 0s"
	w := lipgloss.Width(status)
	h := strings.Split(view(plannedProgress(t, tea.WindowSizeMsg{Width: w + 2 + 20, Height: 20})), "\n")[0]
	if !strings.HasPrefix(h, status+"  ") || lipgloss.Width(h) != w+2+20 || !strings.HasSuffix(h, " 75%") {
		t.Errorf("header %q, want a 20-column bar", h)
	}
	for _, width := range []int{w + 2 + barMin - 1, w, w - 5} {
		h := strings.Split(view(plannedProgress(t, tea.WindowSizeMsg{Width: width, Height: 20})), "\n")[0]
		if strings.Contains(h, "%") || barCells.MatchString(h) {
			t.Errorf("width %d: header %q has a bar", width, h)
		}
		if !strings.HasPrefix(status, h) || lipgloss.Width(h) != min(w, width) {
			t.Errorf("width %d: header %q, want %q cut to the width only", width, h, status)
		}
	}
	m := plannedProgress(t, tea.WindowSizeMsg{Width: w + 2 + barMin, Height: 20})
	if h := strings.Split(view(m), "\n")[0]; !strings.HasSuffix(h, " 75%") {
		t.Errorf("header %q, want a bar of barMin columns", h)
	}
}

// The bar is a gradient only with 256 colors or more; with fewer colors (and
// under NO_COLOR, which limits the profile to ASCII) it is solid, of full
// blocks.
func TestProgressHeaderBarFollowsColorProfile(t *testing.T) {
	t.Parallel()
	truecolor := regexp.MustCompile(`\x1b\[[0-9;:]*38;2;[0-9;:]+m`)
	for _, c := range []struct {
		profile  colorprofile.Profile
		gradient bool
	}{
		{colorprofile.TrueColor, true},
		{colorprofile.ANSI256, true},
		{colorprofile.ANSI, false},
		{colorprofile.ASCII, false},
		{colorprofile.Unknown, false},
	} {
		m := plannedProgress(t, tea.WindowSizeMsg{Width: 120, Height: 20}, tea.ColorProfileMsg{Profile: c.profile})
		raw := strings.Split(m.View().Content, "\n")[0]
		colors := map[string]bool{}
		for _, s := range truecolor.FindAllString(raw, -1) {
			colors[s] = true
		}
		h := strings.Split(view(m), "\n")[0]
		if c.gradient && (len(colors) < 3 || !strings.Contains(h, "▌")) {
			t.Errorf("%v: %d colors in %q, want a gradient of half blocks", c.profile, len(colors), raw)
		}
		if !c.gradient && (strings.Contains(h, "▌") || !strings.Contains(h, "███")) {
			t.Errorf("%v: header %q, want a solid bar of full blocks", c.profile, h)
		}
	}
}

// progressShape classifies the lines of a progress view titled "t": H
// header, R row, N hidden-rows note, I interruption note, ? other.
func progressShape(m tea.Model) string {
	return shape(view(m), func(l string) string {
		switch {
		case strings.HasPrefix(l, "t ·"), strings.HasPrefix(l, "t:"):
			return "H"
		case strings.Contains(l, "repo"):
			return "R"
		case strings.HasPrefix(l, "… "):
			return "N"
		case strings.HasPrefix(l, "Interrupted"):
			return "I"
		}
		return "?"
	})
}

// A blank line separates the header from the rows (B41), with the rule of
// the other views: kept unless dropping it lets every row fit, and dropped
// when no row would fit with it (the hidden-rows note then follows the
// header). No blank line without rows, nor in the final view.
func TestProgressSpacesTheHeader(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rows, height int
		interrupted  bool
		want         string
	}{
		{0, 0, false, "H"},
		{3, 0, false, "H _ R R R"},
		{3, 5, false, "H _ R R R"},
		{3, 4, false, "H R R R"},
		{5, 4, false, "H _ R N"},
		{5, 3, false, "H R N"},
		{2, 5, true, "H _ R R I"},
		{2, 4, true, "H R R I"},
	} {
		p, _ := newTestProgress("t")
		var msgs []tea.Msg
		for i := range c.rows {
			msgs = append(msgs, RowUpdate{Key: fmt.Sprint(i), Label: fmt.Sprintf("repo%d", i), State: RowRunning})
		}
		if c.height > 0 {
			msgs = append(msgs, tea.WindowSizeMsg{Width: 80, Height: c.height})
		}
		if c.interrupted {
			msgs = append(msgs, InterruptedMsg{Count: 1})
		}
		m, _ := feed(p, msgs...)
		if got := progressShape(m); got != c.want {
			t.Errorf("%d rows, height %d: shape %q, want %q:\n%s", c.rows, c.height, got, c.want, view(m))
		}
		m, _ = m.Update(FinishMsg{})
		if got := progressShape(m); got != "H _" {
			t.Errorf("%d rows, height %d: final shape %q, want %q", c.rows, c.height, got, "H _")
		}
	}
}
