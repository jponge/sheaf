package gitx

import (
	"strings"
	"time"
)

// Net runs the network operations of gitx: Clone, CloneFork, FetchRemote,
// FetchAll, FetchRefspecs, FetchTagsFrom and DefaultBranchOf (whose
// ls-remote fallback asks the remote). The zero value runs git like every
// other operation (see Run): in its own session, without a controlling
// terminal, with GIT_TERMINAL_PROMPT=0. A Net is a plain value, safe for
// concurrent use.
type Net struct {
	// Interactive keeps sheaf's controlling terminal, so that ssh can ask
	// for a passphrase or a host key confirmation, and git for HTTPS
	// credentials (interactive git mode). It is meant for the plain CLI
	// processing one repository at a time with standard input on a
	// terminal; with parallel invocations, prompts would interleave.
	//
	// When set, the operations run git:
	//
	//   - in sheaf's own session and process group (no setsid), so git and
	//     ssh can open /dev/tty and a terminal Ctrl-C reaches them directly;
	//   - without the forced GIT_TERMINAL_PROMPT=0 (the inherited value, if
	//     any, is kept); LC_ALL=C and the stripping of repository-redirecting
	//     variables still apply;
	//   - with cancellation and KillAll acting on the git process and, on
	//     Linux, on its descendants found through /proc at that moment (ssh,
	//     upload-pack, ...; descendants still alive after the grace period
	//     get SIGKILL). Elsewhere only git is signalled; its children
	//     normally exit when git's pipes close.
	//
	// Other gitx operations have no interactive mode and always run in
	// their own session, in particular the merge of FastForward, which must
	// not receive a terminal Ctrl-C.
	Interactive bool

	// waitDelay replaces defaultWaitDelay when positive (tests only).
	waitDelay time.Duration
}

// opts returns the run options of a network operation of n.
func (n Net) opts() runOpts { return runOpts{interactive: n.Interactive, waitDelay: n.waitDelay} }

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
