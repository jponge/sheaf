package gitx

import (
	"context"
	"errors"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestFastForwardOnMergeStartOnlyWhenMerging(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	calls := 0
	opts := FastForwardOptions{OnMergeStart: func() { calls++ }}
	if r := FastForwardWithOptions(context.Background(), dir, opts); r.Outcome != PullUpToDate || calls != 0 {
		t.Fatalf("up to date: outcome %v, %d calls, want 0", r.Outcome, calls)
	}
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "file-1.txt", "dirty\n")
	if r := FastForwardWithOptions(context.Background(), dir, opts); r.Outcome != PullSkippedDirty || calls != 0 {
		t.Fatalf("dirty: outcome %v, %d calls, want 0", r.Outcome, calls)
	}
	testutil.Git(t, dir, "checkout", "--", "file-1.txt")
	if r := FastForwardWithOptions(context.Background(), dir, opts); r.Outcome != PullFastForwarded || calls != 1 {
		t.Fatalf("behind: outcome %v, %d calls, want 1", r.Outcome, calls)
	}
}

// A cancellation between the pre-checks and the merge prevents the merge.
func TestFastForwardCancelledAfterPreCheckDoesNotMerge(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	before := head(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterPreCheckHook = cancel
	t.Cleanup(func() { afterPreCheckHook = nil })
	started := false
	r := FastForwardWithOptions(ctx, dir, FastForwardOptions{OnMergeStart: func() { started = true }})
	if r.Outcome != PullFailed || !errors.Is(r.Err, context.Canceled) || started || head(t, dir) != before || r.New != before {
		t.Fatalf("result %+v, merge started %v; want failed (context.Canceled) without merging", r, started)
	}
}
