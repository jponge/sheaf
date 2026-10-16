package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

// runHomeWith executes the CLI with deps d, the TUI forced on and in as
// standard input.
func runHomeWith(t *testing.T, d Deps, in io.Reader, out *syncBuffer, args ...string) (stderr string, code int) {
	t.Helper()
	var errb syncBuffer
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	cmd := NewRootCmdWithDeps(Streams{In: in, Out: out, Err: &errb}, d)
	code = execute(cmd, args)
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
	root := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &out}, e.deps())
	root.RunE = nil
	if code := execute(root, nil); code != 0 {
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
		home := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out1, Err: &err1}, e.deps())
		code1 := execute(home, args)
		ref := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out2, Err: &err2}, e.deps())
		ref.RunE, ref.Run = nil, nil
		code2 := execute(ref, args)
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
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", " ", "u"), keys(), ovKeys(t, &out, "update: done", 1, "q"))
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
}

func TestHomeFetchCurrentRepository(t *testing.T) {
	t.Parallel()
	e, alpha, _, beta, o := homeBundle(t)
	before := testutil.Git(t, beta, "rev-parse", "origin/main")
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "f"), keys(), ovKeys(t, &out, "fetch: done", 1, "q"))
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
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "a"), keys("t", "o", "p", "i", "c", "/", "y", "\r"),
		ovKeys(t, &out, "add: done", 1, "a"), keys("v", "1", "\t", "\r"),
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
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "a"), keys("x", "\x1b"), ovKeys(t, &out, "add: cancelled", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "add: cancelled") || exists(filepath.Join(repo, "x")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

func TestHomeAddWorktreeFailureIsShownAndReturns(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	// main is the primary's branch: wt add refuses it.
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "a"), keys("m", "a", "i", "n", "\r"), ovKeys(t, &out, "add failed", 1, "q")), &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !strings.Contains(errs, "sheaf: ") || !strings.Contains(out.String(), "add failed: ") {
		t.Fatalf("failure not reported\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
}

func TestHomeRemoveWorktreeConfirmsAndRefusesPrimary(t *testing.T) {
	e, alpha, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	// r on the primary is refused in the overview; then r on feat, confirmed.
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "r"), key{keys: "j", wait: waitOutput(t, &out, "primary checkout")}, key{keys: "r"}), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}}, ovKeys(t, &out, "remove: done", 1, "q"))
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
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}}, ovKeys(t, &out, "remove: cancelled", 1, "q"))
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
		ovKeys(t, &out, "STATE", 1, "p"), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}},
		ovKeys(t, &out, "prune: done", 1, "d"),
		ovKeys(t, &out, "doctor: done", 1, "s"), keys(" ", "\r"), keys("\r"), keys(),
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
}

// Enter and u on a row with a problem (here a missing worktree, the last
// row) are refused in the overview, which stays open.
func TestHomeRefusesEnterAndUpdateOnProblemRows(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"\r", "u"} {
		e, _, feat, _, _ := homeBundle(t)
		if err := os.RemoveAll(feat); err != nil {
			t.Fatal(err)
		}
		var out syncBuffer
		in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "\x1b[F", k),
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
