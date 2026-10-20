package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
	"github.com/jponge/sheaf/internal/tui"
)

// runHomeWith executes the CLI with deps d, the TUI forced on and in as
// standard input.
func runHomeWith(t *testing.T, d Deps, in io.Reader, out *syncBuffer, args ...string) (stderr string, code int) {
	t.Helper()
	var errb syncBuffer
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	code = executeDeps(Streams{In: in, Out: out, Err: &errb}, d, args)
	return errb.String(), code
}

// ovKeys are key presses for an overview: the first one waits until the
// output contains marker n times, i.e. until that overview is displayed
// (the final view of an overview is empty: keys processed before its first
// frame would leave no trace of it).
func ovKeys(t *testing.T, out *syncBuffer, marker string, n int, ks ...string) []key {
	t.Helper()
	k := keys(ks...)
	k[0].wait = func() {
		deadline := time.Now().Add(20 * time.Second)
		for strings.Count(out.String(), marker) < n {
			if time.Now().After(deadline) {
				t.Errorf("output never contained %q %d times:\n%s", marker, n, out.String())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return k
}

// reportKey is the key press k for the report view after the action name:
// it waits until a report view of name was displayed, then for the grace
// period during which the view ignores keys (see tui.Report). The input of
// the view is read from its start, so the grace period has then elapsed
// even when an earlier view of name matched.
func reportKey(t *testing.T, out *syncBuffer, name, k string) []key {
	t.Helper()
	wait := waitOutput(t, out, "sheaf "+name+": ")
	return []key{{keys: k, wait: func() { wait(); time.Sleep(tui.ReportGrace) }}}
}

// declineRemoval is the input of the progress view of a worktree removal
// answering no to its question. A last key waits until the view shows the
// removal cancelled, so that the end of the input does not end the view
// before it: its remaining events would then be printed (see
// tuiSession.progress), and the home screen would show them in a report
// view, which a terminal never leads to.
func declineRemoval(t *testing.T, out *syncBuffer) []key {
	t.Helper()
	return []key{{keys: "n", wait: waitOutput(t, out, "[y/N]")}, {keys: "x", wait: waitOutput(t, out, "cancelled after")}}
}

// homeBundle has repositories alpha (primary main, worktree feat tracking
// origin/feat, one commit behind it after a fetch, with an untracked file)
// and beta. origin/main of alpha then gets two commits that are not
// fetched.
func homeBundle(t *testing.T) (e *env, alpha, feat, beta string, o testutil.Origin) {
	t.Helper()
	e = newBundleEnv(t)
	o, alpha = e.addRepo(t, "alpha", "feat")
	_, beta = e.addRepo(t, "beta")
	feat = addWorktree(t, alpha, "feat", "feat")
	testutil.AdvanceOrigin(t, o, "feat", "f.txt", "f\n", "remote feat")
	testutil.Git(t, alpha, "fetch", "-q", "origin")
	writeFile(t, filepath.Join(feat, "new.txt"), "n\n")
	testutil.AdvanceOrigin(t, o, "main", "m1.txt", "1\n", "remote main 1")
	testutil.AdvanceOrigin(t, o, "main", "m2.txt", "2\n", "remote main 2")
	return e, alpha, feat, beta, o
}

// helpOf returns the output of the root command's help, as printed when
// sheaf runs without a command and without the home screen.
func helpOf(t *testing.T, e *env) string {
	t.Helper()
	var out bytes.Buffer
	d := e.deps()
	d.ctl = newInterruptCtl()
	root := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &out}, d)
	root.RunE = nil
	if code := execute(root, d.ctl, nil); code != 0 {
		t.Fatalf("help: exit %d\n%s", code, out.String())
	}
	return out.String()
}

// TestBareSheafPlainBehavesAsANonRunnableRoot is a regression guard: in
// plain mode (no terminal or --no-tui) bare sheaf, its help and its errors
// are exactly those of a root command without the home screen.
func TestBareSheafPlainBehavesAsANonRunnableRoot(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	for _, args := range [][]string{
		{}, {"--no-tui"}, {"-j", "0"}, {"--bundle", "/nonexistent"}, {"--help"}, {"-h"}, {"help"},
		{"nosuch"}, {"__complete", ""}, {"__complete", "--"}, {"__complete", "st"}, {"--no-tui", "status", "--help"},
	} {
		var out1, err1, out2, err2 bytes.Buffer
		code1 := executeDeps(Streams{In: strings.NewReader(""), Out: &out1, Err: &err1}, e.deps(), args)
		d := e.deps()
		d.ctl = newInterruptCtl()
		ref := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out2, Err: &err2}, d)
		ref.RunE, ref.Run = nil, nil
		code2 := execute(ref, d.ctl, args)
		if code1 != code2 || out1.String() != out2.String() || err1.String() != err2.String() {
			t.Errorf("sheaf %q: exit %d, want %d\nstdout:\n%s\nwant:\n%s\nstderr:\n%s\nwant:\n%s",
				args, code1, code2, out1.String(), out2.String(), err1.String(), err2.String())
		}
	}
	if out, _, code := e.run(t); code != 0 || !strings.Contains(out, "Available Commands:") || strings.Contains(out, "sheaf [flags]") {
		t.Fatalf("bare sheaf without a terminal: exit %d\n%s", code, out)
	}
}

func TestBareSheafOutsideABundlePrintsHelpAndHint(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // the working directory is not in a bundle
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false), &out)
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	want := helpOf(t, e) + homeHint + "\n"
	if out.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
	if !strings.Contains(homeHint, "sheaf create") || !strings.Contains(homeHint, "sheaf init") {
		t.Fatalf("hint %q", homeHint)
	}
	// An explicit --bundle that is not a bundle is an error, as for every command.
	_, code = runHomeWith(t, e.deps(), newScript(t, false), &out, "--bundle", e.cwd)
	if code != 1 {
		t.Fatalf("--bundle on a non-bundle: exit %d", code)
	}
}

func TestHomeShowsLocalStateAndQuits(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	for _, quit := range []string{"q", "\x1b"} {
		var out syncBuffer
		errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, quit)), &out)
		if code != 0 {
			t.Fatalf("%q: exit %d\nstdout:\n%s\nstderr:\n%s", quit, code, out.String(), errs)
		}
		o := out.String()
		for _, want := range []string{"alpha", "beta", "feat", "dirty", "↓1", "clean", "REPO", "AGE", "STATE"} {
			if !strings.Contains(o, want) {
				t.Errorf("%q: overview lacks %q:\n%s", quit, want, o)
			}
		}
		// origin/main moved by two commits after the last fetch: the
		// overview does not fetch.
		if strings.Contains(o, "↓2") {
			t.Errorf("the overview fetched:\n%s", o)
		}
	}
}

