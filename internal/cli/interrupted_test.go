package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
)

// Work stopped by an interruption (its error wraps context.Canceled) is
// reported as interrupted, not failed, without the git command line nor
// "context canceled", and counted apart (B52 A6).

func cancelledGit() error {
	return &gitx.GitError{Args: []string{"fetch", "--quiet", "--prune", "origin"}, Dir: "/b/a/main", ExitCode: -1, Err: context.Canceled}
}

func TestPlainEventInterrupted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		e    ops.Event
		want string
	}{
		{ops.Event{Repo: "a", Phase: ops.PhaseFetch, Kind: ops.KindFailed, Err: cancelledGit()}, "a: fetch interrupted"},
		{ops.Event{Repo: "a", Worktree: "main", Phase: ops.PhaseUpdate, Kind: ops.KindFailed, Message: "fetch interrupted", Err: cancelledGit()}, "a/main: update interrupted"},
		{ops.Event{Repo: "a", Phase: ops.PhaseClone, Kind: ops.KindFailed, Message: "not started", Err: context.Canceled}, "a: clone not started (interrupted)"},
	} {
		if got := formatEvent(tc.e); got != tc.want {
			t.Errorf("formatEvent(%+v) = %q, want %q", tc.e, got, tc.want)
		}
	}
}

func TestPrintSummaryInterrupted(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	err := printSummary(&b, ops.Summary{Results: []ops.Result{
		{Repo: "a", Status: ops.StatusFailed, Err: cancelledGit()},
		{Repo: "b", Status: ops.StatusFailed, Err: context.Canceled},
		{Repo: "c", Status: ops.StatusDone, Message: "fetched origin"},
	}})
	want := "interrupted a\ninterrupted b\ndone        c: fetched origin\n1 done, 2 interrupted\n"
	if b.String() != want {
		t.Errorf("printSummary =\n%s\nwant\n%s", b.String(), want)
	}
	if err == nil {
		t.Error("no error: the exit status would not tell the interruption")
	}
}

func TestUpdateSummaryInterrupted(t *testing.T) {
	t.Parallel()
	r := ops.UpdateReport{Checkouts: []ops.CheckoutUpdate{
		{Checkout: ops.Checkout{Repo: "a", Branch: "main", Dir: "main"}, Outcome: gitx.PullFailed, Reason: "fetch interrupted", Err: cancelledGit()},
		{Checkout: ops.Checkout{Repo: "b", Branch: "main", Dir: "main"}, Outcome: gitx.PullFailed, Reason: "not started (interrupted)", Err: context.Canceled},
		{Checkout: ops.Checkout{Repo: "c", Branch: "main", Dir: "main"}, Outcome: gitx.PullUpToDate},
	}}
	var b bytes.Buffer
	err := printUpdateSummary(&b, r, false)
	want := "interrupted  a/main\ninterrupted  b/main  not started\nup to date   c/main\n1 up to date, 2 interrupted\n"
	if got := b.String(); got != want {
		t.Errorf("printUpdateSummary =\n%q\nwant\n%q", got, want)
	}
	if err == nil {
		t.Error("no error: the exit status would not tell the interruption")
	}
}

func TestPrintPruneInterrupted(t *testing.T) {
	t.Parallel()
	c := ops.PruneCandidate{Repo: "a", Branch: "feat", Worktree: true, Dir: "feat"}
	var b bytes.Buffer
	printPrune(&b, ops.PrunePlan{Candidates: []ops.PruneCandidate{c, c}}, []ops.PruneResult{
		{PruneCandidate: c, Outcome: ops.PruneRemoved, Message: "removed"},
		{PruneCandidate: c, Outcome: ops.PruneFailed, Message: "not started (interrupted)", Err: context.Canceled},
	}, false, "")
	want := "a/feat: removed\na/feat: interrupted: not started\n1 removed, 1 interrupted\n"
	if b.String() != want {
		t.Errorf("printPrune =\n%s\nwant\n%s", b.String(), want)
	}
}

// An interrupted run counts its commands stopped or not started by the
// interruption apart, without "context canceled" (B52 U2 review).
func TestRunSummaryInterrupted(t *testing.T) {
	t.Parallel()
	co := func(repo string) ops.Checkout { return ops.Checkout{Repo: repo, Dir: "main", Branch: "main"} }
	r := ops.RunReport{Results: []ops.RunResult{
		{Checkout: co("a"), Started: true},
		{Checkout: co("b"), Started: true, ExitCode: 3},
		{Checkout: co("c"), Started: true, ExitCode: -1, Err: fmt.Errorf("%w (%v)", context.Canceled, errors.New("signal: terminated"))},
		{Checkout: co("d"), ExitCode: -1, Err: context.Canceled},
	}}
	var b bytes.Buffer
	err := printRunSummary(&b, r)
	want := "1 succeeded, 1 failed, 2 interrupted\nfailed: b/main (exit status 3)\ninterrupted: c/main\ninterrupted: d/main (not started)\n"
	if b.String() != want {
		t.Errorf("printRunSummary =\n%s\nwant\n%s", b.String(), want)
	}
	if err == nil {
		t.Error("no error")
	}
	b.Reset()
	if err := printRunSummary(&b, ops.RunReport{Results: r.Results[3:]}); err == nil || b.String() != "1 interrupted\ninterrupted: d/main (not started)\n" {
		t.Errorf("only interrupted: %v\n%s", err, b.String())
	}
	if got := runFailure(r.Results[2]); got != "interrupted" {
		t.Errorf("runFailure(stopped) = %q, want interrupted", got)
	}
	if got := runFailure(r.Results[3]); got != "not started (interrupted)" {
		t.Errorf("runFailure(not started) = %q", got)
	}
}

