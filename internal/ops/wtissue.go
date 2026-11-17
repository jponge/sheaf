package ops

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// PhaseIssue is the phase of the events of AddIssueWorktree.
const PhaseIssue = "issue"

// issueSlugMax is the maximum length of the slug of IssueBranch.
const issueSlugMax = 50

// IssueBranch is the branch of a new worktree for issue number with title:
// "issue/<number>-<slug>" (worktree directory "issue-<number>-<slug>"), or
// "issue/<number>" when the slug is empty. The slug is the title with ASCII
// letters lowercased and every run of other characters than ASCII letters
// and digits replaced with one '-', without leading or trailing '-'. A slug
// longer than issueSlugMax is cut there, and back to the last '-' before
// the cut when the cut splits a word (and the word has a '-' before it).
func IssueBranch(number int, title string) string {
	var b strings.Builder
	sep := false
	for i := 0; i < len(title); i++ {
		c := title[i]
		switch {
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		default:
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte('-')
		}
		sep = false
		b.WriteByte(c)
	}
	slug := b.String()
	if len(slug) > issueSlugMax {
		cut := slug[:issueSlugMax]
		if slug[issueSlugMax] != '-' {
			if i := strings.LastIndexByte(cut, '-'); i >= 0 {
				cut = cut[:i]
			}
		}
		slug = strings.Trim(cut, "-")
	}
	branch := "issue/" + strconv.Itoa(number)
	if slug != "" {
		branch += "-" + slug
	}
	return branch
}

// IssueIsPullRequestError is the refusal of AddIssueWorktree for a number
// that is a pull request of the repository (GitHub numbers issues and pull
// requests together).
type IssueIsPullRequestError struct {
	Repo   string
	Slug   string
	Number int
	// Command is the command line checking the pull request out (see
	// CommandLine).
	Command string
}

func (e *IssueIsPullRequestError) Error() string {
	return fmt.Sprintf("#%d is a pull request of %s, not an issue\nto check it out in a worktree, run: %s", e.Number, e.Slug, e.Command)
}

