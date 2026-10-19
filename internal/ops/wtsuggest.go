package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jponge/sheaf/internal/gitx"
)

// WorktreeSuggestions are the names that the home screen's prompt of `wt
// add` suggests (see SuggestWorktree), each sorted, without duplicates.
type WorktreeSuggestions struct {
	Branches, Tags []string
}

// SuggestWorktree returns the branches and the tags that `wt add` may be
// given for the repository repo, from the local refs of its primary
// checkout (nothing is fetched), with one git process: the local branches
// and the branches of the remotes searched for an existing branch
// (RemotesOf(r).BranchSearch(): origin, or the fork and upstream), without
// the remote's name (nor the remote's HEAD); the tags. Names that the
// prompt would display or accept altered (see typable) are left out. A
// primary checkout without its .git directory is an error, without running
// git, which would read an enclosing repository.
func SuggestWorktree(ctx context.Context, w *Workspace, repo string) (WorktreeSuggestions, error) {
	r, err := repoNamed(w.Bundle(), repo)
	if err != nil {
		return WorktreeSuggestions{}, err
	}
	primary, err := primaryPath(w.Root, r)
	if err != nil {
		return WorktreeSuggestions{}, err
	}
	if fi, err := os.Lstat(filepath.Join(primary, ".git")); err != nil || !fi.IsDir() {
		return WorktreeSuggestions{}, fmt.Errorf("%s: primary checkout's .git is not a directory", r.Name)
	}
	remotes := RemotesOf(r).BranchSearch()
	args := []string{"for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags"}
	for _, rem := range remotes {
		args = append(args, "refs/remotes/"+rem)
	}
	out, err := gitx.Run(ctx, primary, args...)
	if err != nil {
		return WorktreeSuggestions{}, err
	}
	var s WorktreeSuggestions
	for _, ref := range strings.Split(out, "\n") {
		if !typable(ref) {
			continue
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			s.Branches = append(s.Branches, name)
		} else if name, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			s.Tags = append(s.Tags, name)
		} else {
			for _, rem := range remotes {
				if name, ok := strings.CutPrefix(ref, "refs/remotes/"+rem+"/"); ok && name != "HEAD" {
					s.Branches = append(s.Branches, name)
				}
			}
		}
	}
	slices.Sort(s.Branches)
	slices.Sort(s.Tags)
	s.Branches, s.Tags = slices.Compact(s.Branches), slices.Compact(s.Tags)
	return s, nil
}

// typable reports whether s is valid UTF-8 without control characters (C0,
// DEL, C1), which the prompt would replace or drop: a suggestion accepted
// altered would name another ref.
func typable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
