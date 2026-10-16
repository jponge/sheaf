// Package gitx is a thin, context-aware wrapper over the git command-line
// client. Every operation runs git in an explicit working directory with a
// sanitized environment (see Run) and returns a *GitError on failure.
//
// Functions that need a remote and take no remote name assume it is called
// "origin"; the *Of / *From variants and Remotes support repositories
// cloned from a fork (origin = the fork, upstream = the reference).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GitError describes a failed git invocation.
type GitError struct {
	Args     []string // arguments passed to git (without "git")
	Dir      string   // working directory
	ExitCode int      // process exit code, -1 if git did not exit normally
	Stderr   string   // trimmed standard error
	Err      error    // underlying error: *exec.ExitError, start error, or the context error
}

func (e *GitError) Error() string {
	msg := fmt.Sprintf("git %s (in %s): %v", strings.Join(e.Args, " "), e.Dir, e.Err)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// Unwrap returns the underlying error, so errors.Is(err, context.Canceled)
// works for cancelled invocations.
func (e *GitError) Unwrap() error { return e.Err }

// defaultWaitDelay bounds how long Run waits after cancellation before
// escalating to SIGKILL (see Run), and how long it waits for I/O after git
// was killed (a grandchild may keep the output pipes open). Tests shorten
// it per invocation (runOpts.waitDelay).
const defaultWaitDelay = 5 * time.Second

// Run executes git with args in dir and returns its raw standard output.
//
// If git exits 0 but a process it spawned keeps standard output or error
// open (e.g. a hook that starts `sleep 60 &`), Run stops waiting for the
// output defaultWaitDelay after git exited and reports success with the output read
// so far, unless ctx is done. If ctx is cancelled after git exited 0 but
// while Run is still in that pipe wait, Run returns an error (wrapping
// ctx.Err()) although the git operation itself completed: callers that care
// must verify the effect rather than infer it from the error.
//
// dir must not be empty. The child environment is os.Environ() minus the
// variables that redirect git to another repository (see strippedEnv), with
// LC_ALL=C and GIT_TERMINAL_PROMPT=0 forced, so output is parseable and git
// never prompts. Standard input is empty.
//
// Process tree and cancellation (Unix): git runs as the leader of a new
// session (setsid), so it and everything it spawns (git subcommands, ssh,
// filters, hooks) form one process group without a controlling terminal.
// Consequences:
//
//   - When ctx is done, the whole group receives SIGTERM, which lets git
//     remove its lock files (e.g. index.lock) on the way out. If the group is
//     still alive defaultWaitDelay after that, it receives SIGKILL; Run returns only
//     once the group is gone or has been sent SIGKILL. A process that leaves
//     the group (setsid/setpgid of its own) escapes this.
//   - Neither git nor ssh can open /dev/tty, so they can never prompt on the
//     terminal (and corrupt a TUI); authentication that needs a prompt fails.
//   - A terminal SIGINT (Ctrl-C) is no longer delivered to git: callers must
//     translate SIGINT into cancelling ctx.
//
// On other platforms the default exec.CommandContext behaviour applies (only
// the git process itself is killed).
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, runOpts{}, dir, args...)
}

// ErrForceKilled is the error wrapped by the *GitError of an invocation whose
// process group was killed through a force channel (see
// FastForwardOptions.Force).
var ErrForceKilled = errors.New("gitx: git killed on request")

// runOpts are the internal extras of run over Run.
type runOpts struct {
	// stdin, when non-nil, is git's standard input (default: empty).
	stdin io.Reader
	// force, when non-nil and closed while git runs, SIGKILLs git's whole
	// process group immediately (no SIGTERM grace period, so git cannot remove
	// its lock files); the error then wraps ErrForceKilled.
	force <-chan struct{}
	// interactive keeps the caller's session and controlling terminal (see
	// Net.Interactive).
	interactive bool
	// waitDelay replaces defaultWaitDelay when positive (tests only).
	waitDelay time.Duration
}

// run runs git like Run, with the options o, through the query cache of ctx
// if any (see WithQueryCache).
func run(ctx context.Context, o runOpts, dir string, args ...string) (string, error) {
	if dir == "" {
		return "", errors.New("gitx: empty working directory")
	}
	return runCached(ctx, o, dir, args)
}

