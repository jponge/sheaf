package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
	"github.com/spf13/cobra"
)

func TestUseTUIDecision(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, out, noTUI, want bool
	}{
		{true, true, false, true},
		{true, true, true, false},
		{false, true, false, false},
		{true, false, false, false},
		{false, false, false, false},
	} {
		if got := useTUI(c.in, c.out, c.noTUI); got != c.want {
			t.Errorf("useTUI(stdinTTY=%v, stdoutTTY=%v, noTUI=%v) = %v, want %v", c.in, c.out, c.noTUI, got, c.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for name, x := range map[string]any{"pipe reader": r, "pipe writer": w, "buffer": &bytes.Buffer{}, "nil": nil} {
		if isTerminal(x) {
			t.Errorf("isTerminal(%s) = true", name)
		}
	}
	if UseTUI(r, w, false) {
		t.Error("UseTUI on pipes = true")
	}
}

// stubEmergency replaces the end of the emergency stop of ctl; it returns
// channels receiving the killAll calls and the exit codes.
func stubEmergency(ctl *interruptCtl) (killed chan struct{}, exited chan int) {
	killed, exited = make(chan struct{}, 4), make(chan int, 4)
	ctl.killAll = func() int { killed <- struct{}{}; return 0 }
	ctl.exit = func(code int) { exited <- code }
	return killed, exited
}

// The TUI's ctrl+c key goes through the same function as the signals: the
// first one cancels the command's context, the second one releases the
// terminal, closes the force channel, kills the git process groups and
// exits with 130.
func TestInterruptControllerKeyPath(t *testing.T) {
	t.Parallel()
	d := Deps{ctl: newInterruptCtl()}
	killed, exited := stubEmergency(d.ctl)
	var errb bytes.Buffer
	root := NewRootCmdWithDeps(Streams{Out: &bytes.Buffer{}, Err: &errb}, d)
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		ctx := c.Context()
		ctl := newTUISession(c, d).ctl
		if ctl != d.ctl {
			return errors.New("the command's TUI session does not use the interrupt controller of execute")
		}
		var order []string
		ctl.setRelease(func() { order = append(order, "release") })
		ctl.interrupt()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return errors.New("first interrupt did not cancel the context")
		}
		select {
		case <-d.force():
			return errors.New("force channel closed by the first interrupt")
		default:
		}
		if len(exited) > 0 || len(killed) > 0 {
			return errors.New("first interrupt stopped everything")
		}
		ctl.interrupt()
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d), want 130", code)
			}
		default:
			return errors.New("second interrupt did not exit")
		}
		select {
		case <-killed:
		default:
			return errors.New("git process groups not killed")
		}
		select {
		case <-d.force():
		default:
			return errors.New("force channel not closed")
		}
		if len(order) != 1 {
			return fmt.Errorf("release hook called %d times, want 1", len(order))
		}
		ctl.interrupt() // a third one does nothing more
		if len(exited) > 0 {
			return errors.New("third interrupt exited again")
		}
		return nil
	}})
	if code := execute(root, d.ctl, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "killing git processes") {
		t.Errorf("emergency message missing:\n%s", errb.String())
	}
}

// The controller tells the progress view about every interruption, signals
// included, with the running count.
func TestInterruptControllerNotifies(t *testing.T) {
	t.Parallel()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := newInterruptCtl()
	ctl.bind(cancel, &syncBuffer{})
	stubEmergency(ctl)
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
	t.Parallel()
	e := newBundleEnv(t)
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
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
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
	t.Parallel()
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

// The reporter of a progress view is an ops.Planner: the view's header bar
// shows the finished rows over the planned ones. With Single, the one row
// is the whole work: plans are ignored.
func TestTUIProgressPlansRows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		single, want string
	}{
		{"", " 50%"},
		{"alpha", "100%"},
	} {
		var out syncBuffer
		s := &tuiSession{ctx: context.Background(), ctl: newInterruptCtl(), out: &out, errw: io.Discard}
		planned := false
		s.progress("t", progressOpts{Single: c.single}, func(_ context.Context, rep ops.Reporter, _ askFunc) {
			p, ok := rep.(ops.Planner)
			if planned = ok; !ok {
				return
			}
			p.Plan(2)
			rep.Report(ops.Event{Repo: "alpha", Phase: "p", Kind: ops.KindStart})
			rep.Report(ops.Event{Repo: "alpha", Phase: "p", Kind: ops.KindDone})
			waitScreen(t, &out, c.want)()
			rep.Report(ops.Event{Repo: "beta", Phase: "p", Kind: ops.KindStart})
			rep.Report(ops.Event{Repo: "beta", Phase: "p", Kind: ops.KindDone})
		})
		if !planned {
			t.Errorf("single %q: the reporter is not an ops.Planner", c.single)
		}
	}
}

// A progress view on an output that is not a terminal (a TUI forced on)
// never drives the terminal's progress indicator (OSC 9;4).
func TestTUIProgressOnNonTerminalLeavesTerminalProgressAlone(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	s := &tuiSession{ctx: context.Background(), ctl: newInterruptCtl(), out: &out, errw: io.Discard}
	s.progress("Fetching", progressOpts{}, func(_ context.Context, rep ops.Reporter, _ askFunc) {
		rep.Report(ops.Event{Repo: "alpha", Phase: "p", Kind: ops.KindStart})
		waitScreen(t, &out, "Fetching · 1 running")()
	})
	if strings.Contains(out.String(), "\x1b]9;4;") {
		t.Errorf("output contains OSC 9;4:\n%q", out.String())
	}
}

func TestCredentialHint(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := newBundleEnv(t)
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
	// a is not counted as selected by the user, and the tracking is told.
	for _, want := range []string{"0/1 selected · 1 present", "now tracking o: sync will propose its new repositories"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}
