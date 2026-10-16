package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

// Version is the sheaf version; override with
// -ldflags "-X github.com/jponge/sheaf/internal/cli.Version=...".
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
fetch, add and remove worktrees, prune, doctor, sync). Without a terminal
or with --no-tui, it prints this help.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
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
	if s.Err != nil {
		root.SetErr(s.Err)
	}
	pf := root.PersistentFlags()
	pf.String("bundle", "", "bundle directory (default: nearest parent containing sheaf.yaml)")
	pf.Bool("no-tui", false, "disable the interactive TUI and use plain CLI output")
	pf.IntP("jobs", "j", runtime.NumCPU(), "number of repositories processed in parallel (checkouts for status and run); with -j 1, in plain mode with stdin on a terminal, git keeps the terminal and may prompt (ssh passphrase, host key): remove and wt add/rm/pr always do so")

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
			fmt.Fprintln(c.OutOrStdout(), Version)
		},
	}
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
		fmt.Fprintln(errw, "sheaf: Windows is not supported; use Linux or macOS")
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
	return execute(NewRootCmdWithDeps(s, Deps{Getenv: getenv}), args)
}
