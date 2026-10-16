package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// Run runs m as a bubbletea program reading keys from in and rendering to
// out, and returns the final model. Extra options are appended to the ones
// set by Run (input, output, color profile; see NewProgram).
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
//     CLI mode for non-terminal stdin. Known limits of bubbletea v2's input
//     parser (ultraviolet) on a terminal: a key typed after the one that
//     quits, while the program shuts down, delays the end of Run by up to
//     500 ms (the parser blocks handing it over, and bubbletea waits that
//     long for its read loop); a bracketed paste containing ESC P, ESC X,
//     ESC ^ or ESC _ (the start of a string sequence it never sees
//     terminated) stops the parser: no further key reaches the program,
//     ctrl+c included, and only a signal (SIGINT from elsewhere, SIGTERM,
//     SIGHUP) through the caller's handling ends it.
//   - Any other reader is meant for in-memory input, i.e. tests. The program
//     quits once it reaches EOF, after the keys read before it have been
//     processed. The final model then reports neither confirmation nor
//     cancellation, which callers must treat as an abort. Another read error
//     kills the program (Run returns tea.ErrProgramKilled). The input is
//     parsed by bubbletea's parser (ultraviolet), as a terminal's: one key
//     event per rune (grapheme), a bracketed paste as one tea.PasteMsg, and a
//     lone ESC at the end of a read held back for the escape timeout (50 ms;
//     if the next read arrives within it, ESC followed by a key is alt+key).
//     Reading starts when the program is created, and the parsed events are
//     sent to the program (Program.Send) once it runs: the program must be
//     run or killed. A reader with a method NextSegment() io.Reader holds
//     the input of successive programs: each program reads the reader that
//     NextSegment returns when the program is created.
//
// The output determines the size: on a terminal, the program receives its
// size (tea.WindowSizeMsg) as usual. On any other writer, the size is
// unknown, as with bubbletea v1: bubbletea does not send the model a
// tea.WindowSizeMsg (only messages sent with Program.Send reach it), and the
// view is rendered on a virtual screen of virtualWidth columns. The startup
// size message is removed by a filter (tea.WithFilter), which replaces any
// filter of the caller.
//
// The color profile is detected from out and the environment; colors (not
// text attributes) are disabled when NO_COLOR is set to a non-empty value or
// CLICOLOR=0 (see colorProfile). Pass tea.WithColorProfile to override it.
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

// virtualWidth and virtualHeight are the screen size used to render to an
// output that is not a terminal (inline views are not cut to the height).
// The startup tea.WindowSizeMsg with this size is filtered out.
const virtualWidth, virtualHeight = 256, 100

// segmentedInput is an in-memory input (tests) holding the input of several
// programs in turn. NewProgram claims the next segment when it creates a
// program, before a later program can exist: the reader of a program keeps
// reading after the program has quit (until EOF, see feedKeys), and with a
// shared reader it could take the input meant for the next program.
type segmentedInput interface {
	NextSegment() io.Reader
}

// NewProgram returns the program Run would run, with the same handling of
// in (including ErrInputNotTerminal) and out, for callers that need the
// program itself, e.g. to Send it messages from other goroutines while it
// runs.
func NewProgram(m tea.Model, in io.Reader, out io.Writer, opts ...tea.ProgramOption) (*tea.Program, error) {
	var keys io.Reader // in-memory input, fed to the program by feedKeys
	switch r := in.(type) {
	case nil:
	case *os.File:
		if !isTerminalFile(r) {
			return nil, ErrInputNotTerminal
		}
	default:
		if f, ok := r.(term.File); !ok || !term.IsTerminal(f.Fd()) {
			keys, in = in, nil
			if s, ok := keys.(segmentedInput); ok {
				keys = s.NextSegment()
			}
		}
	}
	base := []tea.ProgramOption{
		tea.WithInput(in), tea.WithOutput(out),
		tea.WithColorProfile(colorProfile(out, os.Environ())),
	}
	sizeKnown := isTerminalWriter(out)
	if !sizeKnown {
		base = append(base, tea.WithWindowSize(virtualWidth, virtualHeight))
	}
	f := &filter{virtualSize: !sizeKnown, drain: in != nil}
	opts = append(base, opts...)
	if f.virtualSize || f.drain {
		opts = append(opts, tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return f.filter(msg) }))
	}
	p := tea.NewProgram(m, opts...)
	f.p = p
	if keys != nil {
		go feedKeys(p, keys)
	}
	return p, nil
}

// drainTimeout bounds the wait for the terminal's answer when a program
// quits (see filter).
const drainTimeout = 300 * time.Millisecond

// drainRounds is the number of primary device attributes queries a quitting
// program waits for (see filter).
const drainRounds = 2

// drainTimeoutMsg ends the wait for the terminal's answers.
type drainTimeoutMsg struct{}

