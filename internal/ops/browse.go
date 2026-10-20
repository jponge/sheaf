package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// BrowseOptions configures ResolveBrowse.
type BrowseOptions struct {
	// Fork opens the user's fork (the remote origin of a clone of a fork)
	// rather than the repository.
	Fork bool
	// Branch opens the tree of the upstream branch of the checkout's
	// branch, on the repository of its remote.
	Branch bool
}

// BrowseTarget is the web page `sheaf browse` opens: Page of Slug.
type BrowseTarget struct {
	Repo string // the repository of the bundle
	Slug string // "owner/name" on GitHub
	Page hosting.Page
	// NoPullRequest is the head whose pull request was looked up in vain
	// (see Browse): the home page of Slug opened instead.
	NoPullRequest string
}

// String describes t in messages: "o/r", "pull request #7 of o/r", "the
// pull request of branch feat of o/r", "branch feat of o/r" or "o/r (no
// pull request for branch feat)".
func (t BrowseTarget) String() string {
	switch {
	case t.Page.PR > 0:
		return fmt.Sprintf("pull request #%d of %s", t.Page.PR, t.Slug)
	case t.Page.Head != "":
		return "the pull request of branch " + t.Page.Head + " of " + t.Slug
	case t.Page.Branch != "":
		return "branch " + t.Page.Branch + " of " + t.Slug
	case t.NoPullRequest != "":
		return t.Slug + " (no pull request for branch " + t.NoPullRequest + ")"
	}
	return t.Slug
}

// Browse opens the page ResolveBrowse returns with h. When gh finds no
// pull request for the head of the page (hosting.ErrNotFound), the home
// page of the repository opens instead. It returns what was opened.
func Browse(ctx context.Context, w *Workspace, h hosting.Hosting, arg, cwd string, opts BrowseOptions) (BrowseTarget, error) {
	t, err := ResolveBrowse(ctx, w, arg, cwd, opts)
	if err != nil {
		return BrowseTarget{}, err
	}
	owner, name, _ := strings.Cut(t.Slug, "/")
	err = h.Browse(ctx, owner, name, t.Page)
	if head := t.Page.Head; head != "" && errors.Is(err, hosting.ErrNotFound) && ctx.Err() == nil {
		t.Page, t.NoPullRequest = hosting.Page{}, head
		err = h.Browse(ctx, owner, name, t.Page)
	}
	if err != nil {
		return BrowseTarget{}, fmt.Errorf("opening %s: %w", t, err)
	}
	return t, nil
}

// ResolveBrowse returns the page to open for the repository named arg, or,
// when arg is empty, for the checkout containing cwd (inside
// <root>/<repo>/<dir>; directly in <root>/<repo>, its primary checkout).
// A named repository stands for its primary checkout.
//
// The repository must be hosted on GitHub (see OnGitHub). The page is:
//
//   - with opts.Branch, the tree of the upstream of the checkout's branch
//     (branch.<branch>.merge without refs/heads/) on the repository of its
//     remote (branch.<branch>.remote: the repository, or the user's fork);
//     refused for a detached checkout, a branch without upstream, one
//     tracking a pull request ref and one tracking another remote, and
//     with opts.Fork (the remote decides);
//   - with opts.Fork, the home page of the user's fork;
//   - the pull request n when the branch tracks refs/pull/<n>/head of the
//     repository's remote (as `wt pr` sets up for a pull request from a
//     fork);
//   - when the branch tracks a branch of the repository or of the fork,
//     other than the repository's default branch, the pull request with
//     that head ("<branch>", or "<fork owner>:<branch>"), looked up by gh
//     (see Browse); not for a branch gh would read as a number;
//   - otherwise the home page of the repository (for a clone of a fork,
//     the reference repository, the remote upstream).
func ResolveBrowse(ctx context.Context, w *Workspace, arg, cwd string, opts BrowseOptions) (BrowseTarget, error) {
	if opts.Fork && opts.Branch {
		return BrowseTarget{}, errors.New("--fork and --branch cannot be combined: --branch opens the branch on the repository it is pushed to")
	}
	ctx = withCommandCache(ctx)
	rc, err := ResolveRepo(w, arg, cwd)
	if err != nil {
		return BrowseTarget{}, err
	}
	r, _ := lookupRepo(w.Bundle(), rc.Name)
	if !OnGitHub(r) {
		return BrowseTarget{}, fmt.Errorf("%s is not a GitHub repository (hosted on %s)", r.Name, r.HostName())
	}
	t := BrowseTarget{Repo: r.Name, Slug: r.Slug}
	if opts.Fork {
		if !r.Forked() {
			return BrowseTarget{}, fmt.Errorf("%s has no fork recorded in the bundle (see `sheaf adopt --recheck`)", r.Name)
		}
		t.Slug = r.Fork.Slug
	}
	var target *WorktreeTarget
	if arg == "" && rc.Worktree != "" {
		target = &WorktreeTarget{Name: rc.Worktree, Kind: TargetDir}
	}
	wt, err := selectCheckout(ctx, w, r.Name, target)
	if errors.Is(err, errNoWorktree) {
		return BrowseTarget{}, fmt.Errorf("%s is not inside a checkout of %s", cwd, r.Name)
	}
	if err != nil {
		return BrowseTarget{}, err
	}
	if wt.Prunable {
		return BrowseTarget{}, fmt.Errorf("the directory of worktree %s is missing (%s)", wt.Path, wt.PrunableReason)
	}
	if opts.Branch && wt.Branch == "" {
		return BrowseTarget{}, fmt.Errorf("%s is not on a branch", wt.Path)
	}
	if opts.Fork || wt.Branch == "" {
		return t, nil
	}
	rs := RemotesOf(r)
	remote, merge := branchRemote(ctx, wt.Path, wt.Branch), branchMerge(ctx, wt.Path, wt.Branch)
	slugs := map[string]string{rs.Reference: r.Slug}
	if rs.Forked() {
		slugs[rs.Fork] = r.Fork.Slug
	}
	pr, isPR := gitx.PullRequestRef(merge)
	isPR = isPR && merge == "refs/pull/"+strconv.Itoa(pr)+"/head"
	upstream, isBranch := strings.CutPrefix(merge, "refs/heads/")
	slug, known := slugs[remote]
	if opts.Branch {
		pushRemote := rs.Reference
		if rs.Forked() {
			pushRemote = rs.Fork
		}
		switch {
		case isPR:
			return BrowseTarget{}, fmt.Errorf("%s tracks pull request #%d, not a branch: run sheaf browse without --branch to open it", wt.Branch, pr)
		case !isBranch || remote == "" || remote == ".":
			return BrowseTarget{}, fmt.Errorf("%s has no upstream branch: push it first (git push -u %s %s)", wt.Branch, pushRemote, wt.Branch)
		case !known:
			return BrowseTarget{}, fmt.Errorf("%s tracks %s/%s: %s is not a remote of %s known to sheaf (%s)", wt.Branch, remote, upstream, remote, r.Name, strings.Join(rs.All(), ", "))
		}
		t.Slug, t.Page.Branch = slug, upstream
		return t, nil
	}
	switch {
	case isPR && remote == rs.Reference:
		t.Page.PR = pr
	case isBranch && known && remote == rs.Reference:
		if _, err := strconv.Atoi(strings.TrimPrefix(upstream, "#")); err != nil && upstream != r.DefaultBranch {
			t.Page.Head = upstream
		}
	case isBranch && known: // the fork
		owner, _, _ := strings.Cut(slug, "/")
		t.Page.Head = owner + ":" + upstream
	}
	return t, nil
}
