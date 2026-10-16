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
	cmd   *exec.Cmd
	delay time.Duration // wait delay of the invocation (see Run)
	// single: git runs in the caller's process group; only its own process
	// is signalled.
	single bool

	mu       sync.Mutex
	pgid     int       // set when Cancel sent SIGTERM
	signaled time.Time // when SIGTERM was sent
	desc     []procID  // single: descendants signalled by Cancel
	killed   bool      // kill sent SIGKILL to the group
}

// startGroup makes cmd start in a new session (when ownSession) and replaces
// its Cancel function so that cancellation signals the whole process group.
// delay is the grace period between SIGTERM and SIGKILL (see finish).
//
// Without ownSession (interactive mode, see Net.Interactive), git stays in
// the caller's session and process group: cancellation sends SIGTERM to the
// git process only (exec's WaitDelay then sends it SIGKILL), kill signals
// only that process, and finish has nothing to do.
func startGroup(cmd *exec.Cmd, delay time.Duration, ownSession bool) *procGroup {
	pg := &procGroup{cmd: cmd, delay: delay, single: !ownSession}
	if !ownSession {
		cmd.Cancel = func() error {
			// Collect the descendants before signalling git: once git exits
			// they are reparented and can no longer be found.
			desc := descendants(cmd.Process.Pid)
			pg.mu.Lock()
			pg.desc, pg.signaled = desc, time.Now()
			pg.mu.Unlock()
			err := cmd.Process.Signal(syscall.SIGTERM)
			for _, d := range desc {
				d.signal(syscall.SIGTERM)
			}
			return err
		}
		return pg
	}
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

// kill sends SIGKILL to the whole process group right now and reports whether
// it was sent. It must only be called between a successful Start and the end
// of Wait (while the leader has not been reaped, its group id cannot be
// reused).
func (pg *procGroup) kill() bool {
	if pg.single {
		desc := descendants(pg.cmd.Process.Pid)
		ok := pg.cmd.Process.Signal(syscall.SIGKILL) == nil
		for _, d := range desc {
			d.signal(syscall.SIGKILL)
		}
		return ok
	}
	if killGroup(pg.cmd.Process.Pid, syscall.SIGKILL) != nil {
		return false
	}
	pg.mu.Lock()
	pg.killed = true
	pg.mu.Unlock()
	return true
}

// killStragglers runs after kill: it sends SIGKILL to the group again every
// 10 ms until the group is empty (the signal fails with ESRCH), stop is
// closed, or pg.delay has elapsed. A process forked while the first SIGKILL
// was being delivered may have missed it: Linux makes fork atomic with
// respect to process-group signals, other kernels may not (this is the
// suspected cause of force-killed merges returning only after the wait
// delay on macOS, the straggler holding git's stderr open). The id cannot be
// reused while the group has members; the only (theoretical) window is
// between the group emptying and the next probe, as in finish.
func (pg *procGroup) killStragglers(stop <-chan struct{}) {
	if pg.single {
		return
	}
	pgid := pg.cmd.Process.Pid
	deadline := time.Now().Add(pg.delay)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for killGroup(pgid, syscall.SIGKILL) == nil && time.Now().Before(deadline) {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// killGroup sends sig to the process group pgid. It is a variable only so
// that tests can simulate a member missing a signal.
var killGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// finish runs after cmd.Wait returned. If the group was cancelled and some
// member outlived the leader (e.g. ssh or a filter ignoring SIGTERM), it
// waits until pg.delay has elapsed since SIGTERM, then sends SIGKILL to the
// group.
//
// While a group has members its id cannot be reused, so probing it with
// signal 0 is safe; the only (theoretical) window is between the group
// emptying and the next probe.
func (pg *procGroup) finish() {
	pg.mu.Lock()
	pgid, signaled, desc, killed := pg.pgid, pg.signaled, pg.desc, pg.killed
	pg.mu.Unlock()
	if killed {
		pg.killStragglers(nil)
		return
	}
	if pg.single {
		pg.finishDescendants(signaled, desc)
		return
	}
	if pgid == 0 {
		return
	}
	deadline := signaled.Add(pg.delay)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finishDescendants is finish for an interactive invocation (Linux only; no
// descendants are recorded elsewhere): descendants signalled by Cancel that
// are still alive pg.delay after SIGTERM receive SIGKILL. Processes are
// identified by pid and start time, so a reused pid is never signalled.
func (pg *procGroup) finishDescendants(signaled time.Time, desc []procID) {
	if len(desc) == 0 {
		return
	}
	deadline := signaled.Add(pg.delay)
	for {
		var alive []procID
		for _, d := range desc {
			if d.alive() {
				alive = append(alive, d)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, d := range alive {
				d.signal(syscall.SIGKILL)
			}
			return
		}
		desc = alive
		time.Sleep(10 * time.Millisecond)
	}
}
