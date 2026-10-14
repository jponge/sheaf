package tui

import (
	"errors"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// Run runs m as a bubbletea program reading keys from in and rendering to
// out, and returns the final model. Extra options (e.g. tea.WithAltScreen())
// are appended to the input/output ones.
//
// The input is handled according to its kind:
//
//   - nil disables input: the program runs until the model quits or it is
//     killed (e.g. through tea.WithContext).
//   - An *os.File must be a terminal; it is handed to bubbletea, which puts
//     it in raw mode (and, through os.File.Fd, in blocking mode). Any other
//     *os.File (pipe, regular file, /dev/null) is rejected with
//     ErrInputNotTerminal before anything is read. bubbletea would read such
//     a file with a goroutine it cannot stop, which outlives Run and swallows
//     input meant for the caller, and would never return at EOF. Use plain
//     CLI mode for non-terminal stdin.
//   - Any other reader is meant for in-memory input, i.e. tests. The program
//     quits once it reaches EOF, after the keys read before it have been
//     processed. The final model then reports neither confirmation nor
//     cancellation, which callers must treat as an abort. As with a terminal,
//     bubbletea parses each Read result on its own, and printable runes that
//     arrive together form one multi-rune key event; a read that fills
//     bubbletea's 256-byte buffer may split such an event, or an escape
//     sequence, in two.
//
// The returned model may be nil when the program panicked or failed to
// start; callers should use a comma-ok type assertion.
func Run(m tea.Model, in io.Reader, out io.Writer, opts ...tea.ProgramOption) (tea.Model, error) {
	p, err := NewProgram(m, in, out, opts...)
	if err != nil {
		return m, err
	}
	return p.Run()
}

// NewProgram returns the program Run would run, with the same handling of
// in (including ErrInputNotTerminal), for callers that need the program
// itself, e.g. to Send it messages from other goroutines while it runs.
func NewProgram(m tea.Model, in io.Reader, out io.Writer, opts ...tea.ProgramOption) (*tea.Program, error) {
	var eof *quitOnEOF
	switch r := in.(type) {
	case nil:
	case *os.File:
		if !isTerminalFile(r) {
			return nil, ErrInputNotTerminal
		}
	default:
		if f, ok := r.(term.File); !ok || !term.IsTerminal(f.Fd()) {
			eof = &quitOnEOF{r: in}
			in = eof
		}
	}
	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithInput(in), tea.WithOutput(out)}, opts...)...)
	if eof != nil {
		eof.p = p // set before p.Run starts the read loop
	}
	return p, nil
}

// isTerminalFile reports whether f is a terminal without calling f.Fd, which
// would switch a non-blocking file (e.g. an os.Pipe) to blocking mode and
// disable its deadlines.
func isTerminalFile(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	isTerm := false
	if err := rc.Control(func(fd uintptr) { isTerm = term.IsTerminal(fd) }); err != nil {
		return false
	}
	return isTerm
}

// quitOnEOF quits the program when the wrapped reader is exhausted.
type quitOnEOF struct {
	r       io.Reader
	p       *tea.Program
	pending error // EOF held back so that bytes returned with it are not dropped
}

// Read is only called from bubbletea's single read-loop goroutine, which
// stops reading at the first error. bubbletea ignores io.EOF from the read
// loop (tty.go readLoop) and keeps waiting for messages, hence the Quit.
func (q *quitOnEOF) Read(b []byte) (int, error) {
	if q.pending != nil {
		// The bytes returned with EOF were delivered by the previous call.
		q.p.Quit()
		return 0, q.pending
	}
	n, err := q.r.Read(b)
	if errors.Is(err, io.EOF) {
		// bubbletea discards bytes returned together with an error, so
		// deliver them first and report EOF on the next call.
		q.pending = err
		if n > 0 {
			return n, nil
		}
		// The message channel is unbuffered: every key parsed from earlier
		// reads has already been received by the event loop, so QuitMsg is
		// handled after them.
		q.p.Quit()
	}
	return n, err
}

// ErrInputNotTerminal is returned by Run for an *os.File input that is not a
// terminal.
var ErrInputNotTerminal = errors.New("tui: input file is not a terminal")
