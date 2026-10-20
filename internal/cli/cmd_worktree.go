package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jponge/sheaf/internal/gitx"
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
			"<bundle>/<repo>/<normalized branch>/ next to the primary checkout. In plain mode with\n" +
			"stdin on a terminal, add, remove and pr let git keep the terminal (ssh may prompt for\n" +
			"a passphrase or a host key), whatever -j.\n\n" +
			singleArgHelp,
	}
	wt.AddCommand(newWorktreeAddCmd(d), newWorktreeListCmd(d), newWorktreeRemoveCmd(d), newWorktreePRCmd(d))
	root.AddCommand(wt, newPathCmd(d))
}

// singleRepoInteractiveGit is interactiveGit for the commands that work on
// a single repository (remove, wt add, wt rm, wt pr): they run one git
// process at a time, so interactive git mode does not depend on -j (prompts
// cannot interleave); it still needs CLI mode and standard input on a
// terminal.
func (d Deps) singleRepoInteractiveGit(c *cobra.Command) bool {
	return gitInteractive(false, 1, d.stdinIsTerminal(c.InOrStdin()))
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

// spinFetches returns emit, which also runs each fetch it reports (an event
// of the fetch phase, e.g. "fetching origin") as a phase of the working
// indicator until the next event, and the function ending the last one.
// Not when git is interactive: it may prompt on the terminal, where the
// indicator would overwrite the prompt.
func (d Deps) spinFetches(emit func(ops.Event), interactive bool) (func(ops.Event), func()) {
	stop := func() {}
	if interactive || d.spin == nil {
		return emit, stop
	}
	return func(e ops.Event) {
		stop()
		emit(e)
		if e.Phase == ops.PhaseFetch {
			stop = d.spin.start(e.Message)
		}
	}, func() { stop() }
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
	if target == nil && len(args) == 0 {
		return "", ops.WorktreeTarget{}, fmt.Errorf("missing %s for repository %s (inferred from the working directory)", what, repo)
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
	var tag string
	cmd := &cobra.Command{
		Use:   "add [<repo>] <branch> | add [<repo>] --tag <tag>",
		Short: "Create a worktree for a branch or a tag and print its path",
		Long: "Create <bundle>/<repo>/<normalized branch>/ for branch: the local branch if it\n" +
			"exists, else a local branch tracking origin/<branch>, else a new branch from --base\n" +
			"(default origin/<default branch>, upstream/<default branch> for a clone of your\n" +
			"fork). origin is fetched first unless --no-fetch (or its alias --offline).\n" +
			"A base that does not exist is reported with the closest remote branches and tags.\n" +
			"A name that is only a tag (no local or origin branch; origin's tags are fetched\n" +
			"for this check unless --no-fetch) is refused: use --tag. So a new branch named\n" +
			"exactly like an existing tag cannot be created with wt add (use git directly).\n\n" +
			"With --tag, create <bundle>/<repo>/<normalized tag>/ with a detached HEAD at the\n" +
			"tag's commit, pinned to the tag: update never moves it, prune never removes it.\n" +
			"origin's tags are fetched first unless --no-fetch. Local tags are never deleted\n" +
			"nor overwritten: a local tag that differs from origin's is kept, with a warning\n" +
			"(and used, if it is the requested one). The only positional argument is then\n" +
			"the repository.\n\n" +
			"For a repository cloned from your fork, upstream (the repository) and origin (your\n" +
			"fork) are both fetched, existing branches are looked up on origin then upstream,\n" +
			"new branches start from upstream/<default branch> and tags come from upstream.\n\n" +
			"The path is printed on stdout (cd \"$(sheaf wt add feat/x)\"); progress goes to stderr.\n\n" +
			singleArgHelp,
		Args: func(c *cobra.Command, args []string) error {
			if c.Flags().Changed("tag") {
				if len(args) > 1 {
					return fmt.Errorf("with --tag, expected at most 1 argument (the repository), got %d", len(args))
				}
				return nil
			}
			return cobra.RangeArgs(1, 2)(c, args)
		},
		RunE: func(c *cobra.Command, args []string) error {
			w, err := openWorkspace(c, d)
			if err != nil {
				return err
			}
			if c.Flags().Changed("tag") {
				repo, target, err := repoAndTarget(d, w, args, &ops.WorktreeTarget{Name: tag}, "--tag")
				if err != nil {
					return err
				}
				opts.Interactive = d.singleRepoInteractiveGit(c)
				emit, stop := d.spinFetches(eventSink(c, repo, ops.PhaseWorktree), opts.Interactive)
				res, err := ops.AddTagWorktree(c.Context(), w, repo, target.Name, opts, emit)
				stop()
				if err != nil {
					return err
				}
				fmt.Fprintln(c.OutOrStdout(), res.Path)
				return nil
			}
			repo, branch, err := repoAndTarget(d, w, args, nil, "<branch>")
			if err != nil {
				return err
			}
			opts.Interactive = d.singleRepoInteractiveGit(c)
			emit, stop := d.spinFetches(eventSink(c, repo, ops.PhaseWorktree), opts.Interactive)
			res, err := ops.AddWorktree(c.Context(), w, repo, branch.Name, opts, emit)
			stop()
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), res.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Base, "base", "", "start point of a new branch (default: origin/<default branch>, or upstream/<default branch> for a clone of your fork)")
	cmd.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not fetch the repository's remotes first")
	cmd.Flags().BoolVar(&opts.NoFetch, "offline", false, "same as --no-fetch")
	cmd.Flags().StringVar(&tag, "tag", "", "create a detached worktree pinned at this tag instead of a branch worktree")
	return cmd
}

// worktreeListJSON is the --json document of `worktree list`.
type worktreeListJSON struct {
	Version   int                `json:"version"`
	Worktrees []ops.WorktreeInfo `json:"worktrees"`
	Errors    []repoErrorJSON    `json:"errors"`
}

type repoErrorJSON struct {
	Repo string `json:"repo"`
	// Path is set for a worktree that cannot be used (wt list).
	Path  string `json:"path,omitempty"`
	Error string `json:"error"`
}

func newWorktreeListCmd(d Deps) *cobra.Command {
	var asJSON, porcelain, nul bool
	cmd := &cobra.Command{
		Use:     "list [<repo>]",
		Aliases: []string{"ls"},
		Short:   "List the checkouts and worktrees of the bundle (or of one repository)",
		Long: "List every checkout of the bundle, or of <repo>: repository, directory, branch\n" +
			"(\"(tag <tag>)\" for a worktree pinned at a tag, \"(tag <tag>, moved to <commit>)\"\n" +
			"once its HEAD left the tag's commit), flags (primary, off-default,\n" +
			"detached, tag, locked, prunable, outside) and path. Ordered\n" +
			"by repository, primary first, then by directory (outside worktrees last). --porcelain\n" +
			"prints the paths only (-z: NUL-terminated); --json prints {\"version\": 1, \"worktrees\":\n" +
			"[...], \"errors\": [{\"repo\", \"path\" (for a worktree), \"error\"}]}. Repositories that\n" +
			"cannot be listed, and worktrees that cannot be used (another repository's, or without\n" +
			".git; left out of the listing), are reported and make the exit status 1.",
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
					doc.Errors = append(doc.Errors, repoErrorJSON{Repo: f.Repo, Path: f.Path, Error: f.Err.Error()})
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
					switch {
					case branch == "" && in.Tag != "":
						moved := ""
						if in.TagMoved {
							moved = in.Head
						}
						branch = tagLabel(in.Tag, moved)
					case branch == "":
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
			repos, wts := 0, 0
			for _, f := range fails {
				msgs = append(msgs, f.Error())
				if f.Path != "" {
					wts++
				} else {
					repos++
				}
			}
			var what []string
			if repos > 0 {
				what = append(what, countOf(repos, "repository", "repositories"))
			}
			if wts > 0 {
				what = append(what, countOf(wts, "worktree", "worktrees"))
			}
			return fmt.Errorf("%s could not be listed:\n  %s", strings.Join(what, " and "), strings.Join(msgs, "\n  "))
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
	if in.Tag != "" {
		f = append(f, "tag")
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
		Long: "Remove a linked worktree, named by its branch (or tag, for a worktree pinned at a\n" +
			"tag) or its directory (--branch or --dir select explicitly when a name is both).\n" +
			"origin is fetched first (unless --offline);\n" +
			"uncommitted changes, untracked files or commits only on a detached HEAD refuse the\n" +
			"removal unless --force (like git worktree remove --force). -d deletes the local\n" +
			"branch too, only if merged into its own upstream or into origin/<default branch>\n" +
			"(upstream/<default branch> for a clone of your fork), or contained in another ref;\n" +
			"-D deletes it even if unmerged (like git branch -d/-D); neither applies to a tag\n" +
			"worktree (the tag is always kept). For a clone of your fork both remotes are\n" +
			"fetched; when your fork is gone, or with --offline, its remote-tracking refs are\n" +
			"not trusted (not pushed, not a merge target). The primary checkout cannot be removed\n" +
			"(use `sheaf remove`). In CLI mode the removal must be confirmed with --yes; in TUI\n" +
			"mode a dialog shows the plan and asks for confirmation, unless --yes is given.\n\n" +
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
		if d.tuiMode(c) {
			if opts.WorkingDir, err = d.getwd(); err != nil {
				return err
			}
			return tuiWorktreeRemove(c, d, w, repo, target, opts, yes)
		}
		emit := eventSink(c, repo, ops.PhaseWorktree)
		opts.Confirm = func(p ops.WorktreeRemovePlan) bool {
			for _, l := range worktreeRemovePlanLines(p, opts.Force) {
				emit(ops.Event{Worktree: p.Worktree.Dir, Message: l})
			}
			return yes
		}
		if opts.WorkingDir, err = d.getwd(); err != nil {
			return err
		}
		opts.Interactive = d.singleRepoInteractiveGit(c)
		msg, err := ops.RemoveWorktree(plainHintContext(c, yes), w, repo, target, opts, emit)
		if errors.Is(err, ops.ErrNotConfirmed) {
			return fmt.Errorf("removing worktree %s of %s needs confirmation (sheaf never prompts in CLI mode); run: %s", target.Name, repo, worktreeRemoveCommand(c.Context(), w.Root, repo, target, opts))
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

// worktreeRemoveCommand returns the command line of wt rm for target of
// repo with opts, confirmed with --yes, in the bundle of resolved root root
// (see ops.YesCommandLine).
func worktreeRemoveCommand(ctx context.Context, root, repo string, target ops.WorktreeTarget, opts ops.WorktreeRemoveOptions) string {
	args := []string{"wt", "rm"}
	if opts.Force {
		args = append(args, "--force")
	}
	switch {
	case opts.ForceDeleteBranch:
		args = append(args, "-D")
	case opts.DeleteBranch:
		args = append(args, "-d")
	}
	if opts.Offline {
		args = append(args, "--offline")
	}
	switch target.Kind {
	case ops.TargetDir:
		args = append(args, "--dir", target.Name)
	case ops.TargetBranch:
		args = append(args, "--branch", target.Name)
	}
	args = append(args, "--yes", "--", repo)
	if target.Kind == ops.TargetAny && target.Name != "" {
		args = append(args, target.Name)
	}
	return ops.YesCommandLine(ctx, root, args...)
}

// worktreeRemovePlanLines describes a worktree removal plan: risks, what is
// removed and what happens to the branch. The unpushed commits of the
// repository's local branches are listed only when a branch is deleted
// (with the branches carrying them, see ops.WorktreeRemovePlan): they
// survive the removal of a worktree whose branch is kept.
func worktreeRemovePlanLines(p ops.WorktreeRemovePlan, force bool) []string {
	var lines []string
	deleting := (p.MergedInto != "" || p.ContainedIn != "" || p.Unmerged != "") && p.BranchKept == ""
	for _, r := range p.NonBlocking() {
		if r.Kind == gitx.RiskUnpushedBranchCommits && !deleting {
			continue
		}
		lines = append(lines, fmt.Sprintf("%v (informational, not blocking)", r))
	}
	if force {
		for _, r := range p.Blocking() {
			lines = append(lines, fmt.Sprintf("%v will be lost (--force)", r))
		}
	}
	lines = append(lines, worktreeRemoveLine(p))
	switch {
	case p.MergedInto != "":
		lines = append(lines, fmt.Sprintf("will delete branch %s (merged into %s)", p.Worktree.Branch, p.MergedInto))
	case p.ContainedIn != "":
		lines = append(lines, fmt.Sprintf("will delete branch %s (contained in %s)", p.Worktree.Branch, p.ContainedIn))
	case p.Unmerged != "":
		lines = append(lines, fmt.Sprintf("will delete unmerged branch %s: %s will be lost (-D)", p.Worktree.Branch, commitCount(p.UnmergedCommits)))
	case p.BranchKept == "" && p.Worktree.Branch != "":
		lines = append(lines, fmt.Sprintf("branch %s is kept", p.Worktree.Branch))
	}
	if p.BranchKept != "" {
		lines = append(lines, "warning: "+p.BranchKept)
	}
	return lines
}

// worktreeRemoveLine is the line of worktreeRemovePlanLines saying which
// worktree is removed.
func worktreeRemoveLine(p ops.WorktreeRemovePlan) string {
	return fmt.Sprintf("will remove worktree %s [%s] at %s", p.Worktree.Dir, branchLabel(p.Worktree), p.Worktree.Path)
}

func branchLabel(in ops.WorktreeInfo) string {
	if in.Branch == "" && in.Tag != "" {
		return "tag " + in.Tag
	}
	if in.Branch == "" {
		return "detached"
	}
	return in.Branch
}

func commitCount(n int) string { return countOf(n, "commit", "commits") }

// countOf is "<n> <one>" or "<n> <many>".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func newPathCmd(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path [<repo>] [<branch-or-dir>]",
		Short: "Print the path of the bundle, a repository's primary checkout or a worktree",
		Long: "Without arguments print the bundle root; with <repo> (or --repo) its primary\n" +
			"checkout; with <repo> <branch-or-dir> (or --dir/--branch) that worktree. A worktree\n" +
			"pinned at a tag is also named by its tag.\n\n" +
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

func newWorktreePRCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "pr [<repo>] <number>",
		Short: "Check out a GitHub pull request in a worktree and print its path",
		Long: "Resolve the pull request with gh (GitHub repositories only) and fetch origin\n" +
			"(for a clone of your fork: resolved against upstream, heads of your fork tracked\n" +
			"from origin, other heads and pull refs from upstream).\n" +
			"A pull request from a branch of the repository is checked out like `sheaf wt add\n" +
			"<head branch>`. A pull request from a fork (or whose head branch was deleted) is\n" +
			"fetched from refs/pull/<number>/head into the local branch pr/<number>, at\n" +
			"<repo>/pr-<number>/, tracking that ref (git pull works there; `sheaf update` skips\n" +
			"it). Running the command again for such a pull request fast-forwards its worktree\n" +
			"to the current head: it refuses uncommitted changes and force-pushed heads. The head\n" +
			"is recorded as refs/sheaf/pull/<number>: `sheaf remove` counts its commits as pushed.\n" +
			"The number may be written #<number>. The path is printed on stdout.\n\n" +
			"With one argument the repository is inferred from the working directory.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			numArg := args[len(args)-1]
			n, err := parsePRNumber(numArg)
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
			repoArg := ""
			if len(args) == 2 {
				repoArg = args[0]
			}
			rc, err := ops.ResolveRepo(w, repoArg, wd)
			if err != nil {
				return err
			}
			emit := eventSink(c, rc.Name, ops.PhasePR)
			res, err := ops.AddPRWorktree(c.Context(), w, d.hosting(), rc.Name, n, ops.PRWorktreeOptions{Interactive: d.singleRepoInteractiveGit(c)}, emit)
			if err != nil {
				return err
			}
			emit(ops.Event{Worktree: res.Dir, Message: res.Message})
			fmt.Fprintln(c.OutOrStdout(), res.Path)
			return nil
		},
	}
}

// parsePRNumber accepts a positive decimal number without sign or leading
// zero, optionally prefixed with '#'.
func parsePRNumber(arg string) (int, error) {
	digits := strings.TrimPrefix(arg, "#")
	bad := digits == "" || digits[0] < '1' || digits[0] > '9' ||
		strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
	n, err := strconv.Atoi(digits)
	if bad || err != nil {
		return 0, fmt.Errorf("invalid pull request number %q", arg)
	}
	return n, nil
}

// tagLabel is the branch column of a worktree pinned at tag: "(tag X)", or
// "(tag X, moved to <short>)" when its HEAD movedTo another commit.
func tagLabel(tag, movedTo string) string {
	if movedTo != "" {
		return "(tag " + tag + ", moved to " + shortHead(movedTo) + ")"
	}
	return "(tag " + tag + ")"
}