// filter is the message filter (tea.WithFilter) of a program; it runs on
// the program's event loop.
//
// With virtualSize, it removes the startup tea.WindowSizeMsg of the virtual
// screen (the first message of that size).
//
// With drain (input from a terminal), it keeps a quitting program reading
// its input until the terminal has answered every query sent before:
// bubbletea queries the terminal at startup (DECRQM 2026 and 2027) and with
// the first frame (kitty keyboard flags), and the answers to queries still
// unanswered when the program ends stay in the terminal input, where the
// shell (or the next reader) gets them as typed text. On tea.QuitMsg, the
// filter sends a primary device attributes query (DA1), which every
// terminal answers, after the earlier ones since terminals answer in order;
// the program quits when the answer arrives, or after drainTimeout. Two
// rounds: the first frame may be written right after the first query, in
// the same flush. Messages arriving meanwhile still reach the model, which
// must ignore them once it has quit (the models of this package do). A
// program ended by its context or Program.Kill is not drained.
type filter struct {
	p           *tea.Program
	virtualSize bool
	drain       bool

	sizeDropped bool
	draining    bool
	answers     int
}

func (f *filter) filter(msg tea.Msg) tea.Msg {
	switch msg.(type) {
	case tea.WindowSizeMsg:
		if f.virtualSize && !f.sizeDropped && msg == (tea.WindowSizeMsg{Width: virtualWidth, Height: virtualHeight}) {
			f.sizeDropped = true
			return nil
		}
	case tea.QuitMsg:
		if !f.drain {
			return msg
		}
		if !f.draining {
			f.draining = true
			p := f.p
			time.AfterFunc(drainTimeout, func() { p.Send(drainTimeoutMsg{}) })
			return tea.RawMsg{Msg: ansi.RequestPrimaryDeviceAttributes}
		}
		return nil // already draining
	case uv.PrimaryDeviceAttributesEvent:
		if f.draining {
			if f.answers++; f.answers < drainRounds {
				return tea.RawMsg{Msg: ansi.RequestPrimaryDeviceAttributes}
			}
			return tea.QuitMsg{}
		}
	case drainTimeoutMsg:
		return tea.QuitMsg{}
	}
	return msg
}

// feedKeys parses the in-memory input r like bubbletea parses a terminal's,
// sends the events to p, and quits p at EOF. bubbletea does not read r
// itself: its parser sends events to the program without watching for the
// program's end, and the goroutine sending an event read after the program
// ended (e.g. a key following the one that quits) would block forever,
// keeping the program in memory. p.Send blocks until p runs and is a no-op
// once p has ended, so the events reach p in order, before the quit, and
// none is left blocked.
func feedKeys(p *tea.Program, r io.Reader) {
	events := make(chan uv.Event)
	done := make(chan error, 1)
	go func() {
		// Background context: StreamEvents returns at EOF (nil error) or on a
		// read error, after sending the events of the bytes read before.
		done <- uv.NewTerminalReader(&errAfterData{r: r}, os.Getenv("TERM")).StreamEvents(context.Background(), events)
	}()
	for {
		select {
		case ev := <-events:
			p.Send(ev)
		case err := <-done: // every event has been received: events is unbuffered
			if err != nil {
				p.Send(nil) // returns once p runs (or has ended); nil messages are dropped
				p.Kill()
				return
			}
			p.Quit()
			return
		}
	}
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

// isTerminalWriter reports whether w is a terminal, as bubbletea checks it
// (an *os.File is checked without calling Fd, see isTerminalFile).
func isTerminalWriter(w io.Writer) bool {
	switch f := w.(type) {
	case *os.File:
		return isTerminalFile(f)
	case term.File:
		return term.IsTerminal(f.Fd())
	}
	return false
}

// colorProfile returns the color profile used to render to out: the one
// detected by colorprofile from out and environ, limited to ASCII (no colors,
// text attributes kept) when NO_COLOR is set to any non-empty value
// (https://no-color.org) or CLICOLOR=0 without CLICOLOR_FORCE. colorprofile
// itself only honours NO_COLOR values parsed as true, and ignores CLICOLOR=0.
func colorProfile(out io.Writer, environ []string) colorprofile.Profile {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if f, ok := out.(*os.File); ok && !isTerminalFile(f) {
		// Detect would call f.Fd (see isTerminalFile).
		out = io.Discard
	}
	p := colorprofile.Detect(out, environ)
	forced := env["CLICOLOR_FORCE"] != "" && env["CLICOLOR_FORCE"] != "0"
	if (env["NO_COLOR"] != "" || (env["CLICOLOR"] == "0" && !forced)) && p > colorprofile.ASCII {
		p = colorprofile.ASCII
	}
	return p
}

// errAfterData returns the bytes that a Read of r returns together with an
// error (e.g. io.EOF) first, and the error on the next call: the parser
// discards bytes returned with an error. Read is only called from the
// parser's read goroutine.
type errAfterData struct {
	r   io.Reader
	err error
}

func (e *errAfterData) Read(b []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.r.Read(b)
	if err != nil && n > 0 {
		e.err = err
		return n, nil
	}
	return n, err
}

// ErrInputNotTerminal is returned by Run for an *os.File input that is not a
// terminal.
var ErrInputNotTerminal = errors.New("tui: input file is not a terminal")
