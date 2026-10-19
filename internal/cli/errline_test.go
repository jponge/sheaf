package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
)

// The places that show an error on one line (overview message, progress
// rows, summary lines) show the Summary of a multi-line refusal, else its
// first line without a trailing colon (B52 A5).

func risksError() error {
	return &ops.RisksError{Plan: ops.RemovePlan{Repo: "old", Items: []ops.RemoveItem{
		{Path: "/b/old/main", Risks: []gitx.Risk{{Kind: gitx.RiskUntracked, Count: 1}}}}}}
}

func TestErrorLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		line string
		more bool
	}{
		{"one line", errors.New("boom: it failed"), "boom: it failed", false},
		{"one line ending in a colon", errors.New("boom:"), "boom:", false},
		{"several lines", errors.New("2 problem(s):\n  a\n  b"), "2 problem(s)", true},
		{"refusal", risksError(), "refusing to remove old: 1 untracked file", true},
		{"wrapped refusal", fmt.Errorf("sync: %w", risksError()), "refusing to remove old: 1 untracked file", true},
	} {
		line, more := errorLine(tc.err)
		if line != tc.line || more != tc.more {
			t.Errorf("%s: errorLine = %q, %v; want %q, %v", tc.name, line, more, tc.line, tc.more)
		}
	}
}

func TestPrintSummaryShowsRefusalSummary(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	_ = printSummary(&b, ops.Summary{Results: []ops.Result{{Repo: "old", Status: ops.StatusFailed, Err: risksError()}}})
	if want := "failed   old: refusing to remove old: 1 untracked file\n"; !strings.HasPrefix(b.String(), want) {
		t.Fatalf("printSummary =\n%s\nwant it to start with\n%s", b.String(), want)
	}
}

// The overview message after a refused wt rm says what is at risk and
// points to the report, which has the details.
func TestHomeRefusedRemovalMessageSummarizes(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), keys(), reportKey(t, &out, "wt rm", "x"), ovKeys(t, &out, "wt rm failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if want := "wt rm failed: refusing to remove alpha/feat: 1 untracked file (see report)"; code != 0 || !strings.Contains(out.String(), want) {
		t.Fatalf("exit %d, want the message %q\nstdout:\n%s\nstderr:\n%s", code, want, out.String(), errs)
	}
}
