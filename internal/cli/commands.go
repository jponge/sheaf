package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// For this step every command runs in plain CLI mode; UseTUI is the switch
// the TUI wiring will use (menus for add --org, confirmation for remove).

func addBundleCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newCreateCmd(d), newInitCmd(d), newAddCmd(d), newAdoptCmd(d), newRemoveCmd(d))
	addCheckoutCommands(root, d)
	addWorktreeCommands(root, d)
	addSyncCommands(root, d)
	addShellCommands(root, d)
	addMaintenanceCommands(root, d)
}

// absFrom resolves p against the injectable working directory.
func absFrom(d Deps, p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	wd, err := d.getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// openWorkspace finds the bundle (--bundle or upwards from the working
// directory) and loads it.
func openWorkspace(c *cobra.Command, d Deps) (*ops.Workspace, error) {
	wd, err := d.getwd()
	if err != nil {
		return nil, err
	}
	root, err := ops.FindBundle(OptionsFrom(c).Bundle, wd)
	if err != nil {
		return nil, err
	}
	return ops.OpenWorkspace(root)
}

func dirArg(d Deps, args []string) (string, error) {
	if len(args) == 0 {
		return absFrom(d, ".")
	}
	return absFrom(d, args[0])
}

func newCreateCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "create [dir]",
		Short: "Create a new, empty bundle (default: the current directory)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirArg(d, args)
			if err != nil {
				return err
			}
			root, err := ops.Create(dir)
			if err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
			fmt.Fprintf(c.OutOrStdout(), "created bundle %s\n", root)
			return nil
		},
	}
}

func newInitCmd(d Deps) *cobra.Command {
	var adoptAll bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Turn an existing directory into a bundle and list the checkouts found in it",
		Long: "Create sheaf.yaml in an existing directory (default: the current directory) and list\n" +
			"the checkouts found in its subdirectories: plain clones (<dir>/.git) and repositories\n" +
			"with a single checkout <dir>/<child>/ (named after a branch, or <dir> named after the\n" +
			"origin repository). With --adopt-all they are adopted right away.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir, err := dirArg(d, args)
			if err != nil {
				return err
			}
			root, cands, err := ops.Init(c.Context(), dir)
			if err != nil {
				return fmt.Errorf("init %s: %w", dir, err)
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "initialized bundle %s\n", root)
			if len(cands) == 0 {
				fmt.Fprintln(out, "no existing checkouts found")
				return nil
			}
			var names, paths []string
			for _, cand := range cands {
				fmt.Fprintf(out, "found %s: %s (%s)\n", cand.Kind, cand.Name, cand.Primary)
				names = append(names, cand.Name)
				paths = append(paths, cand.Path)
			}
			if !adoptAll {
				fmt.Fprintf(out, "adopt them with: sheaf adopt %s\n", strings.Join(names, " "))
				return nil
			}
			w, err := ops.OpenWorkspace(root)
			if err != nil {
				return err
			}
			s := ops.AdoptAll(checkoutContext(c), w, paths, ops.AdoptOptions{}, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
			return printSummary(out, s)
		},
	}
	cmd.Flags().BoolVar(&adoptAll, "adopt-all", false, "adopt every checkout found")
	return cmd
}

