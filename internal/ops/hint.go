package ops

import (
	"context"
	"slices"
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

// hintYesKey is the context key of --yes in the commands of hints (see
// WithHintYes).
type hintYesKey struct{}

// WithHintYes returns ctx with --yes repeated in the commands of the hints
// that run the refused removal again with --force or -D, or that run prune
// again (see hintCommand): set by a command confirmed with --yes in plain
// mode, so that a pasted hint runs as stated instead of asking for --yes.
// TUI mode does not set it: the pasted command then shows its confirmation
// dialog. Never repeated: in --offline hints (offline, the stale refs that
// a failed fetch could not refresh count, so the plan must be confirmed
// again), in hints about worktrees the refused command did not cover (pull
// request and tag worktrees of prune), in the remove hints of sync
// --remove-gone (its --yes confirmed removing gone repositories, not
// forcing past their risks), and in the remove hint for a primary checkout.
func WithHintYes(ctx context.Context) context.Context {
	return context.WithValue(ctx, hintYesKey{}, true)
}

// hintYes reports whether ctx has --yes in the commands of hints (see
// WithHintYes).
func hintYes(ctx context.Context) bool {
	yes, _ := ctx.Value(hintYesKey{}).(bool)
	return yes
}

// hintCommand returns the command line of args (see commandLineIn) with
// bundle as the bundle of hints. With yes, --yes is added before the "--"
// of args (at the end without one), and the bundle is named even when the
// user did not give --bundle (root, the resolved bundle root, when bundle
// is "", see YesCommandLine).
func hintCommand(yes bool, bundle, root string, args ...string) string {
	if !yes {
		return commandLineIn(bundle, args...)
	}
	i := slices.Index(args, "--")
	if i < 0 {
		i = len(args)
	}
	return yesCommandLineIn(bundle, root, slices.Concat(args[:i], []string{"--yes"}, args[i:])...)
}

// yesCommandLineIn is commandLineIn for args that carry --yes: the bundle
// is named even when the user did not give --bundle (root, the resolved
// bundle root, when bundle is "").
func yesCommandLineIn(bundle, root string, args ...string) string {
	if bundle == "" {
		bundle = root
	}
	return commandLineIn(bundle, args...)
}

// YesCommandLine is CommandLine for a command line that carries --yes in
// args, in the bundle of resolved root root: it always names the bundle,
// root unless the user gave --bundle (see WithHintBundle). Pasted in another
// bundle with a repository of the same name, a command that needs no
// confirmation must still act on this bundle.
func YesCommandLine(ctx context.Context, root string, args ...string) string {
	return yesCommandLineIn(hintBundle(ctx), root, args...)
}

// confirmedCommandLine is CommandLine of args with --yes and the bundle
// root when ctx has --yes (see WithHintYes, hintCommand).
func confirmedCommandLine(ctx context.Context, root string, args ...string) string {
	return hintCommand(hintYes(ctx), hintBundle(ctx), root, args...)
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
