package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestPullRequestRef(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ref  string
		n    int
		isPR bool
	}{
		{"refs/pull/12/head", 12, true},
		{"refs/pull/7/merge", 7, true},
		{"refs/pull/0/head", 0, false},
		{"refs/pull/-1/head", 0, false},
		{"refs/pull/12/tail", 0, false},
		{"refs/pull/12", 0, false},
		{"refs/pull/x/head", 0, false},
		{"refs/heads/pull/12/head", 0, false},
		{"", 0, false},
	} {
		n, ok := PullRequestRef(tc.ref)
		if n != tc.n || ok != tc.isPR {
			t.Errorf("PullRequestRef(%q) = %d, %v; want %d, %v", tc.ref, n, ok, tc.n, tc.isPR)
		}
	}
}

func TestLocalBranches(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat/gone", "keep"}})
	testutil.Git(t, dir, "branch", "--track", "feat/gone", "origin/feat/gone")
	testutil.Git(t, dir, "branch", "--track", "keep", "origin/keep")
	testutil.Git(t, dir, "branch", "local-only")
	testutil.Git(t, dir, "branch", "--track", "on-main", "main")
	// A pull request branch: its merge ref is not mapped by origin's refspec.
	testutil.Git(t, o.Path, "update-ref", "refs/pull/5/head", "refs/heads/keep")
	if err := FetchRefspecs(context.Background(), dir, "origin", "refs/pull/5/head:refs/heads/pr/5"); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dir, "config", "branch.pr/5.remote", "origin")
	testutil.Git(t, dir, "config", "branch.pr/5.merge", "refs/pull/5/head")
	testutil.Git(t, o.Path, "branch", "-D", "feat/gone")
	if err := FetchRemote(context.Background(), dir, "origin"); err != nil {
		t.Fatal(err)
	}

	bs, err := LocalBranches(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]LocalBranch{}
	for _, b := range bs {
		names = append(names, b.Name)
		byName[b.Name] = b
	}
	if want := []string{"feat/gone", "keep", "local-only", "main", "on-main", "pr/5"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	keepHead := testutil.Git(t, dir, "rev-parse", "keep")
	if b := byName["keep"]; b.Head != keepHead || b.Remote != "origin" || b.Merge != "refs/heads/keep" ||
		b.Upstream != "refs/remotes/origin/keep" || b.Gone {
		t.Errorf("keep = %+v", b)
	}
	if b := byName["feat/gone"]; !b.Gone || b.Upstream != "refs/remotes/origin/feat/gone" {
		t.Errorf("feat/gone = %+v, want gone", b)
	}
	if b := byName["local-only"]; b.Remote != "" || b.Merge != "" || b.Upstream != "" || b.Gone {
		t.Errorf("local-only = %+v", b)
	}
	if b := byName["on-main"]; b.Remote != "." || b.Upstream != "refs/heads/main" || b.Gone {
		t.Errorf("on-main = %+v", b)
	}
	pr := byName["pr/5"]
	if pr.Remote != "origin" || pr.Merge != "refs/pull/5/head" || pr.Head != keepHead || pr.Gone {
		t.Errorf("pr/5 = %+v", pr)
	}
	if n, ok := pr.PullRequest(); !ok || n != 5 {
		t.Errorf("pr/5 PullRequest = %d, %v", n, ok)
	}
}

func TestFetchRefspecsDoesNotForce(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"a", "b"}})
	testutil.Git(t, o.Path, "update-ref", "refs/pull/1/head", "refs/heads/a")
	ctx := context.Background()
	if err := FetchRefspecs(ctx, dir, "origin", "refs/pull/1/head:refs/heads/pr/1"); err != nil {
		t.Fatal(err)
	}
	// The PR head is force-pushed to an unrelated commit: a non-forced
	// refspec refuses to rewrite the local branch.
	testutil.Git(t, o.Path, "update-ref", "refs/pull/1/head", "refs/heads/b")
	if err := FetchRefspecs(ctx, dir, "origin", "refs/pull/1/head:refs/heads/pr/1"); err == nil {
		t.Fatal("non-fast-forward fetch into a local branch succeeded")
	}
	if got, want := testutil.Git(t, dir, "rev-parse", "pr/1"), testutil.Git(t, o.Path, "rev-parse", "a"); got != want {
		t.Fatalf("pr/1 moved to %s, want %s", got, want)
	}
}

func TestLockFiles(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "worktree", "add", "-q", "-b", "side", filepath.Join(filepath.Dir(dir), "side"))
	git := filepath.Join(dir, ".git")
	if got, err := LockFiles(git); err != nil || len(got) != 0 {
		t.Fatalf("clean repository: %v, %v", got, err)
	}
	locks := []string{
		filepath.Join(git, "index.lock"),
		filepath.Join(git, "refs", "remotes", "origin", "main.lock"),
		filepath.Join(git, "packed-refs.lock"),
		filepath.Join(git, "worktrees", "side", "index.lock"),
		filepath.Join(git, "logs", "HEAD.lock"),
	}
	for _, p := range locks {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(locks[0], old, old); err != nil {
		t.Fatal(err)
	}
	// Not lock files git leaves behind.
	if err := os.WriteFile(filepath.Join(git, "objects", "x.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LockFiles(git)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, l := range got {
		paths = append(paths, l.Path)
	}
	want := slices.Clone(locks)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("LockFiles = %v\nwant %v", paths, want)
	}
	for _, l := range got {
		if l.Path == locks[0] && !l.ModTime.Equal(old) {
			t.Errorf("index.lock ModTime = %v, want %v", l.ModTime, old)
		}
	}
	if _, err := LockFiles(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory: no error")
	}
}
