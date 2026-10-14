package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// chunkReader returns one chunk per Read call, then io.EOF. bubbletea parses
// each Read result on its own, so one chunk is one key press.
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
	// "br" arrives as a single multi-rune key event, as with a paste.
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

// A plain strings.Reader delivers everything in one Read. bubbletea merges
// consecutive printable runes into one multi-rune key event, which the list
// mode of MultiSelect processes rune by rune.
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
