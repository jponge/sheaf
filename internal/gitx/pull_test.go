package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func head(t *testing.T, dir string) string {
	t.Helper()
	return testutil.Git(t, dir, "rev-parse", "HEAD")
}

func wantOutcome(t *testing.T, r PullResult, want PullOutcome) {
	t.Helper()
	if r.Outcome != want {
		t.Fatalf("outcome = %v (err %v), want %v", r.Outcome, r.Err, want)
	}
	if (r.Err != nil) != (want == PullFailed) {
		t.Fatalf("err = %v for outcome %v", r.Err, r.Outcome)
	}
}

func TestFetchUpdatesAndPrunes(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"doomed"}})
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	testutil.Git(t, o.Path, "branch", "-D", "doomed")

	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	if got, want := testutil.Git(t, dir, "rev-parse", "origin/main"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Fatalf("origin/main = %s, want %s", got, want)
	}
	if refs := testutil.Git(t, dir, "for-each-ref", "refs/remotes/origin/doomed"); refs != "" {
		t.Fatalf("origin/doomed not pruned: %q", refs)
	}
}

func TestPullOutcomeString(t *testing.T) {
	t.Parallel()
	for o, want := range map[PullOutcome]string{
		PullUpToDate:          "up-to-date",
		PullFastForwarded:     "fast-forwarded",
		PullSkippedDirty:      "skipped-dirty",
		PullSkippedNoUpstream: "skipped-no-upstream",
		PullSkippedDiverged:   "skipped-diverged",
		PullSkippedDetached:   "skipped-detached",
		PullFailed:            "failed",
		PullOutcome(0):        "PullOutcome(0)",
	} {
		if got := o.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", int(o), got, want)
		}
	}
}

// FastForward never runs a network operation: it uses the remote-tracking
// ref as of the last Fetch.
func TestFastForwardUsesLastFetchOnly(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	old := head(t, dir)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	r := FastForward(context.Background(), dir)
	wantOutcome(t, r, PullUpToDate)
	if r.Old != old || r.New != old {
		t.Fatalf("old/new = %s/%s, want %s", r.Old, r.New, old)
	}
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	r = FastForward(context.Background(), dir)
	wantOutcome(t, r, PullFastForwarded)
	if want := testutil.Git(t, o.Path, "rev-parse", "main"); r.Old != old || r.New != want || head(t, dir) != want {
		t.Fatalf("old/new = %s/%s, want %s/%s", r.Old, r.New, old, want)
	}
}

func TestFastForwardSkips(t *testing.T) {
	t.Parallel()
	t.Run("dirty", func(t *testing.T) {
		o, dir := cloneOrigin(t, testutil.OriginOpts{})
		testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
		testutil.Git(t, dir, "fetch", "-q")
		before := head(t, dir)
		writeFile(t, dir, "file-1.txt", "dirty\n")
		wantOutcome(t, FastForward(context.Background(), dir), PullSkippedDirty)
		if head(t, dir) != before {
			t.Fatal("HEAD moved")
		}
	})
	t.Run("detached", func(t *testing.T) {
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		testutil.Git(t, dir, "checkout", "-q", "--detach")
		wantOutcome(t, FastForward(context.Background(), dir), PullSkippedDetached)
	})
	t.Run("no upstream", func(t *testing.T) {
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		testutil.Git(t, dir, "checkout", "-q", "-b", "local-only")
		wantOutcome(t, FastForward(context.Background(), dir), PullSkippedNoUpstream)
		testutil.Git(t, dir, "config", "branch.local-only.remote", "origin")
		testutil.Git(t, dir, "config", "branch.local-only.merge", "refs/heads/vanished")
		wantOutcome(t, FastForward(context.Background(), dir), PullSkippedNoUpstream)
	})
}

// User configuration asking for merge commits (pull.ff=false,
// merge.ff=false, branch.<b>.mergeOptions=--no-ff) must never produce a merge
// commit; a diverged branch must never move.
func TestFastForwardIgnoresNoFFConfig(t *testing.T) {
	testutil.IsolateGitPerTest(t) // writable global configuration
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	// AdvanceOrigin re-isolates git (fresh global config), so configure after it.
	noFF := func() {
		for _, kv := range [][2]string{
			{"pull.ff", "false"},
			{"merge.ff", "false"},
			{"branch.main.mergeOptions", "--no-ff"},
			{"pull.rebase", "true"},
		} {
			testutil.Git(t, dir, "config", "--global", kv[0], kv[1])
		}
	}
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	noFF()
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	r := FastForward(context.Background(), dir)
	wantOutcome(t, r, PullFastForwarded)
	want := testutil.Git(t, o.Path, "rev-parse", "main")
	if h := head(t, dir); h != want || r.New != want {
		t.Fatalf("HEAD = %s (New %s), want origin tip %s (a merge commit was created?)", h, r.New, want)
	}
	if parents := testutil.Git(t, dir, "rev-list", "--parents", "-n1", "HEAD"); len(strings.Fields(parents)) != 2 {
		t.Fatalf("HEAD is not a single-parent commit: %q", parents)
	}

	// Diverged: never moves, whatever the configuration says.
	testutil.CommitFile(t, dir, "l.txt", "l\n", "local")
	testutil.AdvanceOrigin(t, o, "main", "r2.txt", "r\n", "remote 2")
	noFF()
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	before := head(t, dir)
	wantOutcome(t, FastForward(context.Background(), dir), PullSkippedDiverged)
	if head(t, dir) != before {
		t.Fatal("HEAD moved on a diverged branch")
	}
}

// An untracked file that the fast-forward would overwrite makes it fail
// without touching the file.
func TestFastForwardRefusesToOverwriteUntrackedFile(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "from remote\n", "remote")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	before := head(t, dir)
	writeFile(t, dir, "r.txt", "precious local\n")
	r := FastForward(context.Background(), dir)
	wantOutcome(t, r, PullFailed)
	if r.Old != before || r.New != before || head(t, dir) != before {
		t.Fatalf("old/new/HEAD = %s/%s/%s, want %s", r.Old, r.New, head(t, dir), before)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "r.txt")); err != nil || string(b) != "precious local\n" {
		t.Fatalf("untracked file = %q, %v", b, err)
	}
}

// Fetch fetches origin even when the current branch's
// upstream is a local branch (branch.<b>.remote = .).
func TestFetchFetchesOriginWhenUpstreamIsLocal(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "-b", "topic", "--track", "main")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	if got, want := testutil.Git(t, dir, "rev-parse", "origin/main"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Fatalf("origin/main = %s, want %s", got, want)
	}
}

// A cancellation right after the merge completed must not turn
// the completed fast-forward into a failure (HEAD re-read and ancestry check
// run on a context without cancellation).
func TestFastForwardIgnoresCancellationAfterMerge(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterMergeHook = cancel
	t.Cleanup(func() { afterMergeHook = nil })
	r := FastForward(ctx, dir)
	wantOutcome(t, r, PullFastForwarded)
	if h := head(t, dir); r.New != h {
		t.Fatalf("New = %s, HEAD = %s", r.New, h)
	}
}
