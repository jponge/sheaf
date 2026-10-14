package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
)

// chunkReader returns one chunk per Read call, then io.EOF. bubbletea parses
// each Read result on its own (except for a trailing lone ESC, held back for
// its escape timeout), so one chunk is one key press, or one per rune.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

const runDeadline = 5 * time.Second

// runBounded runs m through Run and returns whatever Run returns. The program
// is killed through its context after runDeadline, so a hang does not leak a
// running program; the outer select is a backstop in case the kill itself
// does not make Run return.
func runBounded(t *testing.T, m tea.Model, in io.Reader, out io.Writer) (tea.Model, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runDeadline)
	defer cancel()
	type result struct {
		m   tea.Model
		err error
	}
	done := make(chan result, 1)
	go func() {
		final, err := Run(m, in, out, tea.WithContext(ctx))
		done <- result{final, err}
	}()
	select {
	case r := <-done:
		return r.m, r.err
	case <-time.After(2 * runDeadline):
		t.Fatalf("Run did not return within %v even after its context was cancelled", 2*runDeadline)
		return nil, nil
	}
}

// runWithTimeout runs m and fails the test when Run errors or does not
// return within runDeadline.
func runWithTimeout(t *testing.T, m tea.Model, in io.Reader) (tea.Model, string) {
	t.Helper()
	var out bytes.Buffer
	final, err := runBounded(t, m, in, &out)
	if errors.Is(err, tea.ErrProgramKilled) {
		t.Fatalf("Run did not return within %v: %v", runDeadline, err)
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return final, out.String()
}

func TestRunMultiSelectWithScriptedKeys(t *testing.T) {
	in := &chunkReader{chunks: []string{" ", "j", "\x1b[B", " ", "\r"}} // space, j, down arrow, space, enter
	m, out := runWithTimeout(t, NewMultiSelect("Pick repositories", abc()), in)
	ms := asMS(t, m)
	if !ms.Confirmed() {
		t.Fatalf("not confirmed; output:\n%s", out)
	}
	assertKeys(t, m, "a", "c")
	if !strings.Contains(out, "Pick repositories") {
		t.Fatalf("output lacks the title:\n%s", out)
	}
}

func TestRunMultiSelectFilterThroughInput(t *testing.T) {
	// "br" arrives in one read (one key event per rune).
	in := &chunkReader{chunks: []string{"/", "br", "\r", " ", "\r"}}
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), in)
	if !asMS(t, m).Confirmed() {
		t.Fatal("not confirmed")
	}
	assertKeys(t, m, "b")
}

func TestRunMultiSelectEscCancels(t *testing.T) {
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), &chunkReader{chunks: []string{" ", "\x1b"}})
	if !asMS(t, m).Cancelled() {
		t.Fatal("lone ESC did not cancel")
	}
}

// A plain strings.Reader delivers everything in one Read, from which
// bubbletea emits one key event per rune.
func TestRunWithSingleReadInput(t *testing.T) {
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), strings.NewReader(" j \r"))
	if !asMS(t, m).Confirmed() {
		t.Fatal("not confirmed")
	}
	assertKeys(t, m, "a", "b")
}

func TestRunConfirm(t *testing.T) {
	m, _ := runWithTimeout(t, NewConfirm("Remove?", []string{"x"}), strings.NewReader("y"))
	if !asConfirm(t, m).Confirmed() {
		t.Fatal("y did not confirm")
	}
}

func TestRunReturnsWhenInputEnds(t *testing.T) {
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), strings.NewReader(" "))
	ms := asMS(t, m)
	if ms.Confirmed() || ms.Cancelled() {
		t.Fatalf("Confirmed=%v Cancelled=%v, want neither", ms.Confirmed(), ms.Cancelled())
	}
	assertKeys(t, m, "a") // the key read before EOF was processed
}

func TestRunInputEndingWithDataAndEOF(t *testing.T) {
	// DataErrReader returns the final bytes together with io.EOF.
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), iotest.DataErrReader(strings.NewReader(" ")))
	ms := asMS(t, m)
	if ms.Confirmed() || ms.Cancelled() {
		t.Fatalf("Confirmed=%v Cancelled=%v, want neither", ms.Confirmed(), ms.Cancelled())
	}
	assertKeys(t, m, "a")
}