func newAddCmd(d Deps) *cobra.Command {
	var req ops.AddRequest
	cmd := &cobra.Command{
		Use:   "add <owner/repo>... | add --org <org> (--all | --repo <name>...)",
		Short: "Clone repositories into the bundle",
		Long: "Clone repositories into <bundle>/<repo>/<default branch>/ and record them in sheaf.yaml.\n" +
			"Repositories are given as owner/repo, or selected from an organization with --org and\n" +
			"either --all or one --repo per repository (archived ones need --include-archived).\n" +
			"The organization is then tracked as a source. Repositories already in the bundle are\n" +
			"skipped. Use --as to choose the directory name of a single repository.",
		RunE: func(c *cobra.Command, args []string) error {
			req.Slugs = args
			if len(args) == 0 && req.Org == "" && !req.All && len(req.Repos) == 0 {
				return errors.New("nothing to add: give owner/repo arguments or --org <org>")
			}
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			s, err := ops.Add(checkoutContext(c), w, d.hosting(), req, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
			if err != nil {
				return err
			}
			return printSummary(c.OutOrStdout(), s)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.As, "as", "", "directory name for the repository (single repository only)")
	f.StringVar(&req.Org, "org", "", "add repositories of this organization or user")
	f.BoolVar(&req.All, "all", false, "with --org: add every (non-archived) repository")
	f.StringArrayVar(&req.Repos, "repo", nil, "with --org: add this repository (repeatable)")
	f.BoolVar(&req.IncludeArchived, "include-archived", false, "with --org: also consider archived repositories")
	return cmd
}

func newAdoptCmd(d Deps) *cobra.Command {
	var opts ops.AdoptOptions
	cmd := &cobra.Command{
		Use:   "adopt <dir>...",
		Short: "Register existing checkouts located directly in the bundle",
		Long: "Register existing checkouts. The primary checkout of a repository lives in\n" +
			"<dir>/<default branch>/ and may have any branch checked out. A plain clone <dir>/ is\n" +
			"moved to <dir>/<default branch>/; a single checkout <dir>/<child>/ is renamed to it if\n" +
			"needed (or only registered). Nothing is checked out or switched, linked worktrees are\n" +
			"repaired. Owner, name and host are derived from the origin remote URL; SSH config\n" +
			"aliases are recorded as-is unless --host is given.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			var dirs []string
			for _, a := range args {
				p, err := absFrom(d, a)
				if err != nil {
					return err
				}
				dirs = append(dirs, p)
			}
			s := ops.AdoptAll(checkoutContext(c), w, dirs, opts, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
			return printSummary(c.OutOrStdout(), s)
		},
	}
	cmd.Flags().StringVar(&opts.DefaultBranch, "default-branch", "", "origin's default branch, when it cannot be determined (applies to every <dir>)")
	cmd.Flags().StringVar(&opts.Host, "host", "", "host to record instead of the one in the origin URL, e.g. github.com for an SSH config alias (applies to every <dir>)")
	return cmd
}

func newRemoveCmd(d Deps) *cobra.Command {
	var force, yes, offline bool
	cmd := &cobra.Command{
		Use:   "remove <repo>",
		Short: "Remove a repository, its worktrees and its directory from the bundle",
		Long: "Remove a repository: fetch it, refuse when any checkout or worktree has data that\n" +
			"would be lost (changes, untracked files, stashes, unpushed commits) unless --force,\n" +
			"then delete its worktrees, its directory and its sheaf.yaml entry. In CLI mode the\n" +
			"removal must be confirmed with --yes; sheaf never prompts.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			name := args[0]
			r := plainRenderer{c.ErrOrStderr()}
			emit := func(e ops.Event) {
				if e.Repo == "" {
					e.Repo = name
				}
				if e.Phase == "" {
					e.Phase = ops.PhaseRemove
				}
				if e.Kind == 0 {
					e.Kind = ops.KindProgress
				}
				r.Report(e)
			}
			confirm := func(p ops.RemovePlan) bool {
				for _, it := range p.Items {
					for _, r := range it.Risks {
						if r.Informational() {
							emit(ops.Event{Message: fmt.Sprintf("%s: %v (informational, not blocking)", it.Path, r)})
						}
					}
				}
				if p.Dir != "" {
					emit(ops.Event{Message: "will delete " + p.Dir})
				}
				return yes
			}
			wd, err := d.getwd()
			if err != nil {
				return err
			}
			msg, err := ops.Remove(checkoutContext(c), w, name, ops.RemoveOptions{Force: force, Offline: offline, Confirm: confirm, WorkingDir: wd}, emit)
			if errors.Is(err, ops.ErrNotConfirmed) {
				return fmt.Errorf("removing %s needs confirmation: pass --yes (sheaf never prompts in CLI mode)", name)
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), msg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even if data would be lost")
	cmd.Flags().BoolVar(&offline, "offline", false, "do not fetch first (the remote state may be stale)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm the removal (required in CLI mode)")
	return cmd
}
