package ops

import (
	"context"
	"strings"
)

// hintBundleKey is the context key of the bundle of hints (see
// WithHintBundle).
type hintBundleKey struct{}

// WithHintBundle returns ctx with the bundle root that the command lines
// of hints name with --bundle (see CommandLine): set when the user chose
// the bundle with --bundle, so that a hint pasted elsewhere (e.g. in
// another bundle) acts on the same bundle.
func WithHintBundle(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, hintBundleKey{}, root)
}

// hintBundle returns the bundle of hints of ctx (see WithHintBundle), or
// "".
func hintBundle(ctx context.Context) string {
	root, _ := ctx.Value(hintBundleKey{}).(string)
	return root
}

// CommandLine returns the sheaf command line of args, each one quoted for
// the shell if need be (see shellQuote): "sheaf wt rm --force --dir abc --
// api", with --bundle and the bundle of hints of ctx first, if any (see
// WithHintBundle). The hints that tell what to run next end with "run: "
// and such a line, so that it can be copied to the end of the line and
// pasted. They repeat the options of the command that matter to the hint;
// of the global ones, only --bundle.
func CommandLine(ctx context.Context, args ...string) string {
	return commandLineIn(hintBundle(ctx), args...)
}

// commandLineIn is CommandLine with bundle as the bundle of hints.
func commandLineIn(bundle string, args ...string) string {
	words := []string{"sheaf"}
	if bundle != "" {
		args = append([]string{"--bundle", bundle}, args...)
	}
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
