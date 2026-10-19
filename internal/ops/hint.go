package ops

import "strings"

// CommandLine returns the sheaf command line of args, each one quoted for
// the shell if need be (see shellQuote): "sheaf wt rm --force --dir abc --
// api". The hints that tell what to run next end with "run: " and such a
// line, so that it can be copied to the end of the line and pasted. They
// repeat the options of the command that matter to the hint, not the
// global ones (e.g. --bundle): the command is meant to be run in the
// bundle.
func CommandLine(args ...string) string {
	words := []string{"sheaf"}
	for _, a := range args {
		words = append(words, shellQuote(a))
	}
	return strings.Join(words, " ")
}

// fetchFailed starts the description of a failed fetch (see fetchProblem),
// which may be overcome with --offline (see hasFetchProblem).
const fetchFailed = "fetch failed, the remote state is unknown"

// hasFetchProblem reports whether one of problems describes a failed
// fetch (see fetchProblem).
func hasFetchProblem(problems ...string) bool {
	for _, p := range problems {
		if strings.Contains(p, fetchFailed) {
			return true
		}
	}
	return false
}
