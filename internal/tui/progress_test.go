package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	v := m.View()
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
	header := strings.Split(m.View(), "\n")[0]
	for _, want := range []string{"Updating", "1 running", "1 done", "1 skipped", "1 failed", "2.5s"} {
		if !strings.Contains(header, want) {
			t.Errorf("header %q lacks %q", header, want)
		}
	}
	if l := rowLine(m.View(), "r3"); !strings.Contains(l, "skipped: dirty") {
		t.Errorf("skipped row = %q", l)
	}
	if l := rowLine(m.View(), "r4"); !strings.Contains(l, "✗") || !strings.Contains(l, "failed: boom") {
		t.Errorf("failed row = %q", l)
	}
	if !strings.Contains(m.View(), "r5 (fetch)") {
		t.Errorf("aside row not shown:\n%s", m.View())
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
		t.Fatalf("running=%d done=%d, want 1 and 1:\n%s", running, done, m.View())
	}
	if l := rowLine(m.View(), "late note"); !strings.Contains(l, "✓") {
		t.Errorf("row = %q, want still done", l)
	}
}

func TestProgressShowsWaitMessages(t *testing.T) {
	p, _ := newTestProgress("Updating")
	m, _ := feed(p,
		RowUpdate{Key: "alpha/main", Label: "alpha/main", State: RowRunning, Message: "fast-forward"},
		RowUpdate{Key: "alpha/main", Message: "waiting for in-progress fast-forward to finish (Ctrl-C again to force)"},
	)
	if l := rowLine(m.View(), "alpha/main"); !strings.Contains(l, "waiting for in-progress fast-forward to finish") {
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
	v := m.View()
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
	if m2, cmd := m.Update(RowUpdate{Key: "d", Label: "delta", State: RowRunning}); cmd != nil || strings.Contains(m2.View(), "delta") {
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
	if v := m.View(); !strings.Contains(v, "Ctrl-C again to force") {
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
	if v := m.View(); !strings.Contains(v, "forcing") {
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
	v := m.View()
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
	if v := m.View(); !strings.Contains(v, "Remove beta?") {
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
	if strings.Contains(m.View(), "Remove beta?") {
		t.Fatalf("dialog still shown after ctrl+c:\n%s", m.View())
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
	if strings.Contains(m.View(), "[y/N]") {
		t.Fatalf("dialog still shown:\n%s", m.View())
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
	p, _ := newTestProgress("t\x1b[2J")
	m, _ := feed(p, RowUpdate{Key: "a", Label: "al\x1bpha", State: RowFailed, Message: "line1\nline2\x07"})
	assertNoControlChars(t, m.View())
}

func TestProgressFitsHeight(t *testing.T) {
	p, _ := newTestProgress("t")
	var msgs []tea.Msg
	for i := range 20 {
		msgs = append(msgs, RowUpdate{Key: fmt.Sprint(i), Label: fmt.Sprintf("repo%02d", i), State: RowDone})
	}
	msgs = append(msgs, RowUpdate{Key: "run", Label: "busy", State: RowRunning}, tea.WindowSizeMsg{Width: 80, Height: 6})
	m, _ := feed(p, msgs...)
	v := m.View()
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
