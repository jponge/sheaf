package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// addWorktreeCommands registers the worktree and path commands.
func addWorktreeCommands(root *cobra.Command, d Deps) {
	wt := &cobra.Command{
		Use:     "worktree",
		Aliases: []string{"wt"},
		Short:   "Add, list and remove worktrees (siblings of the primary checkout)",
		Long: "Manage the worktrees of the bundle's repositories, located at\n" +
			"<bundle>/<repo>/<normalized branch>/ next to the primary checkout.\n\n" +
			singleArgHelp,
	}
	wt.AddCommand(newWorktreeAddCmd(d), newWorktreeListCmd(d), newWorktreeRemoveCmd(d))
	root.AddCommand(wt, newPathCmd(d))
}

// eventSink returns an emitter filling the defaults of events (repository,
// phase, kind) and printing them on stderr.
func eventSink(c *cobra.Command, repo, phase string) func(ops.Event) {
	r := plainRenderer{c.ErrOrStderr()}
	return func(e ops.Event) {
		if e.Repo == "" {
			e.Repo = repo
		}
		if e.Phase == "" {
			e.Phase = phase
		}
		if e.Kind == 0 {
			e.Kind = ops.KindProgress
		}
		r.Report(e)
	}
}

// singleArgHelp documents how [<repo>] <target> arguments are read.
const singleArgHelp = "With two arguments the first one is the repository. With one argument: inside a\n" +
	"checkout of the bundle it names a branch or directory of that checkout's repository;\n" +
	"outside any checkout it is a repository name."

// repoAndTarget interprets [<repo>] [<target>] arguments (see
// ops.ParseRepoArgs) and requires a target; what names it in errors.
func repoAndTarget(d Deps, w *ops.Workspace, args []string, sel *ops.WorktreeTarget, what string) (string, ops.WorktreeTarget, error) {
	wd, err := d.getwd()
	if err != nil {
		return "", ops.WorktreeTarget{}, err
	}
	repo, target, err := ops.ParseRepoArgs(w, args, sel, wd)
	if err != nil {
		return "", ops.WorktreeTarget{}, err
	}
	if target == nil {
		return "", ops.WorktreeTarget{}, fmt.Errorf("missing %s for repository %s (outside a checkout, a single argument is a repository name)", what, repo)
	}
	return repo, *target, nil
}

// selectorFlags adds --dir and --branch and returns a function building the
// selector (nil when neither is given).
func selectorFlags(cmd *cobra.Command) func() (*ops.WorktreeTarget, error) {
	var dir, branch string
	cmd.Flags().StringVar(&dir, "dir", "", "select the worktree by directory name")
	cmd.Flags().StringVar(&branch, "branch", "", "select the worktree by branch name")
	return func() (*ops.WorktreeTarget, error) {
		switch {
		case dir != "" && branch != "":
			return nil, errors.New("--dir and --branch are mutually exclusive")
		case dir != "":
			return &ops.WorktreeTarget{Name: dir, Kind: ops.TargetDir}, nil
		case branch != "":
			return &ops.WorktreeTarget{Name: branch, Kind: ops.TargetBranch}, nil
		}
		return nil, nil
	}
}

func newWorktreeAddCmd(d Deps) *cobra.Command {
	var opts ops.WorktreeAddOptions
	cmd := &cobra.Command{
		Use:   "add [<repo>] <branch>",
		Short: "Create a worktree for a branch and print its path",
		Long: "Create <bundle>/<repo>/<normalized branch>/ for branch: the local branch if it\n" +
			"exists, else a local branch tracking origin/<branch>, else a new branch from --base\n" +
			"(default origin/<default branch>). origin is fetched first unless --no-fetch.\n" +
			"The path is printed on stdout (cd \"$(sheaf wt add feat/x)\"); progress goes to stderr.\n\n" +
			singleArgHelp,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			repo, branch, err := repoAndTarget(d, w, args, nil, "<branch>")
			if err != nil {
				return err
			}
			res, err := ops.AddWorktree(c.Context(), w, repo, branch.Name, opts, eventSink(c, repo, ops.PhaseWorktree))
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), res.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Base, "base", "", "start point of a new branch (default: origin/<default branch>)")
	cmd.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not fetch origin first")
	return cmd
}

