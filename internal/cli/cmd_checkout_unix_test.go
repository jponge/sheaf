//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
	"github.com/spf13/cobra"
)

// Second signal: the force channel is closed before the process groups are
// killed (the existing emergency path).
func TestSecondSignalClosesForceBeforeKilling(t *testing.T) {
	net := make(chan os.Signal, 2)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	var mu sync.Mutex
	var force <-chan struct{}
	forcedAtKill := make(chan bool, 1)
	exited := make(chan int, 1)
	d := Deps{ctl: newInterruptCtl()}
	d.ctl.killAll = func() int {
		mu.Lock()
		f := force
		mu.Unlock()
		select {
		case <-f:
			forcedAtKill <- true
		default:
			forcedAtKill <- false
		}
		return 0
	}
	d.ctl.exit = func(code int) { exited <- code }

	root := NewRootCmdWithDeps(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}, d)
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		mu.Lock()
		force = d.force()
		mu.Unlock()
		if force == nil {
			return errors.New("no force channel in the command's Deps")
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-c.Context().Done()
		select {
		case <-force:
			return errors.New("force closed by the first signal")
		default:
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			return errors.New("second signal did not exit")
		}
		if !<-forcedAtKill {
			return errors.New("force channel not closed before killing")
		}
		return nil
	}})
	var errb bytes.Buffer
	root.SetErr(&errb)
	if code := execute(root, d.ctl, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// The emergency stop kills git and the commands of `sheaf run`.
func TestKillAllKillsGitAndRunCommands(t *testing.T) {
	t.Parallel()
	var calls []string
	killAll := killAllOf(func() int { calls = append(calls, "git"); return 2 }, func() int { calls = append(calls, "commands"); return 1 })
	if n := killAll(); n != 3 || strings.Join(calls, ",") != "git,commands" {
		t.Fatalf("killAll = %d, calls %v", n, calls)
	}
}

// watchWriter is a goroutine-safe writer that runs onMatch once when the
// written text contains match.
type watchWriter struct {
	mu      sync.Mutex
	b       bytes.Buffer
	match   string
	onMatch func()
	fired   bool
}

func (w *watchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.b.Write(p)
	if !w.fired && strings.Contains(w.b.String(), w.match) {
		w.fired = true
		w.onMatch()
	}
	return n, err
}

func (w *watchWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// First Ctrl-C during `sheaf update`: the message names the in-progress
// fast-forward, which completes; the next checkout is not started; exit 130.
func TestUpdateFirstInterruptWaitsForFastForward(t *testing.T) {
	net := make(chan os.Signal, 2)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	e := newBundleEnv(t)
	o, alpha := e.addRepo(t, "alpha", "feat")
	addWorktree(t, alpha, "feat", "feat")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	testutil.AdvanceOrigin(t, o, "feat", "r.txt", "r\n", "remote")
	before := testutil.Git(t, alpha, "rev-parse", "HEAD")
	hooks := t.TempDir()
	release := filepath.Join(hooks, "release")
	// The hook of alpha/main interrupts sheaf (this test process), then
	// blocks until released.
	hook := "#!/bin/sh\ncase \"$PWD\" in */feat) exit 0;; esac\nkill -INT " + strconv.Itoa(os.Getpid()) +
		"\nwhile [ ! -e '" + release + "' ]; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, alpha, "config", "core.hooksPath", hooks)

	errw := &watchWriter{
		match:   "waiting for in-progress fast-forward in alpha/main to finish (Ctrl-C again to force)",
		onMatch: func() { _ = os.WriteFile(release, nil, 0o644) },
	}
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- executeDeps(Streams{In: strings.NewReader(""), Out: &out, Err: errw}, e.deps(), []string{"update", "-j", "1"})
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(30 * time.Second):
		_ = os.WriteFile(release, nil, 0o644)
		t.Fatalf("update did not finish; stderr:\n%s", errw.String())
	}
	if code != 130 {
		t.Errorf("exit %d, want 130\nstdout:\n%s\nstderr:\n%s", code, out.String(), errw.String())
	}
	if !strings.Contains(errw.String(), errw.match) {
		t.Errorf("no waiting message:\n%s", errw.String())
	}
	if h := testutil.Git(t, alpha, "rev-parse", "HEAD"); h == before {
		t.Error("the in-progress fast-forward did not complete")
	}
	if !strings.Contains(out.String(), "alpha/feat") || !strings.Contains(out.String(), "not started") {
		t.Errorf("alpha/feat not reported as not started:\n%s", out.String())
	}
}

// TestSheafProcessHelper is not a test: it runs sheaf with the arguments in
// SHEAF_HELPER_ARGS (separated by \x1f) on the real standard streams, for tests
// that need a separate process.
func TestSheafProcessHelper(t *testing.T) {
	args := os.Getenv("SHEAF_HELPER_ARGS")
	if args == "" {
		return
	}
	os.Exit(Execute(strings.Split(args, "\x1f"), Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}

// `sheaf run ... | head`: when stdout's reader goes away, sheaf is not
// killed by SIGPIPE; the broken pipe cancels the run (commands terminated)
// and sheaf exits with 141 after its cleanup.
func TestRunBrokenStdoutPipe(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	pidfile := filepath.Join(t.TempDir(), "pid")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	args := []string{"--bundle", e.cwd, "run", "-j", "1", "--shell", "--", "echo $$ > '" + pidfile + "'; yes | head -c 1000000; exec sleep 30"}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSheafProcessHelper$", "-test.count=1")
	cmd.Env = testutil.WithoutRaceExitDelay(append(os.Environ(), "SHEAF_HELPER_ARGS="+strings.Join(args, "\x1f")))
	cmd.Stdout = w
	var errb bytes.Buffer
	cmd.Stderr = &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close() // the child holds the write end
	// Like `| head`: read some of the command's output, then go away.
	got := 0
	buf := make([]byte, 32<<10)
	for got < 256<<10 {
		n, err := r.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	r.Close()
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("sheaf did not stop on a broken pipe; stderr:\n%s", errb.String())
	}
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() || ws.ExitStatus() != 141 {
		t.Errorf("wait status %v (signaled %v, exit %d), want exit 141; stderr:\n%s", ws, ws.Signaled(), ws.ExitStatus(), errb.String())
	}
	b, _ := os.ReadFile(pidfile)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		deadline := time.Now().Add(3 * time.Second)
		for syscall.Kill(pid, 0) == nil && !zombie(pid) {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("command %d survived sheaf", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	} else {
		t.Fatalf("command never started: %v", err)
	}
}

func zombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
