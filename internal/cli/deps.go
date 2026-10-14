package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// Deps are the external dependencies of the commands. Zero fields mean the
// production implementations.
type Deps struct {
	// Hosting returns the hosting service; default: the gh CLI adapter.
	Hosting func() hosting.Hosting
	// Getwd returns the directory used for bundle discovery and relative
	// paths; default: os.Getwd.
	Getwd func() (string, error)
	// GHProbe checks that gh is installed and authenticated (doctor);
	// default: defaultGHProbe, which runs `gh auth status`.
	GHProbe func(context.Context) error
	// UseTUI decides whether a command runs its TUI; default: UseTUI
	// (standard input and output are terminals, no --no-tui).
	UseTUI func(in io.Reader, out io.Writer, noTUI bool) bool
}

func (d Deps) hosting() hosting.Hosting {
	if d.Hosting != nil {
		return d.Hosting()
	}
	return hosting.NewGH(hosting.GHOptions{})
}

func (d Deps) getwd() (string, error) {
	if d.Getwd != nil {
		return d.Getwd()
	}
	return os.Getwd()
}

// Seams for the second-signal emergency stop.
var (
	killGit      = gitx.KillAll
	killCommands = ops.KillCommands
	killAll      = defaultKillAll
	exit         = os.Exit
)

// defaultKillAll kills the process groups of the running git invocations
// and of the commands of `sheaf run`.
func defaultKillAll() int { return killGit() + killCommands() }

type ctlKey struct{}

// interruptCtl implements the two-stage interruption shared by the signal
// handler of execute and the ctrl+c key of the TUI (bubbletea's raw mode
// turns ctrl+c into a key, not a SIGINT): see interrupt.
type interruptCtl struct {
	cancel context.CancelFunc
	force  chan struct{}
	errw   io.Writer
	count  atomic.Int32

	mu      sync.Mutex
	release func() // restores the terminal of the running TUI program, if any
	notify  func(int)
}

// setNotify registers f, called with the number of interruptions so far on
// each one (the progress view displays it); nil unregisters it. f must not
// block.
func (c *interruptCtl) setNotify(f func(int)) {
	c.mu.Lock()
	c.notify = f
	c.mu.Unlock()
}

func newInterruptCtl(cancel context.CancelFunc, errw io.Writer) *interruptCtl {
	return &interruptCtl{cancel: cancel, force: make(chan struct{}), errw: errw}
}

// interruptFrom returns the controller installed by execute, nil outside
// execute.
func interruptFrom(ctx context.Context) *interruptCtl {
	c, _ := ctx.Value(ctlKey{}).(*interruptCtl)
	return c
}

// forceFrom returns the channel that the emergency stop closes right
// before killing processes; commands pass it to operations that otherwise
// ignore cancellation (the fast-forward merges of update). It is nil
// outside execute.
func forceFrom(ctx context.Context) <-chan struct{} {
	if c := interruptFrom(ctx); c != nil {
		return c.force
	}
	return nil
}

// setRelease registers f, which gives the terminal back (e.g. kills the
// running bubbletea program and waits for it), to be called before the
// emergency stop writes its message and exits; nil unregisters it.
func (c *interruptCtl) setRelease(f func()) {
	c.mu.Lock()
	c.release = f
	c.mu.Unlock()
}

// interrupt is called for every interruption, a signal or a ctrl+c key:
// the first one cancels the command's context; the second one is the
// emergency stop (see execute); later ones do nothing. It is safe for
// concurrent use, but must not be called from the event loop of a running
// TUI program (its release hook waits for that program to end).
func (c *interruptCtl) interrupt() {
	n := c.count.Add(1)
	c.mu.Lock()
	notify := c.notify
	c.mu.Unlock()
	if notify != nil && n <= 2 {
		notify(int(n))
	}
	switch n {
	case 1:
		c.cancel()
	case 2:
		c.emergencyStop()
	}
}

