package cli

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

// plainRenderer prints one line per event (CLI mode). ops serializes the
// calls, so no locking is needed.
type plainRenderer struct{ w io.Writer }

func (p plainRenderer) Report(e ops.Event) { fmt.Fprintln(p.w, formatEvent(e)) }

func formatEvent(e ops.Event) string {
	who := e.Repo
	if e.Worktree != "" {
		who += "/" + e.Worktree
	}
	var line string
	switch e.Kind {
	case ops.KindStart:
		line = fmt.Sprintf("%s: %s started", who, e.Phase)
	case ops.KindProgress:
		line = fmt.Sprintf("%s: %s: %s", who, e.Phase, e.Message)
	case ops.KindSkipped:
		line = fmt.Sprintf("%s: %s skipped: %s", who, e.Phase, e.Message)
	case ops.KindFailed:
		line = fmt.Sprintf("%s: %s failed", who, e.Phase)
		if interruptedErr(e.Err) {
			line = fmt.Sprintf("%s: %s interrupted", who, e.Phase)
			if interruptedNote(e.Message) != "" {
				line = fmt.Sprintf("%s: %s not started (interrupted)", who, e.Phase)
			}
		} else if e.Err != nil {
			line += ": " + e.Err.Error()
		} else if e.Message != "" {
			line += ": " + e.Message
		}
	default:
		line = fmt.Sprintf("%s: %s %s", who, e.Phase, e.Kind)
		if e.Message != "" {
			line += ": " + e.Message
		}
	}
	return line
}

