package cli

import (
	"bytes"
	"context"
	"testing"

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
	want := "interrupted a\ninterrupted b\ndone        c: fetched origin\n1 done, 0 skipped, 0 failed, 2 interrupted\n"
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
	want := "interrupted  a/main  \ninterrupted  b/main  not started\nup to date   c/main  \n0 fast-forwarded, 1 up to date, 0 skipped, 0 failed, 2 interrupted\n"
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
	want := "a/feat: removed\na/feat: interrupted: not started\n1 removed, 0 kept, 0 failed, 1 interrupted\n"
	if b.String() != want {
		t.Errorf("printPrune =\n%s\nwant\n%s", b.String(), want)
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
	if want := "failed   tool: " + err.Msg + "\n0 done, 0 skipped, 1 failed\n"; b.String() != want {
		t.Errorf("printSummary =\n%s\nwant\n%s", b.String(), want)
	}
}
