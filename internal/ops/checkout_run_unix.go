//go:build unix

package ops

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// coProcTree is the process group of one command of RunCommand.
type coProcTree struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	signaled time.Time // when SIGTERM was sent, zero if never
}

// coPrepareTree makes cmd start in a new session (so it and its children form
// one process group without controlling terminal) and makes cancellation send
// SIGTERM to the whole group.
func coPrepareTree(cmd *exec.Cmd) *coProcTree {
	t := &coProcTree{cmd: cmd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		t.mu.Lock()
		t.signaled = time.Now()
		t.mu.Unlock()
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	return t
}

// leftovers reports whether the process group of the exited command still
// has members (background processes). While it has members its id cannot be
// reused, so the probe is safe.
func (t *coProcTree) leftovers() bool {
	return syscall.Kill(-t.cmd.Process.Pid, 0) == nil
}

// terminate sends SIGTERM to the group of an exited command whose leftover
// background processes kept its output open; finish then escalates.
func (t *coProcTree) terminate() {
	t.mu.Lock()
	t.signaled = time.Now()
	t.mu.Unlock()
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
}

// finish runs after Wait: if the group was sent SIGTERM and members remain
// (children ignoring SIGTERM that outlived the leader), it waits until grace
// has elapsed since SIGTERM, then sends SIGKILL to the group. While the group
// has members its id cannot be reused, so probing it is safe.
func (t *coProcTree) finish(grace time.Duration) {
	t.mu.Lock()
	signaled := t.signaled
	t.mu.Unlock()
	if signaled.IsZero() {
		return
	}
	pgid := t.cmd.Process.Pid
	deadline := signaled.Add(grace)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// coKillTree sends SIGKILL to the process group of a started, not yet reaped
// command.
func coKillTree(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// coUnlinkOpen removes the name of an open temporary file; the data stays
// reachable through the open descriptor.
func coUnlinkOpen(name string) bool { return os.Remove(name) == nil }
