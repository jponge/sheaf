package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
	"github.com/jponge/sheaf/internal/tui"
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
	writeStatusTable(&b, r, nil)
	lines := strings.Split(b.String(), "\n")
	if f := strings.Fields(lines[2]); len(f) < 2 || f[len(f)-2] != "2" {
		t.Errorf("stash count not on the first successful row:\n%s", b.String())
	}
	if f := strings.Fields(lines[3]); f[len(f)-2] == "2" {
		t.Errorf("stash count repeated:\n%s", b.String())
	}
}

// The STATE cell of status ("clean" or "dirty", or the error) is colored
// with the level of the overview's STATE (B52 E1, see homeLevel): red for a
// divergence, an upstream gone or an error, yellow for local changes or
// commits to push or pull, faint without upstream, green otherwise.
func TestStatusTableStateLevels(t *testing.T) {
	t.Parallel()
	row := func(dir string, detached bool, s gitx.RepoStatus) ops.CheckoutStatus {
		return ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a", Dir: dir, Branch: dir, Detached: detached}, Status: &s}
	}
	up := "origin/x"
	r := ops.StatusReport{Checkouts: []ops.CheckoutStatus{
		row("clean", false, gitx.RepoStatus{Upstream: up}),
		row("diverged", false, gitx.RepoStatus{Upstream: up, Ahead: 1, Behind: 2}),
		row("gone", false, gitx.RepoStatus{Upstream: up, UpstreamGone: true}),
		row("dirty", false, gitx.RepoStatus{Upstream: up, Unstaged: 1}),
		row("ahead", false, gitx.RepoStatus{Upstream: up, Ahead: 1}),
		row("behind", false, gitx.RepoStatus{Upstream: up, Behind: 1}),
		row("local", false, gitx.RepoStatus{}),
		row("det", true, gitx.RepoStatus{}),
		{Checkout: ops.Checkout{Repo: "a", Dir: "err", Branch: "err"}, Err: errors.New("boom")},
	}}
	want := map[string]string{
		"clean": "<OK clean>", "diverged": "<FAIL clean>", "gone": "<FAIL clean>", "dirty": "<WARN dirty>",
		"ahead": "<WARN clean>", "behind": "<WARN clean>", "local": "<INFO clean>", "det": "<OK clean>", "err": "<FAIL error: boom>",
	}
	names := map[tui.Level]string{tui.LevelOK: "OK", tui.LevelInfo: "INFO", tui.LevelWarn: "WARN", tui.LevelFail: "FAIL"}
	var b bytes.Buffer
	writeStatusTable(&b, r, func(l tui.Level, s string) string { return "<" + names[l] + " " + s + ">" })
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")[1:]
	if len(lines) != len(want) {
		t.Fatalf("%d rows, want %d:\n%s", len(lines), len(want), b.String())
	}
	for _, l := range lines {
		dir := strings.Fields(l)[1]
		if !strings.HasSuffix(l, want[dir]) {
			t.Errorf("%s: row %q, want the state %s", dir, l, want[dir])
		}
	}
}

// On a terminal narrower than the full table, status prints the compact
// table of the overview (B62 H1): REPO grouped, DIR, BRANCH, CHANGES
// (+staged ~unstaged ?untracked !conflicts $stashes, zeros left out) and
// STATE as the overview shows it, colored alike; problems follow as
// usual. Wide enough, or with an unknown width, the full table is
// printed unchanged.
func TestStatusCompactOnNarrowTerminal(t *testing.T) {
	t.Parallel()
	prob := ops.Problem{Repo: "b", Path: "/x/b/gone", Err: errors.New("worktree directory missing")}
	r := ops.StatusReport{Checkouts: []ops.CheckoutStatus{
		{Checkout: ops.Checkout{Repo: "a", Dir: "main", Primary: true, Branch: "main"}, Status: &gitx.RepoStatus{Branch: "main",
			Upstream: "origin/main", Ahead: 1, Behind: 1, Staged: 2, Unstaged: 1, Untracked: 3, Conflicted: 1, Stashes: 1}},
		{Checkout: ops.Checkout{Repo: "a", Dir: "feat", Branch: "feat"}, Status: &gitx.RepoStatus{Branch: "feat", Stashes: 1}},
		{Checkout: ops.Checkout{Repo: "b", Dir: "main", Primary: true, Branch: "main"}, Err: errors.New("boom")},
	}, Problems: []ops.Problem{prob}}
	var full bytes.Buffer
	writeStatusTable(&full, r, nil)
	for _, width := range []int{0, 200} {
		var b bytes.Buffer
		writeStatus(&b, r, nil, width)
		if b.String() != full.String() {
			t.Errorf("width %d:\n%s\nwant the full table\n%s", width, b.String(), full.String())
		}
	}
	var b bytes.Buffer
	writeStatus(&b, r, nil, 80)
	want := strings.Join([]string{
		"REPO  DIR   BRANCH  CHANGES         STATE",
		"a     main  main    +2 ~1 ?3 !1 $1  dirty, ↑1 ↓1",
		"      feat  feat                    clean, no upstream",
		"b     main  main                    error: boom",
		"problem: " + prob.Error(),
		"",
	}, "\n")
	if b.String() != want {
		t.Errorf("compact:\n%q\nwant\n%q", b.String(), want)
	}
	b.Reset()
	writeStatus(&b, r, func(l tui.Level, s string) string { return "\x1b[" + strconv.Itoa(int(l)) + "m" + s + "\x1b[m" }, 80)
	for _, l := range []string{
		"main  main    +2 ~1 ?3 !1 $1  \x1b[" + strconv.Itoa(int(tui.LevelFail)) + "mdirty, ↑1 ↓1\x1b[m\n",
		"feat  feat                    \x1b[" + strconv.Itoa(int(tui.LevelInfo)) + "mclean, no upstream\x1b[m\n",
		"main  main                    \x1b[" + strconv.Itoa(int(tui.LevelFail)) + "merror: boom\x1b[m\n",
	} {
		if !strings.Contains(b.String(), l) {
			t.Errorf("colored compact lacks %q:\n%q", l, b.String())
		}
	}
	// Narrower still: BRANCH, then DIR, are cut with "…" before STATE.
	r.Checkouts[1].Branch, r.Checkouts[1].Dir = "feature/very-long-name", "feature-very-long-name"
	b.Reset()
	writeStatus(&b, r, nil, 60)
	if !strings.Contains(b.String(), "\n      feature…  feature…"+strings.Repeat(" ", 2+14+2)+"clean, no upstream\n") {
		t.Errorf("shrunk compact:\n%s", b.String())
	}
}

