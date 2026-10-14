package cli

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// completionTimeout bounds one completion request; completions never touch
// the network, this only guards against a stuck git.
const completionTimeout = 5 * time.Second

// installCompletions attaches the dynamic completion functions to the
// commands registered on root, by command path, so the commands themselves
// do not know about completion. Completion functions never print errors and
// never use the network: when anything fails they offer no candidates. The
// bundle is the one of the --bundle flag if it is on the command line, else
// the one around the working directory.
func installCompletions(root *cobra.Command, d Deps) { _ = installCompletionsReport(root, d) }

// installCompletionsReport does the work of installCompletions and returns
// the targets (command paths, "path --flag") it did not find; tests require
// it to be empty so that a renamed command cannot silently lose completion.
func installCompletionsReport(root *cobra.Command, d Deps) []string {
	k := completer{d}
	var missing []string
	find := func(path []string) *cobra.Command {
		c, _, err := root.Find(path)
		if err != nil || c == root || c.Name() != path[len(path)-1] {
			missing = append(missing, strings.Join(path, " "))
			return nil
		}
		return c
	}
	argsOf := func(path []string, fn cobra.CompletionFunc) {
		if c := find(path); c != nil {
			c.ValidArgsFunction = fn
		}
	}
	flagOf := func(path []string, flag string, fn cobra.CompletionFunc) {
		if c := find(path); c != nil {
			if c.Flags().Lookup(flag) == nil {
				missing = append(missing, strings.Join(path, " ")+" --"+flag)
				return
			}
			_ = c.RegisterFlagCompletionFunc(flag, fn)
		}
	}

	// --bundle takes a directory.
	if root.PersistentFlags().Lookup("bundle") != nil {
		_ = root.RegisterFlagCompletionFunc("bundle", cobra.FixedCompletions(nil, cobra.ShellCompDirectiveFilterDirs))
	} else {
		missing = append(missing, "--bundle")
	}
	// Commands without positional arguments must not offer files.
	for _, p := range [][]string{{"version"}, {"status"}, {"update"}, {"fetch"}, {"sync"},
		{"completion"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}} {
		argsOf(p, cobra.NoFileCompletions)
	}
	for _, f := range []string{"repo", "as"} { // names on the hosting service, not in the bundle
		flagOf([]string{"add"}, f, cobra.NoFileCompletions)
	}

	argsOf([]string{"shell-init"}, k.positional(func(_ *cobra.Command, args []string) []string {
		if len(args) == 0 {
			return shells
		}
		return nil
	}))
	argsOf([]string{"remove"}, k.positional(k.repoOnly))
	argsOf([]string{"worktree", "list"}, k.positional(k.repoOnly))
	argsOf([]string{"worktree", "add"}, k.positional(func(c *cobra.Command, args []string) []string {
		switch len(args) {
		case 0:
			return append(k.repos(c), k.branchesToAdd(c, "")...)
		case 1:
			return k.branchesToAdd(c, args[0])
		}
		return nil
	}))
	argsOf([]string{"worktree", "remove"}, k.positional(func(c *cobra.Command, args []string) []string {
		switch len(args) {
		case 0:
			return append(k.repos(c), k.worktrees(c, "", true, kindAny)...)
		case 1:
			return k.worktrees(c, args[0], true, kindAny)
		}
		return nil
	}))
	argsOf([]string{"path"}, k.positional(func(c *cobra.Command, args []string) []string {
		switch len(args) {
		case 0:
			if repo, _ := c.Flags().GetString("repo"); repo != "" {
				return k.worktrees(c, repo, false, kindAny)
			}
			return append(k.repos(c), k.worktrees(c, "", false, kindAny)...)
		case 1:
			if repo, _ := c.Flags().GetString("repo"); repo == "" {
				return k.worktrees(c, args[0], false, kindAny)
			}
		}
		return nil
	}))

	for _, p := range [][]string{{"status"}, {"update"}, {"run"}, {"fetch"}, {"path"}} {
		flagOf(p, "repo", k.flag(func(c *cobra.Command, _ []string) []string { return k.repos(c) }))
	}
	selector := func(kind targetKind, excludePrimary bool) cobra.CompletionFunc {
		return k.flag(func(c *cobra.Command, args []string) []string {
			repo, _ := c.Flags().GetString("repo")
			if repo == "" && len(args) > 0 {
				repo = args[0]
			}
			return k.worktrees(c, repo, excludePrimary, kind)
		})
	}
	flagOf([]string{"path"}, "branch", selector(kindBranch, false))
	flagOf([]string{"path"}, "dir", selector(kindDir, false))
	flagOf([]string{"worktree", "remove"}, "branch", selector(kindBranch, true))
	flagOf([]string{"worktree", "remove"}, "dir", selector(kindDir, true))
	for _, p := range [][]string{{"add"}, {"sync"}} {
		flagOf(p, "org", k.flag(func(c *cobra.Command, _ []string) []string { return k.orgs(c) }))
	}
	return missing
}