// worktreeListJSON is the --json document of `worktree list`.
type worktreeListJSON struct {
	Version   int                `json:"version"`
	Worktrees []ops.WorktreeInfo `json:"worktrees"`
	Errors    []repoErrorJSON    `json:"errors"`
}

type repoErrorJSON struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

func newWorktreeListCmd(d Deps) *cobra.Command {
	var asJSON, porcelain, nul bool
	cmd := &cobra.Command{
		Use:     "list [<repo>]",
		Aliases: []string{"ls"},
		Short:   "List the checkouts and worktrees of the bundle (or of one repository)",
		Long: "List every checkout of the bundle, or of <repo>: repository, directory, branch,\n" +
			"flags (primary, off-default, detached, locked, prunable, outside) and path. Ordered\n" +
			"by repository, primary first, then by directory. --porcelain prints the paths only\n" +
			"(-z: NUL-terminated); --json prints {\"version\": 1, \"worktrees\": [...], \"errors\": [...]}.\n" +
			"Repositories that cannot be listed are reported and make the exit status 1.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if asJSON && porcelain {
				return errors.New("--json and --porcelain are mutually exclusive")
			}
			if nul && !porcelain {
				return errors.New("-z requires --porcelain")
			}
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			repo := ""
			if len(args) == 1 {
				repo = args[0]
			}
			infos, fails, err := ops.ListWorktrees(c.Context(), w, repo)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			switch {
			case asJSON:
				doc := worktreeListJSON{Version: 1, Worktrees: infos, Errors: []repoErrorJSON{}}
				if doc.Worktrees == nil {
					doc.Worktrees = []ops.WorktreeInfo{}
				}
				for _, f := range fails {
					doc.Errors = append(doc.Errors, repoErrorJSON{Repo: f.Repo, Error: f.Err.Error()})
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(doc); err != nil {
					return err
				}
			case porcelain:
				term := "\n"
				if nul {
					term = "\x00"
				}
				for _, in := range infos {
					fmt.Fprint(out, in.Path+term)
				}
			default:
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "REPO\tDIR\tBRANCH\tFLAGS\tPATH")
				for _, in := range infos {
					dir, branch := in.Dir, in.Branch
					if dir == "" {
						dir = "-"
					}
					if branch == "" {
						branch = "(detached " + shortHead(in.Head) + ")"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", in.Repo, dir, branch, worktreeFlags(in), in.Path)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if len(fails) == 0 {
				return nil
			}
			var msgs []string
			for _, f := range fails {
				msgs = append(msgs, f.Error())
			}
			return fmt.Errorf("%d of the repositories could not be listed:\n  %s", len(fails), strings.Join(msgs, "\n  "))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&porcelain, "porcelain", false, "print one path per line (for scripts and completion)")
	cmd.Flags().BoolVarP(&nul, "null", "z", false, "with --porcelain: terminate paths with NUL instead of newline")
	return cmd
}

func shortHead(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

func worktreeFlags(in ops.WorktreeInfo) string {
	var f []string
	if in.Primary {
		f = append(f, "primary")
	}
	if in.OffDefault {
		f = append(f, "off-default")
	}
	if in.Detached {
		f = append(f, "detached")
	}
	if in.Locked {
		f = append(f, "locked")
	}
	if in.Prunable {
		f = append(f, "prunable")
	}
	if in.Outside {
		f = append(f, "outside")
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

func newWorktreeRemoveCmd(d Deps) *cobra.Command {
	var opts ops.WorktreeRemoveOptions
	var yes bool
	cmd := &cobra.Command{
		Use:     "remove [<repo>] [<branch-or-dir>]",
		Aliases: []string{"rm"},
		Short:   "Remove a worktree (and optionally its branch)",
		Long: "Remove a linked worktree, named by its branch or its directory (--branch or --dir\n" +
			"select explicitly when a name is both). origin is fetched first (unless --offline);\n" +
			"uncommitted changes, untracked files or commits only on a detached HEAD refuse the\n" +
			"removal unless --force (like git worktree remove --force). -d deletes the local\n" +
			"branch too, only if merged into its upstream or origin/<default branch>; -D deletes\n" +
			"it even if unmerged (like git branch -d/-D). The primary checkout cannot be removed\n" +
			"(use `sheaf remove`). In CLI mode the removal must be confirmed with --yes.\n\n" +
			singleArgHelp,
		Args: cobra.MaximumNArgs(2),
	}
	selector := selectorFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		sel, err := selector()
		if err != nil {
			return err
		}
		w, err := openWorkspace(c, d)
		if err != nil {
			return err
		}
		repo, target, err := repoAndTarget(d, w, args, sel, "<branch-or-dir>")
		if err != nil {
			return err
		}
		emit := eventSink(c, repo, ops.PhaseWorktree)
		opts.Confirm = func(p ops.WorktreeRemovePlan) bool {
			wt := p.Worktree.Dir
			for _, r := range p.NonBlocking() {
				emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("%v (informational, not blocking)", r)})
			}
			if opts.Force {
				for _, r := range p.Blocking() {
					emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("%v will be lost (--force)", r)})
				}
			}
			emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("will remove worktree %s [%s] at %s", p.Worktree.Dir, branchLabel(p.Worktree), p.Worktree.Path)})
			switch {
			case p.MergedInto != "":
				emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("will delete branch %s (merged into %s)", p.Worktree.Branch, p.MergedInto)})
			case p.ContainedIn != "":
				emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("will delete branch %s (contained in %s)", p.Worktree.Branch, p.ContainedIn)})
			case p.Unmerged != "":
				emit(ops.Event{Worktree: wt, Message: fmt.Sprintf("will delete unmerged branch %s: %s will be lost (-D)", p.Worktree.Branch, commitCount(p.UnmergedCommits))})
			}
			if p.BranchKept != "" {
				emit(ops.Event{Worktree: wt, Message: "warning: " + p.BranchKept})
			}
			return yes
		}
		if opts.WorkingDir, err = d.getwd(); err != nil {
			return err
		}
		msg, err := ops.RemoveWorktree(c.Context(), w, repo, target, opts, emit)
		if errors.Is(err, ops.ErrNotConfirmed) {
			return fmt.Errorf("removing worktree %s of %s needs confirmation: pass --yes (sheaf never prompts in CLI mode)", target.Name, repo)
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), msg)
		return nil
	}
	f := cmd.Flags()
	f.BoolVar(&opts.Force, "force", false, "remove the worktree even if uncommitted changes or untracked files would be lost")
	f.BoolVarP(&opts.DeleteBranch, "delete-branch", "d", false, "also delete the local branch if it is merged")
	f.BoolVarP(&opts.ForceDeleteBranch, "force-delete-branch", "D", false, "also delete the local branch even if it is not merged (its commits are lost)")
	f.BoolVar(&opts.Offline, "offline", false, "do not fetch first (the remote state may be stale)")
	f.BoolVarP(&yes, "yes", "y", false, "confirm the removal (required in CLI mode)")
	return cmd
}

