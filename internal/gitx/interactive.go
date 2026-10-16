package gitx

import (
	"context"
	"strings"
)

type interactiveKey struct{}

// WithInteractive returns a context under which the network operations of
// gitx (Clone, FetchRemote, FetchAll, and the ls-remote fallback of
// DefaultBranchOf) keep sheaf's controlling terminal, so that ssh can ask for
// a passphrase or a host key confirmation, and git for HTTPS credentials
// (interactive git mode). It is meant for the plain CLI processing one repository at a time
// with standard input on a terminal; with parallel invocations, prompts would
// interleave.
//
// Under such a context these operations run git:
//
//   - in sheaf's own session and process group (no setsid), so git and ssh
//     can open /dev/tty and a terminal Ctrl-C reaches them directly;
//   - without the forced GIT_TERMINAL_PROMPT=0 (the inherited value, if any,
//     is kept); LC_ALL=C and the stripping of repository-redirecting
//     variables still apply;
//   - with cancellation and KillAll acting on the git process and, on Linux,
//     on its descendants found through /proc at that moment (ssh,
//     upload-pack, ...; descendants still alive after the grace period get
//     SIGKILL). Elsewhere only git is signalled; its children normally exit
//     when git's pipes close.
//
// Every other gitx operation ignores the mark and always runs in its own
// session, in particular the merge of FastForward, which must not receive a
// terminal Ctrl-C. The mark survives derived contexts (including
// context.WithoutCancel).
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive reports whether ctx (or a parent) was made by
// WithInteractive.
func IsInteractive(ctx context.Context) bool {
	v, _ := ctx.Value(interactiveKey{}).(bool)
	return v
}

// netOpts returns the run options of a network operation under ctx.
func netOpts(ctx context.Context) runOpts { return runOpts{interactive: IsInteractive(ctx)} }

// StripRepoEnv returns environ without the variables that redirect git to
// another repository (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, ...; the list
// Run strips, see strippedEnv). Use it for commands other than git that run in
// a checkout and may call git, e.g. user commands. Nothing is added.
func StripRepoEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !strippedEnv[k] {
			out = append(out, kv)
		}
	}
	return out
}

// interactiveEnv is the child environment of an interactive invocation:
// childEnv without forcing GIT_TERMINAL_PROMPT=0.
func interactiveEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range StripRepoEnv(environ) {
		if k, _, _ := strings.Cut(kv, "="); k != "LC_ALL" {
			out = append(out, kv)
		}
	}
	return append(out, "LC_ALL=C")
}
