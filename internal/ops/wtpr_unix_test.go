//go:build unix

package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// racingGit puts on PATH a git wrapper that, right after any command
// fetching refs/pull/7/head, fetches refs/pull/8/head in the same directory
// (a concurrent fetch overwriting FETCH_HEAD).
func racingGit(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n\"" + realGit + "\" \"$@\"; rc=$?\n" +
		"case \"$*\" in\n  *fetch*refs/pull/7/head*) \"" + realGit + "\" fetch -q origin refs/pull/8/head ;;\nesac\nexit $rc\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The existing pr/7 is fast-forwarded to PR 7's head, not to whatever a
// concurrent fetch left in FETCH_HEAD.
func TestPRHeadNotFromSharedFetchHead(t *testing.T) {
	f, primary, o, head := forkPR7(t, false)
	next7 := pushPullRef(t, o, 7, head, "seven.txt")
	pushPullRef(t, o, 8, next7, "eight.txt") // descends from pr/7 too
	racingGit(t)
	res, err := f.addPRWorktree(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, primary, "rev-parse", "pr/7"); got != next7 {
		t.Fatalf("pr/7 = %s, want PR 7's head %s", got, next7)
	}
	// Refresh of the checked-out worktree: same.
	next7b := pushPullRef(t, o, 7, next7, "seven-b.txt")
	pushPullRef(t, o, 8, next7b, "eight-b.txt")
	if _, err := f.addPRWorktree(t, "a", 7); err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next7b {
		t.Fatalf("refresh: HEAD = %s, want PR 7's head %s", got, next7b)
	}
	// No temporary ref is left behind.
	if out := testutil.Git(t, primary, "for-each-ref", "refs/sheaf/tmp/"); out != "" {
		t.Fatalf("leftover refs: %s", out)
	}
}

// The PR head of an existing, not checked-out pr/7 is fetched without
// holding .sheaf.lock (a credential prompt must not block other sheaf
// processes), and the head is still recorded as refs/sheaf/pull/7.
func TestPRFetchOutsideBundleLock(t *testing.T) {
	t.Parallel()
	f, primary, o, head := forkPR7(t, false)
	next := pushPullRef(t, o, 7, head, "fork2.txt")
	fetches, held := 0, 0
	hook := func(string) {
		fetches++
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a single, non-blocking attempt
		unlock, err := lockBundle(ctx, f.w.Root, nil)
		if err != nil {
			held++
			return
		}
		unlock()
	}
	res, err := f.addPRWorktreeWith(t, "a", 7, PRWorktreeOptions{fetchHook: hook})
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 || held != 0 {
		t.Fatalf("%d PR-head fetches, %d of them under %s; want 1, 0", fetches, held, lockFileName)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next {
		t.Fatalf("HEAD %s, want %s", got, next)
	}
	if got := testutil.Git(t, primary, "rev-parse", gitx.PullHeadRef(7)); got != next {
		t.Fatalf("%s = %s, want %s", gitx.PullHeadRef(7), got, next)
	}
}

// Branch pr/7 moved by someone else while its PR head was being fetched (e.g.
// a concurrent `sheaf wt pr` that fetched a newer head): nothing is changed,
// no worktree is created, and the message says what happened instead of
// comparing pr/7 with a fetch result that may be stale.
func TestPRBranchChangedDuringFetch(t *testing.T) {
	t.Parallel()
	f, primary, o, head := forkPR7(t, false)
	pushPullRef(t, o, 7, head, "fork2.txt")
	tree := testutil.Git(t, primary, "rev-parse", head+"^{tree}")
	moved := testutil.Git(t, primary, "commit-tree", tree, "-p", head, "-m", "concurrent")
	hook := func(dir string) {
		testutil.Git(t, dir, "update-ref", "refs/heads/pr/7", moved)
	}
	_, err := f.addPRWorktreeWith(t, "a", 7, PRWorktreeOptions{fetchHook: hook})
	if err == nil || !strings.Contains(err.Error(), "pr/7 changed while") || !strings.Contains(err.Error(), "run this command again") {
		t.Fatalf("err = %v", err)
	}
	if got := testutil.Git(t, primary, "rev-parse", "pr/7"); got != moved {
		t.Fatalf("pr/7 = %s, want %s (left as the other writer set it)", got, moved)
	}
	if exists(filepath.Join(f.w.Root, "a", "pr-7")) {
		t.Fatal("worktree created")
	}
}
