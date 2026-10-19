package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

// SGR sequences of the level styles (tui.Styles): green, yellow, red
// foregrounds, faint, and the reset lipgloss ends them with.
const (
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrRed    = "\x1b[31m"
	sgrFaint  = "\x1b[2m"
	sgrReset  = "\x1b[m"
)

// colorSGR matches an SGR sequence setting a foreground color.
var colorSGR = regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?(?:3[0-9]|9[0-7])(?:;[0-9;]*)?m")

func TestLevelMark(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line, word string
		level      tui.Level
	}{
		// doctor
		{"OK    git 2.50.0", "OK", tui.LevelOK},
		{"INFO  /b/x is not a repository of the bundle", "INFO", tui.LevelInfo},
		{"WARN  gh not found", "WARN", tui.LevelWarn},
		{"FAIL  b: directory /b/b is missing", "FAIL", tui.LevelFail},
		// summaries (printSummary, update, prune)
		{"done     alpha: fetched origin", "done", tui.LevelOK},
		{"skipped  alpha: nothing to do", "skipped", tui.LevelInfo},
		{"failed   alpha: boom", "failed", tui.LevelFail},
		{"fast-forwarded  alpha  main", "fast-forwarded", tui.LevelOK},
		{"up to date      alpha", "up to date", tui.LevelOK},
		{"pinned          alpha/v1  tag v1", "pinned", tui.LevelInfo},
		{"skipped alpha/feat: has unpushed commits", "skipped", tui.LevelInfo},
		// warnings and notes
		{"sheaf: warning: listing of o failed", "warning:", tui.LevelWarn},
		{"sheaf: note: o/a may have moved", "note:", tui.LevelInfo},
		{"alpha/feat: worktree: warning: kept branch feat", "warning:", tui.LevelWarn},
		{"alpha: remove: note: something", "note:", tui.LevelInfo},
		{"warning: x", "warning:", tui.LevelWarn},
		{"  note: kept", "note:", tui.LevelInfo},
	} {
		k := levelMark(tc.line)
		if k.Level != tc.level || k.Start < 0 || k.End > len(tc.line) || k.Start > k.End || tc.line[k.Start:k.End] != tc.word {
			t.Errorf("levelMark(%q) = %+v, want %q at level %d", tc.line, k, tc.word, tc.level)
		}
	}
	for _, line := range []string{
		"", "OK", "OKAY  x", "ok    git", "      hint: install the GitHub CLI", "1 done, 0 skipped, 0 failed",
		"donee    x", "alpha: fetch failed: boom", "sheaf: interrupted", "a description with warning: inside it",
		"sheaf: alpha: fatal: warning: x", "nothing to prune",
	} {
		if k := levelMark(line); k.Level != tui.LevelNone {
			t.Errorf("levelMark(%q) = %+v, want none", line, k)
		}
	}
}

// fakePaint renders a level word as <level:word>.
func fakePaint(l tui.Level, s string) string { return fmt.Sprintf("<%d:%s>", l, s) }

