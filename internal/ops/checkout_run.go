package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
)

// RunSink receives the output of RunCommand. Its calls are serialized and
// made in checkout order (see RunCommand).
type RunSink interface {
	// Begin announces the output of c and returns where to write it.
	Begin(c Checkout) io.Writer
	// End reports the result of the checkout announced by the last Begin.
	End(r RunResult)
}

// DefaultRunGrace is the default delay between SIGTERM and SIGKILL when a
// command is cancelled.
const DefaultRunGrace = 5 * time.Second

// RunOptions configures RunCommand.
type RunOptions struct {
	Filter CheckoutFilter
	// DirtyOnly keeps the checkouts that are not clean (see
	// CheckoutStatus.Clean); checkouts whose status fails become problems.
	DirtyOnly bool
	Jobs      int
	// Shell runs Args[0] as a script with `sh -c`; the remaining Args are its
	// positional parameters $1, $2, ... ($0 is "sh"). Otherwise Args[0] is
	// executed directly (looked up in PATH), without a shell.
	Shell bool
	Args  []string
	// Stream runs the checkouts one at a time (Jobs is ignored) and writes
	// the output to the sink's writer as it is produced; otherwise the output
	// of each checkout is captured and handed to the sink once it finished.
	Stream bool
	// Grace is the delay between SIGTERM and SIGKILL on cancellation (zero:
	// DefaultRunGrace).
	Grace time.Duration
	Sink  RunSink
}

// RunResult is the outcome of the command in one checkout.
type RunResult struct {
	Checkout
	// Started is false when the command could not be started (Err says why)
	// or was not started because the context was cancelled first.
	Started bool
	// ExitCode is the exit status, -1 when the command did not exit normally
	// (killed by a signal) or did not start.
	ExitCode int
	// Err is nil when the command ran and exited (whatever its status);
	// otherwise the start error, the signal, or the context's error.
	Err error
	// Note is an informational remark that does not make the result a
	// failure, e.g. truncated output.
	Note string
}

// Failed reports whether the command did not run or did not exit with 0.
func (r RunResult) Failed() bool { return !r.Started || r.Err != nil || r.ExitCode != 0 }

// RunReport is the result of RunCommand.
type RunReport struct {
	Results  []RunResult // sorted like ListCheckouts
	Problems []Problem
}

type discardSink struct{}

func (discardSink) Begin(Checkout) io.Writer { return io.Discard }
func (discardSink) End(RunResult)            {}