// AddIssueWorktree creates a worktree for issue number of repo (a GitHub
// repository of the bundle, see checkGitHub) with AddWorktree, on the
// branch of the issue.
//
// After a fetch (unless opts.NoFetch, as AddWorktree), the branch of the
// issue is looked for among the local branches and the branches of the
// remotes AddWorktree searches (RemotesOf(r).BranchSearch()): a branch
// named "issue/<number>" or starting with "issue/<number>-". One name (a
// local branch, a remote one, or both) is used without asking the hosting
// service, so that a retitled issue keeps its branch; several names are
// refused. Without one, the issue is read with h.Issue from the repository
// (the reference repository r.Slug, not the user's fork), a pull request is
// refused (IssueIsPullRequestError), and the branch IssueBranch(number,
// title) is created from opts.Base (default: the default branch). A closed
// issue is accepted, with a warning. On every refusal, nothing is created.
func AddIssueWorktree(ctx context.Context, w *Workspace, h hosting.Hosting, repo string, number int, opts WorktreeAddOptions, emit func(Event)) (WorktreeAddResult, error) {
	ctx = withCommandCache(ctx)
	r, err := repoNamed(w.Bundle(), repo)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	alias, err := checkGitHub(r, "worktrees for issues can only be added")
	if err != nil {
		return WorktreeAddResult{}, err
	}
	if number <= 0 {
		return WorktreeAddResult{}, fmt.Errorf("invalid issue number %d", number)
	}
	report := func(phase, msg string) {
		emit(Event{Repo: r.Name, Phase: phase, Kind: KindProgress, Message: msg})
	}
	primary, _, prob := checkPrimary(ctx, w.Root, r)
	if prob != nil {
		return WorktreeAddResult{}, prob
	}
	rs := RemotesOf(r)
	if !opts.NoFetch {
		report(PhaseFetch, "fetching "+rs.String())
		if err := fetchRemotes(ctx, gitx.Net{Interactive: opts.Interactive}, primary, rs); err != nil {
			if ctx.Err() != nil {
				return WorktreeAddResult{}, ctx.Err()
			}
			// Without a branch for the issue, --no-fetch still asks the
			// hosting service: "without fetching", not "from the local state".
			return WorktreeAddResult{}, fmt.Errorf("fetching %s failed: %w\nto create the worktree without fetching, run: %s", r.Name, err, issueNoFetchCommand(ctx, r.Name, number, opts))
		}
	}
	branches, err := issueBranches(ctx, primary, number, rs.BranchSearch())
	if err != nil {
		return WorktreeAddResult{}, err
	}
	var branch string
	var warnings []string
	switch len(branches) {
	case 0:
		if alias {
			report(PhaseIssue, fmt.Sprintf("treating host %s as an alias of github.com", r.HostName()))
		}
		report(PhaseIssue, fmt.Sprintf("resolving issue #%d of %s", number, r.Slug))
		owner, name, _ := strings.Cut(r.Slug, "/")
		is, err := h.Issue(ctx, owner, name, number)
		if err != nil {
			return WorktreeAddResult{}, fmt.Errorf("issue #%d of %s: %w", number, r.Slug, err)
		}
		if is.PullRequest {
			return WorktreeAddResult{}, &IssueIsPullRequestError{Repo: r.Name, Slug: r.Slug, Number: number,
				Command: CommandLine(ctx, "wt", "pr", "--", r.Name, strconv.Itoa(number))}
		}
		if strings.EqualFold(is.State, "CLOSED") {
			warnings = append(warnings, fmt.Sprintf("issue #%d of %s is closed", number, r.Slug))
			report(PhaseIssue, "warning: "+warnings[0])
		}
		branch = IssueBranch(number, is.Title)
	case 1:
		branch = branches[0]
		report(PhaseIssue, fmt.Sprintf("using existing branch %s for issue #%d", branch, number))
	default:
		args := []string{"wt", "add"}
		if opts.NoFetch {
			args = append(args, "--no-fetch")
		}
		return WorktreeAddResult{}, fmt.Errorf("%s has several branches for issue #%d: %s\nto add a worktree for one of them, run: %s",
			r.Name, number, strings.Join(branches, ", "), CommandLine(ctx, append(args, "--", r.Name, branches[0])...))
	}
	// Fetched above: AddWorktree does not fetch again, so it does not fetch
	// the tags either before creating a new branch (they only serve its
	// check that the name is not only a tag's, see checkNotOnlyATag, which
	// an issue/<n>-<slug> name is unlikely to be).
	opts.NoFetch, opts.fetched = true, !opts.NoFetch
	res, err := AddWorktree(ctx, w, r.Name, branch, opts, emit)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	res.Warnings = append(warnings, res.Warnings...)
	return res, nil
}

// issueNoFetchCommand returns the command line of wt add --issue for issue
// number of repo with opts and --no-fetch (see CommandLine).
func issueNoFetchCommand(ctx context.Context, repo string, number int, opts WorktreeAddOptions) string {
	args := []string{"wt", "add", "--no-fetch"}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	return CommandLine(ctx, append(args, "--issue="+strconv.Itoa(number), "--", repo)...)
}

// issueBranches returns the names of the local branches of the repository
// of dir and of the branches of remotes that are branches of issue number:
// "issue/<number>", or starting with "issue/<number>-" (not
// "issue/<number><digit>"), sorted, without duplicates.
func issueBranches(ctx context.Context, dir string, number int, remotes []string) ([]string, error) {
	args := []string{"for-each-ref", "--format=%(refname)", "refs/heads/issue/"}
	prefixes := []string{"refs/heads/"}
	for _, rem := range remotes {
		args = append(args, "refs/remotes/"+rem+"/issue/")
		prefixes = append(prefixes, "refs/remotes/"+rem+"/")
	}
	out, err := gitx.Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	exact := "issue/" + strconv.Itoa(number)
	var names []string
	for _, ref := range strings.Split(out, "\n") {
		for _, p := range prefixes {
			if name, ok := strings.CutPrefix(ref, p); ok && (name == exact || strings.HasPrefix(name, exact+"-")) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}