type targetKind int

const (
	kindAny targetKind = iota
	kindBranch
	kindDir
)

type completer struct{ d Deps }

// positional wraps a candidate function into a ValidArgsFunction.
func (k completer) positional(fn func(c *cobra.Command, args []string) []string) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filterPrefix(fn(c, args), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// flag wraps a candidate function into a flag completion function.
func (k completer) flag(fn func(c *cobra.Command, args []string) []string) cobra.CompletionFunc {
	return k.positional(fn)
}

func (k completer) repoOnly(c *cobra.Command, args []string) []string {
	if len(args) == 0 {
		return k.repos(c)
	}
	return nil
}

func filterPrefix(names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func completionContext(c *cobra.Command) (context.Context, context.CancelFunc) {
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, completionTimeout)
}

func (k completer) workspace(c *cobra.Command) *ops.Workspace {
	w, err := openWorkspace(c, k.d)
	if err != nil {
		return nil
	}
	return w
}

func (k completer) repos(c *cobra.Command) []string {
	w := k.workspace(c)
	if w == nil {
		return nil
	}
	var names []string
	for _, r := range w.Bundle().Repositories {
		names = append(names, r.Name)
	}
	return names
}

func (k completer) orgs(c *cobra.Command) []string {
	w := k.workspace(c)
	if w == nil {
		return nil
	}
	var names []string
	for _, s := range w.Bundle().Sources {
		names = append(names, s.Org)
	}
	return names
}

// listing resolves repoArg (empty: inferred from the working directory) and
// lists its checkouts.
func (k completer) listing(c *cobra.Command, repoArg string) (*ops.Workspace, string, []ops.WorktreeInfo) {
	w := k.workspace(c)
	if w == nil {
		return nil, "", nil
	}
	wd, err := k.d.getwd()
	if err != nil {
		return nil, "", nil
	}
	rc, err := ops.ResolveRepo(w, repoArg, wd)
	if err != nil {
		return nil, "", nil
	}
	ctx, cancel := completionContext(c)
	defer cancel()
	infos, _, err := ops.ListWorktrees(ctx, w, rc.Name)
	if err != nil {
		return nil, "", nil
	}
	return w, rc.Name, infos
}

// worktrees returns the branch and directory names of the checkouts of the
// repository (see listing); the primary checkout is left out when
// excludePrimary is set, and so are missing (prunable) ones unless it is.
func (k completer) worktrees(c *cobra.Command, repoArg string, excludePrimary bool, kind targetKind) []string {
	_, _, infos := k.listing(c, repoArg)
	var names []string
	for _, in := range infos {
		if in.Primary && excludePrimary || in.Prunable && !excludePrimary {
			continue
		}
		if kind != kindBranch && in.Dir != "" {
			names = append(names, in.Dir)
		}
		if kind != kindDir && in.Branch != "" {
			names = append(names, in.Branch)
		}
	}
	slices.Sort(names)
	return names
}

// branchesToAdd returns the branches `worktree add` can create a worktree
// for: the local branches and origin's branches (from the refs already in
// the primary checkout, no fetch) that are not checked out anywhere, and not
// the default branch (it lives in the primary checkout).
func (k completer) branchesToAdd(c *cobra.Command, repoArg string) []string {
	w, repo, infos := k.listing(c, repoArg)
	if w == nil || len(infos) == 0 || !infos[0].Primary {
		return nil
	}
	taken := map[string]bool{}
	for _, in := range infos {
		taken[in.Branch] = true
	}
	for _, r := range w.Bundle().Repositories {
		if r.Name == repo {
			taken[r.DefaultBranch] = true
		}
	}
	ctx, cancel := completionContext(c)
	defer cancel()
	out, err := gitx.Run(ctx, infos[0].Path, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil
	}
	var names []string
	for _, ref := range strings.Split(out, "\n") {
		var b string
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			b = strings.TrimPrefix(ref, "refs/heads/")
		case strings.HasPrefix(ref, "refs/remotes/origin/"):
			b = strings.TrimPrefix(ref, "refs/remotes/origin/")
		}
		if b != "" && b != "HEAD" && !taken[b] {
			names = append(names, b)
		}
	}
	slices.Sort(names)
	return names
}
