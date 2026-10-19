package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
	// Getenv reads an environment variable (SHEAF_CD_FILE, see runHome);
	// default: os.Getenv. Execute answers SHEAF_CD_FILE with the value read
	// at startup, as it removes the variable from the environment.
	Getenv func(string) string

	// ctl is the interrupt controller of the execution (see execute),
	// shared by the TUI sessions of the commands and the force channel of
	// update; nil when the commands do not run through execute (a TUI
	// session then makes its own, see newTUISession).
	ctl *interruptCtl
	// unchanged, if set, is told when a command ends without changing
	// anything (e.g. "nothing to prune"): the home screen reports that
	// rather than "done".
	unchanged func(note string)
	// cancelled, if set, is told when the user chose nothing in a menu
	// and the command ended without error (sync): the home screen counts
	// that as a cancellation.
	cancelled func()
	// term, if set, replaces standard output as the terminal on which TUI
	// programs render and which decides the TUI mode (see tuiMode): the
	// home screen captures what its actions print for its report view.
	term io.Writer
	// windowTitle, if set, is the terminal window title of the TUI programs
	// of the commands (see tuiSession): the home screen names itself and
	// its actions.
	windowTitle string
	// stdinTTY replaces isTerminal for standard input (interactive git
	// mode); tests only.
	stdinTTY func(io.Reader) bool
	// environ replaces os.Environ for the color decisions (see leveled);
	// tests only.
	environ func() []string
	// spin is the working indicator on standard error, nil when it is not
	// a terminal (see NewRootCmdWithDeps).
	spin *spinner
	// now replaces time.Now for the AGE column of the home screen; tests
	// only.
	now func() time.Time
}

// reportUnchanged tells d.unchanged, if set, that the command changed
// nothing, and why.
func (d Deps) reportUnchanged(note string) {
	if d.unchanged != nil {
		d.unchanged(note)
	}
}

// reportCancelled tells d.cancelled, if set, that the user chose nothing.
func (d Deps) reportCancelled() {
	if d.cancelled != nil {
		d.cancelled()
	}
}

func (d Deps) getenv(k string) string {
	if d.Getenv != nil {
		return d.Getenv(k)
	}
	return os.Getenv(k)
}

// hosting returns the hosting service; its gh default repository and
// browse calls show the working indicator (see spinHosting).
func (d Deps) hosting() hosting.Hosting {
	var h hosting.Hosting
	if d.Hosting != nil {
		h = d.Hosting()
	} else {
		h = hosting.NewGH(hosting.GHOptions{})
	}
	if d.spin != nil {
		h = spinHosting{h, d.spin}
	}
	return h
}

func (d Deps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d Deps) getwd() (string, error) {
	if d.Getwd != nil {
		return d.Getwd()
	}
	return os.Getwd()
}

// killAllOf returns the kill function of the emergency stop: it kills the
// process groups of the running git invocations (killGit, gitx.KillAll) and
// of the commands of `sheaf run` (killCommands, ops.KillCommands), and
// returns how many were signalled.
func killAllOf(killGit, killCommands func() int) func() int {
	return func() int { return killGit() + killCommands() }
}

// interruptCtl implements the two-stage interruption shared by the signal
// handler of execute and the ctrl+c key of the TUI (bubbletea's raw mode
// turns ctrl+c into a key, not a SIGINT): see interrupt.
type interruptCtl struct {
	cancel context.CancelFunc
	force  chan struct{}
	errw   io.Writer
	count  atomic.Int32
	// killAll and exit end the emergency stop (tests replace them).
	killAll func() int
	exit    func(int)

	mu      sync.Mutex
	release func() // restores the terminal of the running TUI program, if any
	notify  func(int)
	notes   []string // see during
	// registered, if set, is called with each note registered by during
	// (tests only).
	registered func(note string)
}

// setNotify registers f, called with the number of interruptions so far on
// each one (the progress view displays it); nil unregisters it. f must not
// block.
func (c *interruptCtl) setNotify(f func(int)) {
	c.mu.Lock()
	c.notify = f
	c.mu.Unlock()
}

// newInterruptCtl returns a controller that is not bound yet: bind must
// be called before the first interruption.
func newInterruptCtl() *interruptCtl {
	return &interruptCtl{force: make(chan struct{}), killAll: killAllOf(gitx.KillAll, ops.KillCommands), exit: os.Exit}
}

// bind sets the function cancelling the context of the commands (first
// interruption) and the writer of the emergency stop's message.
func (c *interruptCtl) bind(cancel context.CancelFunc, errw io.Writer) {
	c.cancel, c.errw = cancel, errw
}

// force returns the channel that the emergency stop closes right before
// killing processes; commands pass it to operations that otherwise ignore
// cancellation (the fast-forward merges of update). It is nil outside
// execute.
func (d Deps) force() <-chan struct{} {
	if d.ctl != nil {
		return d.ctl.force
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

// Advice of the emergency stop about the operations it may interrupt (see
// interruptCtl.during).
const (
	noteAdopt = "An adopt may have been interrupted mid-move: rerun `sheaf adopt <dir>`, and if <dir> is missing\n" +
		"look for the clone in a .sheaf-adopt-* folder of the bundle and move it back first."
	noteClone       = "An interrupted add may leave a partial clone directory: delete it before adding again."
	noteFastForward = "A fast-forward killed by update may leave .git/index.lock and a half-updated work tree: check `git status` there."
)

// during registers note, which the emergency stop prints until the
// returned function is called: the advice about the operation running in
// the meantime. The same note may be registered several times.
func (c *interruptCtl) during(note string) (done func()) {
	c.mu.Lock()
	c.notes = append(c.notes, note)
	registered := c.registered
	c.mu.Unlock()
	if registered != nil {
		registered(note)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if i := slices.Index(c.notes, note); i >= 0 {
				c.notes = slices.Delete(c.notes, i, i+1)
			}
		})
	}
}

// emergencyNote is interruptCtl.during on the controller of d, if any.
func (d Deps) emergencyNote(note string) (done func()) {
	if d.ctl == nil {
		return func() {}
	}
	return d.ctl.during(note)
}

func (c *interruptCtl) emergencyStop() {
	close(c.force)
	c.mu.Lock()
	release := c.release
	c.mu.Unlock()
	if release != nil {
		release()
	}
	c.mu.Lock()
	var notes []string
	for _, n := range c.notes {
		if !slices.Contains(notes, n) {
			notes = append(notes, n)
		}
	}
	c.mu.Unlock()
	fmt.Fprintln(c.errw, strings.Join(append([]string{"sheaf: interrupted again, killing git processes and exiting without cleanup."}, notes...), "\n"))
	c.killAll()
	c.exit(130)
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
// the channel returned by Deps.force is closed and the terminal of a running
// TUI program restored.
//
// ctl is the controller of the Deps cmd was built with (it binds it), so
// that the TUI sessions of the commands share it; nil gives execute a
// controller of its own, which only the signals reach.
func execute(cmd *cobra.Command, ctl *interruptCtl, args []string) int {
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
	if ctl == nil {
		ctl = newInterruptCtl()
	}
	ctl.bind(cancel, cmd.ErrOrStderr())
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
