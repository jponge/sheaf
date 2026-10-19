package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

// githubClone clones a fresh origin into dir and points origin at
// github.com/o/<base of dir> (origin/HEAD stays known locally, so adopt
// needs no network), as a GitHub clone made outside sheaf looks.
func githubClone(t *testing.T, dir string) {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, filepath.Dir(dir), "clone", "-q", o.URL, dir)
	testutil.Git(t, dir, "remote", "set-url", "origin", "https://github.com/o/"+filepath.Base(dir)+".git")
}

//lint:ignore ST1005 reproduces gh's stderr verbatim, final period included.
var errGHUnauthenticated = fmt.Errorf("gh repo set-default: %w: exit status 4: To get started with GitHub CLI, please run:  gh auth login\n"+
	"Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token.", hosting.ErrGHNotAuthenticated)

func sortedCalls(f *hostingtest.Fake) []hostingtest.DefaultRepoCall {
	calls := f.DefaultRepoCalls()
	slices.SortFunc(calls, func(a, b hostingtest.DefaultRepoCall) int { return strings.Compare(a.Dir, b.Dir) })
	return calls
}

func TestAdoptSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	githubClone(t, filepath.Join(e.cwd, "x"))
	_, errs, code := e.run(t, "adopt", "x")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	want := []hostingtest.DefaultRepoCall{{Dir: filepath.Join(e.cwd, "x", "main"), Slug: "o/x"}}
	if got := sortedCalls(e.fake); !slices.Equal(got, want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
	if !strings.Contains(errs, "gh default repository: o/x") {
		t.Fatalf("stderr:\n%s", errs)
	}
}

func TestInitAdoptAllSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	for _, tui := range []bool{false, true} {
		t.Run(fmt.Sprintf("tui=%v", tui), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			root := filepath.Join(t.TempDir(), "b")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			githubClone(t, filepath.Join(root, "x"))
			root, _ = filepath.EvalSymlinks(root)
			if tui {
				var out syncBuffer
				if errs, code := e.runTUI(t, newScript(t, true), &out, "init", root, "--adopt-all"); code != 0 {
					t.Fatalf("exit %d\n%s\n%s", code, out.String(), errs)
				}
			} else {
				e.mustRun(t, "init", root, "--adopt-all")
			}
			want := []hostingtest.DefaultRepoCall{{Dir: filepath.Join(root, "x", "main"), Slug: "o/x"}}
			if got := sortedCalls(e.fake); !slices.Equal(got, want) {
				t.Fatalf("calls = %+v, want %+v", got, want)
			}
		})
	}
}

// A bundle whose repositories have no gh default (gh was not logged in, or
// they were registered by an older sheaf) is fixed with
// `adopt --recheck --all`, which the warning names.
func TestAdoptRecheckAllSetsGHDefaultOfEveryRepository(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	for _, n := range []string{"x", "y"} {
		githubClone(t, filepath.Join(e.cwd, n))
	}
	e.fake.DefaultRepoErr = errGHUnauthenticated
	_, errs, code := e.run(t, "adopt", "-j", "1", "x", "y")
	if code != 0 {
		t.Fatalf("adopt must not fail because of gh: exit %d\n%s", code, errs)
	}
	if w := warningLines(errs); len(w) != 1 || !strings.Contains(w[0], "gh auth login") || !strings.Contains(w[0], "sheaf adopt --recheck --all") {
		t.Fatalf("want one warning naming the fix, got %q\nstderr:\n%s", w, errs)
	}
	e.fake.DefaultRepoErr = nil
	before := len(e.fake.DefaultRepoCalls())
	out := e.mustRun(t, "adopt", "--recheck", "--all")
	got := e.fake.DefaultRepoCalls()[before:]
	slices.SortFunc(got, func(a, b hostingtest.DefaultRepoCall) int { return strings.Compare(a.Dir, b.Dir) })
	want := []hostingtest.DefaultRepoCall{{Dir: filepath.Join(e.cwd, "x", "main"), Slug: "o/x"}, {Dir: filepath.Join(e.cwd, "y", "main"), Slug: "o/y"}}
	if !slices.Equal(got, want) {
		t.Fatalf("new calls = %+v, want %+v", got, want)
	}
	for _, n := range []string{"x", "y"} {
		if !strings.Contains(out, n) {
			t.Fatalf("summary lacks %s:\n%s", n, out)
		}
	}
}

