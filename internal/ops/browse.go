package ops

import (
	"context"
	"fmt"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// BrowseOptions configures ResolveBrowse.
type BrowseOptions struct {
	// Fork opens the user's fork (the remote origin of a clone of a fork)
	// rather than the repository.
	Fork bool
	// Branch opens the tree of the checkout's branch.
	Branch bool
}

// BrowseTarget is the web page `sheaf browse` opens: Page of Slug.
type BrowseTarget struct {
	Repo string // the repository of the bundle
	Slug string // "owner/name" on GitHub
	Page hosting.Page
}

// String describes t in messages: "o/r", "pull request #7 of o/r" or
// "branch feat of o/r".
func (t BrowseTarget) String() string {
	switch {
	case t.Page.PR > 0:
		return fmt.Sprintf("pull request #%d of %s", t.Page.PR, t.Slug)
	case t.Page.Branch != "":
		return "branch " + t.Page.Branch + " of " + t.Slug
	}
	return t.Slug
}

// ResolveBrowse returns the page to open for the repository named arg, or,
// when arg is empty, for the checkout containing cwd (inside
// <root>/<repo>/<dir>; directly in <root>/<repo>, its primary checkout).
// A named repository stands for its primary checkout.
//
// The repository must be hosted on GitHub (see OnGitHub). The page is on
// the repository (for a clone of a fork, the reference repository, the
// remote upstream), or on the user's fork with opts.Fork. It is the tree of
// the checkout's branch with opts.Branch (refused on a detached checkout);
// otherwise, on the repository, the pull request whose head the checkout's
// branch tracks (branch.<branch>.merge = refs/pull/<n>/head, as `wt pr`
// sets up for a pull request from a fork); otherwise the home page. A pull
// request checked out from a branch of the repository itself is an
// ordinary branch: nothing records its number, and its home page opens.
func ResolveBrowse(ctx context.Context, w *Workspace, arg, cwd string, opts BrowseOptions) (BrowseTarget, error) {
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
	if err != nil {
		return BrowseTarget{}, err
	}
	if wt.Prunable {
		return BrowseTarget{}, fmt.Errorf("the directory of worktree %s is missing (%s)", wt.Path, wt.PrunableReason)
	}
	switch {
	case opts.Branch && wt.Branch == "":
		return BrowseTarget{}, fmt.Errorf("%s is not on a branch", wt.Path)
	case opts.Branch:
		t.Page.Branch = wt.Branch
	case !opts.Fork && wt.Branch != "":
		if n, ok := gitx.PullRequestRef(branchMerge(ctx, wt.Path, wt.Branch)); ok {
			t.Page.PR = n
		}
	}
	return t, nil
}
