package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/ops"
)

// testSpinner returns a spinner drawing on a buffer of 30 columns, with a
// clock advanced by the test and no ticker goroutine (the test calls step).
func testSpinner() (*spinner, *bytes.Buffer, *time.Time) {
	var b bytes.Buffer
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := &spinner{term: &b, width: func() int { return 30 }, now: func() time.Time { return now }}
	return s, &b, &now
}

const eraseLine = "\r\x1b[K"

// The spinner shows once a phase has been silent for spinnerDelay, on one
// line cut to the width, advances on each step, and is erased when the
// phase ends: nothing is left on the line.
func TestSpinnerShowsAfterDelayAndIsErased(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	s.step()
	stop := s.start("listing repositories of a-very-long-organization")
	s.step()
	*now = now.Add(spinnerDelay - time.Millisecond)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn before the delay: %q", b.String())
	}
	*now = now.Add(time.Millisecond)
	s.step()
	want := "\r" + spinnerFrames[0] + " listing repositories of a-v\x1b[K"
	if b.String() != want {
		t.Fatalf("first frame %q, want %q", b.String(), want)
	}
	b.Reset()
	*now = now.Add(spinnerInterval)
	s.step()
	if !strings.HasPrefix(b.String(), "\r"+spinnerFrames[1]+" ") {
		t.Fatalf("second frame %q", b.String())
	}
	b.Reset()
	stop()
	if b.String() != eraseLine {
		t.Fatalf("stop wrote %q, want the line erased", b.String())
	}
	b.Reset()
	stop() // idempotent
	*now = now.Add(time.Second)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn after the phase: %q", b.String())
	}
	// A phase that ends before the delay leaves nothing at all.
	stop = s.start("short")
	s.step()
	stop()
	if b.Len() != 0 {
		t.Fatalf("short phase wrote %q", b.String())
	}
}

// A phase without a label (after a line that names it) shows the frame
// alone.
func TestSpinnerWithoutLabel(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	defer s.start("")()
	*now = now.Add(spinnerDelay)
	s.step()
	if want := "\r" + spinnerFrames[0] + "\x1b[K"; b.String() != want {
		t.Fatalf("drew %q, want %q", b.String(), want)
	}
}

// With the width of the terminal unknown, the line could wrap and leave a
// trail when erased: the spinner does not draw.
func TestSpinnerUnknownWidth(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	s.width = func() int { return 0 }
	defer s.start("x")()
	*now = now.Add(time.Second)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn with an unknown width: %q", b.String())
	}
}

// After a bubbletea program, the spinner waits spinnerDelay again before
// drawing.
func TestSpinnerWaitsAfterRelease(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	defer s.start("x")()
	s.hold()
	*now = now.Add(time.Second)
	s.release()
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn right after release: %q", b.String())
	}
	*now = now.Add(spinnerDelay)
	s.step()
	if b.Len() == 0 {
		t.Fatal("not drawn spinnerDelay after release")
	}
}

// Output through the spinner's writer erases the spinner first; the spinner
// comes back only after another spinnerDelay of silence, and never in the
// middle of a line.
func TestSpinnerWriterErasesAndDefers(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	w := s.writer(b)
	defer s.start("fetching origin")()
	*now = now.Add(spinnerDelay)
	s.step()
	b.Reset()
	if n, err := io.WriteString(w, "alpha: done\n"); n != 12 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if b.String() != eraseLine+"alpha: done\n" {
		t.Fatalf("wrote %q, want the spinner erased before the output", b.String())
	}
	b.Reset()
	*now = now.Add(spinnerDelay - time.Millisecond)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn too soon after output: %q", b.String())
	}
	io.WriteString(w, "partial")
	b.Reset()
	*now = now.Add(time.Second)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn in the middle of a line: %q", b.String())
	}
	io.WriteString(w, " end\n")
	*now = now.Add(spinnerDelay)
	b.Reset()
	s.step()
	if !strings.HasPrefix(b.String(), "\r") || !strings.HasSuffix(b.String(), " fetching origin\x1b[K") {
		t.Fatalf("not drawn again after silence: %q", b.String())
	}
	// Without a spinner shown, writes pass through unchanged.
	b.Reset()
	s2, b2, _ := testSpinner()
	io.WriteString(s2.writer(b2), "x\n")
	if b2.String() != "x\n" {
		t.Fatalf("idle writer wrote %q", b2.String())
	}
}

// While held (a bubbletea program owns the terminal), the spinner is
// erased and does not draw.
func TestSpinnerHold(t *testing.T) {
	t.Parallel()
	s, b, now := testSpinner()
	defer s.start("x")()
	*now = now.Add(spinnerDelay)
	s.step()
	b.Reset()
	s.hold()
	if b.String() != eraseLine {
		t.Fatalf("hold wrote %q", b.String())
	}
	b.Reset()
	*now = now.Add(time.Second)
	s.step()
	if b.Len() != 0 {
		t.Fatalf("drawn while held: %q", b.String())
	}
	s.release()
	*now = now.Add(spinnerDelay)
	s.step()
	if b.Len() == 0 {
		t.Fatal("not drawn after release")
	}
}

