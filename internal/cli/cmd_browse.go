package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jponge/sheaf/internal/ops"
)

func addBrowseCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newBrowseCmd(d))
}

func newBrowseCmd(d Deps) *cobra.Command {
	var opts ops.BrowseOptions
	cmd := &cobra.Command{
		Use:   "browse [<repo>]",
		Short: "Open a repository, its pull request or branch on GitHub in the web browser",
		Long: "Open a page of a GitHub repository in the web browser, with gh (which must be\n" +
			"installed and logged in). Without argument, the repository of the checkout\n" +
			"containing the working directory; with one, that repository of the bundle (its\n" +
			"primary checkout).\n\n" +
			"For a clone of your fork, the page is on the repository (upstream); --fork opens\n" +
			"the home page of your fork (origin). In a worktree of a pull request from a fork\n" +
			"checked out by `sheaf wt pr` (branch pr/<number>), the pull request opens. For a\n" +
			"branch tracking another branch of the repository or of your fork (except the\n" +
			"default branch), gh looks for the pull request of that branch, and the home page\n" +
			"of the repository opens when there is none; otherwise the home page opens.\n\n" +
			"--branch opens the tree of the branch the checkout's branch tracks, on the\n" +
			"repository it is pushed to (the repository or your fork); it is refused for a\n" +
			"branch without upstream and with --fork. Repositories of other hosts are refused.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			wd, err := d.getwd()
			if err != nil {
				return err
			}
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			t, err := ops.Browse(c.Context(), w, d.hosting(), arg, wd, opts)
			if err != nil {
				return err
			}
			fmt.Fprintln(c.ErrOrStderr(), "opened "+t.String())
			// The home screen shows it as the outcome (nothing changed).
			d.reportUnchanged("opened " + t.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.Fork, "fork", false, "open the home page of your fork (origin) rather than the repository (upstream)")
	cmd.Flags().BoolVar(&opts.Branch, "branch", false, "open the tree of the upstream branch of the checkout's branch")
	return cmd
}