// A bundle without repositories says what to run to add some (B62 H3),
// one command per line, with --bundle when it was given; init is not
// suggested: it refuses a directory that already has a sheaf.yaml. adopt
// takes a path from the current directory: outside the bundle root, the
// hint gives it from the root.
func TestHomeEmptyBundleSaysWhatToRun(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	root, elsewhere := e.cwd, t.TempDir()
	for _, c := range []struct {
		cwd        string
		args       []string
		cmd, adopt string
	}{
		{root, nil, "sheaf ", "<dir>"},
		{root, []string{"--bundle", root}, "sheaf --bundle " + root + " ", "<dir>"},
		{elsewhere, []string{"--bundle", root}, "sheaf --bundle " + root + " ", root + "/<dir>"},
	} {
		var out syncBuffer
		d := e.deps()
		d.Getwd = func() (string, error) { return c.cwd, nil }
		errs, code := runHomeWith(t, d, newScript(t, false, ovKeys(t, &out, "STATE", 1, "q")), &out, c.args...)
		o := ansi.Strip(out.String())
		want := []string{"No repositories yet. Add some with:", "  " + c.cmd + "add owner/repo", "  " + c.cmd + "add --org <org>",
			"  " + c.cmd + "adopt " + c.adopt + "  (clones already in the bundle)"}
		for _, w := range want {
			if code != 0 || !strings.Contains(o, w) || strings.Contains(o, "No rows.") {
				t.Fatalf("%q from %s: exit %d, want %q\nstdout:\n%s\nstderr:\n%s", c.args, c.cwd, code, w, o, errs)
			}
		}
	}
}

func TestHomeShowsProblems(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "problem: worktree directory missing") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// overviewRows returns the row lines of the first overview of the home
// screen in bundle e, as rendered without a window size and colors.
func overviewRows(t *testing.T, e *env) []string {
	t.Helper()
	_, rows := overviewLines(t, e.deps(), e.cwd, 0)
	return rows
}

// overviewLines returns the header and row lines of the first overview of
// the home screen with deps d in bundle root, as rendered without colors in
// a window width columns wide (0: no window size).
func overviewLines(t *testing.T, d Deps, root string, width int) (header string, rows []string) {
	t.Helper()
	h := &home{d: d, s: &tuiSession{ctx: context.Background()}, root: root, jobs: 4}
	ov, err := h.overview()
	if err != nil {
		t.Fatal(err)
	}
	var m tea.Model = ov
	if width > 0 {
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
	}
	row := regexp.MustCompile(`^(> |  )\[[ x]\] `)
	for _, l := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		switch {
		case row.MatchString(l):
			rows = append(rows, l)
		case strings.Contains(l, "REPO"):
			header = l
		}
	}
	return header, rows
}

// STATE gives the commits to push and pull as arrows, from local refs, and
// AGE the age of the HEAD commit of each checkout; a problem row has no
// age.
func TestHomeShowsAheadBehindAndAge(t *testing.T) {
	t.Parallel()
	e, alpha, feat, beta, _ := homeBundle(t)
	now := time.Now().Truncate(time.Second)
	testutil.CommitAt(t, alpha, "local", now.Add(-50*time.Hour))
	testutil.CommitAt(t, feat, "local", now.Add(-3*time.Hour-time.Minute))
	testutil.CommitAt(t, beta, "local", now.Add(-8*7*24*time.Hour))
	d := e.deps()
	d.now = func() time.Time { return now }
	header, rows := overviewLines(t, d, e.cwd, 0)
	want := []string{
		"> [ ] alpha  main  main    2d   clean, ↑1",
		"  [ ]        feat  feat    3h   dirty, ↑1 ↓1",
		"  [ ] beta   main  main    1mo  clean, ↑1",
	}
	if header != "      REPO   DIR   BRANCH  AGE  STATE" || !slices.Equal(rows, want) {
		t.Errorf("overview:\n%s\n%s\nwant rows:\n%s", header, strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	_, rows = overviewLines(t, d, e.cwd, 0)
	if len(rows) != 3 || !strings.HasPrefix(rows[1], "  [ ]        feat  -            problem: ") {
		t.Errorf("problem row with an age:\n%s", strings.Join(rows, "\n"))
	}
}

// The overview groups rows by repository: the rows of a repository are
// contiguous, problem rows included, and only the first shows its name.
func TestHomeGroupsRowsByRepository(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	want := func(rows []string, prefixes ...string) {
		t.Helper()
		if len(rows) != len(prefixes) {
			t.Fatalf("rows:\n%s\nwant prefixes %q", strings.Join(rows, "\n"), prefixes)
		}
		for i, p := range prefixes {
			if !strings.HasPrefix(rows[i], p) {
				t.Errorf("row %d %q, want prefix %q\nrows:\n%s", i, rows[i], p, strings.Join(rows, "\n"))
			}
		}
	}
	want(overviewRows(t, e), "> [ ] alpha  main ", "  [ ]        feat ", "  [ ] beta   main ")
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	rows := overviewRows(t, e)
	want(rows, "> [ ] alpha  main ", "  [ ]        feat ", "  [ ] beta   main ")
	if !strings.Contains(rows[1], "problem: worktree directory missing") {
		t.Errorf("row 1 is not the problem: %q", rows[1])
	}
}

// In a narrow window, BRANCH and then DIR are truncated so that STATE
// fits.
func TestHomeTruncatesBranchThenDir(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, _ := homeBundle(t)
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "feature/a-very-long-branch", filepath.Join(filepath.Dir(alpha), "a-very-long-directory"))
	row := func(width int) string {
		t.Helper()
		_, rows := overviewLines(t, e.deps(), e.cwd, width)
		for _, r := range rows {
			if strings.Contains(r, "a-very-") {
				return r
			}
		}
		t.Fatalf("width %d: no row of the worktree in\n%s", width, strings.Join(rows, "\n"))
		return ""
	}
	if r := row(70); !strings.Contains(r, " a-very-long-directory  feature/…  ") || !strings.HasSuffix(r, "  clean, no upstream") {
		t.Errorf("width 70: %q", r)
	}
	if r := row(55); !strings.Contains(r, " a-very-…  feature…  ") {
		t.Errorf("width 55: %q", r)
	}
}

// The AGE column uses the largest unit that fits, rounded down; see
// homeAge for the scale.
func TestHomeAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{-time.Hour, "now"},
		{0, "now"},
		{59 * time.Second, "now"},
		{time.Minute, "1m"},
		{time.Hour - time.Second, "59m"},
		{time.Hour, "1h"},
		{day - time.Second, "23h"},
		{day, "1d"},
		{7*day - time.Second, "6d"},
		{7 * day, "1w"},
		{30*day - time.Second, "4w"},
		{30 * day, "1mo"},
		{360*day - time.Second, "11mo"},
		{360 * day, "1y"},
		{365*day - time.Second, "1y"},
		{365 * day, "1y"},
		{800 * day, "2y"},
	} {
		if got := homeAge(now, now.Add(-tc.age)); got != tc.want {
			t.Errorf("age %v: %q, want %q", tc.age, got, tc.want)
		}
	}
	if got := homeAge(now, time.Time{}); got != "" {
		t.Errorf("unknown date: %q, want empty", got)
	}
}

