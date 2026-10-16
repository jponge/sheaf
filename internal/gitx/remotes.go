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
	// OriginRemotes: a single remote "origin", the reference repository.
	OriginRemotes = Remotes{Reference: "origin"}
	// ForkRemotes: "origin" is the user's fork, "upstream" the reference
	// repository.
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
func CloneFork(ctx context.Context, upstreamURL, forkURL, dest, branch string) error {
	if err := clone(ctx, upstreamURL, dest, branch, ForkRemotes.Reference); err != nil {
		return err
	}
	if _, err := Run(ctx, dest, "remote", "add", "--", ForkRemotes.Fork, forkURL); err != nil {
		return fmt.Errorf("adding the fork remote %s: %w", ForkRemotes.Fork, err)
	}
	if err := FetchRemote(ctx, dest, ForkRemotes.Fork); err != nil {
		return fmt.Errorf("fetching the fork %s: %w", forkURL, err)
	}
	return nil
}

// FetchRemote runs `git fetch --quiet --prune <remote>` in dir; see Fetch
// for the serialization rules.
func FetchRemote(ctx context.Context, dir, remote string) error {
	_, err := run(ctx, netOpts(ctx), dir, "fetch", "--quiet", "--prune", remote)
	return err
}

// DefaultBranchOf is DefaultBranch for the given remote: it reads
// refs/remotes/<remote>/HEAD and, when missing, asks the remote with
// `git ls-remote --symref <remote> HEAD`.
func DefaultBranchOf(ctx context.Context, dir, remote string) (string, error) {
	return defaultBranchOf(ctx, dir, remote)
}

// FetchTagsFrom is FetchTags for the given remote.
func FetchTagsFrom(ctx context.Context, dir, remote string) ([]string, error) {
	return fetchTags(ctx, dir, remote)
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
