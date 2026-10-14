package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// The controller tells the progress view about every interruption, signals
// included, with the running count.
func TestInterruptControllerNotifies(t *testing.T) {
	stubEmergency(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := newInterruptCtl(cancel, &syncBuffer{})
	var got []int
	ctl.setNotify(func(n int) { got = append(got, n) })
	ctl.interrupt()
	ctl.interrupt()
	if !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("notified %v, want [1 2]", got)
	}
}

// Ctrl-C in a menu interrupts the command like SIGINT: exit 130.
func TestTUIMenuCtrlCExits130(t *testing.T) {
	e := coBundle(t)
	e.origin(t, "o", "a", false)
	before := readYAML(t, e.cwd)
	var out syncBuffer
	errs, code := e.runTUI(t, newScript(t, false, keys("\x03")), &out, "add", "--org", "o")
	if code != 130 || !strings.Contains(errs, "interrupted") {
		t.Fatalf("exit %d, stderr %q; want 130 and interrupted", code, errs)
	}
	if readYAML(t, e.cwd) != before {
		t.Fatal("bundle changed")
	}
}

// Ctrl-C in a dialog of the progress view: no, and exit 130.
func TestTUIRemoveDialogCtrlCExits130(t *testing.T) {
	e := coBundle(t)
	e.coAddRepo(t, "alpha")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "\x03", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "remove", "alpha")
	if code != 130 || !strings.Contains(errs, "interrupted") {
		t.Fatalf("exit %d, stderr %q; want 130 and interrupted", code, errs)
	}
	if !exists(filepath.Join(e.cwd, "alpha", "main")) {
		t.Fatal("alpha removed")
	}
}

// warningLines keeps the lines of CLI stderr that are warnings or notes
// (not routine progress).
func warningLines(stderr string) []string {
	var out []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "informational, not blocking") || strings.Contains(l, "offline:") || strings.Contains(l, "warning:") {
			out = append(out, l)
		}
	}
	return out
}

func wantSameWarnings(t *testing.T, cliErr, tuiErr string) {
	t.Helper()
	want := warningLines(cliErr)
	if len(want) == 0 {
		t.Fatalf("fixture: CLI printed no warning:\n%s", cliErr)
	}
	if got := warningLines(tuiErr); !slices.Equal(got, want) {
		t.Fatalf("TUI warnings:\n%s\nwant (CLI):\n%s\nTUI stderr:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), tuiErr)
	}
}

// informationalBundle returns a bundle with o/a whose clone has commits only
// reachable from a tag (an informational risk of remove).
func informationalBundle(t *testing.T) (*env, string) {
	e := newEnv(t)
	o := e.origin(t, "o", "a", false)
	work := filepath.Join(t.TempDir(), "work")
	testutil.Git(t, filepath.Dir(work), "clone", "-q", o.URL, work)
	testutil.Git(t, work, "checkout", "-q", "--detach")
	testutil.Git(t, work, "commit", "-q", "--allow-empty", "-m", "release")
	testutil.Git(t, work, "tag", "v1.0")
	testutil.Git(t, work, "push", "-q", "origin", "v1.0")
	root := filepath.Join(t.TempDir(), "b")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	e.mustRun(t, "add", "o/a")
	return e, root
}

func TestTUIReplaysWarningsLikeCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"informational risks", []string{"remove", "a", "--yes"}},
		{"offline", []string{"remove", "a", "--yes", "--offline"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := informationalBundle(t)
			_, cliErr, code := e.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("CLI: exit %d\n%s", code, cliErr)
			}
			e2, _ := informationalBundle(t)
			var out syncBuffer
			tuiErr, code := e2.runTUI(t, newScript(t, true), &out, tc.args...)
			if code != 0 {
				t.Fatalf("TUI: exit %d\n%s", code, tuiErr)
			}
			// Paths differ between the two fixtures.
			norm := func(s, root string) string { return strings.ReplaceAll(s, root, "<root>") }
			wantSameWarnings(t, norm(cliErr, e.cwd), norm(tuiErr, e2.cwd))
		})
	}
}

func TestCredentialHint(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"git@github.com: Permission denied (publickey).", true},
		{"Host key verification failed.", true},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", true},
		{"fatal: could not read Password for 'https://x@github.com': No such device or address", true},
		{"Enter passphrase for key '/home/x/.ssh/id_ed25519':", true},
		{"fatal: repository 'x' not found", false},
	} {
		if got := looksLikeCredentialFailure(tc.msg); got != tc.want {
			t.Errorf("looksLikeCredentialFailure(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// Confirming only repositories that are already present tracks the
// organization, like --repo with them or --all would.
func TestTUIAddOrgPresentOnlyTracksSource(t *testing.T) {
	e := coBundle(t)
	e.origin(t, "o", "a", false)
	e.origin(t, "o", "b", false)
	e.mustRun(t, "add", "o/a")
	var out syncBuffer
	errs, code := e.runTUI(t, newScript(t, true, keys("\r")), &out, "add", "--org", "o")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errs)
	}
	b := load(t, e.cwd)
	if len(b.Sources) != 1 || b.Sources[0].Org != "o" {
		t.Fatalf("sources = %+v, want o tracked", b.Sources)
	}
	if exists(filepath.Join(e.cwd, "b")) {
		t.Fatal("b cloned although not selected")
	}
}
