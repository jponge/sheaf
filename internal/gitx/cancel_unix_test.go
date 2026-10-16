//go:build unix

package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

// writeScript writes an executable shell script.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hangingScript returns a script body that records its own PID and the PID
// of a background child in pidfile, then waits for the child (30s sleep).
func hangingScript(pidfile string) string {
	return "echo $$ >> '" + pidfile + "'\nsleep 30 &\necho $! >> '" + pidfile + "'\nwait\ncat >/dev/null\n"
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	// On Linux, a zombie still answers kill(pid, 0); it is dead for our purpose.
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) && s[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// readPIDs reads the PIDs recorded in pidfile; it fails when there are none,
// because the test would then not prove anything.
func readPIDs(t *testing.T, pidfile string) []int {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("no PID recorded (the hanging helper never ran): %v", err)
	}
	var pids []int
	for f := range strings.FieldsSeq(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("bad pid %q", f)
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		t.Fatal("no PID recorded")
	}
	return pids
}

// wantProcessesGone fails if any recorded process is still alive after a
// short grace period (signal delivery and reaping by init are asynchronous).
// Survivors are killed so they do not outlive the test.
func wantProcessesGone(t *testing.T, pidfile string) {
	t.Helper()
	pids := readPIDs(t, pidfile)
	deadline := time.Now().Add(time.Second)
	for {
		var alive []int
		for _, p := range pids {
			if processAlive(p) {
				alive = append(alive, p)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, p := range alive {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
			t.Fatalf("processes %v survived the cancellation", alive)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// slowSmudgeClone returns a clone whose next fast-forward checks out a file
// through a smudge filter that hangs, and the file where the filter records
// its PIDs.
func slowSmudgeClone(t *testing.T) (dir, pidfile string) {
	t.Helper()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	scripts := t.TempDir()
	pidfile = filepath.Join(scripts, "pids")
	smudge := filepath.Join(scripts, "smudge.sh")
	writeScript(t, smudge, hangingScript(pidfile))
	testutil.Git(t, dir, "config", "filter.slow.smudge", smudge)
	testutil.Git(t, dir, "config", "filter.slow.required", "true")
	writeFile(t, filepath.Join(dir, ".git", "info"), "attributes", "*.slow filter=slow\n")
	testutil.AdvanceOrigin(t, o, "main", "big.slow", "payload\n", "slow file")
	return dir, pidfile
}

// wantPromptCancel checks the common post-conditions of a cancelled or timed
// out mutating operation (deadline: 700ms).
func wantPromptCancel(t *testing.T, elapsed time.Duration, err error, pidfile string) {
	t.Helper()
	if elapsed > 3*time.Second {
		t.Errorf("returned after %v, want prompt return after the 700ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	wantProcessesGone(t, pidfile)
}

func TestFastForwardMergeTimeoutMidCheckoutKillsWholeProcessTree(t *testing.T) {
	opts := FastForwardOptions{MergeTimeout: 700 * time.Millisecond}
	dir, pidfile := slowSmudgeClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	// The caller's earlier cancellation does not interrupt the merge; only
	// the merge timeout does.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := FastForwardWithOptions(ctx, dir, opts)
	elapsed := time.Since(start)
	if r.Outcome != PullFailed {
		t.Errorf("outcome = %v, want failed", r.Outcome)
	}
	if elapsed < 700*time.Millisecond {
		t.Errorf("returned after %v, before the merge timeout", elapsed)
	}
	wantPromptCancel(t, elapsed, r.Err, pidfile)
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); err == nil {
		t.Error("index.lock left behind")
	}
	if h := head(t, dir); r.New != h {
		t.Errorf("result New = %s, but HEAD is %s", r.New, h)
	}
	// The repository is usable afterwards: no stale lock blocks a new attempt.
	testutil.Git(t, dir, "config", "--unset", "filter.slow.required")
	testutil.Git(t, dir, "config", "filter.slow.smudge", "cat")
	if r := FastForwardWithOptions(context.Background(), dir, opts); r.Outcome != PullFastForwarded {
		t.Fatalf("retry outcome = %v (err %v), want fast-forwarded", r.Outcome, r.Err)
	}
}

// sshFakeClone points origin of a fresh clone at an ssh:// URL served by the
// fake ssh script body (via GIT_SSH_COMMAND; nothing touches the network).
func sshFakeClone(t *testing.T, body string) string {
	t.Helper()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	writeScript(t, fake, body)
	checkNoSurvivors(t, fake)
	testutil.Git(t, dir, "config", "remote.origin.url", "ssh://sheaf-test.invalid/repo.git")
	testutil.Git(t, dir, "config", "ssh.variant", "ssh")
	t.Setenv("GIT_SSH_COMMAND", fake)
	return dir
}

func TestFetchCancelledKillsSSH(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pids")
	dir := sshFakeClone(t, hangingScript(pidfile))
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Net{}.FetchRemote(ctx, dir, "origin")
	wantPromptCancel(t, time.Since(start), err, pidfile)
}

// hangingHookClone returns a clone whose next fast-forward succeeds in moving
// HEAD and then hangs in a post-merge hook, and the hook's PID file.
func hangingHookClone(t *testing.T) (dir, pidfile string) {
	t.Helper()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	hooks := t.TempDir()
	pidfile = filepath.Join(t.TempDir(), "pids")
	writeScript(t, filepath.Join(hooks, "post-merge"), hangingScript(pidfile))
	testutil.Git(t, dir, "config", "core.hooksPath", hooks)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	return dir, pidfile
}

func TestFastForwardMergeTimeoutAfterHeadMovedReportsObservedHead(t *testing.T) {
	opts := FastForwardOptions{MergeTimeout: 700 * time.Millisecond}
	dir, pidfile := hangingHookClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	old := head(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := FastForwardWithOptions(ctx, dir, opts)
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Errorf("returned after %v, before the merge timeout", elapsed)
	}
	wantPromptCancel(t, elapsed, r.Err, pidfile)
	h := head(t, dir)
	if h == old {
		t.Fatal("precondition: HEAD did not move before the hook")
	}
	if r.Outcome != PullFailed || r.Old != old || r.New != h {
		t.Fatalf("result = %v old=%s new=%s, want failed old=%s new=%s", r.Outcome, r.Old, r.New, old, h)
	}
}

// killRecorded kills the processes recorded in pidfile at cleanup.
func killRecorded(t *testing.T, pidfile string) {
	t.Cleanup(func() {
		b, _ := os.ReadFile(pidfile)
		for f := range strings.FieldsSeq(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// A hook that backgrounds a process inheriting stderr keeps
// the pipe open after git exited 0; exec then reports ErrWaitDelay. The
// fast-forward did happen, so it must not be reported as a failure.
func TestFastForwardSucceedsWhenHookBackgroundsProcessHoldingStderr(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	hooks := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "pids")
	killRecorded(t, pidfile)
	writeScript(t, filepath.Join(hooks, "post-merge"), "sleep 5 &\necho $! >> '"+pidfile+"'\nexit 0\n")
	testutil.Git(t, dir, "config", "core.hooksPath", hooks)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{waitDelay: 300 * time.Millisecond})
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("returned after %v", el)
	}
	wantOutcome(t, r, PullFastForwarded)
	if h := head(t, dir); r.New != h {
		t.Fatalf("New = %s, HEAD = %s", r.New, h)
	}
}

// slowCheckoutClone returns a fetched clone whose next fast-forward checks out
// big.slow through a smudge filter that touches marker, sleeps, then behaves
// like cat.
func slowCheckoutClone(t *testing.T, sleep string) (dir, marker string) {
	t.Helper()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	scripts := t.TempDir()
	marker = filepath.Join(scripts, "started")
	smudge := filepath.Join(scripts, "smudge.sh")
	writeScript(t, smudge, "touch '"+marker+"'\nsleep "+sleep+"\nexec cat\n")
	testutil.Git(t, dir, "config", "filter.slow.smudge", smudge)
	testutil.Git(t, dir, "config", "filter.slow.clean", "cat")
	testutil.Git(t, dir, "config", "filter.slow.required", "true")
	writeFile(t, filepath.Join(dir, ".git", "info"), "attributes", "*.slow filter=slow\n")
	testutil.AdvanceOrigin(t, o, "main", "big.slow", "payload\n", "slow file")
	return dir, marker
}

// cancelWhenExists cancels when path appears (polling), i.e. while the merge
// is running its checkout.
func cancelWhenExists(path string, cancel context.CancelFunc) {
	go func() {
		defer cancel()
		for i := 0; i < 1000; i++ {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func wantCleanFastForward(t *testing.T, dir string, r PullResult) {
	t.Helper()
	wantOutcome(t, r, PullFastForwarded)
	if h, up := head(t, dir), testutil.Git(t, dir, "rev-parse", "origin/main"); r.New != h || h != up {
		t.Errorf("New = %s, HEAD = %s, origin/main = %s", r.New, h, up)
	}
	if st := mustStatus(t, dir); st.Dirty() || st.Untracked != 0 {
		t.Errorf("work tree not clean after the fast-forward: %+v", st)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "big.slow")); err != nil || string(b) != "payload\n" {
		t.Errorf("big.slow = %q, %v", b, err)
	}
}

// Once the merge has started, cancellation is
// not honoured; the fast-forward completes and is reported as such.
func TestFastForwardCompletesWhenCancelledDuringCheckout(t *testing.T) {
	dir, marker := slowCheckoutClone(t, "1")
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenExists(marker, cancel)
	r := FastForward(ctx, dir)
	if ctx.Err() == nil {
		t.Fatal("precondition: ctx was not cancelled during the merge")
	}
	wantCleanFastForward(t, dir, r)
}

// closeWhenRecorded closes ch once pidfile exists and is non-empty (the
// hanging helper is running), i.e. while the merge is in progress.
func closeWhenRecorded(pidfile string, ch chan struct{}) {
	go func() {
		defer close(ch)
		for i := 0; i < 2000; i++ {
			if fi, err := os.Stat(pidfile); err == nil && fi.Size() > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// wantForceKilled checks the common post-conditions of a merge killed through
// FastForwardOptions.Force while MergeTimeout (20s) was far away.
func wantForceKilled(t *testing.T, elapsed time.Duration, r PullResult, pidfile string) {
	t.Helper()
	if elapsed > 3*time.Second {
		t.Errorf("returned %v after Force was closed, want promptly", elapsed)
	}
	if r.Outcome != PullFailed {
		t.Errorf("outcome = %v (err %v), want failed", r.Outcome, r.Err)
	}
	if !errors.Is(r.Err, ErrForceKilled) {
		t.Errorf("err = %v, want ErrForceKilled", r.Err)
	}
	wantProcessesGone(t, pidfile)
}

// Closing Force kills the merge's process group at once.
func TestFastForwardWithOptionsForceKillsMergeMidCheckout(t *testing.T) {
	dir, pidfile := slowSmudgeClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	old := head(t, dir)
	force := make(chan struct{})
	closeWhenRecorded(pidfile, force)
	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{MergeTimeout: 20 * time.Second, Force: force})
	wantForceKilled(t, time.Since(start), r, pidfile)
	// An interrupted fast-forward leaves HEAD where it was.
	if h := head(t, dir); h != old || r.Old != old || r.New != old {
		t.Errorf("HEAD = %s, result old=%s new=%s, want all %s", h, r.Old, r.New, old)
	}
}

func TestFastForwardWithOptionsForceAfterHeadMovedReportsObservedHead(t *testing.T) {
	dir, pidfile := hangingHookClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	old := head(t, dir)
	force := make(chan struct{})
	closeWhenRecorded(pidfile, force)
	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{MergeTimeout: 20 * time.Second, Force: force})
	wantForceKilled(t, time.Since(start), r, pidfile)
	h := head(t, dir)
	if h == old {
		t.Fatal("precondition: HEAD did not move before the hook")
	}
	if r.Old != old || r.New != h {
		t.Errorf("result old=%s new=%s, want old=%s new=%s (observed HEAD)", r.Old, r.New, old, h)
	}
}

// Force closed before the merge starts kills it as soon as it runs; it is not
// reported as a divergence or a timeout.
func TestFastForwardWithOptionsForceAlreadyClosed(t *testing.T) {
	dir, pidfile := slowSmudgeClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	force := make(chan struct{})
	close(force)
	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{MergeTimeout: 20 * time.Second, Force: force})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("returned after %v", elapsed)
	}
	if r.Outcome != PullFailed || !errors.Is(r.Err, ErrForceKilled) {
		t.Errorf("result = %v, err %v, want failed with ErrForceKilled", r.Outcome, r.Err)
	}
	// The smudge helper may or may not have started before the kill. It may
	// also have been killed after its shell created the pidfile (`>>`) but
	// before writing its PID: an empty file means no process to check.
	if fi, err := os.Stat(pidfile); err == nil && fi.Size() > 0 {
		wantProcessesGone(t, pidfile)
	}
}

// A Force channel that is never closed, or closed after the merge finished,
// changes nothing; MergeTimeout overrides the default bound.
func TestFastForwardWithOptionsForceIgnoredWhenMergeCompletes(t *testing.T) {
	dir, _ := slowCheckoutClone(t, "0")
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	force := make(chan struct{})
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{Force: force})
	close(force)
	wantCleanFastForward(t, dir, r)
}

func TestFastForwardWithOptionsMergeTimeoutOverridesDefault(t *testing.T) {
	dir, pidfile := slowSmudgeClone(t)
	if err := (Net{}).FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := FastForwardWithOptions(context.Background(), dir, FastForwardOptions{MergeTimeout: 700 * time.Millisecond})
	wantPromptCancel(t, time.Since(start), r.Err, pidfile)
	if r.Outcome != PullFailed {
		t.Errorf("outcome = %v, want failed", r.Outcome)
	}
}
