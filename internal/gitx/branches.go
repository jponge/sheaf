package gitx

import (
	"context"
	"fmt"
	"strings"
)

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