// levelWriter paints the level word of each line, also when lines arrive
// in several writes (a line start that may be a level word waits for the
// rest), and passes everything else through.
func TestLevelWriter(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	w := &levelWriter{w: &b, paint: fakePaint}
	for _, s := range []string{"OK    a\nWARN  b\n", "done     alpha", ": ok\n", "      hint: h\nFAIL", "  c\n", "partial ", "OK not a word\n"} {
		if n, err := io.WriteString(w, s); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	want := "<1:OK>    a\n<3:WARN>  b\n<1:done>     alpha: ok\n      hint: h\n<4:FAIL>  c\npartial OK not a word\n"
	if b.String() != want {
		t.Errorf("wrote %q, want %q", b.String(), want)
	}
}

// The update summary, written by a tabwriter one cell at a time, gets its
// level words painted like a summary written a line at a time (B1 of the
// step 2 review: the word arrived without its space and was skipped).
func TestLevelWriterTabwriterUpdateSummary(t *testing.T) {
	t.Parallel()
	r := ops.UpdateReport{Checkouts: []ops.CheckoutUpdate{
		{Checkout: ops.Checkout{Repo: "alpha", Branch: "main", Dir: "main"}, Outcome: gitx.PullUpToDate},
		{Checkout: ops.Checkout{Repo: "beta", Branch: "main", Dir: "main"}, Outcome: gitx.PullFastForwarded},
		{Checkout: ops.Checkout{Repo: "gamma", Branch: "main", Dir: "main"}, Outcome: gitx.PullFailed, Err: errors.New("boom")},
	}}
	var plain, b strings.Builder
	_ = printUpdateSummary(&plain, r, false)
	_ = printUpdateSummary(&levelWriter{w: &b, paint: fakePaint}, r, false)
	want := strings.NewReplacer("up to date ", "<1:up to date> ", "fast-forwarded ", "<1:fast-forwarded> ", "failed ", "<4:failed> ").Replace(plain.String())
	if b.String() != want {
		t.Errorf("wrote\n%q\nwant\n%q", b.String(), want)
	}
	if !strings.Contains(b.String(), "<1:up to date>") {
		t.Fatalf("fixture: %q", b.String())
	}
}

// A line start that may still become a level word is held back only while
// it is a prefix of one; anything else (a prompt, text) passes at once.
func TestLevelWriterHoldsOnlyLevelWordPrefixes(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	w := &levelWriter{w: &b, paint: fakePaint}
	io.WriteString(w, "Remove alpha? [y/N] ")
	if b.String() != "Remove alpha? [y/N] " {
		t.Fatalf("prompt held back: %q", b.String())
	}
	b.Reset()
	io.WriteString(w, "\nup to")
	if b.String() != "\n" {
		t.Fatalf("level word prefix not held: %q", b.String())
	}
	w.Flush()
	if b.String() != "\nup to" {
		t.Fatalf("Flush: %q", b.String())
	}
	io.WriteString(w, " date  x\n") // the line went out unclassified
	if b.String() != "\nup to date  x\n" {
		t.Fatalf("after Flush: %q", b.String())
	}
}

// colorTTY is the environment of a terminal that allows colors.
var colorTTY = []string{"TERM=xterm-256color", "TTY_FORCE=1"}

// runColored runs args with the TUI forced or not, with environ for the
// color decisions.
func runColored(t *testing.T, d Deps, tuiMode bool, environ []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return tuiMode }
	d.environ = func() []string { return environ }
	return runDeps(t, d, args...)
}

// In TUI mode on a terminal that allows colors, doctor colors its level
// words; with NO_COLOR, in plain mode and with --json, its output is as
// before.
func TestDoctorColorsLevels(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	if err := os.RemoveAll(root + "/b"); err != nil {
		t.Fatal(err)
	}
	d := probeDeps(e, errors.New("gh: not logged in"))
	plain, _, _ := runDeps(t, d, "doctor")
	out, _, code := runColored(t, d, true, colorTTY, "doctor")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{sgrGreen + "OK" + sgrReset + "    git ", sgrYellow + "WARN" + sgrReset + "  gh ", sgrRed + "FAIL" + sgrReset + "  b: "} {
		if !strings.Contains(out, want) {
			t.Errorf("colored doctor lacks %q:\n%q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") && strings.Contains(out, "hint: \x1b") {
		t.Errorf("hint colored:\n%q", out)
	}
	if got := ansi.Strip(out); got != plain {
		t.Errorf("colored doctor without its colors differs from the plain output:\n%q\nwant\n%q", got, plain)
	}
	for _, tc := range []struct {
		name    string
		tuiMode bool
		environ []string
		args    []string
	}{
		{"NO_COLOR", true, append(colorTTY, "NO_COLOR=1"), []string{"doctor"}},
		{"CLICOLOR=0", true, append(colorTTY, "CLICOLOR=0"), []string{"doctor"}},
		{"not a terminal", true, []string{"TERM=xterm-256color"}, []string{"doctor"}},
		{"plain mode", false, colorTTY, []string{"doctor"}},
		{"json", true, colorTTY, []string{"doctor", "--json"}},
	} {
		got, _, _ := runColored(t, d, tc.tuiMode, tc.environ, tc.args...)
		if colorSGR.MatchString(got) {
			t.Errorf("%s: colors in\n%q", tc.name, got)
		}
		if tc.name != "NO_COLOR" && tc.name != "CLICOLOR=0" && strings.Contains(got, "\x1b") {
			t.Errorf("%s: escape sequences in\n%q", tc.name, got)
		}
		if tc.name == "plain mode" && got != plain {
			t.Errorf("plain mode: output changed:\n%q\nwant\n%q", got, plain)
		}
	}
}

// The summary of a TUI command colors its status words; its notes and
// warnings on standard error color their marker.
func TestTUISummaryAndWarningsColored(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	d := e.deps()
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	d.environ = func() []string { return colorTTY }
	var out, errb syncBuffer
	if code := executeDeps(Streams{In: newScript(t, false), Out: &out, Err: &errb}, d, []string{"fetch"}); code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out.String(), errb.String())
	}
	if want := sgrGreen + "done" + sgrReset + "     alpha: "; !strings.Contains(out.String(), want) {
		t.Errorf("fetch summary lacks %q:\n%q", want, out.String())
	}
	d.environ = func() []string { return append(colorTTY, "NO_COLOR=1") }
	out = syncBuffer{}
	executeDeps(Streams{In: newScript(t, false), Out: &out, Err: &errb}, d, []string{"fetch"})
	if colorSGR.MatchString(out.String()) {
		t.Errorf("NO_COLOR: colors in\n%q", out.String())
	}

	e2 := newBundleEnv(t)
	e2.origin(t, "o", "a", false)
	e2.mustRun(t, "add", "--org", "o", "--all")
	e2.fake.ListErr["o"] = errors.New("listing failed")
	d = e2.deps()
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	d.environ = func() []string { return colorTTY }
	out, errb = syncBuffer{}, syncBuffer{}
	executeDeps(Streams{In: newScript(t, false), Out: &out, Err: &errb}, d, []string{"sync"})
	if want := "sheaf: " + sgrYellow + "warning:" + sgrReset + " "; !strings.Contains(errb.String(), want) {
		t.Errorf("sync stderr lacks %q:\n%q", want, errb.String())
	}
	// --json never colors, on standard output nor on standard error.
	out, errb = syncBuffer{}, syncBuffer{}
	executeDeps(Streams{In: newScript(t, false), Out: &out, Err: &errb}, d, []string{"sync", "--json"})
	if !strings.Contains(errb.String(), "sheaf: warning: ") || strings.Contains(out.String()+errb.String(), "\x1b") {
		t.Errorf("sync --json: stdout %q, stderr %q", out.String(), errb.String())
	}
}

