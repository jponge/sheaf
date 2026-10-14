// Package gitx is a thin, context-aware wrapper over the git command-line
// client. Every operation runs git in an explicit working directory with a
// sanitized environment (see Run) and returns a *GitError on failure.
//
// Functions that need a remote assume it is called "origin".
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// waitDelay bounds how long Run waits after cancellation before escalating
// to SIGKILL (see Run), and how long it waits for I/O after git was killed (a
// grandchild may keep the output pipes open).
//
// It is a variable only so that tests can shorten it; it must not be changed
// while git commands are running.
var waitDelay = 5 * time.Second

// Run executes git with args in dir and returns its raw standard output.
//
// If git exits 0 but a process it spawned keeps standard output or error
// open (e.g. a hook that starts `sleep 60 &`), Run stops waiting for the
// output waitDelay after git exited and reports success with the output read
// so far, unless ctx is done.
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
//     still alive waitDelay after that, it receives SIGKILL; Run returns only
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
	if dir == "" {
		return "", errors.New("gitx: empty working directory")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ())
	delay := waitDelay
	cmd.WaitDelay = delay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	pg := startGroup(cmd, delay)
	err := cmd.Run()
	pg.finish()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() && ctx.Err() == nil {
		// git exited 0, but a process it spawned (typically a hook running
		// something in the background) kept stdout or stderr open past
		// waitDelay. The git operation itself succeeded; the output captured
		// until the pipes were closed is kept.
		err = nil
	}
	if err != nil {
		ge := &GitError{Args: args, Dir: dir, ExitCode: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ge.ExitCode = ee.ExitCode()
		}
		if ctx.Err() != nil {
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
