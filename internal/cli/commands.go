package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// Commands with menus, confirmations or long-running work run their TUI
// variant (cmd_tui.go) when Deps.tuiMode says so; the others are always
// plain: status, path, wt list/add/pr, version, completion, shell-init,
// doctor, run (which streams the commands' output), create.

func addBundleCommands(root *cobra.Command, d Deps) {
	root.AddCommand(newCreateCmd(d), newInitCmd(d), newAddCmd(d), newAdoptCmd(d), newRemoveCmd(d))
	addCheckoutCommands(root, d)
	addWorktreeCommands(root, d)
	addSyncCommands(root, d)
	addShellCommands(root)
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
			if d.tuiMode(c) {
				return tuiInitAdopt(c, d, root, cands, adoptAll, OptionsFrom(c).Jobs)
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
			defer d.emergencyNote(noteAdopt)()
			s := ops.AdoptAll(c.Context(), w, paths, ops.AdoptOptions{Interactive: d.interactiveGit(c)}, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
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
			"skipped. Use --as to choose the directory name of a single repository.\n" +
			"Your forks are looked up (one gh listing): a repository you forked is cloned with\n" +
			"origin = your fork and upstream = the repository, its default branch tracking\n" +
			"upstream; --no-fork disables this. sheaf never creates forks.",
		RunE: func(c *cobra.Command, args []string) error {
			req.Slugs = args
			if len(args) == 0 && req.Org == "" && !req.All && len(req.Repos) == 0 {
				return errors.New("nothing to add: give owner/repo arguments or --org <org>")
			}
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			if d.tuiMode(c) {
				return tuiAdd(c, d, w, d.hosting(), req, OptionsFrom(c).Jobs)
			}
			req.Interactive = d.interactiveGit(c)
			before := w.Bundle()
			defer d.emergencyNote(noteClone)()
			s, err := ops.Add(c.Context(), w, d.hosting(), req, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
			if err != nil {
				return err
			}
			err = printSummary(c.OutOrStdout(), s)
			printNewSources(c.OutOrStdout(), before, w.Bundle())
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.As, "as", "", "directory name for the repository (single repository only)")
	f.StringVar(&req.Org, "org", "", "add repositories of this organization or user")
	f.BoolVar(&req.All, "all", false, "with --org: add every (non-archived) repository")
	f.StringArrayVar(&req.Repos, "repo", nil, "with --org: add this repository (repeatable)")
	f.BoolVar(&req.IncludeArchived, "include-archived", false, "with --org: also consider archived repositories")
	f.BoolVar(&req.NoFork, "no-fork", false, "do not look for your forks: clone each repository as origin")
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
			"aliases are recorded as-is unless --host is given. A clone with a remote upstream\n" +
			"naming another repository whose default branch shares history with origin's is\n" +
			"recorded as a fork: upstream is the repository, origin your fork (a default branch\n" +
			"that does not track upstream is reported, with the fix).\n\n" +
			"With --recheck the arguments are repositories already registered in sheaf.yaml (by name\n" +
			"or directory). Their primary checkout's remotes are re-examined with the same rules\n" +
			"(for example after `git fetch upstream`) and the fork is recorded when they now\n" +
			"qualify, using the recorded default branch (--default-branch is ignored). Nothing is\n" +
			"moved, checked out or re-tracked. A recorded fork is never dropped or changed (a\n" +
			"warning says so), and the repository slug only changes when no other repository has it.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			var dirs []string
			for _, a := range args {
				if _, ok := w.Bundle().Repository(a); ok && opts.Recheck && a == filepath.Base(a) {
					dirs = append(dirs, filepath.Join(w.Root, a)) // a registered repository, by name
					continue
				}
				p, err := absFrom(d, a)
				if err != nil {
					return err
				}
				dirs = append(dirs, p)
			}
			if !opts.Recheck { // --recheck moves nothing
				defer d.emergencyNote(noteAdopt)()
			}
			if d.tuiMode(c) {
				return tuiSummary(c, d, "Adopting checkouts", func(ctx context.Context, rep ops.Reporter) ops.Summary {
					return ops.AdoptAll(ctx, w, dirs, opts, OptionsFrom(c).Jobs, rep)
				})
			}
			opts.Interactive = d.interactiveGit(c)
			s := ops.AdoptAll(c.Context(), w, dirs, opts, OptionsFrom(c).Jobs, plainRenderer{c.ErrOrStderr()})
			return printSummary(c.OutOrStdout(), s)
		},
	}
	cmd.Flags().StringVar(&opts.DefaultBranch, "default-branch", "", "the repository's default branch (upstream's for a clone of your fork), when it cannot be determined (applies to every <dir>)")
	cmd.Flags().BoolVar(&opts.Recheck, "recheck", false, "re-examine the remotes of repositories already registered (record a fork now visible) instead of adopting")
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
			"then delete its worktrees, its directory and its sheaf.yaml entry. Commits of a pull\n" +
			"request head checked out by `sheaf wt pr` count as pushed (with --offline, as of its\n" +
			"last fetch). Entries of the repository directory that are not checkouts of it are\n" +
			"data at risk too: --force deletes them unchecked.\n\n" +
			"--force never overrides these refusals: the working directory is inside the\n" +
			"repository directory; the repository directory is a symbolic link; no (or several)\n" +
			"primary checkouts; a locked worktree; a worktree outside the repository directory; a\n" +
			"worktree of another repository anywhere inside it (or a directory there that cannot\n" +
			"be read); a fetch that failed for another reason than a gone remote.\n\n" +
			"In CLI mode the removal must be confirmed with --yes; sheaf never prompts. In TUI mode\n" +
			"a dialog shows what will be deleted and asks for confirmation, unless --yes is given.\n" +
			"In plain mode with stdin on a terminal, git keeps the terminal (ssh may prompt).",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			name := args[0]
			wd, err := d.getwd()
			if err != nil {
				return err
			}
			if d.tuiMode(c) {
				return tuiRemove(c, d, w, name, ops.RemoveOptions{Force: force, Offline: offline, WorkingDir: wd}, yes)
			}
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
				for _, l := range removePlanLines(p) {
					emit(ops.Event{Message: l})
				}
				return yes
			}
			msg, err := ops.Remove(c.Context(), w, name, ops.RemoveOptions{Force: force, Offline: offline, Confirm: confirm, WorkingDir: wd, Interactive: d.singleRepoInteractiveGit(c)}, emit)
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

// removePlanLines describes a removal plan: the informational risks and the
// directory that will be deleted.
func removePlanLines(p ops.RemovePlan) []string {
	var lines []string
	for _, it := range p.Items {
		for _, r := range it.Risks {
			if r.Informational() {
				lines = append(lines, fmt.Sprintf("%s: %v (informational, not blocking)", it.Path, r))
			}
		}
	}
	if p.Dir != "" {
		lines = append(lines, "will delete "+p.Dir)
	}
	return lines
}
