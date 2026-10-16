package ops

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// B6 S1: commits reachable from the head of a pull request checked out by
// `sheaf wt pr` (branch pr/<n> tracking refs/pull/<n>/head, which has no
// remote-tracking ref) count as published; only commits beyond it are at
// risk.

// prRepo adds repository a with pull request 7 from a fork checked out by
// wt pr at a/pr-7, and returns the primary checkout, origin, the worktree
// and the pull request head.
func prRepo(t *testing.T) (f *fixture, primary string, o testutil.Origin, wt, head string) {
	t.Helper()
	f = newFixture(t)
	primary, o = f.clonedRepo(t, "a")
	head = pushPullRef(t, o, 7, "main", "fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "topic", HeadOwner: "someone", HeadRepo: "a", IsCrossRepository: true})
	res, err := f.wtPR(t, "a", 7)
	if err != nil || res.Branch != "pr/7" {
		t.Fatalf("wt pr: %+v, %v", res, err)
	}
	return f, primary, o, res.Path, head
}

func TestWtPRRecordsThePullRequestHead(t *testing.T) {
	t.Parallel()
	f, primary, o, wt, head := prRepo(t)
	if got := testutil.Git(t, primary, "rev-parse", "refs/sheaf/pull/7"); got != head {
		t.Fatalf("refs/sheaf/pull/7 = %s, want the head %s", got, head)
	}
	// Refreshing the worktree records the new head.
	next := pushPullRef(t, o, 7, head, "more.txt")
	if _, err := f.wtPR(t, "a", 7); err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, wt, "rev-parse", "refs/sheaf/pull/7"); got != next {
		t.Fatalf("after the refresh refs/sheaf/pull/7 = %s, want %s", got, next)
	}
}

func TestRemoveCleanPRWorktreeIsPublished(t *testing.T) {
	t.Parallel()
	for _, offline := range []bool{false, true} {
		f, _, _, _, _ := prRepo(t)
		if _, err := f.removeOpts(t, "a", RemoveOptions{Offline: offline, WorkingDir: f.w.Root, Confirm: yes}); err != nil {
			t.Fatalf("offline %v: %v", offline, err)
		}
		if exists(filepath.Join(f.w.Root, "a")) {
			t.Fatalf("offline %v: not removed", offline)
		}
	}
}

// wantUnpushedPR checks a refusal for the commits of pr/7 only, reported on
// the primary checkout and naming the branch.
func wantUnpushedPR(t *testing.T, err error, primary string, n int) {
	t.Helper()
	var re *RisksError
	if !errors.As(err, &re) || re.Plan.Refused(true) {
		t.Fatalf("err = %v, want a refusal that --force overrides", err)
	}
	want := primary + " [main]: 1 unpushed commit on local branches: pr/7"
	if n != 1 {
		want = primary + " [main]: 2 unpushed commits on local branches: pr/7"
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal lacks %q:\n%v", want, err)
	}
}

func TestRemovePRWorktreeCommitsBeyondTheHeadAreUnpushed(t *testing.T) {
	t.Parallel()
	f, primary, _, wt, _ := prRepo(t)
	testutil.CommitFile(t, wt, "review.txt", "r\n", "review fix")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantUnpushedPR(t, err, primary, 1)
}

// Remove fetches the current head: commits of a force-pushed-away head are
// no longer published.
func TestRemovePRHeadRewrittenCountsAsUnpushed(t *testing.T) {
	t.Parallel()
	f, primary, o, _, _ := prRepo(t)
	pushPullRef(t, o, 7, "main", "rewritten.txt")
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantUnpushedPR(t, err, primary, 1)
}

// A pull request worktree made before sheaf recorded the head: offline,
// the head cannot be established and its commits count as unpushed; online,
// remove fetches it.
func TestRemovePRWorktreeWithoutRecordedHead(t *testing.T) {
	t.Parallel()
	f, primary, _, _, _ := prRepo(t)
	testutil.Git(t, primary, "update-ref", "-d", "refs/sheaf/pull/7")
	_, err := f.removeOpts(t, "a", RemoveOptions{Offline: true, WorkingDir: f.w.Root, Confirm: yes})
	wantUnpushedPR(t, err, primary, 1)
	if _, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes}); err != nil {
		t.Fatalf("online: %v", err)
	}
}

// When the head cannot be fetched, the recorded one is not trusted.
func TestRemovePRHeadFetchFailureCountsAsUnpushed(t *testing.T) {
	t.Parallel()
	f, primary, o, _, _ := prRepo(t)
	testutil.Git(t, o.Path, "update-ref", "-d", "refs/pull/7/head")
	evs, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	wantUnpushedPR(t, err, primary, 1)
	if !strings.Contains(messages(evs), "pull request heads") {
		t.Fatalf("no event about the failed fetch:\n%s", messages(evs))
	}
}

// The refusal names the branches with unpushed commits as Remove counts
// them: a clean pull request branch (its head published) is not named.
func TestRemoveRefusalNamesOnlyTheUnpushedBranches(t *testing.T) {
	t.Parallel()
	f, primary, _, _, _ := prRepo(t)
	testutil.Git(t, primary, "branch", "topic", "main")
	testutil.Git(t, primary, "update-ref", "refs/heads/topic", testutil.Git(t, primary, "commit-tree", "-p", "main", "-m", "local", "main^{tree}"))
	_, err := f.removeOpts(t, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes})
	if want := primary + " [main]: 1 unpushed commit on local branches: topic\n"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