// RunCommand runs a command in each selected checkout directory, up to Jobs
// at a time. Standard output and error are combined; standard input is
// empty. The command runs in a new session (Unix: setsid), so it has no
// controlling terminal, and its environment is sheaf's minus the variables
// that redirect git (gitx.StripRepoEnv), plus SHEAF_REPO, SHEAF_DIR (the
// checkout's directory name) and SHEAF_BRANCH (empty when detached).
//
// Output ordering: the sink sees Begin, the output, then End for each
// checkout in checkout order. With Stream this happens live, one checkout at
// a time; otherwise outputs are captured and released in order as soon as
// all preceding checkouts finished. Checkouts not started because ctx was
// cancelled get no Begin/End; they are in the results with Started false and
// Err matching ctx.Err().
//
// Cancellation sends SIGTERM to the process group of each running command,
// then SIGKILL to the group after the grace period. When a command exits
// (whatever its status) while processes it started in the background are
// still in its process group, they are terminated the same way and the
// result gets a Note. Commands meant to leave a daemon behind must detach it
// from the group themselves (e.g. with setsid). The error is only set for an
// invalid filter or an empty command.
func RunCommand(ctx context.Context, w *Workspace, opts RunOptions) (RunReport, error) {
	if len(opts.Args) == 0 || opts.Args[0] == "" {
		return RunReport{}, errors.New("no command given")
	}
	argv := opts.Args
	if opts.Shell {
		argv = append([]string{"sh", "-c", opts.Args[0], "sh"}, opts.Args[1:]...)
	}
	sets, probs, err := enumerate(ctx, w, opts.Filter, opts.Jobs)
	if err != nil {
		return RunReport{}, err
	}
	var cs []Checkout
	for _, s := range sets {
		cs = append(cs, s.checkouts...)
	}
	if opts.DirtyOnly {
		sts := make([]CheckoutStatus, len(cs))
		forEach(opts.Jobs, len(cs), func(i int) {
			sts[i].Checkout = cs[i]
			sts[i].Status, sts[i].Err = gitx.Status(ctx, cs[i].Path)
		})
		cs = cs[:0]
		for _, s := range sts {
			switch {
			case s.Err != nil:
				probs = append(probs, Problem{Repo: s.Repo, Path: s.Path, Err: fmt.Errorf("status: %w", s.Err)})
			case !s.Clean():
				cs = append(cs, s.Checkout)
			}
		}
		sortProblems(probs)
	}
	sink := opts.Sink
	if sink == nil {
		sink = discardSink{}
	}
	grace := opts.Grace
	if grace <= 0 {
		grace = DefaultRunGrace
	}
	results := make([]RunResult, len(cs))
	notStarted := func(i int) bool {
		if err := ctx.Err(); err != nil {
			results[i] = RunResult{Checkout: cs[i], ExitCode: -1, Err: err}
			return true
		}
		return false
	}
	if opts.Stream {
		for i, c := range cs {
			if notStarted(i) {
				continue
			}
			out := sink.Begin(c)
			results[i] = runIn(ctx, c, argv, writerOnly{out}, grace)
			sink.End(results[i])
		}
		return RunReport{Results: results, Problems: probs}, nil
	}

	var mu sync.Mutex
	outputs := make([]*spillBuffer, len(cs))
	done := make([]bool, len(cs))
	next := 0
	flush := func() {
		for next < len(cs) && done[next] {
			if outputs[next] != nil {
				w := sink.Begin(cs[next])
				outputs[next].replay(w)
				sink.End(results[next])
				outputs[next] = nil
			}
			next++
		}
	}
	forEach(opts.Jobs, len(cs), func(i int) {
		if !notStarted(i) {
			buf := &spillBuffer{}
			r := runIn(ctx, cs[i], argv, buf, grace)
			mu.Lock()
			results[i], outputs[i] = r, buf
			mu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		done[i] = true
		flush()
	})
	return RunReport{Results: results, Problems: probs}, nil
}

// writerOnly hides any other method of the writer (in particular *os.File's),
// so that exec always copies the output through a pipe: the command never
// writes to sheaf's terminal directly (no pager, no terminal detection).
type writerOnly struct{ w io.Writer }

func (w writerOnly) Write(p []byte) (int, error) { return w.w.Write(p) }

// spillLimit is the size above which a captured output moves from memory
// to a temporary file.
const spillLimit = 1 << 20

// spillBuffer captures the combined output of one command (its writes are
// serialized): in memory up to spillLimit, then in a temporary file of
// os.TempDir(). On Unix the file is unlinked as soon as it is created (the
// open descriptor keeps the data), so nothing is left behind even when sheaf
// exits without cleanup (emergency stop); elsewhere it is removed after
// replay.
type spillBuffer struct {
	mu       sync.Mutex
	unlinked bool
	buf      bytes.Buffer
	f        *os.File
	err      error // first spill error; later output is dropped
}

func (s *spillBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return len(p), nil // keep the command running; the loss is reported
	}
	if s.f == nil && s.buf.Len()+len(p) > spillLimit {
		s.spill()
		if s.err != nil {
			return len(p), nil
		}
	}
	if s.f == nil {
		return s.buf.Write(p)
	}
	if _, err := s.f.Write(p); err != nil {
		s.err = err
	}
	return len(p), nil
}

// spill moves the buffered output to a new temporary file.
func (s *spillBuffer) spill() {
	f, err := os.CreateTemp("", "sheaf-run-output-*")
	if err != nil {
		s.err = err
		return
	}
	s.f = f
	s.unlinked = unlinkOpen(f.Name())
	if spillHook != nil {
		spillHook(s.f.Name())
	}
	if _, err := s.f.Write(s.buf.Bytes()); err != nil {
		s.err = err
	}
	s.buf = bytes.Buffer{}
}

