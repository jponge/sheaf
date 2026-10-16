package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
)

// Failures keep git's full (multi-line) error in the update summary.
func TestUpdateSummaryKeepsFullError(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	testutil.Git(t, alpha, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "missing.git"))
	out, _, _ := e.run(t, "update")
	if !strings.Contains(out, "Could not read from remote repository") {
		t.Fatalf("summary lost the rest of git's error:\n%s", out)
	}
}

func TestUpdateUntrackedInTheWayShownAsSkipped(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, alpha := e.addRepo(t, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "remote\n", "remote")
	writeFile(t, filepath.Join(alpha, "r.txt"), "mine\n")
	out, _, code := e.run(t, "update")
	if code != 0 || !regexp.MustCompile(`skipped\s+alpha/main\s+untracked files in the way: r.txt`).MatchString(out) {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Stashes appear on the repository's first row whose status is known.
func TestStatusTableStashOnFirstSuccessfulRow(t *testing.T) {
	t.Parallel()
	r := ops.StatusReport{Checkouts: []ops.CheckoutStatus{
		{Checkout: ops.Checkout{Repo: "a", Dir: "main", Primary: true, Branch: "main"}, Err: errors.New("boom")},
		{Checkout: ops.Checkout{Repo: "a", Dir: "feat", Branch: "feat"}, Status: &gitx.RepoStatus{Branch: "feat", Stashes: 2}},
		{Checkout: ops.Checkout{Repo: "a", Dir: "more", Branch: "more"}, Status: &gitx.RepoStatus{Branch: "more", Stashes: 2}},
	}}
	var b bytes.Buffer
	writeStatusTable(&b, r, false)
	lines := strings.Split(b.String(), "\n")
	if f := strings.Fields(lines[2]); len(f) < 2 || f[len(f)-2] != "2" {
		t.Errorf("stash count not on the first successful row:\n%s", b.String())
	}
	if f := strings.Fields(lines[3]); f[len(f)-2] == "2" {
		t.Errorf("stash count repeated:\n%s", b.String())
	}
}

// Worktrees outside <bundle>/<repo>/ are marked in every output.
func TestOutsideWorktreeMarked(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha", "far")
	far := filepath.Join(t.TempDir(), "far")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "far", far, "origin/far")
	far, _ = filepath.EvalSymlinks(far)
	if out := e.mustRun(t, "status"); !regexp.MustCompile(`alpha\s+far \(outside\)\s+far`).MatchString(out) {
		t.Errorf("status:\n%s", out)
	}
	if out := e.mustRun(t, "status", "--json"); !strings.Contains(out, `"outside": true`) {
		t.Errorf("status --json:\n%s", out)
	}
	if out := e.mustRun(t, "update"); !strings.Contains(out, "alpha/far (outside: "+far+")") {
		t.Errorf("update:\n%s", out)
	}
	if out := e.mustRun(t, "run", "true"); !strings.Contains(out, "==> alpha/far (outside: "+far+") <==") {
		t.Errorf("run:\n%s", out)
	}
}

// Not-started work reads the same everywhere.
func TestNotStartedEventWording(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	r := checkoutRenderer{plainRenderer{&b}}
	r.Report(ops.Event{Repo: "a", Worktree: "main", Phase: ops.PhaseUpdate, Kind: ops.KindFailed, Message: "not started (interrupted)", Err: context.Canceled})
	r.Report(ops.Event{Repo: "b", Phase: ops.PhaseFetch, Kind: ops.KindFailed, Message: "not started", Err: context.Canceled})
	want := "a/main: update not started (interrupted)\nb: fetch not started (interrupted)\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestUpdateSummaryPluralization(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	e.addRepo(t, "beta")
	if err := os.RemoveAll(alpha); err != nil {
		t.Fatal(err)
	}
	out, errs, _ := e.run(t, "update")
	if !strings.Contains(out, ", 1 problem\n") || strings.Contains(out+errs, "1 problems") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out, errs)
	}
}

func TestRunBlockShowsNote(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	s := &blockSink{w: &b}
	w := s.Begin(ops.Checkout{Repo: "a", Dir: "main"})
	_, _ = w.Write([]byte("x\n"))
	s.End(ops.RunResult{Started: true, Note: "output may be truncated"})
	if want := "==> a/main <==\nx\n[note: output may be truncated]\n\n"; b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}