func TestRunMultiRuneKeyInListMode(t *testing.T) {
	m, _ := runWithTimeout(t, NewMultiSelect("Pick", abc()), strings.NewReader("jj \r"))
	if !asMS(t, m).Confirmed() {
		t.Fatal("not confirmed")
	}
	assertKeys(t, m, "c")
}

// newPipe returns an os.Pipe whose ends are closed at the end of the test.
// Closing the write end also releases any goroutine still reading r.
func newPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	return r, w
}

func TestRunRejectsNonTerminalFile(t *testing.T) {
	r, w := newPipe(t)
	if _, err := w.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	_, err := runBounded(t, NewConfirm("Remove?", nil), r, io.Discard)
	if !errors.Is(err, ErrInputNotTerminal) {
		t.Fatalf("Run on a pipe: err = %v, want ErrInputNotTerminal", err)
	}
}

// After Run returns, input written to a caller's file must reach the caller,
// not a read-loop goroutine left behind by bubbletea.
func TestRunDoesNotSwallowFileInputAfterReturn(t *testing.T) {
	r, w := newPipe(t)
	if _, err := w.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	_, _ = runBounded(t, NewConfirm("Remove?", nil), r, io.Discard) // outcome irrelevant here
	if _, err := w.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	// Read in a goroutine: r may have been switched to blocking mode, so a
	// read deadline cannot be relied on. Closing w in cleanup releases it.
	got := make(chan []byte, 1)
	go func() {
		var all []byte
		buf := make([]byte, 16)
		for !bytes.Contains(all, []byte("z")) {
			n, err := r.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- all
	}()
	select {
	case b := <-got:
		if !bytes.Contains(b, []byte("z")) {
			t.Fatalf("caller read %q, want the byte written after Run", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("byte written after Run was not readable by the caller within 2s")
	}
}

// os.File.Fd switches a file to blocking mode, after which read deadlines no
// longer take effect (os.File.Fd docs). Run must not do that to a file it does
// not hand to bubbletea.
func TestRunKeepsNonTerminalFileNonBlocking(t *testing.T) {
	r, _ := newPipe(t)
	_, _ = runBounded(t, NewConfirm("Remove?", nil), r, io.Discard) // outcome irrelevant here
	if err := r.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("read deadline after Run: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 1)) // the pipe is empty
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read on empty pipe: %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read deadline ignored after Run: the pipe was switched to blocking mode")
	}
}

// NewProgram applies Run's input handling and returns the program, so that
// callers can Send messages to it while it runs.
func TestNewProgramSendsAndHonoursInputRules(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := NewProgram(NewConfirm("q", nil), r, io.Discard); !errors.Is(err, ErrInputNotTerminal) {
		t.Fatalf("pipe input: err = %v, want ErrInputNotTerminal", err)
	}

	p, err := NewProgram(NewProgress("t"), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	go p.Send(RowUpdate{Key: "a", Label: "alpha", State: RowDone})
	go func() { time.Sleep(50 * time.Millisecond); p.Send(FinishMsg{}) }()
	final, err := p.Run()
	if err != nil {
		t.Fatal(err)
	}
	if pr := final.(Progress); !pr.Finished() {
		t.Fatal("not finished")
	}

	// In-memory input still quits at EOF.
	p, err = NewProgram(NewConfirm("q", nil), strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(runDeadline):
		p.Kill()
		t.Fatal("program did not quit at EOF")
	}
}

// NO_COLOR set to any non-empty value (https://no-color.org) or CLICOLOR=0
// disables colors but keeps text attributes (bold, faint: the ASCII
// profile); otherwise the profile is detected from the output and TERM.
// TTY_FORCE=1 makes colorprofile treat the buffer as a terminal.
func TestColorProfileHonoursNoColor(t *testing.T) {
	tty := []string{"TERM=xterm-256color", "TTY_FORCE=1"}
	for _, tc := range []struct {
		env  []string
		want func(colorprofile.Profile) bool
		desc string
	}{
		{tty, func(p colorprofile.Profile) bool { return p > colorprofile.ASCII }, "colors"},
		{append(tty, "NO_COLOR=1"), isASCII, "ASCII"},
		{append(tty, "NO_COLOR=yes"), isASCII, "ASCII"},
		{append(tty, "CLICOLOR=0"), isASCII, "ASCII"},
		{append(tty, "CLICOLOR=0", "CLICOLOR_FORCE=1"), func(p colorprofile.Profile) bool { return p > colorprofile.ASCII }, "colors"},
		{[]string{"TERM=xterm-256color"}, func(p colorprofile.Profile) bool { return p == colorprofile.NoTTY }, "NoTTY"}, // not a terminal
	} {
		if p := colorProfile(&bytes.Buffer{}, tc.env); !tc.want(p) {
			t.Errorf("%v: profile %v, want %s", tc.env, p, tc.desc)
		}
	}
}

func isASCII(p colorprofile.Profile) bool { return p == colorprofile.ASCII }

// A program fed in-memory input must not leave goroutines (and thereby
// itself) behind when input continues after the key that quits it.
func TestRunLeavesNoGoroutineBehind(t *testing.T) {
	// Warm-up: process-wide goroutines (os/signal's loop) start once.
	runWithTimeout(t, NewConfirm("Remove?", nil), strings.NewReader("y"))
	before := runtime.NumGoroutine()
	for range 10 {
		// y quits; the keys after it are parsed after the program ended.
		m, _ := runWithTimeout(t, NewConfirm("Remove?", nil), strings.NewReader("yjjj\x1b"))
		if !asConfirm(t, m).Confirmed() {
			t.Fatal("y did not confirm")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines before, %d after 10 programs:\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On an output that is not a terminal, only the first message of the
// virtual size (sent by bubbletea at startup) is removed: the same size sent
// with Program.Send reaches the model.
func TestFilterDropsOnlyTheStartupVirtualSize(t *testing.T) {
	f := &filter{virtualSize: true}
	size := tea.WindowSizeMsg{Width: virtualWidth, Height: virtualHeight}
	if got := f.filter(size); got != nil {
		t.Fatalf("first virtual size: %v, want dropped", got)
	}
	if got := f.filter(size); got != size {
		t.Fatalf("second virtual size: %v, want kept", got)
	}
	if got := f.filter(tea.WindowSizeMsg{Width: 80, Height: 24}); got != (tea.WindowSizeMsg{Width: 80, Height: 24}) {
		t.Fatalf("other size: %v", got)
	}
}

// With terminal input, a quit is delayed until the terminal has answered
// two primary device attributes queries (or the timeout).
func TestFilterDrainsTerminalRepliesBeforeQuitting(t *testing.T) {
	p := tea.NewProgram(NewConfirm("q", nil), tea.WithInput(nil), tea.WithOutput(io.Discard))
	t.Cleanup(p.Kill) // the drain timers Send to p: a no-op once killed
	da1 := tea.RawMsg{Msg: "\x1b[c"}
	f := &filter{p: p, drain: true}
	if got := f.filter(tea.QuitMsg{}); got != da1 {
		t.Fatalf("quit: %#v, want the DA1 query", got)
	}
	if got := f.filter(tea.QuitMsg{}); got != nil {
		t.Fatalf("second quit while draining: %#v, want dropped", got)
	}
	if got := f.filter(tea.KeyPressMsg{Code: 'x', Text: "x"}); got != (tea.KeyPressMsg{Code: 'x', Text: "x"}) {
		t.Fatalf("key while draining: %#v, want passed", got)
	}
	if got := f.filter(uv.PrimaryDeviceAttributesEvent{62, 22}); got != da1 {
		t.Fatalf("first answer: %#v, want a second DA1 query", got)
	}
	if got := f.filter(uv.PrimaryDeviceAttributesEvent{62, 22}); got != (tea.QuitMsg{}) {
		t.Fatalf("second answer: %#v, want quit", got)
	}

	f = &filter{p: p, drain: true}
	f.filter(tea.QuitMsg{})
	if got := f.filter(drainTimeoutMsg{}); got != (tea.QuitMsg{}) {
		t.Fatalf("timeout: %#v, want quit", got)
	}
	// Without terminal input, quitting is immediate.
	if got := (&filter{}).filter(tea.QuitMsg{}); got != (tea.QuitMsg{}) {
		t.Fatalf("no drain: %#v", got)
	}
}