// At any width, the lines of the compact status fit the terminal (problem
// lines included, cut with "…"), never end with a double "……" (a cell cut,
// then the line), and STATE stays whole while every other column can
// still give way, down to one character.
func TestStatusCompactFitsNarrowTerminals(t *testing.T) {
	t.Parallel()
	prob := ops.Problem{Repo: "b", Path: "/x/b/gone", Err: errors.New("worktree directory missing")}
	r := ops.StatusReport{Checkouts: []ops.CheckoutStatus{
		{Checkout: ops.Checkout{Repo: "alpha", Dir: "main", Primary: true, Branch: "main"}, Status: &gitx.RepoStatus{Branch: "main",
			Upstream: "origin/main", Ahead: 1, Behind: 1, Staged: 2, Unstaged: 1, Untracked: 3, Conflicted: 1, Stashes: 1}},
		{Checkout: ops.Checkout{Repo: "alpha", Dir: "feature-very-long-name", Branch: "feature/very-long-name"}, Status: &gitx.RepoStatus{Branch: "feature/very-long-name"}},
	}, Problems: []ops.Problem{prob}}
	// 5 columns of one character, 4 gaps of 2, "clean, no upstream".
	const stateWhole = 5 + 8 + len("clean, no upstream")
	for width := 12; width <= 60; width++ {
		var b bytes.Buffer
		writeStatus(&b, r, nil, width)
		out := b.String()
		for l := range strings.Lines(out) {
			l = strings.TrimSuffix(l, "\n")
			if w := ansi.StringWidth(l); w > width || strings.Contains(l, "……") {
				t.Errorf("width %d: line %q (%d columns)", width, l, w)
			}
		}
		if !strings.Contains(out, "problem: ") {
			t.Errorf("width %d: no problem line:\n%s", width, out)
		}
		if width >= stateWhole && (!strings.Contains(out, "  dirty, ↑1 ↓1\n") || !strings.Contains(out, "  clean, no upstream\n")) {
			t.Errorf("width %d: STATE cut:\n%s", width, out)
		}
	}
}

// status chooses the compact table only in TUI mode, from the width of
// the terminal; plain mode (pipes, --no-tui) and --json are unchanged.
func TestStatusCompactOnlyOnNarrowTUITerminal(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	plain := e.mustRun(t, "status")
	asJSON := e.mustRun(t, "status", "--json")
	d := e.deps()
	d.termWidth = func(io.Writer) int { return 80 }
	d.environ = func() []string { return nil }
	for _, tc := range []struct {
		tui  bool
		args []string
		want string
	}{
		{false, []string{"status"}, plain},
		{false, []string{"status", "--json"}, asJSON},
		{true, []string{"status", "--json"}, asJSON},
		{true, []string{"status"}, "REPO   DIR   BRANCH  CHANGES  STATE\nalpha  main  main             clean\n"},
	} {
		d.UseTUI = func(io.Reader, io.Writer, bool) bool { return tc.tui }
		if got, _, code := runDeps(t, d, tc.args...); code != 0 || got != tc.want {
			t.Errorf("tui %v, %q: exit %d\n%q\nwant\n%q", tc.tui, tc.args, code, got, tc.want)
		}
	}
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	d.termWidth = func(io.Writer) int { return 500 }
	if got, _, _ := runDeps(t, d, "status"); got != plain {
		t.Errorf("wide terminal:\n%q\nwant\n%q", got, plain)
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
