package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/jponge/sheaf/internal/ops"
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