// replay writes the captured output to w in chunks of at most 64 KiB, then
// releases the temporary file. It must be called once, after the command
// finished.
func (s *spillBuffer) replay(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		_, _ = w.Write(s.buf.Bytes())
	} else {
		defer func() {
			name := s.f.Name()
			_ = s.f.Close()
			if !s.unlinked {
				_ = os.Remove(name)
			}
		}()
		if _, err := s.f.Seek(0, io.SeekStart); err != nil && s.err == nil {
			s.err = err
		}
		chunk := make([]byte, 64<<10)
		for s.err == nil {
			n, err := s.f.Read(chunk)
			if n > 0 {
				_, _ = w.Write(chunk[:n])
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				s.err = err
			}
		}
	}
	if s.err != nil {
		fmt.Fprintf(w, "\n[sheaf: output lost: %v]\n", s.err)
	}
}

// runningCommands holds the running commands for KillCommands.
var runningCommands = struct {
	sync.Mutex
	stopped bool
	m       map[*exec.Cmd]bool
}{m: map[*exec.Cmd]bool{}}

// errCommandsKilled is the error of commands not started after KillCommands.
var errCommandsKilled = errors.New("not started (killed on request)")

// KillCommands sends SIGKILL to the process group (outside Unix: the process)
// of every command started by RunCommand that is still running, and returns
// how many were signalled. From then on RunCommand starts no command any
// more. It is meant for an emergency stop right before the process exits
// (like gitx.KillAll).
func KillCommands() int {
	runningCommands.Lock()
	defer runningCommands.Unlock()
	runningCommands.stopped = true
	n := 0
	for cmd := range runningCommands.m {
		if killTree(cmd) == nil {
			n++
		}
	}
	return n
}

// runIn runs argv in c.Path with combined output written to out.
func runIn(ctx context.Context, c Checkout, argv []string, out io.Writer, grace time.Duration) RunResult {
	res := RunResult{Checkout: c, ExitCode: -1}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = c.Path
	cmd.Env = append(gitx.StripRepoEnv(os.Environ()), "SHEAF_REPO="+c.Repo, "SHEAF_DIR="+c.Dir, "SHEAF_BRANCH="+c.Branch)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = grace
	t := prepareTree(cmd)
	runningCommands.Lock()
	if runningCommands.stopped {
		runningCommands.Unlock()
		res.Err = errCommandsKilled
		return res
	}
	err := cmd.Start()
	if err == nil {
		runningCommands.m[cmd] = true
	}
	runningCommands.Unlock()
	if err != nil {
		res.Err = err
		return res
	}
	res.Started = true
	err = cmd.Wait()
	bgHeld := errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && ctx.Err() == nil
	// Whatever the exit status, processes the command left in its process
	// group (background jobs) are terminated like a cancelled command: the
	// group is sheaf's to clean up. A process that left the group (setsid,
	// or a daemon that detaches itself) is not affected.
	leftovers := ctx.Err() == nil && (bgHeld || t.leftovers())
	if leftovers {
		t.terminate()
	}
	t.finish(grace)
	// Unregister only now: until finish returns, members of the group may
	// still be alive (KillCommands must reach them).
	runningCommands.Lock()
	delete(runningCommands.m, cmd)
	runningCommands.Unlock()
	var ee *exec.ExitError
	switch {
	case bgHeld:
		res.ExitCode = cmd.ProcessState.ExitCode()
		res.Note = "output may be truncated: a background process kept the output open (background processes were terminated)"
		if res.ExitCode < 0 {
			res.Err = errors.New(cmd.ProcessState.String())
		}
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee) && ee.ExitCode() >= 0 && ctx.Err() == nil:
		res.ExitCode = ee.ExitCode()
	case ctx.Err() != nil:
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
		res.Err = fmt.Errorf("%w (%v)", ctx.Err(), err)
	default:
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
		res.Err = err
	}
	if leftovers && res.Note == "" {
		res.Note = "background processes left by the command were terminated"
	}
	return res
}

// spillHook, when set by a test, is called with the path of each temporary
// file used to hold a large output.
var spillHook func(path string)

// resetCommands re-enables starting commands after KillCommands (tests
// only).
func resetCommands() {
	runningCommands.Lock()
	runningCommands.stopped = false
	runningCommands.Unlock()
}