// Interrupted lines get the level of a warning in TUI mode.
func TestLevelMarkInterrupted(t *testing.T) {
	t.Parallel()
	if m := levelMark("interrupted  a/main"); m.Start != 0 || m.End != len("interrupted") || m.Level == 0 {
		t.Errorf("levelMark = %+v, want the word interrupted marked", m)
	}
}

// A failure whose cleanup failed too stays a failure, with its full text,
// even when an interruption stopped it (B52 U2 review).
func TestPrintSummaryCleanupFailureAfterInterruption(t *testing.T) {
	t.Parallel()
	err := &ops.CleanupError{Err: context.Canceled, Msg: "adopting /b/tool failed: context canceled; rolling back also failed, fix manually: permission denied"}
	var b bytes.Buffer
	_ = printSummary(&b, ops.Summary{Results: []ops.Result{{Repo: "tool", Status: ops.StatusFailed, Err: err}}})
	if want := "failed   tool: " + err.Msg + "\n1 failed\n"; b.String() != want {
		t.Errorf("printSummary =\n%s\nwant\n%s", b.String(), want)
	}
}

// The count lines that end the output of a command leave out the zero
// counts (B62 L6), or show the first count alone when every count is
// zero.
func TestSummariesLeaveOutZeroCounts(t *testing.T) {
	t.Parallel()
	last := func(s string) string {
		lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
		return lines[len(lines)-1]
	}
	var b bytes.Buffer
	_ = printSummary(&b, ops.Summary{Results: []ops.Result{{Repo: "a", Status: ops.StatusSkipped}, {Repo: "b", Status: ops.StatusFailed, Err: context.Canceled}}})
	if got := last(b.String()); got != "1 skipped, 1 interrupted" {
		t.Errorf("printSummary: %q", got)
	}
	b.Reset()
	_ = printSummary(&b, ops.Summary{})
	if got := last(b.String()); got != "0 done" {
		t.Errorf("printSummary of nothing: %q", got)
	}
	b.Reset()
	err := printUpdateSummary(&b, ops.UpdateReport{
		Checkouts: []ops.CheckoutUpdate{{Checkout: ops.Checkout{Repo: "c", Branch: "main", Dir: "main"}, Outcome: gitx.PullUpToDate}},
		Problems:  []ops.Problem{{Repo: "d", Path: "/b/d/x", Err: errors.New("boom")}},
	}, false)
	if got := last(b.String()); got != "1 up to date, 1 problem" || err == nil || err.Error() != "update: 1 problem" {
		t.Errorf("printUpdateSummary: %q, error %v", got, err)
	}
	b.Reset()
	_ = printUpdateSummary(&b, ops.UpdateReport{}, false)
	if got := last(b.String()); got != "0 fast-forwarded" {
		t.Errorf("printUpdateSummary of nothing: %q", got)
	}
	b.Reset()
	c := ops.PruneCandidate{Repo: "a", Branch: "feat", Worktree: true, Dir: "feat"}
	printPrune(&b, ops.PrunePlan{Candidates: []ops.PruneCandidate{c}}, []ops.PruneResult{{PruneCandidate: c, Outcome: ops.PruneKept, Message: "why"}}, false, "")
	if got := last(b.String()); got != "1 kept" {
		t.Errorf("printPrune: %q", got)
	}
	b.Reset()
	_ = printRunSummary(&b, ops.RunReport{Results: []ops.RunResult{{Checkout: ops.Checkout{Repo: "a", Dir: "main", Branch: "main"}, Started: true}}})
	if got := b.String(); got != "1 succeeded\n" {
		t.Errorf("printRunSummary: %q", got)
	}
}

// summaryLines returns the byte offsets in out of the lines equal to line
// once escape sequences and carriage returns are left out: a count line
// such as "1 done" must be the whole line, not part of "1 done, 1 failed"
// or of a progress header.
func summaryLines(out, line string) []int {
	var at []int
	for i := 0; i < len(out); {
		end := strings.IndexByte(out[i:], '\n')
		if end < 0 {
			end = len(out) - i
		}
		if strings.TrimRight(ansi.Strip(out[i:i+end]), "\r") == line {
			at = append(at, i)
		}
		i += end + 1
	}
	return at
}

// hasSummary reports whether out has the line line (see summaryLines).
func hasSummary(out, line string) bool { return len(summaryLines(out, line)) > 0 }

// summaryIndex returns the offset of the first line line of out (see
// summaryLines), or -1.
func summaryIndex(out, line string) int {
	if at := summaryLines(out, line); len(at) > 0 {
		return at[0]
	}
	return -1
}
