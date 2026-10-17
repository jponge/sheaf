package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// newTestPause returns a Pause on a settable clock.
func newTestPause(text string) (Pause, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	return NewPause(text).WithClock(c.now), c
}

// Any key ends a Pause after the grace period, ctrl+c as an interruption;
// its view is then empty.
func TestPauseEndsOnAnyKey(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"x", "enter", "esc", "space", "q", "up", "ctrl+c"} {
		m, c := newTestPause("press any key to continue")
		c.t = c.t.Add(PauseGrace)
		if v := view(m); v != "press any key to continue" {
			t.Fatalf("view %q", v)
		}
		next, cmd := press(m, k)
		p := next.(Pause)
		if !isQuit(cmd) || !p.Done() || p.Interrupted() != (k == "ctrl+c") {
			t.Errorf("%s: quit %v, done %v, interrupted %v", k, isQuit(cmd), p.Done(), p.Interrupted())
		}
		if v := view(p); v != "" {
			t.Errorf("%s: final view %q", k, v)
		}
		// Later keys are ignored.
		if again, cmd := press(p, "ctrl+c"); cmd != nil || again.(Pause).Interrupted() != (k == "ctrl+c") {
			t.Errorf("%s: a later ctrl+c changed the model", k)
		}
	}
}

// Pastes, key releases and other messages do not end a Pause; its text is
// sanitized.
func TestPauseIgnoresOtherMessagesAndSanitizes(t *testing.T) {
	t.Parallel()
	m := NewPause("press\x1b[2J a key")
	for _, msg := range []tea.Msg{paste("x"), tea.KeyReleaseMsg{Code: 'x', Text: "x"}, tea.WindowSizeMsg{Width: 10, Height: 2}} {
		next, cmd := m.Update(msg)
		if cmd != nil || next.(Pause).Done() {
			t.Fatalf("%T ended the pause", msg)
		}
		m = next.(Pause)
	}
	assertNoControlChars(t, view(m))
}

// During the grace period, key presses (e.g. typed ahead while the failed
// action ran) are ignored, except ctrl+c, which interrupts at once.
func TestPauseIgnoresKeysDuringGrace(t *testing.T) {
	t.Parallel()
	m, c := newTestPause("press any key to continue")
	c.t = c.t.Add(PauseGrace - time.Millisecond)
	next, cmd := press(m, "x", "enter", "q")
	if cmd != nil || next.(Pause).Done() || view(next) != "press any key to continue" {
		t.Fatalf("a key during the grace period ended the pause: quit %v, done %v", isQuit(cmd), next.(Pause).Done())
	}
	if p, cmd := press(next, "ctrl+c"); !isQuit(cmd) || !p.(Pause).Interrupted() {
		t.Fatalf("ctrl+c during the grace period: quit %v, interrupted %v", isQuit(cmd), p.(Pause).Interrupted())
	}
	c.t = c.t.Add(time.Millisecond)
	if p, cmd := press(next, "x"); !isQuit(cmd) || !p.(Pause).Done() || p.(Pause).Interrupted() {
		t.Fatalf("a key after the grace period: quit %v, done %v", isQuit(cmd), p.(Pause).Done())
	}
}
