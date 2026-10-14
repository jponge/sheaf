package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// UnpushedCommits counts the commits that would be lost if the repository
// containing dir were deleted: commits reachable from any local branch, or
// from dir's HEAD (which may be detached), that are not reachable from any
// remote-tracking ref. Without remotes every commit counts. The result is as
// fresh as the last fetch. Equivalent to
// `git rev-list --count HEAD --branches --not --remotes`.
//
// Detached HEADs of other worktrees are not included: call it per worktree.
func UnpushedCommits(ctx context.Context, dir string) (int, error) {
	args := []string{"rev-list", "--count"}
	ok, err := refExists(ctx, dir, "HEAD")
	if err != nil {
		return 0, err
	}
	if ok {
		args = append(args, "HEAD")
	}
	out, err := Run(ctx, dir, append(args, "--branches", "--not", "--remotes")...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("gitx: rev-list --count in %s: unexpected output %q", dir, out)
	}
	return n, nil
}

// GoneBranches returns the local branches, sorted by name, whose configured
// upstream no longer exists (its remote-tracking ref was pruned), i.e. those
// reported as "[gone]" by %(upstream:track). Branches without upstream are
// not included. Run Fetch first for an up-to-date answer.
func GoneBranches(ctx context.Context, dir string) ([]string, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname)%09%(upstream:track)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var gone []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		ref, track, _ := strings.Cut(line, "\t")
		if track == "[gone]" {
			gone = append(gone, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return gone, nil
}

// MergedBranches returns the local branches, sorted by name, whose tip is
// reachable from target (any commit-ish, e.g. "origin/main"). A local branch
// with the same tip as target, including target itself when it is a local
// branch, is included; callers filter it out as needed. Squash- or
// rebase-merged branches are not detected.
func MergedBranches(ctx context.Context, dir, target string) ([]string, error) {
	if _, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", target+"^{commit}"); err != nil {
		return nil, fmt.Errorf("gitx: merged branches: unknown target %q: %w", target, err)
	}
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname)", "--merged", target, "refs/heads")
	if err != nil {
		return nil, err
	}
	var merged []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			merged = append(merged, strings.TrimPrefix(line, "refs/heads/"))
		}
	}
	return merged, nil
}
