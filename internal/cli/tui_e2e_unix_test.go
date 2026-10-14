//go:build unix

package cli

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// Ctrl-C twice in the progress view of update: the first press cancels
// (no new work, the in-progress fast-forward is waited for and the view
// says so), the second one is the emergency stop (force channel, kill,
// exit 130), as with signals in CLI mode.
func TestTUIUpdateCtrlCTwiceExits130(t *testing.T) {
	killed, exited := stubEmergency(t)
	e := coBundle(t)
	o, primary := e.coAddRepo(t, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	hooks := t.TempDir()
	started := filepath.Join(hooks, "started")
	// The merge hangs in its post-merge hook until killed.
	script := "#!/bin/sh\ntouch '" + started + "'\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, primary, "config", "core.hooksPath", hooks)

	var out syncBuffer
	waitStarted := func() {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(started); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Error("merge never started")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	in := newScript(t, true, []key{
		{keys: "\x03", wait: waitStarted},
		{keys: "\x03", wait: waitOutput(t, &out, "waiting for in-progress fast-forward to finish")},
	})
	done := make(chan struct{})
	var errs string
	var code int
	go func() {
		defer close(done)
		errs, code = e.runTUI(t, in, &out, "update", "-j", "1")
	}()
	select {
	case c := <-exited:
		if c != 130 {
			t.Errorf("exit(%d), want 130", c)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no emergency exit\nstdout:\n%s", out.String())
	}
	if len(killed) != 1 {
		t.Error("git process groups not killed")
	}
	// The stubbed exit returns: the force channel kills the merge and the
	// command ends as interrupted.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("command did not return after the forced stop")
	}
	if code != 130 || !strings.Contains(errs, "killing git processes") {
		t.Errorf("exit %d, stderr:\n%s", code, errs)
	}
	if !strings.Contains(out.String(), "Ctrl-C again to force") {
		t.Errorf("progress view lacks the interruption note:\n%s", out.String())
	}
}

// A signal while the removal dialog is displayed interrupts the command
// (exit 130, like CLI mode), rather than counting as a declined dialog.
func TestTUIRemoveInterruptedDuringDialogExits130(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGTERM)
	defer signal.Stop(net)
	e := coBundle(t)
	e.coAddRepo(t, "alpha")
	var out syncBuffer
	wait := waitOutput(t, &out, "[y/N]")
	in := newScript(t, true)
	// Like an idle terminal: no key, no EOF after the signal.
	in.segs = [][]key{{{wait: func() { wait(); _ = syscall.Kill(os.Getpid(), syscall.SIGTERM); <-in.release }}}}
	errs, code := e.runTUI(t, in, &out, "remove", "alpha")
	if code != 130 || !strings.Contains(errs, "interrupted") {
		t.Fatalf("exit %d, stderr %q; want 130 and interrupted", code, errs)
	}
	if !exists(filepath.Join(e.cwd, "alpha", "main")) {
		t.Fatal("alpha removed")
	}
}

// A signal in the progress view shows the interruption note like Ctrl-C.
func TestTUIProgressShowsSignalInterruption(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGTERM)
	defer signal.Stop(net)
	_, exited := stubEmergency(t)
	e := coBundle(t)
	o, primary := e.coAddRepo(t, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	hooks := t.TempDir()
	started := filepath.Join(hooks, "started")
	script := "#!/bin/sh\ntouch '" + started + "'\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, primary, "config", "core.hooksPath", hooks)
	var out syncBuffer
	waitStarted := func() {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			}
		}
		t.Error("merge never started")
	}
	// SIGTERM once the merge runs; then, once the note is shown, ctrl+c
	// (the emergency stop, stubbed) ends the hanging merge.
	in := newScript(t, true, []key{
		{wait: waitStarted},
		{keys: "\x03", wait: waitOutput(t, &out, "Interrupted: no new work is started")},
	})
	errs, code := e.runTUI(t, in, &out, "update", "-j", "1")
	// The emergency stop runs in its own goroutine: wait for its (stubbed)
	// exit before the seams are restored.
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("emergency stop not reached (exit %d)\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// A git failure that looks like a credential prompt that could not be
// shown gets a hint in TUI mode.
func TestTUICredentialFailureHint(t *testing.T) {
	e := coBundle(t)
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'git@sheaf-test.invalid: Permission denied (publickey).' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fake)
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "alpha", CloneURL: "ssh://sheaf-test.invalid/alpha.git", DefaultBranch: "main"})
	var out syncBuffer
	errs, code := e.runTUI(t, newScript(t, true), &out, "add", "o/alpha")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	if !strings.Contains(errs, "run with --no-tui -j 1 to let git prompt in the terminal") {
		t.Fatalf("no credential hint:\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
}