func TestAdoptAllFlagNeedsRecheckAndNoArguments(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	githubClone(t, filepath.Join(e.cwd, "x"))
	e.mustRun(t, "adopt", "x")
	for _, args := range [][]string{{"adopt", "--all"}, {"adopt", "--recheck", "--all", "x"}, {"adopt"},
		{"adopt", "--recheck", "--all", "--host", "github.com"}, {"adopt", "--recheck", "--all", "--default-branch", "main"}} {
		if _, errs, code := e.run(t, args...); code == 0 || errs == "" {
			t.Errorf("sheaf %s: exit %d, stderr %q; want an error", strings.Join(args, " "), code, errs)
		}
	}
}

// The once-per-command warning is printed again after the TUI's progress
// view, like CLI mode prints it.
func TestTUIReplaysGHDefaultWarning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *env)
		args  []string
	}{
		{"add", func(t *testing.T, e *env) { e.origin(t, "o", "a", false) }, []string{"add", "o/a"}},
		{"adopt", func(t *testing.T, e *env) { githubClone(t, filepath.Join(e.cwd, "x")) }, []string{"adopt", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := func() *env {
				e := newBundleEnv(t)
				e.fake.DefaultRepoErr = errGHUnauthenticated
				tc.setup(t, e)
				return e
			}
			e := fixture()
			_, cliErr, code := e.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("CLI: exit %d\n%s", code, cliErr)
			}
			e2 := fixture()
			var out syncBuffer
			tuiErr, code := e2.runTUI(t, newScript(t, true), &out, tc.args...)
			if code != 0 {
				t.Fatalf("TUI: exit %d\n%s", code, tuiErr)
			}
			norm := func(s, root string) string { return strings.ReplaceAll(s, root, "<root>") }
			wantSameWarnings(t, norm(cliErr, e.cwd), norm(tuiErr, e2.cwd))
		})
	}
}

func TestAdoptRecheckAllWithoutRepositories(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	out := e.mustRun(t, "adopt", "--recheck", "--all")
	if !strings.Contains(out, "no repositories registered; nothing to re-examine") || len(e.fake.DefaultRepoCalls()) != 0 {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestAdoptUsageLine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out := e.mustRun(t, "adopt", "--help"); !strings.Contains(out, "sheaf adopt (<dir>... | --recheck --all) [flags]") {
		t.Fatalf("help:\n%s", out)
	}
}

// A kept gh default is noted, in CLI mode and again after the TUI's
// progress view.
func TestKeptGHDefaultNoteShownInTUI(t *testing.T) {
	t.Parallel()
	const note = "note: gh default repository of x is me/x (reference: o/x); kept"
	fixture := func() *env {
		e := newBundleEnv(t)
		githubClone(t, filepath.Join(e.cwd, "x"))
		testutil.Git(t, filepath.Join(e.cwd, "x"), "config", "remote.origin.gh-resolved", "me/x")
		return e
	}
	e := fixture()
	if _, errs, code := e.run(t, "adopt", "x"); code != 0 || !strings.Contains(errs, note) {
		t.Fatalf("CLI: exit %d\n%s", code, errs)
	}
	e2 := fixture()
	var out syncBuffer
	errs, code := e2.runTUI(t, newScript(t, true), &out, "adopt", "x")
	if code != 0 || !strings.Contains(errs, note) {
		t.Fatalf("TUI: exit %d, stderr:\n%s", code, errs)
	}
	if n := len(e.fake.DefaultRepoCalls()) + len(e2.fake.DefaultRepoCalls()); n != 0 {
		t.Fatalf("%d gh calls, want none", n)
	}
}
