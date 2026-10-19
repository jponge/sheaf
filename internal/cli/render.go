package cli

import (
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
		if e.Err != nil {
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
// the summary's error (non-nil when a repository failed).
func printSummary(w io.Writer, s ops.Summary) error {
	for _, r := range s.Results {
		var status, detail string
		switch r.Status {
		case ops.StatusDone:
			status, detail = "done", r.Message
		case ops.StatusSkipped:
			status, detail = "skipped", r.Message
		default:
			status = "failed"
			if r.Err != nil {
				detail = firstLine(r.Err.Error())
			}
		}
		fmt.Fprintf(w, "%-8s %s", status, r.Repo)
		if detail != "" {
			fmt.Fprintf(w, ": %s", detail)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d done, %d skipped, %d failed\n", s.Count(ops.StatusDone), s.Count(ops.StatusSkipped), s.Count(ops.StatusFailed))
	return s.Err()
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
	"WARN": tui.LevelWarn, "warning:": tui.LevelWarn,
	"FAIL": tui.LevelFail, "failed": tui.LevelFail,
}

// levelLine matches a line starting with a level word followed by a space:
// group 1 is a word of doctor or of a summary, at the start of the line;
// group 2 a warning or note marker, at the start of the line or after
// indentation, "sheaf: " or the "<who>: <phase>: " of an event.
var levelLine = regexp.MustCompile(`^(?:(OK|INFO|WARN|FAIL|done|fast-forwarded|up to date|skipped|pinned|failed)|(?: +|sheaf: |\S+: \S+: )?(warning:|note:)) `)

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
// levelMark) rendered by paint. A line written in several parts is
// classified by the part that starts it, which must hold its level word
// and the space after it (as the commands print them).
type levelWriter struct {
	mu    sync.Mutex
	w     io.Writer
	paint func(tui.Level, string) string
	mid   bool // the last write ended inside a line
}

func (l *levelWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for rest := string(p); rest != ""; {
		line, after, nl := strings.Cut(rest, "\n")
		if k := levelMark(line); !l.mid && k.Level != tui.LevelNone {
			line = line[:k.Start] + l.paint(k.Level, line[k.Start:k.End]) + line[k.End:]
		}
		b.WriteString(line)
		if nl {
			b.WriteByte('\n')
		}
		l.mid, rest = !nl, after
	}
	if _, err := io.WriteString(l.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
