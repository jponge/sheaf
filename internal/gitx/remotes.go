package gitx

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Remotes names the remotes of a repository: the reference repository (the
// one sheaf.yaml identifies, whose default branch is followed) and, when the
// user works through a fork, the fork.
type Remotes struct {
	// Reference is the remote of the reference repository: "origin"
	// without a fork, "upstream" with one.
	Reference string
	// Fork is the remote of the user's fork ("origin"), empty without one.
	Fork string
}

var (
	// OriginRemotes is a single remote "origin", the reference repository.
	OriginRemotes = Remotes{Reference: "origin"}
	// ForkRemotes is the fork layout: "origin" is the user's fork,
	// "upstream" the reference repository.
	ForkRemotes = Remotes{Reference: "upstream", Fork: "origin"}
)

// Forked reports whether a fork remote is configured.
func (r Remotes) Forked() bool { return r.Fork != "" }

// All returns the remotes to fetch, reference first.
func (r Remotes) All() []string {
	if r.Forked() {
		return []string{r.Reference, r.Fork}
	}
	return []string{r.Reference}
}

// BranchSearch returns the remotes in which an existing branch is looked
// for, fork first (feature branches live there), then the reference.
func (r Remotes) BranchSearch() []string {
	if r.Forked() {
		return []string{r.Fork, r.Reference}
	}
	return []string{r.Reference}
}

// String describes the remotes, e.g. "origin" or "upstream and origin".
func (r Remotes) String() string { return strings.Join(r.All(), " and ") }

// CloneFork clones the reference repository upstreamURL into dest as the
// remote "upstream" with branch checked out (tracking upstream/<branch>),
// then adds the fork forkURL as the remote "origin" and fetches it. New
// branches thus push to origin by default (git's default remote) while the
// default branch follows the reference repository. On a failure after the
// clone, dest is left in place: callers remove it.
func (n Net) CloneFork(ctx context.Context, upstreamURL, forkURL, dest, branch string) error {
	if err := n.clone(ctx, upstreamURL, dest, branch, ForkRemotes.Reference); err != nil {
		return err
	}
	if _, err := Run(ctx, dest, "remote", "add", "--", ForkRemotes.Fork, forkURL); err != nil {
		return fmt.Errorf("adding the fork remote %s: %w", ForkRemotes.Fork, err)
	}
	if err := n.FetchRemote(ctx, dest, ForkRemotes.Fork); err != nil {
		return fmt.Errorf("fetching the fork %s: %w", forkURL, err)
	}
	return nil
}

// FetchRemote runs `git fetch --quiet --prune <remote>` in dir: it updates
// the remote-tracking refs of remote and deletes those whose remote branch no
// longer exists. Branches tracking another remote are not refreshed and
// FastForward sees them as of their last fetch.
//
// Remote-tracking refs are shared by all worktrees of a repository, and two
// concurrent fetches into the same repository fail with "cannot lock ref".
// Callers must therefore serialize fetches per repository (group worktrees by
// CommonDir) and fetch once per repository, then call FastForward on each
// worktree, which may run in parallel. FetchRemote is cancellable at any
// point through ctx; a fetch killed with SIGKILL may leave a ref lock behind
// (see StaleLockPath). With n.Interactive, git keeps the caller's
// controlling terminal.
func (n Net) FetchRemote(ctx context.Context, dir, remote string) error {
	_, err := run(ctx, n.opts(), dir, "fetch", "--quiet", "--prune", remote)
	return err
}

// DefaultBranchOf returns the short name of the default branch of remote
// (e.g. "main"). It reads the local refs/remotes/<remote>/HEAD symbolic ref
// and, when that is missing, asks the remote with `git ls-remote --symref
// <remote> HEAD` (network access; nothing is written locally).
func (n Net) DefaultBranchOf(ctx context.Context, dir, remote string) (string, error) {
	return n.defaultBranchOf(ctx, dir, remote)
}

// FetchTagsFrom runs `git fetch --no-prune --tags <remote>`: the remote's
// branches (the configured refspec) and all its tags, including tags on
// commits that no branch contains. Pruning is disabled explicitly: with
// fetch.pruneTags or remote.<remote>.pruneTags configured, a pruning fetch
// would delete the local tags missing on the remote.
//
// Git refuses to overwrite a local tag that differs from the remote's tag of
// the same name ("would clobber existing tag"): it applies the rest of the
// fetch and exits with status 1. FetchTagsFrom returns the names of such kept
// local tags (sorted) and no error when these refusals are the only
// failures; any other failure is an error.
func (n Net) FetchTagsFrom(ctx context.Context, dir, remote string) ([]string, error) {
	return n.fetchTags(ctx, dir, remote)
}

// remoteBranchRef returns the first remote of remotes (default origin) that
// has refs/remotes/<remote>/<branch>, or "".
func remoteBranchRef(ctx context.Context, dir, branch string, remotes []string) (string, error) {
	if len(remotes) == 0 {
		remotes = OriginRemotes.BranchSearch()
	}
	for _, r := range slices.Clone(remotes) {
		ok, err := refExists(ctx, dir, "refs/remotes/"+r+"/"+branch)
		if err != nil {
			return "", err
		}
		if ok {
			return r, nil
		}
	}
	return "", nil
}