// The titles of the overview and of the report view shorten the home
// directory ($HOME) to ~ (B52 E2); the lines of the report are what the
// action printed, unchanged (its hints must stay pasteable: a quoted ~ is
// not expanded).
func TestHomeTitlesShortenHome(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	root, err := filepath.EvalSymlinks(e.cwd)
	if err != nil {
		t.Fatal(err)
	}
	d := e.deps()
	d.Getenv = func(k string) string {
		if k == "HOME" {
			return filepath.Dir(root)
		}
		return ""
	}
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"), ovKeys(t, &out, "fetch: done", 1, "q"))
	errs, code := runHomeWith(t, d, in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	text := ansi.Strip(out.String())
	short := "~" + string(filepath.Separator) + filepath.Base(root)
	for _, want := range []string{"sheaf: " + short, "sheaf fetch: " + short} {
		if !strings.Contains(text, want) {
			t.Errorf("no title %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ": "+root) {
		t.Errorf("a title names %s:\n%s", root, text)
	}
	if got := homeReport("doctor", short, "OK    bundle "+root+"\n", "doctor: done").View().Content; !strings.Contains(ansi.Strip(got), "bundle "+root) {
		t.Errorf("report line changed:\n%s", got)
	}
}

// tildePath shortens the home directory home at the start of path to ~.
func TestTildePath(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	home := filepath.Join(sep+"home", "u")
	for _, tc := range []struct{ path, home, want string }{
		{filepath.Join(home, "b"), home, "~" + sep + "b"},
		{filepath.Join(home, "b", "c"), home + sep, "~" + sep + filepath.Join("b", "c")},
		{home, home, "~"},
		{home + "x" + sep + "b", home, home + "x" + sep + "b"},
		{filepath.Join(sep+"srv", "b"), home, filepath.Join(sep+"srv", "b")},
		{filepath.Join(home, "b"), "", filepath.Join(home, "b")},
		{filepath.Join(home, "b"), sep, filepath.Join(home, "b")},
		{filepath.Join(home, "b"), "relative", filepath.Join(home, "b")},
	} {
		if got := tildePath(tc.path, tc.home); got != tc.want {
			t.Errorf("tildePath(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
		}
	}
}

func TestHomeCtrlCExits130(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "\x03")), &out)
	if code != 130 || !strings.Contains(errs, "interrupted") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestHomeEnterPrintsPath(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "\r")), &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !strings.HasSuffix(out.String(), feat+"\n") {
		t.Fatalf("stdout does not end with the path of feat:\n%q", out.String())
	}
}

// Without the shell function (no SHEAF_CD_FILE), the key help labels enter
// "path"; the path is printed alone on standard output and a hint on
// standard error tells how to add the shell function for the shell of
// $SHELL (bash, with the startup file of macOS login shells; zsh; fish),
// else where to read about it (B52 D2).
func TestHomeEnterWithoutShellFunction(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	for _, tc := range []struct{ shell, hint string }{
		{"/bin/bash", `sheaf: to cd on enter, add the shell function to ~/.bashrc (~/.bash_profile on macOS): eval "$(sheaf shell-init bash)"`},
		{"/usr/bin/zsh", `sheaf: to cd on enter, add the shell function to ~/.zshrc: eval "$(sheaf shell-init zsh)"`},
		{"/opt/homebrew/bin/fish", `sheaf: to cd on enter, add the shell function to ~/.config/fish/config.fish: sheaf shell-init fish | source`},
		{"/bin/tcsh", `sheaf: to cd on enter, add the sheaf shell function (see "Shell integration" in the README)`},
		{"", `sheaf: to cd on enter, add the sheaf shell function (see "Shell integration" in the README)`},
	} {
		d := e.deps()
		d.Getenv = func(k string) string {
			if k == "SHELL" {
				return tc.shell
			}
			return ""
		}
		var out syncBuffer
		errs, code := runHomeWith(t, d, newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "\r")), &out)
		if code != 0 {
			t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", tc.shell, code, out.String(), errs)
		}
		if !strings.HasSuffix(out.String(), feat+"\n") || strings.Contains(out.String(), "shell") {
			t.Errorf("%s: stdout does not end with the path of feat alone:\n%q", tc.shell, out.String())
		}
		if errs != tc.hint+"\n" {
			t.Errorf("%s: stderr %q, want %q", tc.shell, errs, tc.hint+"\n")
		}
		// The short help stays one line of 79 columns (fits in 80).
		if text := ansi.Strip(out.String()); !strings.Contains(text, "space toggle • a/n all/none • enter path • u update • f fetch • ? more • q quit") || strings.Contains(text, "enter cd") {
			t.Errorf("%s: key help without \"enter path\":\n%s", tc.shell, text)
		}
	}
}

func TestHomeEnterWritesTheCDFileOfTheShellWrapper(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	cdFile := filepath.Join(t.TempDir(), "cd")
	writeFile(t, cdFile, "")
	d := e.deps()
	d.Getenv = func(k string) string {
		if k == "SHEAF_CD_FILE" {
			return cdFile
		}
		return ""
	}
	var out syncBuffer
	errs, code := runHomeWith(t, d, newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "\r")), &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if got, _ := os.ReadFile(cdFile); string(got) != feat {
		t.Fatalf("cd file = %q, want %q", got, feat)
	}
	if strings.Contains(out.String(), feat+"\n") {
		t.Fatalf("the path was also printed:\n%q", out.String())
	}
	if errs != "" || !strings.Contains(ansi.Strip(out.String()), "• enter cd •") {
		t.Fatalf("hint printed, or no \"enter cd\" in the key help:\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
}

func TestHomeUpdateSelectedCheckoutThenReturns(t *testing.T) {
	t.Parallel()
	e, alpha, feat, _, o := homeBundle(t)
	writeFile(t, filepath.Join(feat, "new.txt"), "") // untracked files do not block update
	oldMain := testutil.Git(t, alpha, "rev-parse", "HEAD")
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", " ", "u"), keys(), reportKey(t, &out, "update", "q"), ovKeys(t, &out, "update: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if got, want := testutil.Git(t, feat, "rev-parse", "HEAD"), testutil.Git(t, o.Path, "rev-parse", "feat"); got != want {
		t.Errorf("feat not fast-forwarded:\n%s\n%s", out.String(), errs)
	}
	if got := testutil.Git(t, alpha, "rev-parse", "HEAD"); got != oldMain {
		t.Error("alpha/main was updated although not selected")
	}
	o2 := out.String()
	for _, want := range []string{"fast-forwarded  alpha/feat", "1 fast-forwarded, 0 up to date, 0 skipped, 0 failed", "update: done"} {
		if !strings.Contains(o2, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o2)
		}
	}
	// The overview was opened again, after the summary, refreshed.
	if i, j := strings.Index(o2, "1 fast-forwarded"), strings.LastIndex(o2, "STATE"); j < i {
		t.Errorf("no overview after the summary:\n%s", o2)
	}
	// The summary is printed as update runs, and the report view shows it
	// again, but not the progress view, which renders on the terminal only.
	view, done := strings.Index(o2, "sheaf update: "), strings.Index(o2, "update: done")
	if i := strings.Index(o2, "1 fast-forwarded"); i < 0 || i > view {
		t.Errorf("the summary was not printed before the report view:\n%s", o2)
	}
	if view < 0 || done < view || !strings.Contains(o2[view:done], "fast-forwarded") || strings.Contains(o2[view:done], "Updating") {
		t.Errorf("the report view lacks the summary or shows the progress view:\n%s", o2)
	}
}

