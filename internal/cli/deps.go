package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
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
	killAll = gitx.KillAll
	exit    = os.Exit
)

// cancelSignals cancel the context of the running command. git runs in its
// own session (see gitx.Run), so it does not receive the terminal's SIGINT
// itself: cancellation is what stops it.
var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// execute runs cmd with args under a context cancelled by the first of
// cancelSignals, and returns the exit code: 130 with "interrupted" when the
// command failed after such a signal. A second signal is an emergency stop:
// the process groups of the running git commands receive SIGKILL (git then
// leaves its lock files behind, see gitx.StaleLockPath) and sheaf exits with
// 130 immediately, without cleanups (partial clones may remain).
func execute(cmd *cobra.Command, args []string) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, cancelSignals...)
	defer signal.Stop(sigs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sigs:
			fmt.Fprintln(cmd.ErrOrStderr(), "sheaf: interrupted again, killing git processes and exiting without cleanup.\n"+
				"An adopt may have been interrupted mid-move: rerun `sheaf adopt <dir>`, and if <dir> is missing\n"+
				"look for the clone in a .sheaf-adopt-* folder of the bundle and move it back first.\n"+
				"An interrupted add may leave a partial clone directory: delete it before adding again.")
			killAll()
			exit(130)
		case <-done:
		}
	}()
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
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