// The update summary of the TUI is colored on the terminal (its tabwriter
// writes one cell at a time).
func TestTUIUpdateSummaryColored(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	d := e.deps()
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	d.environ = func() []string { return colorTTY }
	var out, errb syncBuffer
	if code := executeDeps(Streams{In: newScript(t, false), Out: &out, Err: &errb}, d, []string{"update"}); code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out.String(), errb.String())
	}
	if want := sgrGreen + "up to date" + sgrReset + "  "; !strings.Contains(out.String(), want) {
		t.Errorf("update summary lacks %q:\n%q", want, out.String())
	}
}

// The report view of the home screen colors the level words of what the
// action printed, although its output was captured without escape
// sequences.
func TestHomeReportColorsLevels(t *testing.T) {
	t.Parallel()
	output := "OK    git\n\x1b[33mWARN\x1b[m  gh not found\n      hint: install gh\nfailed   alpha: boom\n"
	raw := homeReport("doctor", "/b", output, "doctor: done").View().Content
	for _, want := range []string{sgrGreen + "OK", sgrYellow + "WARN", sgrRed + "failed"} {
		if !strings.Contains(raw, want) {
			t.Errorf("report lacks %q:\n%q", want, raw)
		}
	}
	if strings.Contains(raw, "\x1b[33mWARN\x1b[m  gh") && strings.Count(raw, sgrYellow) > 1 {
		t.Errorf("captured escape sequences kept:\n%q", raw)
	}
}

// The STATE cell of an overview row has the level of the checkout's
// state, from its structured status: worst wins.
func TestHomeLevel(t *testing.T) {
	t.Parallel()
	st := func(mod func(s *gitx.RepoStatus)) ops.CheckoutStatus {
		s := &gitx.RepoStatus{Upstream: "origin/main"}
		if mod != nil {
			mod(s)
		}
		return ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a", Branch: "main"}, Status: s}
	}
	detached := st(func(s *gitx.RepoStatus) { s.Upstream = "" })
	detached.Detached = true
	for _, tc := range []struct {
		name string
		c    ops.CheckoutStatus
		want tui.Level
	}{
		{"clean", st(nil), tui.LevelOK},
		{"clean, detached (tag)", detached, tui.LevelOK},
		{"clean, no upstream", st(func(s *gitx.RepoStatus) { s.Upstream = "" }), tui.LevelInfo},
		{"dirty", st(func(s *gitx.RepoStatus) { s.Unstaged = 1 }), tui.LevelWarn},
		{"untracked", st(func(s *gitx.RepoStatus) { s.Untracked = 1 }), tui.LevelWarn},
		{"dirty, no upstream", st(func(s *gitx.RepoStatus) { s.Upstream, s.Staged = "", 1 }), tui.LevelWarn},
		{"ahead", st(func(s *gitx.RepoStatus) { s.Ahead = 2 }), tui.LevelWarn},
		{"behind", st(func(s *gitx.RepoStatus) { s.Behind = 1 }), tui.LevelWarn},
		{"diverged", st(func(s *gitx.RepoStatus) { s.Ahead, s.Behind = 1, 1 }), tui.LevelFail},
		{"upstream gone", st(func(s *gitx.RepoStatus) { s.UpstreamGone = true }), tui.LevelFail},
		{"dirty, behind", st(func(s *gitx.RepoStatus) { s.Conflicted, s.Behind = 1, 3 }), tui.LevelWarn},
		{"dirty, diverged", st(func(s *gitx.RepoStatus) { s.Untracked, s.Ahead, s.Behind = 1, 2, 3 }), tui.LevelFail},
		{"error", ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a"}, Err: errors.New("boom")}, tui.LevelFail},
		{"PR", prStatus(nil, ops.PullRequestHead{Number: 7}), tui.LevelOK},
		{"PR recorded, in sync", prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true}), tui.LevelOK},
		{"PR, dirty", prStatus(func(s *gitx.RepoStatus) { s.Unstaged = 1 }, ops.PullRequestHead{Number: 7}), tui.LevelWarn},
		{"PR, ahead", prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true, Ahead: 1}), tui.LevelWarn},
		{"PR, behind", prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true, Behind: 2}), tui.LevelWarn},
		{"PR, diverged", prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true, Ahead: 1, Behind: 2}), tui.LevelFail},
	} {
		if got := homeLevel(tc.c); got != tc.want {
			t.Errorf("%s (%s): level %d, want %d", tc.name, homeState(tc.c), got, tc.want)
		}
	}
}

