//go:build unix

package gitx

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// procGroup tracks the process group of one git invocation (see Run).
type procGroup struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	pgid     int       // set when Cancel sent SIGTERM
	signaled time.Time // when SIGTERM was sent
}

// startGroup makes cmd start in a new session and replaces its Cancel
// function so that cancellation signals the whole process group.
func startGroup(cmd *exec.Cmd) *procGroup {
	pg := &procGroup{cmd: cmd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid // session leader: pgid == pid
		pg.mu.Lock()
		pg.pgid, pg.signaled = pid, time.Now()
		pg.mu.Unlock()
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	return pg
}

// finish runs after cmd.Wait returned. If the group was cancelled and some
// member outlived the leader (e.g. ssh or a filter ignoring SIGTERM), it
// waits until waitDelay has elapsed since SIGTERM, then sends SIGKILL to the
// group.
//
// While a group has members its id cannot be reused, so probing it with
// signal 0 is safe; the only (theoretical) window is between the group
// emptying and the next probe.
func (pg *procGroup) finish() {
	pg.mu.Lock()
	pgid, signaled := pg.pgid, pg.signaled
	pg.mu.Unlock()
	if pgid == 0 {
		return
	}
	deadline := signaled.Add(waitDelay)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