func TestHomeFetchCurrentRepository(t *testing.T) {
	t.Parallel()
	e, alpha, _, beta, o := homeBundle(t)
	before := testutil.Git(t, beta, "rev-parse", "origin/main")
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"), ovKeys(t, &out, "fetch: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if got, want := testutil.Git(t, alpha, "rev-parse", "origin/main"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Errorf("alpha not fetched:\n%s", out.String())
	}
	if got := testutil.Git(t, beta, "rev-parse", "origin/main"); got != before {
		t.Error("beta changed")
	}
	if !strings.Contains(out.String(), "1 done, 0 skipped, 0 failed") || !strings.Contains(out.String(), "fetch: done") {
		t.Errorf("summary or message missing:\n%s", out.String())
	}
	// After the fetch, the refreshed overview shows alpha/main behind.
	if !strings.Contains(out.String(), "↓2") {
		t.Errorf("overview not refreshed:\n%s", out.String())
	}
}

// On an output that is not a terminal (a TUI forced on), the home screen
// and its actions never set the window title (OSC 0, 2).
func TestHomeOnNonTerminalSetsNoWindowTitle(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"), ovKeys(t, &out, "fetch: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), "Fetching") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	for _, seq := range []string{"\x1b]0;", "\x1b]2;"} {
		if strings.Contains(out.String(), seq) {
			t.Errorf("output contains %q:\n%q", seq, out.String())
		}
	}
}

// On an output that is not a terminal (a TUI forced on), the overview and
// the report view never turn mouse reporting on (B49).
func TestHomeOnNonTerminalEnablesNoMouse(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"), ovKeys(t, &out, "fetch: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), "sheaf fetch: ") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if m := regexp.MustCompile(`\x1b\[\?100[0236][hl]`).FindAllString(out.String(), -1); len(m) > 0 {
		t.Errorf("mouse mode sequences %q", m)
	}
}

func TestHomeAddWorktreeForBranchAndTag(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, o := homeBundle(t)
	testutil.TagOrigin(t, o, "v1", "main", false)
	var out syncBuffer
	// Each prompt shows the hint of its mode (waited for on the emulated
	// screen: after tab, the renderer may rewrite only part of the line).
	hinted := func(hint string, ks ...string) []key {
		k := keys(ks...)
		k[0].wait = waitScreen(t, &out, hint)
		return k
	}
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "w"), hinted("an existing local or remote branch, else a new branch from the default branch", "t", "o", "p", "i", "c", "/", "y", "\r"), reportKey(t, &out, "wt add", "q"),
		ovKeys(t, &out, "wt add: done", 1, "w"), append(keys("v", "1", "\t"), hinted("a tag: a detached worktree pinned at it", "\r")...), reportKey(t, &out, "wt add", "q"),
		ovKeys(t, &out, "wt add: done", 2, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	repo := filepath.Dir(alpha)
	if !exists(filepath.Join(repo, "topic-y", ".git")) || !exists(filepath.Join(repo, "v1", ".git")) {
		t.Fatalf("worktrees not created\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
	if got := testutil.Git(t, filepath.Join(repo, "topic-y"), "branch", "--show-current"); got != "topic/y" {
		t.Errorf("branch %q", got)
	}
	o2 := out.String()
	for _, want := range []string{"New worktree of alpha", filepath.Join(repo, "topic-y") + "\n", "(tag v1)", "wt add: done"} {
		if !strings.Contains(o2, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o2)
		}
	}
	// esc in the prompt cancels and returns to the overview.
	out = syncBuffer{}
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("x", "\x1b"), ovKeys(t, &out, "wt add: cancelled", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "wt add: cancelled") || exists(filepath.Join(repo, "x")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// The w prompt suggests the branches and the tags of the repository, read
// from its local refs (B47): → accepts the suggested branch, ctrl+e the
// suggested tag.
func TestHomeAddWorktreeSuggestsBranchesAndTags(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, _ := homeBundle(t)
	testutil.Git(t, alpha, "branch", "fix/local")
	testutil.Git(t, alpha, "tag", "v1.2")
	var out syncBuffer
	suggested := func(want string, ks ...string) []key {
		k := keys(ks...)
		k[0].wait = waitScreen(t, &out, want)
		return k
	}
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "w"), slices.Concat(keys("f", "i"), suggested("branch: fi█x/local", "\x1b[C"), suggested("branch: fix/local█", "\r")), reportKey(t, &out, "wt add", "q"),
		ovKeys(t, &out, "wt add: done", 1, "w"), slices.Concat(keys("\t", "v"), suggested("tag: v█1.2", "\x05"), suggested("tag: v1.2█", "\r")), reportKey(t, &out, "wt add", "q"),
		ovKeys(t, &out, "wt add: done", 2, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	repo := filepath.Dir(alpha)
	if got := testutil.Git(t, filepath.Join(repo, "fix-local"), "branch", "--show-current"); got != "fix/local" {
		t.Errorf("branch %q", got)
	}
	if !exists(filepath.Join(repo, "v1.2", ".git")) {
		t.Fatalf("tag worktree not created\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
}

// a selects every row (as in every list) and no longer adds a worktree,
// which w does; n deselects every row. The fetch summaries count the
// repositories targeted: both after a, the current one after a then n.
func TestHomeSelectAllAndNoneAndAddWithW(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "a", "f"), keys(), reportKey(t, &out, "fetch", "q"),
		ovKeys(t, &out, "fetch: done", 1, "a", "n", "f"), keys(), reportKey(t, &out, "fetch", "q"),
		ovKeys(t, &out, "fetch: done", 2, "w"), keys("t", "o", "p", "i", "c", "/", "z", "\r"), reportKey(t, &out, "wt add", "q"),
		ovKeys(t, &out, "wt add: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	all := strings.Index(o, "2 done, 0 skipped, 0 failed")
	none := strings.Index(o, "1 done, 0 skipped, 0 failed")
	prompt := strings.Index(o, "New worktree of alpha")
	if all < 0 || none < all || prompt < none {
		t.Fatalf("a/n did not select all/no rows, or w did not open the prompt after them\nstdout:\n%s\nstderr:\n%s", o, errs)
	}
	if !exists(filepath.Join(filepath.Dir(alpha), "topic-z", ".git")) {
		t.Fatalf("worktree not created\nstdout:\n%s\nstderr:\n%s", o, errs)
	}
}

// Each program reads its own segment of the scripted input, whatever the
// scheduling of the readers: here the overview quits on w before its reader
// has read the end of its segment, and the prompt's reader reads first.
func TestHomeScriptSegmentsBelongToTheirProgram(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, _ := homeBundle(t)
	var out syncBuffer
	q := keys("q")
	q[0].wait = waitOutput(t, &out, "wt add: done")
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("x", "y", "\r"), reportKey(t, &out, "wt add", "q"), q)
	in.holdFirstEnd = true
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), "wt add: done") || !exists(filepath.Join(filepath.Dir(alpha), "xy", ".git")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// A failed action shows its error in the report view until a key is
// pressed, then the overview comes back with the failure as message.
func TestHomeAddWorktreeFailureIsShownAndReturns(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	// main is the primary's branch: wt add refuses it.
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("m", "a", "i", "n", "\r"),
		reportKey(t, &out, "wt add", "q"), ovKeys(t, &out, "wt add failed", 1, "q")), &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	if !strings.Contains(errs, "sheaf: ") || !strings.Contains(o, "wt add failed: ") {
		t.Fatalf("failure not reported, or not on stderr\nstdout:\n%s\nstderr:\n%s", o, errs)
	}
	view, failed := strings.Index(o, "sheaf wt add: "), strings.Index(o, "wt add failed: ")
	if strings.Count(o, "sheaf wt add: ") != 1 || view > failed || !strings.Contains(o[view:failed], "sheaf: ") {
		t.Fatalf("no report view with the error before the overview came back\nstdout:\n%s", o)
	}
}

// ctrl+c in the report view after a failure is an interruption, as in the
// other dialogs of the home screen (exit 130); input ending there returns
// to the overview.
func TestHomeFailureReportCtrlCAndEOF(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("m", "a", "i", "n", "\r"),
		[]key{{keys: "\x03", wait: waitOutput(t, &out, "sheaf wt add: ")}}), &out)
	if code != 130 || !strings.Contains(errs, "interrupted") || strings.Contains(out.String(), "wt add failed: ") {
		t.Fatalf("ctrl+c in the report view: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	out = syncBuffer{}
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("m", "a", "i", "n", "\r"),
		keys(), ovKeys(t, &out, "wt add failed", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "sheaf wt add: ") || !strings.Contains(out.String(), "wt add failed: ") {
		t.Fatalf("input ending in the report view: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// doctor shows its report in the report view (see tui.Report), its failure
// included: a key after the grace period returns to the overview, ctrl+c
// exits 130 and input ending returns to the overview.
func TestHomeDoctorShowsReportView(t *testing.T) {
	t.Parallel()
	t.Run("warnings", func(t *testing.T) {
		t.Parallel()
		e, _, _, _, _ := homeBundle(t)
		var out syncBuffer
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), reportKey(t, &out, "doctor", "q"), ovKeys(t, &out, "doctor: done", 1, "q"))
		errs, code := runHomeWith(t, probeDeps(e, hosting.ErrGHNotFound), in, &out)
		if o := out.String(); code != 0 || errs != "" || !strings.Contains(o, "sheaf doctor: ") || !strings.Contains(o, "WARN  gh not found") {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, o, errs)
		}
	})
	t.Run("failure and warnings", func(t *testing.T) {
		t.Parallel()
		e, _, _, beta, _ := homeBundle(t)
		if err := os.RemoveAll(filepath.Dir(beta)); err != nil {
			t.Fatal(err)
		}
		var out syncBuffer
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), reportKey(t, &out, "doctor", "q"), ovKeys(t, &out, "doctor failed", 1, "q"))
		errs, code := runHomeWith(t, probeDeps(e, hosting.ErrGHNotFound), in, &out)
		o := out.String()
		if code != 0 || !strings.Contains(errs, "sheaf: doctor found problems (FAIL)") {
			t.Fatalf("exit %d, the error is not on stderr\nstdout:\n%s\nstderr:\n%s", code, o, errs)
		}
		// The report, then the error, are in the report view (before the
		// overview with the failure as message).
		report := o[strings.Index(o, "sheaf doctor: "):strings.Index(o, "doctor failed: doctor found problems (FAIL)")]
		if i, j, k := strings.Index(report, "sheaf doctor: "), strings.Index(report, "FAIL  beta: "), strings.Index(report, "sheaf: doctor found problems (FAIL)"); i < 0 || j < i || k < j {
			t.Fatalf("report view lacks the title, the report or the error:\n%s", o)
		}
	})
	t.Run("ctrl+c and EOF", func(t *testing.T) {
		t.Parallel()
		e, _, _, _, _ := homeBundle(t)
		var out syncBuffer
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), []key{{keys: "\x03", wait: waitOutput(t, &out, "sheaf doctor: ")}})
		errs, code := runHomeWith(t, probeDeps(e, nil), in, &out)
		if code != 130 || !strings.Contains(errs, "interrupted") || strings.Contains(out.String(), "doctor: done") {
			t.Fatalf("ctrl+c in the report: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
		}
		out = syncBuffer{}
		in = newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), keys(), ovKeys(t, &out, "doctor: done", 1, "q"))
		errs, code = runHomeWith(t, probeDeps(e, nil), in, &out)
		if code != 0 || !strings.Contains(out.String(), "sheaf doctor: ") {
			t.Fatalf("input ending in the report: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
		}
	})
}

