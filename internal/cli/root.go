// Package cli implements the sheaf command line on top of ops: the cobra
// commands, the rendering of their results, the interactive screens, shell
// integration and completion.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/jponge/sheaf/internal/ops"
)

// Version is the sheaf version; override with
// -ldflags "-X github.com/jponge/sheaf/internal/cli.Version=...". When it is
// not overridden, `sheaf version` falls back to the build information Go
// records (see versionString).
var Version = "dev"

// Streams groups the standard streams used by commands.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Options holds global configuration shared by all commands. Later steps extend it.
type Options struct {
	Bundle string
	NoTUI  bool
	Jobs   int
}

// NewRootCmd builds the root command wired to the given streams and the
// production dependencies.
func NewRootCmd(s Streams) *cobra.Command { return NewRootCmdWithDeps(s, Deps{}) }

// NewRootCmdWithDeps builds the root command with explicit dependencies
// (tests inject a fake hosting service and working directory).
func NewRootCmdWithDeps(s Streams, d Deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "sheaf",
		Short: "Manage git repository checkouts and worktrees in a bundle",
		Long: `Manage git repository checkouts and worktrees in a bundle.

Without a command, inside a bundle and with a terminal, sheaf opens its
home screen: an overview of the bundle's checkouts with actions (update,
fetch, add and remove worktrees, check out pull requests, open on GitHub,
prune, doctor, sync). Without a terminal or with --no-tui, it prints this help;
outside a bundle, it says how to create one.

-j sets how many repositories are processed in parallel (checkouts for status
and run). With -j 1, in plain mode with stdin on a terminal, git keeps the
terminal and may prompt (ssh passphrase, host key): remove and wt add/rm/pr
always do so.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			c.SetContext(hintContext(c, d))
			if !c.HasParent() {
				return nil // bare sheaf: runHome checks --jobs only when it opens the home screen
			}
			if o := OptionsFrom(c); o.Jobs < 1 {
				return fmt.Errorf("invalid --jobs %d: must be at least 1", o.Jobs)
			}
			return nil
		},
	}
	if s.In != nil {
		root.SetIn(s.In)
	}
	if s.Out != nil {
		root.SetOut(s.Out)
	}
	errw := s.Err
	if errw == nil {
		errw = os.Stderr
	}
	// The working indicator draws on standard error when it is a terminal;
	// the commands of the home screen share the home screen's.
	if sp := spinnerOf(errw); sp != nil {
		d.spin = sp
	} else if f, ok := terminalOf(errw).(*os.File); ok && isTerminal(f) {
		d.spin = newSpinner(f)
		errw = d.spin.writer(errw)
	}
	if s.Err != nil || d.spin != nil {
		root.SetErr(errw)
	}
	pf := root.PersistentFlags()
	pf.String("bundle", "", "bundle directory (default: nearest sheaf.yaml upwards)")
	pf.Bool("no-tui", false, "disable the interactive TUI and use plain CLI output")
	pf.IntP("jobs", "j", runtime.NumCPU(), "repositories at once (-j 1: sheaf --help)")

	root.AddCommand(newVersionCmd())
	addBundleCommands(root, d)
	installCompletions(root, d)
	installHome(root, d)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sheaf version",
		Args:  cobra.NoArgs,
		Run: func(c *cobra.Command, _ []string) {
			bi, _ := debug.ReadBuildInfo()
			fmt.Fprintln(c.OutOrStdout(), versionString(Version, bi))
		},
	}
}

// hintContext returns the context of c with the bundle that hints name
// (see ops.WithHintBundle): the bundle given with --bundle (d.hintBundle
// when set), resolved like the commands do; none without --bundle.
func hintContext(c *cobra.Command, d Deps) context.Context {
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	b := OptionsFrom(c).Bundle
	if d.hintBundle != nil {
		b = *d.hintBundle
	}
	if b == "" {
		return ctx
	}
	if wd, err := d.getwd(); err == nil {
		if root, err := ops.FindBundle(b, wd); err == nil {
			b = root
		}
	}
	return ops.WithHintBundle(ctx, b)
}

// plainHintContext returns the context of c for a removal run in plain
// mode, confirmed with --yes when yes: its hints then repeat --yes (see
// ops.WithHintYes).
func plainHintContext(c *cobra.Command, yes bool) context.Context {
	if yes {
		return ops.WithHintYes(c.Context())
	}
	return c.Context()
}

// OptionsFrom reads the global options from a command's flags.
func OptionsFrom(c *cobra.Command) Options {
	var o Options
	o.Bundle, _ = c.Flags().GetString("bundle")
	o.NoTUI, _ = c.Flags().GetBool("no-tui")
	o.Jobs, _ = c.Flags().GetInt("jobs")
	return o
}

// Execute runs the CLI with the given arguments and returns the process exit
// code. SIGINT, SIGTERM and SIGHUP cancel the context of the running command
// (see execute). SHEAF_CD_FILE (see runHome) is read, then removed from the
// environment before anything runs: the processes sheaf starts (commands of
// `sheaf run`, git and its hooks) must not be able to write that file.
func Execute(args []string, s Streams) int { return executeOn("", args, s) }

// executeOn is Execute for the operating system goos (runtime.GOOS when
// empty). Windows is not supported: sheaf relies on Unix process groups,
// signals and file locks there, so it refuses to run rather than run with
// those safeguards missing.
func executeOn(goos string, args []string, s Streams) int {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" {
		errw := s.Err
		if errw == nil {
			errw = os.Stderr
		}
		fmt.Fprintln(errw, "sheaf: Windows is not supported; use Linux, macOS or WSL")
		return 1
	}
	cdFile := os.Getenv(cdFileVar)
	_ = os.Unsetenv(cdFileVar)
	getenv := func(k string) string {
		if k == cdFileVar {
			return cdFile
		}
		return os.Getenv(k)
	}
	d := Deps{Getenv: getenv, ctl: newInterruptCtl()}
	return execute(NewRootCmdWithDeps(s, d), d.ctl, args)
}

// versionString returns the version to print: stamped (Version) when set at
// build time, else the module version Go recorded in bi (`go install
// ...@v1.2.3`, or a pseudo-version for a build in a git checkout), else
// "dev" with the commit when recorded. bi may be nil.
func versionString(stamped string, bi *debug.BuildInfo) string {
	if stamped != "dev" || bi == nil {
		return stamped
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return stamped
	}
	rev = rev[:min(12, len(rev))]
	if modified {
		rev += "-dirty"
	}
	return stamped + " (" + rev + ")"
}
