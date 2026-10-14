//go:build unix

package ops

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// racingGit puts on PATH a git wrapper that, right after any command
// fetching refs/pull/7/head, fetches refs/pull/8/head in the same directory
// (a concurrent fetch overwriting FETCH_HEAD).
func racingGit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n\"" + real + "\" \"$@\"; rc=$?\n" +
		"case \"$*\" in\n  *fetch*refs/pull/7/head*) \"" + real + "\" fetch -q origin refs/pull/8/head ;;\nesac\nexit $rc\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// N2: the existing pr/7 is fast-forwarded to PR 7's head, not to whatever a
// concurrent fetch left in FETCH_HEAD.
func TestR10PRHeadNotFromSharedFetchHead(t *testing.T) {
	f, primary, o, head := forkPR7(t, false)
	next7 := pushPullRef(t, o, 7, head, "seven.txt")
	pushPullRef(t, o, 8, next7, "eight.txt") // descends from pr/7 too
	racingGit(t)
	res, err := f.wtPR(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, primary, "rev-parse", "pr/7"); got != next7 {
		t.Fatalf("pr/7 = %s, want PR 7's head %s", got, next7)
	}
	// Refresh of the checked-out worktree: same.
	next7b := pushPullRef(t, o, 7, next7, "seven-b.txt")
	pushPullRef(t, o, 8, next7b, "eight-b.txt")
	if _, err := f.wtPR(t, "a", 7); err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next7b {
		t.Fatalf("refresh: HEAD = %s, want PR 7's head %s", got, next7b)
	}
	// No temporary ref is left behind.
	if out := testutil.Git(t, primary, "for-each-ref", "refs/sheaf/"); out != "" {
		t.Fatalf("leftover refs: %s", out)
	}
}