func (c *interruptCtl) emergencyStop() {
	close(c.force)
	c.mu.Lock()
	release := c.release
	c.mu.Unlock()
	if release != nil {
		release()
	}
	fmt.Fprintln(c.errw, "sheaf: interrupted again, killing git processes and exiting without cleanup.\n"+
		"An adopt may have been interrupted mid-move: rerun `sheaf adopt <dir>`, and if <dir> is missing\n"+
		"look for the clone in a .sheaf-adopt-* folder of the bundle and move it back first.\n"+
		"An interrupted add may leave a partial clone directory: delete it before adding again.\n"+
		"A fast-forward killed by update may leave .git/index.lock and a half-updated work tree: check `git status` there.")
	killAll()
	exit(130)
}

// cancelSignals cancel the context of the running command. git runs in its
// own session (see gitx.Run), so it does not receive the terminal's SIGINT
// itself: cancellation is what stops it.
var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// execute runs cmd with args under a context cancelled by the first of
// cancelSignals, and returns the exit code: 130 with "interrupted" when the
// command failed after such a signal. A second signal (or TUI ctrl+c, see
// interruptCtl; they count together) is an emergency stop:
// the process groups of the running git commands receive SIGKILL (git then
// leaves its lock files behind, see gitx.StaleLockPath) and sheaf exits with
// 130 immediately, without cleanups (partial clones may remain); before that,
// the channel returned by forceFrom is closed and the terminal of a running
// TUI program restored.
func execute(cmd *cobra.Command, args []string) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, cancelSignals...)
	defer signal.Stop(sigs)
	// With SIGPIPE caught, a write to a closed pipe (e.g. `sheaf run ... |
	// head`) returns EPIPE instead of killing sheaf, so cancellation and
	// cleanups still run; see pipeWatch.
	pipes := make(chan os.Signal, 1)
	signal.Notify(pipes, syscall.SIGPIPE)
	defer signal.Stop(pipes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := newInterruptCtl(cancel, cmd.ErrOrStderr())
	ctx = context.WithValue(ctx, ctlKey{}, ctl)
	out := &pipeWatch{w: cmd.OutOrStdout(), cancel: cancel}
	cmd.SetOut(out)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for range 2 {
			select {
			case <-sigs:
				ctl.interrupt()
			case <-done:
				return
			}
		}
	}()
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	if out.broken.Load() {
		// Standard output was closed by its reader: stop quietly with the
		// status of a process killed by SIGPIPE, as pipelines expect.
		return 141
	}
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "sheaf: interrupted")
			return 130
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "sheaf:", err)
		return 1
	}
	return 0
}

// UseTUI reports whether a command should run its TUI: only when both in
// and out are terminals and noTUI (--no-tui) is not set. In plain CLI mode
// every choice must come from flags and missing input is an error.
func UseTUI(in io.Reader, out io.Writer, noTUI bool) bool {
	return useTUI(isTerminal(in), isTerminal(out), noTUI)
}

func useTUI(inTTY, outTTY, noTUI bool) bool { return inTTY && outTTY && !noTUI }

// isTerminal reports whether x is an *os.File connected to a terminal. It
// uses SyscallConn rather than Fd, which would switch a non-blocking file to
// blocking mode.
func isTerminal(x any) bool {
	if p, ok := x.(*pipeWatch); ok {
		x = p.w
	}
	f, ok := x.(*os.File)
	if !ok || f == nil {
		return false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	isTerm := false
	if err := rc.Control(func(fd uintptr) { isTerm = term.IsTerminal(fd) }); err != nil {
		return false
	}
	return isTerm
}

// pipeWatch wraps standard output: the first write failing with EPIPE (the
// reader went away) cancels the command, like an interrupt.
type pipeWatch struct {
	w      io.Writer
	cancel context.CancelFunc
	broken atomic.Bool
}

func (p *pipeWatch) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if err != nil && errors.Is(err, syscall.EPIPE) && p.broken.CompareAndSwap(false, true) {
		p.cancel()
	}
	return n, err
}
