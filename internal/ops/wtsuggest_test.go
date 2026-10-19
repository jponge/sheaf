package ops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// The suggestions of the home screen's w prompt (B47) are read from local
// refs with one git process: the local branches and the branches of the
// remotes wt add searches (origin; the fork and upstream for a clone of a
// fork), without the remote prefix nor HEAD, deduplicated and sorted, and
// the tags. Other remotes are not suggested: wt add would not find their
// branches.
func TestWorktreeSuggestions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "plain")
	testutil.Git(t, primary, "branch", "local-only")
	testutil.Git(t, primary, "tag", "v1.0")
	testutil.Git(t, primary, "update-ref", "refs/remotes/upstream/not-searched", "HEAD")
	testutil.Git(t, primary, "update-ref", "refs/remotes/originals/not-searched-either", "HEAD")
	_, forked := f.addForked(t, "forked", testutil.OriginOpts{Branches: []string{"both"}})
	testutil.Git(t, forked, "update-ref", "refs/remotes/origin/on-fork", "HEAD")
	testutil.Git(t, forked, "update-ref", "refs/remotes/upstream/on-upstream", "HEAD")
	testutil.Git(t, forked, "update-ref", "refs/remotes/other/elsewhere", "HEAD")
	testutil.Git(t, forked, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	testutil.Git(t, forked, "tag", "v2")
	for _, tc := range []struct {
		repo string
		want WorktreeSuggestions
	}{
		{"plain", WorktreeSuggestions{Branches: []string{"feat", "local-only", "main"}, Tags: []string{"v1.0"}}},
		{"forked", WorktreeSuggestions{Branches: []string{"both", "main", "on-fork", "on-upstream"}, Tags: []string{"v2"}}},
	} {
		ctx, counter := gitx.WithCallCounter(context.Background())
		got, err := SuggestWorktree(ctx, f.w, tc.repo)
		if err != nil {
			t.Fatalf("%s: %v", tc.repo, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: suggestions %+v, want %+v", tc.repo, got, tc.want)
		}
		if n := len(counter.Calls()); n != 1 {
			t.Errorf("%s: %d git processes, want 1", tc.repo, n)
		}
	}
	// An unknown repository, or a primary checkout without its .git
	// directory (git would read an enclosing repository), give an error,
	// without running git.
	if err := os.Rename(filepath.Join(primary, ".git"), filepath.Join(primary, "git.moved")); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"plain", "nosuch"} {
		ctx, counter := gitx.WithCallCounter(context.Background())
		if got, err := SuggestWorktree(ctx, f.w, repo); err == nil || got.Branches != nil || got.Tags != nil || len(counter.Calls()) > 0 {
			t.Errorf("%s: %+v, error %v, %d git processes", repo, got, err, len(counter.Calls()))
		}
	}
}
