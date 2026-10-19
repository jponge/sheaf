package cli

import (
	"context"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/jponge/sheaf/internal/hosting"
)

// spinnerDelay is how long a phase must have been silent before the
// spinner shows, and spinnerInterval the time between its frames.
const (
	spinnerDelay    = 200 * time.Millisecond
	spinnerInterval = 100 * time.Millisecond
)

// spinnerFrames are the frames of the spinner.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner is the working indicator of a command whose standard error is a
// terminal: during a phase that prints nothing for a while (start), a line
// "<frame> <label>" is drawn on the terminal, below what was printed, and
// redrawn every spinnerInterval once the phase has been silent for
// spinnerDelay. It is erased (the line cleared, the cursor at its start)
// when the phase ends, before anything is written through writer (and
// drawn again after another spinnerDelay of silence), and while a
// bubbletea program owns the terminal (hold). It never draws in the middle
// of a line written through writer. A nil *spinner does nothing: standard
// error is not a terminal, and the output is unchanged.
type spinner struct {
	mu   sync.Mutex
	term io.Writer // where it draws; the spinner's lines never reach a copy
	// width returns the columns of term (0 = unknown): the line is cut to
	// fit, so that erasing it clears it entirely.
	width func() int
	now   func() time.Time
	// ticker returns the channel of the frames and a function stopping
	// it; nil (tests) runs no goroutine: step is then called directly.
	ticker func(time.Duration) (<-chan time.Time, func())

	phases []*string // labels of the running phases; the last one shows
	quiet  time.Time // last output through writer, or start of a phase
	mid    bool      // the last output ended inside a line
	held   int       // bubbletea programs running
	shown  bool      // the line is drawn
	frame  int       // next frame
	done   chan bool // closed when the last phase ends (ticker goroutine)
}

// newSpinner returns a spinner drawing on the terminal f.
func newSpinner(f *os.File) *spinner {
	return &spinner{
		term: f,
		width: func() int {
			// SyscallConn rather than Fd, as isTerminal.
			w := 0
			if rc, err := f.SyscallConn(); err == nil {
				_ = rc.Control(func(fd uintptr) { w, _, _ = term.GetSize(fd) })
			}
			return w
		},
		now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	}
}

// start begins a phase labelled label and returns the function ending it
// (idempotent).
func (s *spinner) start(label string) func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &label
	s.phases = append(s.phases, p)
	s.quiet = s.now()
	if len(s.phases) == 1 && s.ticker != nil {
		s.done = make(chan bool)
		ticks, stop := s.ticker(spinnerInterval)
		go func(done chan bool) {
			defer stop()
			for {
				select {
				case <-ticks:
					s.step()
				case <-done:
					return
				}
			}
		}(s.done)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.phases = slices.DeleteFunc(s.phases, func(q *string) bool { return q == p })
			s.erase()
			if len(s.phases) == 0 && s.done != nil {
				close(s.done)
				s.done = nil
			}
		})
	}
}

// step draws the next frame if a phase has been silent long enough.
func (s *spinner) step() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.phases) == 0 || s.held > 0 || s.mid || s.now().Sub(s.quiet) < spinnerDelay {
		return
	}
	line := spinnerFrames[s.frame%len(spinnerFrames)] + " " + *s.phases[len(s.phases)-1]
	if w := s.width(); w > 0 {
		line = ansi.Truncate(line, w-1, "")
	}
	_, _ = io.WriteString(s.term, "\r"+line+"\x1b[K")
	s.shown = true
	s.frame++
}

// erase clears the line if drawn; s.mu is held.
func (s *spinner) erase() {
	if s.shown {
		_, _ = io.WriteString(s.term, "\r\x1b[K")
		s.shown = false
	}
}

// hold erases the spinner and keeps it from drawing until release: a
// bubbletea program owns the terminal.
func (s *spinner) hold() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held++
	s.erase()
}

// release ends a hold.
func (s *spinner) release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held--
	s.quiet = s.now()
}

// writer returns w, which writes to the terminal of s, erasing the spinner
// before each write.
func (s *spinner) writer(w io.Writer) io.Writer {
	if s == nil {
		return w
	}
	return &spinWriter{w: w, s: s}
}

// spinWriter is the writer of a spinner (see spinner.writer).
type spinWriter struct {
	w io.Writer
	s *spinner
}

func (w *spinWriter) Write(p []byte) (int, error) {
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erase()
	if len(p) > 0 {
		s.mid, s.quiet = p[len(p)-1] != '\n', s.now()
	}
	return w.w.Write(p)
}

// spinHosting runs the gh default repository calls of h as phases of s.
type spinHosting struct {
	hosting.Hosting
	s *spinner
}

func (h spinHosting) SetDefaultRepo(ctx context.Context, dir, owner, name string) error {
	defer h.s.start("setting the gh default repository to " + owner + "/" + name)()
	return h.Hosting.SetDefaultRepo(ctx, dir, owner, name)
}
