//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
	"github.com/spf13/cobra"
)

// The context given to commands must be cancelled by SIGINT, SIGTERM and
// SIGHUP: git runs in its own session and no longer receives the terminal's
// SIGINT, so cancellation is the only way to stop it. The test sends the
// signal to its own process from inside a command, i.e. after execute has
// installed its handler; a safety-net signal.Notify keeps the test process
// alive if the handler is missing, so that case fails on the assertion.
func TestExecuteCancelsCommandContextOnSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			net := make(chan os.Signal, 1)
			signal.Notify(net, sig)
			defer signal.Stop(net)

			var errb bytes.Buffer
			root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
			root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
				if err := syscall.Kill(os.Getpid(), sig); err != nil {
					return err
				}
				select {
				case <-c.Context().Done():
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("command context not cancelled by " + sig.String())
				}
			}})
			if code := execute(root, nil, []string{"probe"}); code != 0 {
				t.Fatalf("exit code %d: %s", code, errb.String())
			}
		})
	}
}

func TestInterruptedCommandExits130(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	var errb bytes.Buffer
	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-c.Context().Done():
			return c.Context().Err()
		case <-time.After(5 * time.Second):
			return errors.New("not cancelled")
		}
	}})
	if code := execute(root, nil, []string{"probe"}); code != 130 || !strings.Contains(errb.String(), "interrupted") {
		t.Fatalf("exit %d, stderr %q; want 130 and \"interrupted\"", code, errb.String())
	}
}

// A command interrupted whose error carries a CleanupError (a cleanup or
// rollback that failed) failed: execute prints the error and exits with 1,
// not "interrupted" and 130 (B62 H4 review).
func TestInterruptedCommandWithCleanupErrorFails(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	ce := &ops.CleanupError{Err: context.Canceled, Msg: "clone interrupted (cleaning up /b/x also failed: boom)"}
	var errb bytes.Buffer
	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-c.Context().Done():
			return ops.Summary{Results: []ops.Result{{Repo: "x", Status: ops.StatusFailed, Err: ce}}}.Err()
		case <-time.After(5 * time.Second):
			return errors.New("not cancelled")
		}
	}})
	if code := execute(root, nil, []string{"probe"}); code != 1 || errb.String() != "sheaf: 1 of 1 repositories failed\n" {
		t.Fatalf("exit %d, stderr %q; want 1 and the error", code, errb.String())
	}
}

// A second signal kills the running git process groups and exits at once.
func TestSecondSignalKillsGitAndExits(t *testing.T) {
	net := make(chan os.Signal, 2)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	killed := make(chan struct{}, 1)
	exited := make(chan int, 1)
	ctl := newInterruptCtl()
	ctl.killAll = func() int { killed <- struct{}{}; return 0 }
	ctl.exit = func(code int) { exited <- code }

	root := NewRootCmdWithDeps(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, Deps{ctl: ctl})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-c.Context().Done()
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d), want 130", code)
			}
		case <-time.After(5 * time.Second):
			return errors.New("second signal did not exit")
		}
		select {
		case <-killed:
		default:
			return errors.New("git process groups not killed before exiting")
		}
		return nil
	}})
	var errb bytes.Buffer
	root.SetErr(&errb)
	// The probe checks exit(130) and killAll itself and returns nil (the
	// stubbed exit returns, unlike os.Exit).
	if code := execute(root, ctl, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "killing git processes") {
		t.Errorf("emergency stop message:\n%s", errb.String())
	}
	// The probe neither adopts, adds nor updates: no advice about them.
	for _, unwanted := range []string{"sheaf adopt", ".sheaf-adopt-", "partial clone", "fast-forward"} {
		if strings.Contains(errb.String(), unwanted) {
			t.Errorf("emergency stop message mentions %q:\n%s", unwanted, errb.String())
		}
	}
}

// The emergency stop gives the advice registered by the operations running
// at that moment (during), and only that.
func TestEmergencyStopMentionsOnlyRunningOperations(t *testing.T) {
	t.Parallel()
	stop := func(register func(c *interruptCtl)) string {
		var errb bytes.Buffer
		c := newInterruptCtl()
		stubEmergency(c)
		c.bind(func() {}, &errb)
		register(c)
		c.interrupt()
		c.interrupt()
		return errb.String()
	}
	msg := stop(func(c *interruptCtl) { c.during(noteAdopt) })
	if !strings.Contains(msg, "killing git processes") || !strings.Contains(msg, ".sheaf-adopt-") || strings.Contains(msg, "fast-forward") {
		t.Errorf("during an adopt:\n%s", msg)
	}
	msg = stop(func(c *interruptCtl) {
		c.during(noteClone)
		c.during(noteFastForward)()
	})
	if !strings.Contains(msg, "partial clone") || strings.Contains(msg, "fast-forward") || strings.Contains(msg, "adopt") {
		t.Errorf("during an add, after an update:\n%s", msg)
	}
}

