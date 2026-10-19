package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jponge/sheaf/internal/hosting"
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
	return []key{{keys: "n", wait: waitOutput(t, out, "[y/N]")}, {keys: "x", wait: waitOutput(t, out, "1 cancelled")}}
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
		for _, want := range []string{"alpha", "beta", "feat", "dirty", "behind 1", "clean", "REPO", "STATE"} {
			if !strings.Contains(o, want) {
				t.Errorf("%q: overview lacks %q:\n%s", quit, want, o)
			}
		}
		// origin/main moved by two commits after the last fetch: the
		// overview does not fetch.
		if strings.Contains(o, "behind 2") {
			t.Errorf("the overview fetched:\n%s", o)
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
	h := &home{d: e.deps(), s: &tuiSession{ctx: context.Background()}, root: e.cwd, jobs: 4}
	ov, err := h.overview()
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`^(> |  )\[[ x]\] `)
	var rows []string
	for _, l := range strings.Split(ansi.Strip(ov.View().Content), "\n") {
		if row.MatchString(l) {
			rows = append(rows, l)
		}
	}
	return rows
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
}

func TestHomeUpdateSelectedCheckoutThenReturns(t *testing.T) {
	t.Parallel()
	e, alpha, feat, _, o := homeBundle(t)
	writeFile(t, filepath.Join(feat, "new.txt"), "") // untracked files do not block update
	oldMain := testutil.Git(t, alpha, "rev-parse", "HEAD")
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", " ", "u"), keys(), reportKey(t, &out, "update", "x"), ovKeys(t, &out, "update: done", 1, "q"))
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
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "x"), ovKeys(t, &out, "fetch: done", 1, "q"))
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
	if !strings.Contains(out.String(), "behind 2") {
		t.Errorf("overview not refreshed:\n%s", out.String())
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
		ovKeys(t, &out, "STATE", 1, "w"), hinted("an existing local or remote branch, else a new branch from the default branch", "t", "o", "p", "i", "c", "/", "y", "\r"), reportKey(t, &out, "add", "x"),
		ovKeys(t, &out, "add: done", 1, "w"), append(keys("v", "1", "\t"), hinted("a tag: a detached worktree pinned at it", "\r")...), reportKey(t, &out, "add", "x"),
		ovKeys(t, &out, "add: done", 2, "q"))
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
	for _, want := range []string{"New worktree of alpha", filepath.Join(repo, "topic-y") + "\n", "(tag v1)", "add: done"} {
		if !strings.Contains(o2, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o2)
		}
	}
	// esc in the prompt cancels and returns to the overview.
	out = syncBuffer{}
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("x", "\x1b"), ovKeys(t, &out, "add: cancelled", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "add: cancelled") || exists(filepath.Join(repo, "x")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
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
		ovKeys(t, &out, "STATE", 1, "a", "f"), keys(), reportKey(t, &out, "fetch", "x"),
		ovKeys(t, &out, "fetch: done", 1, "a", "n", "f"), keys(), reportKey(t, &out, "fetch", "x"),
		ovKeys(t, &out, "fetch: done", 2, "w"), keys("t", "o", "p", "i", "c", "/", "z", "\r"), reportKey(t, &out, "add", "x"),
		ovKeys(t, &out, "add: done", 1, "q"))
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
	q[0].wait = waitOutput(t, &out, "add: done")
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("x", "y", "\r"), reportKey(t, &out, "add", "x"), q)
	in.holdFirstEnd = true
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), "add: done") || !exists(filepath.Join(filepath.Dir(alpha), "xy", ".git")) {
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
		reportKey(t, &out, "add", "x"), ovKeys(t, &out, "add failed", 1, "q")), &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	if !strings.Contains(errs, "sheaf: ") || !strings.Contains(o, "add failed: ") {
		t.Fatalf("failure not reported, or not on stderr\nstdout:\n%s\nstderr:\n%s", o, errs)
	}
	view, failed := strings.Index(o, "sheaf add: "), strings.Index(o, "add failed: ")
	if strings.Count(o, "sheaf add: ") != 1 || view > failed || !strings.Contains(o[view:failed], "sheaf: ") {
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
		[]key{{keys: "\x03", wait: waitOutput(t, &out, "sheaf add: ")}}), &out)
	if code != 130 || !strings.Contains(errs, "interrupted") || strings.Contains(out.String(), "add failed: ") {
		t.Fatalf("ctrl+c in the report view: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	out = syncBuffer{}
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "w"), keys("m", "a", "i", "n", "\r"),
		keys(), ovKeys(t, &out, "add failed", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "sheaf add: ") || !strings.Contains(out.String(), "add failed: ") {
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
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), reportKey(t, &out, "doctor", "x"), ovKeys(t, &out, "doctor: done", 1, "q"))
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
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "d"), reportKey(t, &out, "doctor", "x"), ovKeys(t, &out, "doctor failed", 1, "q"))
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
		ovKeys(t, &out, "STATE", 1, "f"), keys(), reportKey(t, &out, "fetch", "x"),
		ovKeys(t, &out, "fetch: done", 1, "w"), keys("x", "\x1b"),
		ovKeys(t, &out, "add: cancelled", 1, "j", "r"), declineRemoval(t, &out),
		ovKeys(t, &out, "remove: cancelled", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "x"),
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
	if n := strings.Count(o, "sheaf prune: "); n != 1 || prune < at("remove: cancelled") || !strings.Contains(o[prune:], "nothing to prune") || prune > at("prune: nothing to prune") {
		t.Errorf("%d prune report views, want one with its note after the cancelled actions:\n%s", n, o)
	}
	if strings.Contains(o, "sheaf add: ") || strings.Contains(o, "sheaf remove: ") {
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
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "r"), key{keys: "j", wait: waitOutput(t, &out, "primary checkout")}, key{keys: "r"}), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, reportKey(t, &out, "remove", "x"), ovKeys(t, &out, "remove: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(feat) || !exists(alpha) {
		t.Fatalf("feat not removed or primary removed\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
	for _, want := range []string{"primary checkout", "Remove worktree feat of alpha?", "remove: done"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

func TestHomeRemoveDeclinedKeepsWorktree(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), declineRemoval(t, &out), ovKeys(t, &out, "remove: cancelled", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !exists(feat) || !strings.Contains(out.String(), "remove: cancelled") {
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
		ovKeys(t, &out, "STATE", 1, "p"), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, reportKey(t, &out, "prune", "x"),
		ovKeys(t, &out, "prune: done", 1, "d"), reportKey(t, &out, "doctor", "x"),
		ovKeys(t, &out, "doctor: done", 1, "s"), keys(" ", "\r"), keys("\r"), keys(), reportKey(t, &out, "sync", "x"),
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

// Enter and u on a row with a problem (here a missing worktree of alpha,
// the second row, below its primary) are refused in the overview, which
// stays open.
func TestHomeRefusesEnterAndUpdateOnProblemRows(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"\r", "u"} {
		e, _, feat, _, _ := homeBundle(t)
		if err := os.RemoveAll(feat); err != nil {
			t.Fatal(err)
		}
		var out syncBuffer
		in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", k),
			key{keys: "q", wait: waitOutput(t, &out, "not available on a row with a problem")}))
		errs, code := runHomeWith(t, e.deps(), in, &out)
		if code != 0 || strings.Contains(out.String(), "Updating") || strings.HasSuffix(out.String(), feat+"\n") {
			t.Fatalf("%q: exit %d\nstdout:\n%s\nstderr:\n%s", k, code, out.String(), errs)
		}
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
	if code != 0 || !exists(outside) || strings.Contains(out.String(), "remove failed") {
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
		ovKeys(t, &out, "STATE", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "x"),
		ovKeys(t, &out, "prune: ", 2, "s"), keys("\r"), reportKey(t, &out, "sync", "x"),
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
		ovKeys(t, &out, "remove: cancelled", 1, "r"), ovKeys(t, &out, "[y/N]", 2, "y"), reportKey(t, &out, "remove", "x"),
		ovKeys(t, &out, "remove: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || exists(feat) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o := out.String()
	cancelled := strings.Index(o, "remove: cancelled")
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
		ovKeys(t, &out, "STATE", 1, "a", "f"), keys(), reportKey(t, &out, "fetch", "x"),
		ovKeys(t, &out, "fetch: done", 1, "p"), []key{{keys: "x", wait: waitOutput(t, &out, "nothing to prune")}}, reportKey(t, &out, "prune", "x"),
		ovKeys(t, &out, "prune: nothing to prune", 1, "f"), keys(), reportKey(t, &out, "fetch", "x"),
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
		in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "s"), keys(k), reportKey(t, &out, "sync", "x"), ovKeys(t, &out, "sync: cancelled", 1, "q"))
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
