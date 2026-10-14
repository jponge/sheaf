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

// Since the merge ignores cancellation (review 2, item 3), a checkout that
// hangs is bounded by mergeTimeout, which kills the whole process tree.
func TestPullFFOnlyMergeTimeoutMidCheckoutKillsWholeProcessTree(t *testing.T) {
	setDuration(t, &mergeTimeout, 700*time.Millisecond)
	dir, pidfile := slowSmudgeClone(t)
	start := time.Now()
	r := PullFFOnly(context.Background(), dir)
	elapsed := time.Since(start)
	if r.Outcome != PullFailed {
		t.Errorf("outcome = %v, want failed", r.Outcome)
	}
	wantPromptCancel(t, elapsed, r.Err, pidfile)
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); err == nil {
		t.Error("index.lock left behind")
	}
	if h := head(t, dir); r.New != h {
		t.Errorf("result New = %s, but HEAD is %s", r.New, h)
	}
}

func TestFastForwardMergeTimeoutMidCheckoutKillsWholeProcessTree(t *testing.T) {
	setDuration(t, &mergeTimeout, 700*time.Millisecond)
	dir, pidfile := slowSmudgeClone(t)
	if err := Fetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// The caller's earlier cancellation does not interrupt the merge; only
	// mergeTimeout does.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := FastForward(ctx, dir)
	elapsed := time.Since(start)
	if r.Outcome != PullFailed {
		t.Errorf("outcome = %v, want failed", r.Outcome)
	}
	if elapsed < 700*time.Millisecond {
		t.Errorf("returned after %v, before mergeTimeout", elapsed)
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
	if r := FastForward(context.Background(), dir); r.Outcome != PullFastForwarded {
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
	err := Fetch(ctx, dir)
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

// A post-merge hook that hangs after HEAD moved is bounded by mergeTimeout
// (cancellation does not interrupt the merge, review 2 item 3); the result
// must report the observed HEAD.
func TestPullFFOnlyMergeTimeoutAfterHeadMovedReportsObservedHead(t *testing.T) {
	setDuration(t, &mergeTimeout, 700*time.Millisecond)
	dir, pidfile := hangingHookClone(t)
	old := head(t, dir)
	start := time.Now()
	r := PullFFOnly(context.Background(), dir)
	wantPromptCancel(t, time.Since(start), r.Err, pidfile)
	h := head(t, dir)
	if h == old {
		t.Fatal("precondition: HEAD did not move before the hook")
	}
	if r.Outcome != PullFailed || r.Old != old || r.New != h {
		t.Fatalf("result = %v old=%s new=%s, want failed old=%s new=%s", r.Outcome, r.Old, r.New, old, h)
	}
}

func TestFastForwardMergeTimeoutAfterHeadMovedReportsObservedHead(t *testing.T) {
	setDuration(t, &mergeTimeout, 700*time.Millisecond)
	dir, pidfile := hangingHookClone(t)
	if err := Fetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	old := head(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := FastForward(ctx, dir)
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Errorf("returned after %v, before mergeTimeout", elapsed)
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

// setDuration sets *v to d for the duration of the test.
func setDuration(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
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

// Review 2, item 1: a hook that backgrounds a process inheriting stderr keeps
// the pipe open after git exited 0; exec then reports ErrWaitDelay. The
// fast-forward did happen, so it must not be reported as a failure.
func TestFastForwardSucceedsWhenHookBackgroundsProcessHoldingStderr(t *testing.T) {
	setDuration(t, &waitDelay, 300*time.Millisecond)
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	hooks := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "pids")
	killRecorded(t, pidfile)
	writeScript(t, filepath.Join(hooks, "post-merge"), "sleep 5 &\necho $! >> '"+pidfile+"'\nexit 0\n")
	testutil.Git(t, dir, "config", "core.hooksPath", hooks)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	if err := Fetch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := FastForward(context.Background(), dir)
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

// Review 2, item 3 (decision a): once the merge has started, cancellation is
// not honoured; the fast-forward completes and is reported as such.
func TestFastForwardCompletesWhenCancelledDuringCheckout(t *testing.T) {
	dir, marker := slowCheckoutClone(t, "1")
	if err := Fetch(context.Background(), dir); err != nil {
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

func TestPullFFOnlyCompletesWhenCancelledDuringCheckout(t *testing.T) {
	dir, marker := slowCheckoutClone(t, "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelWhenExists(marker, cancel)
	r := PullFFOnly(ctx, dir)
	if ctx.Err() == nil {
		t.Fatal("precondition: ctx was not cancelled during the merge")
	}
	wantCleanFastForward(t, dir, r)
}