// Every action that ran shows what it printed in the report view before the
// overview comes back, whether it succeeded, failed or changed nothing; an
// action cancelled in its dialog does not.
func TestHomeShowsReportAfterActionsThatRan(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t) // nothing to prune
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"),
		ovKeys(t, &out, "fetch: done", 1, "w"), keys("x", "\x1b"),
		ovKeys(t, &out, "wt add: cancelled", 1, "j", "r"), declineRemoval(t, &out),
		ovKeys(t, &out, "wt rm: cancelled", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "q"),
		ovKeys(t, &out, "prune: nothing to prune", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	o := out.String()
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, o, errs)
	}
	// The view of the fetch shows its summary (printed, not the progress
	// view's header) before the overview reporting it; the view of prune
	// its note. None after the cancelled add and removal.
	at := func(s string) int { return strings.Index(o, s) }
	if n := strings.Count(o, "sheaf fetch: "); n != 1 || at("fetch: done") < at("sheaf fetch: ") || !strings.Contains(o[at("sheaf fetch: "):at("fetch: done")], "alpha: fetched origin") {
		t.Errorf("%d fetch report views, want one with the summary before the overview:\n%s", n, o)
	}
	prune := at("sheaf prune: ")
	if n := strings.Count(o, "sheaf prune: "); n != 1 || prune < at("wt rm: cancelled") || !strings.Contains(o[prune:], "nothing to prune") || prune > at("prune: nothing to prune") {
		t.Errorf("%d prune report views, want one with its note after the cancelled actions:\n%s", n, o)
	}
	if strings.Contains(o, "sheaf wt add: ") || strings.Contains(o, "sheaf wt rm: ") {
		t.Errorf("report view after a cancelled action:\n%s", o)
	}
}