// prStatus returns the status of a checkout on the branch of pull request
// pr (no upstream for git), modified by mod.
func prStatus(mod func(s *gitx.RepoStatus), pr ops.PullRequestHead) ops.CheckoutStatus {
	s := &gitx.RepoStatus{}
	if mod != nil {
		mod(s)
	}
	return ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a", Branch: fmt.Sprintf("pr/%d", pr.Number)}, Status: s, PullRequest: &pr}
}

// A pull request worktree (a branch tracking refs/pull/<n>/head, see
// ops.PullRequestHead) shows its number rather than "no upstream", and its
// commits ahead of and behind the head recorded by the last wt pr (B52
// E4).
func TestHomeStatePullRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c    ops.CheckoutStatus
		want string
	}{
		{prStatus(nil, ops.PullRequestHead{Number: 7}), "clean, PR #7"},
		{prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true}), "clean, PR #7"},
		{prStatus(func(s *gitx.RepoStatus) { s.Untracked = 1 }, ops.PullRequestHead{Number: 12}), "dirty, PR #12"},
		{prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true, Ahead: 2}), "clean, PR #7 ↑2"},
		{prStatus(nil, ops.PullRequestHead{Number: 7, Recorded: true, Ahead: 1, Behind: 3}), "clean, PR #7 ↑1 ↓3"},
	} {
		if got := homeState(tc.c); got != tc.want {
			t.Errorf("state %q, want %q", got, tc.want)
		}
	}
}

// The STATE cell gives the commits to push and pull as arrows (local refs
// only), omitting a zero side, and nothing when in sync.
func TestHomeState(t *testing.T) {
	t.Parallel()
	st := func(mod func(s *gitx.RepoStatus)) ops.CheckoutStatus {
		s := &gitx.RepoStatus{Upstream: "origin/main"}
		if mod != nil {
			mod(s)
		}
		return ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a", Branch: "main"}, Status: s}
	}
	detached := st(func(s *gitx.RepoStatus) { s.Upstream = "" })
	detached.Detached = true
	for _, tc := range []struct {
		c    ops.CheckoutStatus
		want string
	}{
		{st(nil), "clean"},
		{detached, "clean"},
		{st(func(s *gitx.RepoStatus) { s.Upstream = "" }), "clean, no upstream"},
		{st(func(s *gitx.RepoStatus) { s.Ahead = 2 }), "clean, ↑2"},
		{st(func(s *gitx.RepoStatus) { s.Unstaged, s.Behind = 1, 3 }), "dirty, ↓3"},
		{st(func(s *gitx.RepoStatus) { s.Ahead, s.Behind = 2, 1 }), "clean, ↑2 ↓1"},
		{st(func(s *gitx.RepoStatus) { s.Untracked, s.Ahead, s.Behind = 1, 12, 345 }), "dirty, ↑12 ↓345"},
		{st(func(s *gitx.RepoStatus) { s.UpstreamGone = true }), "clean, upstream gone"},
		{ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "a"}, Err: errors.New("boom\nmore")}, "error: boom"},
	} {
		if got := homeState(tc.c); got != tc.want {
			t.Errorf("homeState = %q, want %q", got, tc.want)
		}
	}
}