func branchLabel(in ops.WorktreeInfo) string {
	if in.Branch == "" {
		return "detached"
	}
	return in.Branch
}

func commitCount(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

func newPathCmd(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path [<repo>] [<branch-or-dir>]",
		Short: "Print the path of the bundle, a repository's primary checkout or a worktree",
		Long: "Without arguments print the bundle root; with <repo> (or --repo) its primary\n" +
			"checkout; with <repo> <branch-or-dir> (or --dir/--branch) that worktree.\n\n" +
			"With one argument inside a checkout of a repository, it names a worktree of that\n" +
			"repository if there is one, else a repository; when it names both, use --dir or\n" +
			"--branch for the worktree, or --repo for the repository. Outside any checkout it\n" +
			"is a repository name.",
		Args: cobra.MaximumNArgs(2),
	}
	selector := selectorFlags(cmd)
	var repo string
	cmd.Flags().StringVar(&repo, "repo", "", "select the repository (positional arguments are then worktrees)")
	cmd.RunE = func(c *cobra.Command, args []string) error {
		sel, err := selector()
		if err != nil {
			return err
		}
		w, err := openWorkspace(c, d)
		if err != nil {
			return err
		}
		wd, err := d.getwd()
		if err != nil {
			return err
		}
		p, err := ops.ResolvePath(c.Context(), w, ops.PathQuery{Args: args, Sel: sel, Repo: repo, Cwd: wd})
		if err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), p)
		return nil
	}
	return cmd
}