func TestHomeRemoveWorktreeConfirmsAndRefusesPrimary(t *testing.T) {
	e, alpha, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	// r on the primary is refused in the overview; then r on feat, confirmed.
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "r"), key{keys: "j", wait: waitOutput(t, &out, "primary checkout")}, key{keys: "r"}), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, reportKey(t, &out, "wt rm", "q"), ovKeys(t, &out, "wt rm: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(feat) || !exists(alpha) {
		t.Fatalf("feat not removed or primary removed\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
	for _, want := range []string{"primary checkout, not a worktree; run: sheaf remove -- alpha", "Remove worktree feat of alpha?", "wt rm: done"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

// The hints of the actions do not name the bundle that the home screen
// passes to them with --bundle (B52 U1 review): here r on feat, refused
// for its untracked file.
func TestHomeActionHintsDoNotNameThePassedBundle(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), keys(), reportKey(t, &out, "wt rm", "q"), ovKeys(t, &out, "wt rm failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), "run: sheaf wt rm --force --dir feat -- alpha") || strings.Contains(out.String(), "--bundle") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

func TestHomeRemoveDeclinedKeepsWorktree(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), declineRemoval(t, &out), ovKeys(t, &out, "wt rm: cancelled", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !exists(feat) || !strings.Contains(out.String(), "wt rm: cancelled") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

func TestHomePruneDoctorAndSync(t *testing.T) {
	e, root := syncBundle(t)
	o, primary := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, primary, "feat", "feat")
	testutil.Git(t, o.Path, "branch", "-D", "feat") // gone upstream: a prune candidate
	d := probeDeps(e, nil)
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "p"), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, reportKey(t, &out, "prune", "q"),
		ovKeys(t, &out, "prune: done", 1, "d"), reportKey(t, &out, "doctor", "q"),
		ovKeys(t, &out, "doctor: done", 1, "s"), keys(" ", "\r"), keys("\r"), keys(), reportKey(t, &out, "sync", "q"),
		ovKeys(t, &out, "sync: done", 1, "q"))
	errs, code := runHomeWith(t, d, in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(wt) {
		t.Errorf("not pruned:\n%s", out.String())
	}
	if !exists(filepath.Join(root, "b", "main")) {
		t.Errorf("sync did not add b:\n%s", out.String())
	}
	for _, want := range []string{"1 removed, 0 kept, 0 failed", "prune: done", "OK", "doctor: done", "sync: done"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	for _, name := range []string{"prune", "doctor", "sync"} {
		if n := strings.Count(out.String(), "sheaf "+name+": "); n != 1 {
			t.Errorf("%d report views of %s, want 1:\n%s", n, name, out.String())
		}
	}
}

// Enter, u and r on a row with a problem (here a missing worktree of
// alpha, the second row, below its primary) are refused in the overview,
// which stays open, naming the row.
func TestHomeRefusesEnterAndUpdateOnProblemRows(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"\r", "u", "r"} {
		e, _, feat, _, _ := homeBundle(t)
		if err := os.RemoveAll(feat); err != nil {
			t.Fatal(err)
		}
		var out syncBuffer
		in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", k),
			key{keys: "q", wait: waitOutput(t, &out, problemRefusal)}))
		errs, code := runHomeWith(t, e.deps(), in, &out)
		if code != 0 || strings.Contains(out.String(), "Updating") || strings.Contains(out.String(), "Remove worktree") || strings.HasSuffix(out.String(), feat+"\n") {
			t.Fatalf("%q: exit %d\nstdout:\n%s\nstderr:\n%s", k, code, out.String(), errs)
		}
	}
}

// problemRefusal is the refusal of an action on the problem row of
// homeBundle's feat worktree once deleted.
const problemRefusal = "problem row, see `sheaf doctor`: alpha/feat"

// u on a selection of problem rows only is refused with the reason first,
// then the names of the rows (so that a cut message keeps the reason).
func TestHomeRefusesSelectionOfProblemRowsReasonFirst(t *testing.T) {
	t.Parallel()
	e, alpha, feat, _, _ := homeBundle(t)
	other := filepath.Join(filepath.Dir(alpha), "other")
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "other", other)
	for _, p := range []string{feat, other} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	var out syncBuffer
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", " ", "j", " ", "u"),
		key{keys: "q", wait: waitOutput(t, &out, "problem rows, see `sheaf doctor`: alpha/")}))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	o := out.String()
	if code != 0 || strings.Contains(o, "Updating") || !strings.Contains(o, "alpha/feat") || !strings.Contains(o, "alpha/other") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, o, errs)
	}
}

// u on a selection with a problem row (here every row, selected with a)
// updates the other rows and says the problem row was skipped, naming it
// (B52 A4).
func TestHomeUpdateSkipsSelectedProblemRows(t *testing.T) {
	t.Parallel()
	e, alpha, feat, _, o := homeBundle(t)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "a", "u"), keys(), reportKey(t, &out, "update", "q"),
		ovKeys(t, &out, "update: done · skipped 1 problem row (alpha/feat)", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if got, want := testutil.Git(t, alpha, "rev-parse", "HEAD"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Errorf("alpha not updated: HEAD %s, origin main %s\nstdout:\n%s", got, want, out.String())
	}
}

// r on a selection with a problem row removes the selected worktrees and
// says the problem row was skipped, naming it (B52 A4). The rows: alpha's
// primary, other, the problem row of feat (after alpha's checkouts), beta.
func TestHomeRemoveSkipsSelectedProblemRows(t *testing.T) {
	t.Parallel()
	e, alpha, feat, _, _ := homeBundle(t)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(alpha), "other")
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "other", other)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", " ", "j", " ", "r"),
		[]key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: done · skipped 1 problem row (alpha/feat)", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || exists(other) {
		t.Fatalf("exit %d, other exists: %v\nstdout:\n%s\nstderr:\n%s", code, exists(other), out.String(), errs)
	}
}

// multiRemoveBundle is homeBundle with the clean worktrees alpha/x and
// beta/y. The rows: alpha's primary, feat (refused: untracked file), x,
// beta's primary, y.
func multiRemoveBundle(t *testing.T) (e *env, feat, x, y string) {
	t.Helper()
	e, alpha, feat, beta, _ := homeBundle(t)
	x = filepath.Join(filepath.Dir(alpha), "x")
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "x", x)
	y = filepath.Join(filepath.Dir(beta), "y")
	testutil.Git(t, beta, "worktree", "add", "-q", "-b", "y", y)
	return e, feat, x, y
}

// acceptRemoval is declineRemoval answering yes: its last key waits until
// the view shows the removal finished.
func acceptRemoval(t *testing.T, out *syncBuffer) []key {
	t.Helper()
	return []key{{keys: "y", wait: waitOutput(t, out, "[y/N]")}, {keys: "x", wait: waitOutput(t, out, "finished in")}}
}

// selectFeatXY selects feat, x and y in the overview of multiRemoveBundle,
// then presses r.
func selectFeatXY(t *testing.T, out *syncBuffer) []key {
	return ovKeys(t, out, "STATE", 1, "j", " ", "j", " ", "j", "j", " ", "r")
}

