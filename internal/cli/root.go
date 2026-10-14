package cli

import (
	"fmt"
	"io"
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

// NewRootCmd builds the root command wired to the given streams.
func NewRootCmd(s Streams) *cobra.Command {
	root := &cobra.Command{
		Use:           "sheaf",
		Short:         "Manage git repository checkouts and worktrees in a bundle",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
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
	pf.IntP("jobs", "j", runtime.NumCPU(), "number of repositories processed in parallel")

	root.AddCommand(newVersionCmd())
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

// Execute runs the CLI with the given arguments and returns the process exit code.
func Execute(args []string, s Streams) int {
	cmd := NewRootCmd(s)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "sheaf:", err)
		return 1
	}
	return 0
}
