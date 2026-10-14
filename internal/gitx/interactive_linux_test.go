//go:build linux

package gitx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

// sessionOf returns the session id of pid ("self" for this process) from
// /proc/<pid>/stat.
func sessionOf(t *testing.T, pid string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return f[3] // state ppid pgrp session
}

// recordSessionScript writes the session id of the running script to file.
func recordSessionScript(file string) string {
	return "read -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + file + "'\n"
}

func readTrimmed(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("script never ran: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// In interactive mode, the ssh spawned by a fetch stays in sheaf's session,
// so it can reach sheaf's controlling terminal; otherwise it runs in a new
// session (see Run).
func TestInteractiveFetchStaysInCallerSession(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "sid")
	dir := sshFakeClone(t, recordSessionScript(rec)+"exit 1\n")
	self := sessionOf(t, "self")

	_ = Fetch(context.Background(), dir)
	if got := readTrimmed(t, rec); got == self {
		t.Fatalf("non-interactive fetch: ssh in caller session %s, want a new session", got)
	}
	_ = Fetch(WithInteractive(context.Background()), dir)
	if got := readTrimmed(t, rec); got != self {
		t.Fatalf("interactive fetch: ssh in session %s, want the caller's session %s", got, self)
	}
}

func TestInteractiveFetchAllAndCloneStayInCallerSession(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "sid")
	dir := sshFakeClone(t, recordSessionScript(rec)+"exit 1\n")
	self := sessionOf(t, "self")
	ctx := WithInteractive(context.Background())

	_ = FetchAll(ctx, dir)
	if got := readTrimmed(t, rec); got != self {
		t.Errorf("interactive FetchAll: ssh in session %s, want %s", got, self)
	}
	os.Remove(rec)
	_ = Clone(ctx, "ssh://sheaf-test.invalid/repo.git", filepath.Join(t.TempDir(), "c"), "")
	if got := readTrimmed(t, rec); got != self {
		t.Errorf("interactive Clone: ssh in session %s, want %s", got, self)
	}
}

// Only network operations are interactive: a merge (and its hooks) keeps its
// own session even under an interactive context, so that a terminal Ctrl-C
// never reaches an in-progress fast-forward (it must run to completion).
func TestInteractiveDoesNotApplyToMerge(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	hooks := t.TempDir()
	rec := filepath.Join(t.TempDir(), "sid")
	writeScript(t, filepath.Join(hooks, "post-merge"), recordSessionScript(rec))
	testutil.Git(t, dir, "config", "core.hooksPath", hooks)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	ctx := WithInteractive(context.Background())
	if err := Fetch(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if r := FastForward(ctx, dir); r.Outcome != PullFastForwarded {
		t.Fatalf("outcome %v (%v)", r.Outcome, r.Err)
	}
	if got := readTrimmed(t, rec); got == sessionOf(t, "self") {
		t.Fatal("merge hook ran in the caller's session under an interactive context")
	}
}

// An interactive fetch is still cancellable: git itself receives SIGTERM.
func TestInteractiveFetchCancelReturnsPromptly(t *testing.T) {
	setDuration(t, &waitDelay, time.Second)
	pidfile := filepath.Join(t.TempDir(), "pids")
	dir := sshFakeClone(t, hangingScript(pidfile))
	killRecorded(t, pidfile)
	ctx, cancel := context.WithTimeout(WithInteractive(context.Background()), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Fetch(ctx, dir)
	if el := time.Since(start); el > 4*time.Second {
		t.Errorf("returned after %v", el)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestKillAllKillsInteractiveFetch(t *testing.T) {
	t.Cleanup(resetKillAll)
	setDuration(t, &waitDelay, time.Second)
	pidfile := filepath.Join(t.TempDir(), "pids")
	dir := sshFakeClone(t, hangingScript(pidfile))
	killRecorded(t, pidfile)
	done := make(chan error, 1)
	go func() { done <- Fetch(WithInteractive(context.Background()), dir) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(pidfile)
		if len(strings.Fields(string(b))) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hanging ssh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := KillAll(); n != 1 {
		t.Errorf("KillAll signalled %d invocations, want 1", n)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrForceKilled) {
			t.Errorf("err = %v, want ErrForceKilled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interactive fetch survived KillAll")
	}
}

// TestInteractiveTTYHelperProcess is not a test: it is the body of the
// helper process of TestInteractiveFetchCanOpenControllingTerminal.
func TestInteractiveTTYHelperProcess(t *testing.T) {
	dir := os.Getenv("GITX_ITTY_HELPER_DIR")
	if dir == "" {
		return
	}
	_ = Fetch(WithInteractive(context.Background()), dir) // the fake ssh fails
}

// The point of interactive mode: ssh can open the controlling terminal (to
// ask for a passphrase or confirm a host key).
func TestInteractiveFetchCanOpenControllingTerminal(t *testing.T) {
	result := filepath.Join(t.TempDir(), "tty-result")
	dir := sshFakeClone(t, "if (exec 3</dev/tty) 2>/dev/null; then echo opened > '"+result+"'; else echo denied > '"+result+"'; fi\nexit 1\n")
	_, slave := openPTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInteractiveTTYHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GITX_ITTY_HELPER_DIR="+dir)
	cmd.Stdin = slave
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v\n%s", err, out.String())
	}
	if got := readTrimmed(t, result); got != "opened" {
		t.Fatalf("ssh spawned by an interactive fetch could not open /dev/tty (%q)", got)
	}
}

// slowUploadPackClone returns a clone whose fetches run an upload-pack
// wrapper that hangs with a grandchild (recorded in the returned pidfile).
func slowUploadPackClone(t *testing.T) (dir, pidfile string) {
	t.Helper()
	_, dir = cloneOrigin(t, testutil.OriginOpts{})
	scripts := t.TempDir()
	pidfile = filepath.Join(scripts, "pids")
	wrapper := filepath.Join(scripts, "upload-pack")
	writeScript(t, wrapper, "trap '' TERM\necho $$ >> '"+pidfile+"'\n(trap '' TERM; exec sleep 30) &\necho $! >> '"+pidfile+"'\nwait\n")
	testutil.Git(t, dir, "config", "remote.origin.uploadpack", wrapper)
	killRecorded(t, pidfile)
	return dir, pidfile
}

// S4: cancelling an interactive fetch also stops git's descendants (here an
// upload-pack and its child, both ignoring SIGTERM), not just git.
func TestInteractiveFetchCancelStopsDescendants(t *testing.T) {
	setDuration(t, &waitDelay, 500*time.Millisecond)
	dir, pidfile := slowUploadPackClone(t)
	ctx, cancel := context.WithCancel(WithInteractive(context.Background()))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Fetch(ctx, dir) }()
	waitPIDCount(t, pidfile, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fetch did not return")
	}
	wantProcessesGone(t, pidfile)
}

func TestKillAllStopsInteractiveFetchDescendants(t *testing.T) {
	t.Cleanup(resetKillAll)
	dir, pidfile := slowUploadPackClone(t)
	done := make(chan error, 1)
	go func() { done <- Fetch(WithInteractive(context.Background()), dir) }()
	waitPIDCount(t, pidfile, 2)
	KillAll()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fetch did not return")
	}
	wantProcessesGone(t, pidfile)
}

func waitPIDCount(t *testing.T, pidfile string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(pidfile)
		if len(strings.Fields(string(b))) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("upload-pack wrapper never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