// r on several worktrees asks once (B52 C1): one dialog lists what will
// be removed, with the fate of each branch, and the refused worktree;
// the others are then removed and the refusal reported with its command.
func TestHomeRemovesSelectedWorktreesAfterOneConfirmation(t *testing.T) {
	t.Parallel()
	e, feat, x, y := multiRemoveBundle(t)
	var out syncBuffer
	in := newScript(t, false, selectFeatXY(t, &out), acceptRemoval(t, &out), reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: 2 done, 0 cancelled, 1 failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || exists(x) || exists(y) || !exists(filepath.Join(feat, "new.txt")) {
		t.Fatalf("exit %d, x %v, y %v, feat %v\nstdout:\n%s\nstderr:\n%s", code, exists(x), exists(y), exists(feat), out.String(), errs)
	}
	// The dialog and the report view wrap long lines at their width: the
	// paths, which may be long, are left out here (worktreeRemovalsLines is
	// checked line by line in TestWorktreeRemovalsLines), and the refusal
	// with its command is checked on standard error, where it is whole.
	o := ansi.Strip(out.String())
	for _, want := range []string{
		"Remove 2 worktrees?",
		"will remove worktree alpha/x [x] at", "branch x is kept",
		"will remove worktree beta/y [y] at", "branch y is kept",
		"refusing to remove alpha/feat: 1 untracked file",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("the output lacks %q:\n%s", want, o)
		}
	}
	if want := "sheaf: refusing to remove worktree feat [feat] of alpha:\n  1 untracked file\nto remove anyway (what is listed above will be lost), run: sheaf wt rm --force --dir feat -- alpha\n"; !strings.Contains(errs, want) {
		t.Errorf("stderr lacks %q:\n%s", want, errs)
	}
	report := o[strings.Index(o, "sheaf wt rm: "):]
	for _, want := range []string{"removed worktree x [x] at ", "removed worktree y [y] at ", "refusing to remove worktree feat [feat] of alpha:"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	for _, unwanted := range []string{"Remove worktree x of alpha?", "Remove worktree y of beta?", "unpushed"} {
		if strings.Contains(o, unwanted) {
			t.Errorf("the output shows %q:\n%s", unwanted, o)
		}
	}
}

// The lines of the dialog of several removals, unwrapped: for each
// worktree to remove its line with the path, then its plan indented; a
// refusal on one line; another failure with its repository and target.
func TestWorktreeRemovalsLines(t *testing.T) {
	t.Parallel()
	x := ops.WorktreeRemovePlan{Repo: "alpha", Worktree: ops.WorktreeInfo{Repo: "alpha", Dir: "x", Branch: "x", Path: "/some/long/bundle/alpha/x"},
		Risks: []gitx.Risk{{Kind: gitx.RiskIgnoredFiles, Count: 2}}}
	feat := ops.WorktreeRemovePlan{Repo: "alpha", Worktree: ops.WorktreeInfo{Repo: "alpha", Dir: "feat", Branch: "feat", Path: "/some/long/bundle/alpha/feat"},
		Risks: []gitx.Risk{{Kind: gitx.RiskUntracked, Count: 1}}}
	rs := []ops.WorktreeRemovalResult{
		{WorktreeRemoval: ops.WorktreeRemoval{Repo: "alpha", Target: ops.WorktreeTarget{Name: "x"}}, Plan: &x},
		{WorktreeRemoval: ops.WorktreeRemoval{Repo: "alpha", Target: ops.WorktreeTarget{Name: "feat"}}, Plan: &feat, Err: &ops.WorktreeRefusedError{Plan: feat}},
		{WorktreeRemoval: ops.WorktreeRemoval{Repo: "beta", Target: ops.WorktreeTarget{Name: "gone"}}, Err: errors.New("no worktree \"gone\" in beta\ndetails")},
	}
	lines, removable := worktreeRemovalsLines(rs, false)
	want := []string{
		"will remove worktree alpha/x [x] at /some/long/bundle/alpha/x",
		"  2 ignored files (informational, not blocking)",
		"  branch x is kept",
		"refusing to remove alpha/feat: 1 untracked file",
		"cannot remove beta/gone: no worktree \"gone\" in beta",
	}
	if removable != 1 || !slices.Equal(lines, want) {
		t.Fatalf("lines (%d removable):\n%s\nwant (1):\n%s", removable, strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// The hints of a removal of several worktrees name the bundle given with
// --bundle to bare sheaf, as those of a single removal do (B52 U1).
func TestHomeMultiRemoveHintsNameTheGivenBundle(t *testing.T) {
	t.Parallel()
	e, _, _, _ := multiRemoveBundle(t)
	var out syncBuffer
	in := newScript(t, false, selectFeatXY(t, &out), acceptRemoval(t, &out), reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: 2 done, 0 cancelled, 1 failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out, "--bundle", e.cwd)
	// The complete line on standard error, where the refusal is printed
	// whole (the report view, which shows it too, wraps long lines at its
	// width: the bundle path may be long).
	want := ", run: sheaf --bundle " + e.cwd + " wt rm --force --dir feat -- alpha\n"
	if code != 0 || !strings.Contains(errs, want) || !strings.Contains(out.String(), "sheaf wt rm: ") {
		t.Fatalf("exit %d, want %q on stderr and the report view\nstdout:\n%s\nstderr:\n%s", code, want, out.String(), errs)
	}
}

// The refused worktree is listed in the dialog itself, before the answer
// (the progress rows show the refusals only afterwards).
func TestHomeMultiRemoveDialogListsTheRefusedWorktree(t *testing.T) {
	t.Parallel()
	e, _, _, _ := multiRemoveBundle(t)
	var out syncBuffer
	in := newScript(t, false, selectFeatXY(t, &out), acceptRemoval(t, &out), reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: 2 done, 0 cancelled, 1 failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	o := ansi.Strip(out.String())
	answered := strings.Index(o, "Removing 3 worktrees")
	if code != 0 || answered < 0 || !strings.Contains(o[:answered], "refusing to remove alpha/feat: 1 untracked file") {
		t.Fatalf("exit %d; the dialog does not list the refusal\nstdout:\n%s\nstderr:\n%s", code, o, errs)
	}
}

// A selected worktree containing the working directory of sheaf is
// refused, as by wt rm; the others are removed.
func TestHomeMultiRemoveRefusesTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	e, feat, x, y := multiRemoveBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	d := e.deps()
	d.Getwd = func() (string, error) { return x, nil }
	var out syncBuffer
	in := newScript(t, false, selectFeatXY(t, &out), acceptRemoval(t, &out), reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: 2 done, 0 cancelled, 1 failed", 1, "q"))
	errs, code := runHomeWith(t, d, in, &out)
	if code != 0 || !exists(x) || exists(y) || exists(feat) || !strings.Contains(ansi.Strip(out.String()), "it contains the current working directory") {
		t.Fatalf("exit %d, x %v, y %v, feat %v\nstdout:\n%s\nstderr:\n%s", code, exists(x), exists(y), exists(feat), out.String(), errs)
	}
}

// Declining the one confirmation keeps every worktree and the selection.
func TestHomeMultiRemoveDeclinedKeepsAll(t *testing.T) {
	t.Parallel()
	e, feat, x, y := multiRemoveBundle(t)
	var out syncBuffer
	in := newScript(t, false, selectFeatXY(t, &out), declineRemoval(t, &out), ovKeys(t, &out, "wt rm: cancelled", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !exists(x) || !exists(y) || !exists(feat) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	cancelled := strings.Index(o, "wt rm: cancelled")
	if frame := o[strings.LastIndex(o[:cancelled], "STATE"):cancelled]; !strings.Contains(frame, "3/5 selected") {
		t.Errorf("selection lost after the cancelled removal:\n%s", o)
	}
	if n := strings.Count(o, "sheaf wt rm: "); n != 0 {
		t.Errorf("%d report views after a cancellation, want none:\n%s", n, o)
	}
}

// The key help of the overview is one short line (B45); ? shows the full
// list, and the next overviews of the home screen (here after a fetch)
// keep it.
func TestHomeCompactHelpAndKeptToggle(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	d := e.deps()
	d.Getenv = func(k string) string {
		if k == "SHEAF_CD_FILE" {
			return filepath.Join(t.TempDir(), "cd") // with the shell function: "enter cd"
		}
		return ""
	}
	var out syncBuffer
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "?"), key{keys: "f", wait: waitOutput(t, &out, "? less")}), keys(), reportKey(t, &out, "fetch", "q"), ovKeys(t, &out, "fetch: done", 1, "q"))
	errs, code := runHomeWith(t, d, in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	text := ansi.Strip(out.String())
	if short := "space toggle • a/n all/none • enter cd • u update • f fetch • ? more • q quit"; strings.Count(text, short) != 1 {
		t.Errorf("want the compact help %q once (first overview):\n%s", short, text)
	}
	if n := strings.Count(text, "? less"); n != 2 {
		t.Errorf("full help shown %d times, want 2 (after ?, and in the overview after the fetch):\n%s", n, text)
	}
}

// y (copy the path, B44) is refused on a row with a problem, and on an
// output that is not a terminal (a TUI forced on), where it sends no OSC 52
// sequence; the overview stays open.
func TestHomeCopyRefusedOnProblemRowsAndNonTerminals(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", "y"),
		key{keys: "k", wait: waitOutput(t, &out, problemRefusal)},
		key{keys: "y"},
		key{keys: "q", wait: waitOutput(t, &out, "copy not available: the output is not a terminal, or TERM=dumb")}))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if strings.Contains(out.String(), "\x1b]52;") {
		t.Errorf("output contains an OSC 52 sequence:\n%q", out.String())
	}
}

// r on a worktree outside its repository directory is refused: selecting
// it by directory name could remove another worktree of the same name.
func TestHomeRefusesRemovingOutsideWorktree(t *testing.T) {
	t.Parallel()
	e, alpha, _, _, _ := homeBundle(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "elsewhere", outside)
	var out syncBuffer
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "/", "e", "l", "s", "e", "\r", "r"),
		key{keys: "q", wait: waitOutput(t, &out, "outside its repository directory")}))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !exists(outside) || strings.Contains(out.String(), "wt rm failed") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

func TestHomeJobsValidatedInTUIMode(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, keys("q")), &out, "-j", "0")
	if code != 1 || !strings.Contains(errs, "invalid --jobs 0") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