// A nil spinner (standard error not a terminal) does nothing.
func TestNilSpinner(t *testing.T) {
	t.Parallel()
	var s *spinner
	s.start("x")()
	s.step()
	s.hold()
	s.release()
	var b bytes.Buffer
	if w := s.writer(&b); w != io.Writer(&b) {
		t.Fatalf("writer of a nil spinner wraps: %T", w)
	}
}

// With a ticker, the spinner draws by itself during a phase and stops
// drawing once it ends.
func TestSpinnerTicker(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var b bytes.Buffer
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	// The phase starts at t0; every later reading of the clock is a second
	// later.
	t0, calls := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), 0
	clock := func() time.Time {
		calls++
		if calls == 1 {
			return t0
		}
		return t0.Add(time.Second)
	}
	s := &spinner{term: lockedWriter{&mu, &b}, width: func() int { return 30 }, now: clock,
		ticker: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() { close(stopped) } }}
	stop := s.start("work")
	ticks <- time.Time{}
	ticks <- time.Time{} // the first tick has been handled
	stop()
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(b.String(), spinnerFrames[0]+" work") || !strings.HasSuffix(b.String(), eraseLine) {
		t.Fatalf("output %q", b.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// heldModel is a program that records whether the spinner is held while it
// runs, and quits.
type heldModel struct {
	s    *spinner
	held *int
}

func (m heldModel) Init() tea.Cmd { return func() tea.Msg { return "go" } }

func (m heldModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg == "go" {
		m.s.mu.Lock()
		*m.held = m.s.held
		m.s.mu.Unlock()
		return m, tea.Quit
	}
	return m, nil
}

func (m heldModel) View() tea.View { return tea.NewView("") }

// The spinner is held while the programs of a TUI session (menus, dialogs,
// progress views) own the terminal, and released after.
func TestTUISessionHoldsSpinner(t *testing.T) {
	t.Parallel()
	sp, _, _ := testSpinner()
	s := &tuiSession{ctx: context.Background(), ctl: newInterruptCtl(), out: io.Discard, errw: io.Discard, spin: sp}
	var inModel, inWork int
	if _, err := s.runModel(heldModel{sp, &inModel}); err != nil {
		t.Fatal(err)
	}
	s.progress("t", progressOpts{}, func(context.Context, ops.Reporter, askFunc) {
		sp.mu.Lock()
		inWork = sp.held
		sp.mu.Unlock()
	})
	if inModel != 1 || inWork != 1 || sp.held != 0 {
		t.Fatalf("held %d in a program, %d during a progress view's work, %d after; want 1, 1, 0", inModel, inWork, sp.held)
	}
}

// The gh default repository calls run as a spinner phase.
func TestSpinnerHostingSetDefaultRepo(t *testing.T) {
	t.Parallel()
	s, _, _ := testSpinner()
	f := hostingtest.NewFake()
	if _, ok := (Deps{Hosting: func() hosting.Hosting { return f }, spin: s}).hosting().(spinHosting); !ok {
		t.Error("the hosting of Deps with a spinner does not spin")
	}
	if _, ok := (Deps{Hosting: func() hosting.Hosting { return f }}).hosting().(spinHosting); ok {
		t.Error("the hosting of Deps without a spinner spins")
	}
	var during int
	f.DefaultRepoHook = func(context.Context) {
		s.mu.Lock()
		during = len(s.phases)
		s.mu.Unlock()
	}
	var h hosting.Hosting = spinHosting{f, s}
	if err := h.SetDefaultRepo(context.Background(), "/abs/dir", "o", "a"); err != nil {
		t.Fatal(err)
	}
	if during != 1 || len(s.phases) != 0 {
		t.Fatalf("phases during the call %d, after %d; want 1 and 0", during, len(s.phases))
	}
}

// gh may be slow to open a page (pull request lookup): Browse is a phase.
func TestSpinnerHostingBrowse(t *testing.T) {
	t.Parallel()
	s, _, _ := testSpinner()
	f := hostingtest.NewFake()
	var during int
	f.BrowseHook = func(context.Context) {
		s.mu.Lock()
		during = len(s.phases)
		s.mu.Unlock()
	}
	var h hosting.Hosting = spinHosting{f, s}
	if err := h.Browse(context.Background(), "o", "a", hosting.Page{}); err != nil {
		t.Fatal(err)
	}
	if during != 1 || len(s.phases) != 0 {
		t.Fatalf("phases during the call %d, after %d; want 1 and 0", during, len(s.phases))
	}
}