// A signal and a TUI ctrl+c count together: signal first, key second is
// the emergency stop.
func TestSignalThenKeyIsEmergencyStop(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGTERM)
	defer signal.Stop(net)
	d := Deps{ctl: newInterruptCtl()}
	killed, exited := stubEmergency(d.ctl)
	root := NewRootCmdWithDeps(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, d)
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-c.Context().Done():
		case <-time.After(5 * time.Second):
			return errors.New("signal did not cancel")
		}
		ctl := newTUISession(c, d).ctl
		if ctl != d.ctl {
			return errors.New("no interrupt controller")
		}
		ctl.interrupt()
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d)", code)
			}
		case <-time.After(5 * time.Second):
			return errors.New("key after signal did not exit")
		}
		if len(killed) != 1 {
			return errors.New("not killed")
		}
		return nil
	}})
	var errb bytes.Buffer
	root.SetErr(&errb)
	if code := execute(root, d.ctl, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// A SIGINT during an action of the home screen (see
// interruptCtl.beginAction) cancels that action only, for every action
// (B62 H4: ctrl+c outside the raw mode of a TUI program is a SIGINT);
// SIGTERM and SIGHUP still cancel the command, action or not.
func TestSignalsDuringHomeActions(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			net := make(chan os.Signal, 8)
			signal.Notify(net, syscall.SIGINT, sig)
			defer signal.Stop(net)
			d := Deps{ctl: newInterruptCtl()}
			_, exited := stubEmergency(d.ctl)
			root := NewRootCmdWithDeps(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, d)
			var probeErr error // execute reports any error as "interrupted" once cancelled
			root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
				probeErr = probeActionSignals(c.Context(), d.ctl, sig, exited)
				return probeErr
			}})
			var errb bytes.Buffer
			root.SetErr(&errb)
			if code := execute(root, d.ctl, []string{"probe"}); code != 0 || probeErr != nil {
				t.Fatalf("exit %d: %v\n%s", code, probeErr, errb.String())
			}
		})
	}
}

// probeActionSignals is the command of TestSignalsDuringHomeActions: three
// actions, each cancelled by a SIGINT, then sig during a fourth one.
func probeActionSignals(ctx context.Context, ctl *interruptCtl, sig syscall.Signal, exited chan int) error {
	for i := 1; i <= 3; i++ {
		actx, end := ctl.beginAction(ctx)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-actx.Done():
		case <-time.After(5 * time.Second):
			end()
			return fmt.Errorf("SIGINT %d did not cancel the action", i)
		}
		end()
		if ctx.Err() != nil {
			return fmt.Errorf("SIGINT %d cancelled the command", i)
		}
		if len(exited) > 0 {
			return fmt.Errorf("SIGINT %d was an emergency stop", i)
		}
	}
	actx, end := ctl.beginAction(ctx)
	defer end()
	_ = syscall.Kill(os.Getpid(), sig)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		return errors.New(sig.String() + " during an action did not cancel the command")
	}
	// The action's context is cancelled right after the command's (the
	// children of a context are cancelled after its Done channel closes).
	select {
	case <-actx.Done():
	case <-time.After(5 * time.Second):
		return errors.New(sig.String() + " during an action did not cancel the action")
	}
	if len(exited) > 0 {
		return fmt.Errorf("%s: emergency stop (%d)", sig, len(exited))
	}
	return nil
}

// notesDuring runs sheaf args with the TUI forced on when in is not nil,
// and returns the emergency notes registered while it ran (see
// interruptCtl.during); every note must be released when it returns.
func notesDuring(t *testing.T, e *env, in io.Reader, args ...string) []string {
	t.Helper()
	var mu sync.Mutex
	var notes []string
	ctl := newInterruptCtl()
	ctl.registered = func(n string) {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, n)
	}
	d := e.deps()
	d.ctl = ctl
	if in != nil {
		d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	} else {
		in = strings.NewReader("")
	}
	var out, errb syncBuffer
	if code := executeDeps(Streams{In: in, Out: &out, Err: &errb}, d, args); code != 0 {
		t.Fatalf("sheaf %v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, out.String(), errb.String())
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.notes) > 0 {
		t.Errorf("sheaf %v: notes still registered after it returned: %q", args, ctl.notes)
	}
	mu.Lock()
	defer mu.Unlock()
	return notes
}

// Each command that clones, adopts or fast-forwards registers the advice
// of the emergency stop about it while it runs, in plain and TUI mode.
func TestCommandsRegisterTheirEmergencyNotes(t *testing.T) {
	t.Parallel()
	want := func(name string, got []string, note string) {
		t.Helper()
		if !slices.Equal(got, []string{note}) {
			t.Errorf("%s: notes %q, want %q", name, got, []string{note})
		}
	}
	clones := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		o := testutil.NewOrigin(t, testutil.OriginOpts{})
		testutil.Git(t, dir, "clone", "-q", o.URL, filepath.Join(dir, "p"))
		return dir
	}

	e, _ := syncBundle(t) // a tracked; b and c new in o
	want("add", notesDuring(t, e, nil, "add", "o/b"), noteClone)
	want("sync --add-new", notesDuring(t, e, nil, "sync", "--add-new"), noteClone)

	e, _ = syncBundle(t)
	want("TUI add", notesDuring(t, e, newScript(t, true), "add", "o/b"), noteClone)
	want("TUI sync", notesDuring(t, e, newScript(t, true, keys(" ", "\r")), "sync"), noteClone)

	e = newBundleEnv(t)
	o, _ := e.addRepo(t, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	want("update", notesDuring(t, e, nil, "update"), noteFastForward)
	plain := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, e.cwd, "clone", "-q", plain.URL, filepath.Join(e.cwd, "p"))
	want("adopt", notesDuring(t, e, nil, "adopt", "p"), noteAdopt)
	if got := notesDuring(t, e, nil, "adopt", "--recheck", "p"); len(got) != 0 {
		t.Errorf("adopt --recheck moves nothing: notes %q", got)
	}

	e = newEnv(t)
	want("init --adopt-all", notesDuring(t, e, nil, "init", clones(t), "--adopt-all"), noteAdopt)
	want("TUI init", notesDuring(t, e, newScript(t, true, keys("\r")), "init", clones(t)), noteAdopt)
}
