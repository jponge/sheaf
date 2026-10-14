//go:build linux

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ptyRun is the result of running the sheaf binary on a pseudo-terminal.
type ptyRun struct {
	elapsed time.Duration
	output  []byte // everything the process wrote to the terminal
	master  *os.File
	slave   *os.File // the terminal, still open after the process exited
}

// runOnSilentPTY runs `sheaf args...` with a pseudo-terminal as controlling
// terminal and standard streams, like an interactive shell would. The
// terminal never answers queries. typeahead is written to the terminal
// before the process starts, as if typed ahead by the user.
func runOnSilentPTY(t *testing.T, typeahead string, args ...string) ptyRun {
	t.Helper()
	bin := filepath.Join(sheafBinary(t), "sheaf")
	master, slave := openPTY(t)

	var mu sync.Mutex
	var out bytes.Buffer
	go func() { // exits when master is closed (test cleanup)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if typeahead != "" {
		if _, err := master.Write([]byte(typeahead)); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=xterm-256color", "LC_ALL=C"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sheaf %v: %v", args, err)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("sheaf %v did not exit within 20s", args)
	}
	elapsed := time.Since(start)

	// The output is "dev" (Version) after anything written at startup;
	// wait for it so that the whole output has been read from the master.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := bytes.Contains(out.Bytes(), []byte(Version))
		mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("version never written to the terminal; output: %q", out.Bytes())
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return ptyRun{elapsed: elapsed, output: bytes.Clone(out.Bytes()), master: master, slave: slave}
}

// Commands that do not run a TUI must not query the terminal: a query
// stalls on terminals that never answer (bubbletea v1 queried the
// background colour (OSC 11) in its package initialisation, with a 5 s
// timeout, on every invocation with a terminal as stdout).
func TestStartupDoesNotQueryTerminal(t *testing.T) {
	r := runOnSilentPTY(t, "", "version")
	if bytes.Contains(r.output, []byte("\x1b]11;?")) {
		t.Errorf("sheaf version queried the background colour (OSC 11); terminal output: %q", r.output)
	}
	if bytes.Contains(r.output, []byte("\x1b")) {
		t.Errorf("sheaf version wrote an escape sequence to the terminal: %q", r.output)
	}
	if r.elapsed >= time.Second {
		t.Errorf("sheaf version took %v on a terminal that never answers queries, want < 1s", r.elapsed)
	}
}

// Input typed ahead of a command that does not run a TUI belongs to
// whatever reads the terminal next (typically the shell); sheaf must not
// consume it.
func TestStartupKeepsTypeahead(t *testing.T) {
	r := runOnSilentPTY(t, "abc\n", "version")
	// Type a marker line after sheaf has exited: the first line read from
	// the terminal must be the typeahead, not the marker. (exec switched
	// the slave to blocking mode, so read deadlines do not apply.)
	master := r.master
	if _, err := master.Write([]byte("zzz\n")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := r.slave.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case line := <-got:
		if !strings.HasPrefix(line, "abc") {
			t.Fatalf("first line read after sheaf version is %q, want the typeahead abc: sheaf consumed it; terminal output: %q", line, r.output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing readable from the terminal within 10s")
	}
}