// runProcess starts one git process (see Run).
func runProcess(ctx context.Context, o runOpts, dir string, args ...string) (string, error) {
	recordCall(ctx, dir, args)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if o.interactive {
		cmd.Env = interactiveEnv(os.Environ())
	} else {
		cmd.Env = childEnv(os.Environ())
	}
	delay := o.waitDelay
	if delay <= 0 {
		delay = defaultWaitDelay
	}
	cmd.WaitDelay = delay
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.stdin, &stdout, &stderr
	pg := startGroup(cmd, delay, !o.interactive)
	var forced atomic.Bool
	// Start under the read side of the gate: KillAll (write side) then sees
	// every started process registered, and no process starts after it.
	running.gate.RLock()
	if running.stopped {
		running.gate.RUnlock()
		return "", &GitError{Args: args, Dir: dir, ExitCode: -1, Err: ErrForceKilled}
	}
	err := cmd.Start()
	if err == nil {
		register(pg, &forced)
	}
	running.gate.RUnlock()
	if err == nil {
		done := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-o.force: // nil channel: never
				if pg.kill() {
					forced.Store(true)
					// Members that missed the signal would keep Wait
					// waiting for the output pipes (see killStragglers).
					pg.killStragglers(done)
				}
			case <-done:
			}
		}()
		err = cmd.Wait()
		unregister(pg)
		close(done)
		<-stopped
	}
	pg.finish()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() && ctx.Err() == nil {
		// git exited 0, but a process it spawned (typically a hook running
		// something in the background) kept stdout or stderr open past
		// the wait delay. The git operation itself succeeded; the output captured
		// until the pipes were closed is kept.
		err = nil
	}
	if err != nil {
		ge := &GitError{Args: args, Dir: dir, ExitCode: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			ge.ExitCode = ee.ExitCode()
		}
		if forced.Load() {
			ge.Err = ErrForceKilled
		} else if ctx.Err() != nil {
			ge.Err = ctx.Err()
		}
		return stdout.String(), ge
	}
	return stdout.String(), nil
}

// strippedEnv lists the variables removed from the child environment. Git
// exports some of them to hooks; inheriting them would make every command
// operate on the hook's repository instead of dir. It covers `git rev-parse
// --local-env-vars` (checked by a test against the installed git) except
// GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT (with GIT_CONFIG_KEY_n and
// GIT_CONFIG_VALUE_n), which carry user configuration, not a repository
// location, plus GIT_NAMESPACE and GIT_QUARANTINE_PATH (set by receive-pack
// for its hooks).
var strippedEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_PREFIX":                       true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG":                       true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_QUARANTINE_PATH":              true,
}

// forcedEnv is appended to the child environment, replacing inherited values.
var forcedEnv = []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0"}

func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+len(forcedEnv))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strippedEnv[k] || k == "LC_ALL" || k == "GIT_TERMINAL_PROMPT" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, forcedEnv...)
}

// staleLockRE matches git's message for a lock file that already exists, e.g.
// "fatal: Unable to create '/r/.git/index.lock': File exists." or "error:
// cannot lock ref 'refs/remotes/origin/main': Unable to create
// '/r/.git/refs/remotes/origin/main.lock': File exists.".
var staleLockRE = regexp.MustCompile(`(?m)Unable to create '(.+\.lock)': File exists\.`)

// StaleLockPath reports whether err (or an error it wraps) is a *GitError
// caused by a lock file that already existed, and returns that file's path.
// Such a lock is either held by a concurrent git process or left behind by
// one that was killed (git removes its locks on SIGTERM, but not on SIGKILL
// or a crash); gitx cannot tell which, so callers should report the path
// rather than delete it. A relative path is resolved against GitError.Dir.
func StaleLockPath(err error) (string, bool) {
	var ge *GitError
	if !errors.As(err, &ge) {
		return "", false
	}
	m := staleLockRE.FindStringSubmatch(ge.Stderr)
	if m == nil {
		return "", false
	}
	p := m[1]
	if !filepath.IsAbs(p) {
		p = filepath.Join(ge.Dir, p)
	}
	return p, true
}

// running holds the process groups of the git invocations between Start and
// the end of Wait, for KillAll.
var running = struct {
	gate    sync.RWMutex // read: starting a process; write: KillAll
	stopped bool         // set by KillAll, guarded by gate
	sync.Mutex
	m map[*procGroup]*atomic.Bool
}{m: map[*procGroup]*atomic.Bool{}}

func register(pg *procGroup, forced *atomic.Bool) {
	running.Lock()
	running.m[pg] = forced
	running.Unlock()
}

func unregister(pg *procGroup) {
	running.Lock()
	delete(running.m, pg)
	running.Unlock()
}

// KillAll sends SIGKILL to the process group (outside Unix: the process) of
// every git invocation currently running and returns how many were
// signalled; their Run calls fail with an error wrapping ErrForceKilled. It
// is meant for an emergency stop (e.g. a second Ctrl-C) right before the
// process exits: from then on no git process is started any more (Run fails
// with ErrForceKilled), and git does not remove its lock files on SIGKILL
// (see StaleLockPath).
//
// Processes being started when KillAll is called are waited for and killed.
// Remaining window: as with the per-call force option, an invocation whose
// leader has just been reaped by Wait but not yet unregistered could in
// theory be signalled after its process group id was reused by an
// unrelated process; the window is a few instructions.
func KillAll() int {
	running.gate.Lock()
	running.stopped = true
	running.gate.Unlock()
	running.Lock()
	defer running.Unlock()
	n := 0
	for pg, forced := range running.m {
		if pg.kill() {
			forced.Store(true)
			n++
		}
	}
	return n
}

// resetStopped re-enables starting processes after KillAll (tests only).
func resetStopped() {
	running.gate.Lock()
	running.stopped = false
	running.gate.Unlock()
}