// An action that changed nothing is reported as such, not as done; enter
// in the sync menu with nothing selected is a cancellation.
func TestHomeReportsActionsThatChangedNothing(t *testing.T) {
	t.Parallel()
	e, _ := syncBundle(t) // a tracked; b and c proposed by sync
	var out syncBuffer
	// "prune: " is in the title of the report view, then in the message
	// of the overview; likewise for sync.
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "q"),
		ovKeys(t, &out, "prune: ", 2, "s"), keys("\r"), reportKey(t, &out, "sync", "q"),
		ovKeys(t, &out, "sync: ", 2, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	for _, want := range []string{"prune: nothing to prune", "sync: cancelled"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
	for _, unwanted := range []string{"prune: done", "sync: done"} {
		if strings.Contains(o, unwanted) {
			t.Errorf("stdout has %q:\n%s", unwanted, o)
		}
	}
}

// A declined removal keeps the selection in the next overview (each
// worktree is still confirmed on its own); a completed one clears it.
func TestHomeKeepsSelectionAfterCancelledRemoval(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "j", " ", "r"), declineRemoval(t, &out),
		ovKeys(t, &out, "wt rm: cancelled", 1, "r"), ovKeys(t, &out, "[y/N]", 2, "y"), reportKey(t, &out, "wt rm", "q"),
		ovKeys(t, &out, "wt rm: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || exists(feat) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	cancelled := strings.Index(o, "wt rm: cancelled")
	if frame := o[strings.LastIndex(o[:cancelled], "STATE"):cancelled]; !strings.Contains(frame, "1/3 selected") {
		t.Errorf("selection lost after the cancelled removal:\n%s", o)
	}
	afterDone := o[strings.LastIndex(o, "STATE"):]
	if !strings.Contains(afterDone, "0/2 selected") {
		t.Errorf("selection kept after the removal:\n%s", afterDone)
	}
}

// A fetch of the selected rows that succeeded clears the selection; the
// note of an earlier action that changed nothing is not carried over.
func TestHomeClearsSelectionAfterSuccessAndForgetsEarlierNotes(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t) // nothing to prune
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "a", "f"), keys(), reportKey(t, &out, "fetch", "q"),
		ovKeys(t, &out, "fetch: done", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "q"),
		ovKeys(t, &out, "prune: nothing to prune", 1, "f"), keys(), reportKey(t, &out, "fetch", "q"),
		ovKeys(t, &out, "fetch: done", 2, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	fetched := strings.Index(o, "fetch: done")
	if frame := o[strings.LastIndex(o[:fetched], "STATE"):fetched]; !strings.Contains(frame, "0/3 selected") {
		t.Errorf("selection kept after a successful fetch of it:\n%s", frame)
	}
	if strings.Count(o, "fetch: done") != 2 || strings.Contains(o, "fetch: nothing to prune") {
		t.Errorf("the second fetch is not reported done:\n%s", o)
	}
}

// The report view shows the lines an action printed, without escape
// sequences, or its outcome when it printed nothing.
func TestHomeReportLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output string
		want   []string
	}{
		{"", []string{"update: done"}},
		{"\n", []string{"update: done"}},
		{"a\n\nb\n", []string{"a", "", "b"}},
		{"no newline", []string{"no newline"}},
		{"\x1b[31mred\x1b[0m\n", []string{"red"}},
	} {
		if got := homeReportLines(tc.output, "update: done"); !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.output, got, tc.want)
		}
	}
}

// What an action prints is written live to the terminal (here the line
// sync prints before listing, on standard error) and shown in the report
// view. Enter in the sync menu with nothing selected counts as a
// cancellation, as esc does; the report view is shown all the same, as the
// action printed something.
func TestHomeSyncOutputIsLiveAndInReport(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"\r", "\x1b"} {
		e, _ := syncBundle(t)
		var out syncBuffer
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "s"), keys(k), reportKey(t, &out, "sync", "q"), ovKeys(t, &out, "sync: cancelled", 1, "q"))
		errs, code := runHomeWith(t, e.deps(), in, &out)
		o := out.String()
		if code != 0 || !strings.Contains(errs, "listing repositories of ") {
			t.Fatalf("%q: exit %d, the listing line is not on stderr\nstdout:\n%s\nstderr:\n%s", k, code, o, errs)
		}
		view, msg := strings.Index(o, "sheaf sync: "), strings.Index(o, "sync: cancelled")
		if view < 0 || msg < view || !strings.Contains(o[view:msg], "listing repositories of ") {
			t.Fatalf("%q: no report view with the listing line before the overview\nstdout:\n%s", k, o)
		}
	}
}