// printSummary writes one line per result and the counts to w, and returns
// the summary's error (non-nil when a repository failed or was
// interrupted). Failures caused by an interruption (see interruptedErr)
// are counted as interrupted, without their error.
func printSummary(w io.Writer, s ops.Summary) error {
	// The status column fits "skipped" and a space, or "interrupted".
	interrupted, width := 0, 8
	for _, r := range s.Results {
		if r.Status == ops.StatusFailed && interruptedErr(r.Err) {
			interrupted, width = interrupted+1, len("interrupted")
		}
	}
	for _, r := range s.Results {
		var status, detail string
		switch {
		case r.Status == ops.StatusDone:
			status, detail = "done", r.Message
		case r.Status == ops.StatusSkipped:
			status, detail = "skipped", r.Message
		case interruptedErr(r.Err):
			status, detail = "interrupted", interruptedNote(r.Message)
		default:
			status = "failed"
			if r.Err != nil {
				detail = errorText(r.Err)
			}
		}
		fmt.Fprintf(w, "%-*s %s", width, status, r.Repo)
		if detail != "" {
			fmt.Fprintf(w, ": %s", detail)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d done, %d skipped, %d failed", s.Count(ops.StatusDone), s.Count(ops.StatusSkipped), s.Count(ops.StatusFailed)-interrupted)
	if interrupted > 0 {
		fmt.Fprintf(w, ", %d interrupted", interrupted)
	}
	fmt.Fprintln(w)
	return s.Err()
}

// interruptedErr reports whether err is the failure of work stopped by an
// interruption (see ops.Interrupted; the command's context is cancelled by
// interruptCtl, and by pipeWatch for a closed standard output).
func interruptedErr(err error) bool { return ops.Interrupted(err) }

// interruptedNote returns what is said of work stopped by an interruption
// besides "interrupted", from its message: "not started" when it never
// started, else nothing (its error, e.g. a git command line ending in
// "context canceled", is not shown).
func interruptedNote(msg string) string {
	if strings.HasPrefix(msg, "not started") {
		return "not started"
	}
	return ""
}

// errorLine returns err on one line, for the places that show one (the
// overview message, progress rows, summary lines), and whether its full
// text says more (the caller prints it elsewhere or points to it): the
// Summary of a multi-line refusal (ops.RisksError,
// ops.WorktreeRefusedError), wrapped or not, else the first line, without
// a trailing colon when more lines follow (their introduction).
func errorLine(err error) (line string, more bool) {
	msg := err.Error()
	var s interface{ Summary() string }
	if errors.As(err, &s) {
		return s.Summary(), true
	}
	line = firstLine(msg)
	if line == msg {
		return line, false
	}
	return strings.TrimSuffix(line, ":"), true
}

// errorText is the line of errorLine.
func errorText(err error) string {
	line, _ := errorLine(err)
	return line
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// levelWords are the level words of the lines commands print, by level:
// doctor's levels, the outcomes of summaries (printSummary, update, prune)
// and the markers of warnings and notes.
var levelWords = map[string]tui.Level{
	"OK": tui.LevelOK, "done": tui.LevelOK, "fast-forwarded": tui.LevelOK, "up to date": tui.LevelOK,
	"INFO": tui.LevelInfo, "skipped": tui.LevelInfo, "pinned": tui.LevelInfo, "note:": tui.LevelInfo,
	"WARN": tui.LevelWarn, "warning:": tui.LevelWarn, "interrupted": tui.LevelWarn,
	"FAIL": tui.LevelFail, "failed": tui.LevelFail,
}

// levelLine matches a line starting with a level word followed by a space:
// group 1 is a word of doctor or of a summary, at the start of the line;
// group 2 a warning or note marker, at the start of the line or after
// indentation, "sheaf: " or the "<who>: <phase>: " of an event.
var levelLine = regexp.MustCompile(`^(?:(OK|INFO|WARN|FAIL|done|fast-forwarded|up to date|skipped|pinned|failed|interrupted)|(?: +|sheaf: |\S+: \S+: )?(warning:|note:)) `)

// levelMark marks the level word of a line printed by a command (see
// levelWords); none for other lines.
func levelMark(line string) tui.Mark {
	m := levelLine.FindStringSubmatchIndex(line)
	if m == nil {
		return tui.Mark{}
	}
	for g := 1; g <= 2; g++ {
		if start, end := m[2*g], m[2*g+1]; start >= 0 {
			return tui.Mark{Start: start, End: end, Level: levelWords[line[start:end]]}
		}
	}
	return tui.Mark{}
}

// levelWriter writes to w, with the level word of each line (see
// levelMark) rendered by paint. A line written in several parts (e.g. by a
// tabwriter, one cell at a time) is classified as soon as its start decides
// it: the start of a line is held back while it is a strict prefix of a
// level word of doctor or of a summary followed by its space (at most 15
// bytes, e.g. "up to date" before its padding), and written out as soon as
// it is anything else, so that text without a newline, such as a prompt,
// is never held back unless it reads like the start of a level word. The
// warning and note markers are classified on lines written whole, as the
// commands print them. Flush writes a held start out unclassified; nothing
// is held across a newline.
type levelWriter struct {
	mu    sync.Mutex
	w     io.Writer
	paint func(tui.Level, string) string
	mid   bool   // the last write ended inside a line (after its start)
	held  string // start of the current line, held back
}

func (l *levelWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	rest := l.held + string(p)
	l.held = ""
	for rest != "" {
		line, after, nl := strings.Cut(rest, "\n")
		switch k := levelMark(line); {
		case l.mid:
		case k.Level != tui.LevelNone:
			line = line[:k.Start] + l.paint(k.Level, line[k.Start:k.End]) + line[k.End:]
		case !nl && levelWordPrefix(line):
			l.held = line
			line = ""
		}
		b.WriteString(line)
		if nl {
			b.WriteByte('\n')
		}
		l.mid, rest = !nl && l.held == "", after
	}
	if _, err := io.WriteString(l.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes out the start of a line held back, unclassified.
func (l *levelWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held != "" {
		_, _ = io.WriteString(l.w, l.held)
		l.held, l.mid = "", true
	}
}

// levelWordPrefix reports whether s is a strict prefix of a level word of
// doctor or of a summary followed by a space.
func levelWordPrefix(s string) bool {
	for w := range levelWords {
		if !strings.HasSuffix(w, ":") && len(s) <= len(w) && strings.HasPrefix(w, s) {
			return true
		}
	}
	return false
}

// flush flushes w if it is a levelWriter.
func flush(w io.Writer) {
	if l, ok := w.(*levelWriter); ok {
		l.Flush()
	}
}
