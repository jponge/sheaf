package ops

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// pushPullRef creates (or moves) refs/pull/<n>/head on origin to a new commit
// on top of parent (a commit-ish of origin, e.g. "main" or a previous PR
// head), as GitHub does for a pull request from a fork, and returns it.
func pushPullRef(t *testing.T, o testutil.Origin, n int, parent, file string) string {
	t.Helper()
	work := originWork(t, o)
	ref := "refs/pull/" + strconv.Itoa(n) + "/head"
	testutil.Git(t, work, "fetch", "-q", "origin", "+refs/pull/*:refs/remotes/pull/*")
	start := parent // a commit of a previous PR head
	if parent == "main" {
		start = "origin/main"
	}
	testutil.Git(t, work, "switch", "-q", "--detach", start)
	testutil.CommitFile(t, work, file, file+"\n", "fork work "+file)
	testutil.Git(t, work, "push", "-q", "-f", "origin", "HEAD:"+ref)
	return testutil.Git(t, work, "rev-parse", "HEAD")
}

func (f *fixture) addPRWorktree(t *testing.T, repo string, n int) (PRWorktreeResult, error) {
	t.Helper()
	return AddPRWorktree(context.Background(), f.w, f.fake, repo, n, noEmit)
}

func TestPRWorktreeSameRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
	res, err := f.addPRWorktree(t, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.w.Root, "a", "feat")
	if res.Path != want || res.Branch != "feat" || res.FromPullRef || !res.Created {
		t.Fatalf("result %+v", res)
	}
	if up := testutil.Git(t, want, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/feat" {
		t.Fatalf("upstream %q", up)
	}
	// Again: the existing worktree is returned.
	res, err = f.addPRWorktree(t, "a", 3)
	if err != nil || res.Path != want || res.Created {
		t.Fatalf("again: %+v, %v", res, err)
	}
}

func TestPRWorktreeFromFork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	head := pushPullRef(t, o, 7, "main", "fork.txt")
	// The fork's head branch is named like the default branch: it must not
	// be confused with origin/main.
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "main", HeadOwner: "someone", HeadRepo: "a", IsCrossRepository: true})
	res, err := f.addPRWorktree(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.w.Root, "a", "pr-7")
	if res.Path != want || res.Branch != "pr/7" || res.Dir != "pr-7" || !res.FromPullRef || !res.Created {
		t.Fatalf("result %+v", res)
	}
	if got := testutil.Git(t, want, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if got := testutil.Git(t, primary, "config", "branch.pr/7.merge"); got != "refs/pull/7/head" {
		t.Fatalf("merge %q", got)
	}
	if got := testutil.Git(t, primary, "config", "branch.pr/7.remote"); got != "origin" {
		t.Fatalf("remote %q", got)
	}
	// git pull in the worktree works with that configuration.
	next := pushPullRef(t, o, 7, head, "fork2.txt")
	testutil.Git(t, want, "pull", "-q", "--ff-only")
	if got := testutil.Git(t, want, "rev-parse", "HEAD"); got != next {
		t.Fatalf("after git pull: HEAD %s, want %s", got, next)
	}
}

func TestPRWorktreeDeletedForkAndDeletedHeadBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, o := f.clonedRepo(t, "a")
	head := pushPullRef(t, o, 8, "main", "gone-fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 8, HeadBranch: "fix", IsCrossRepository: true})
	res, err := f.addPRWorktree(t, "a", 8)
	if err != nil || res.Branch != "pr/8" || testutil.Git(t, res.Path, "rev-parse", "HEAD") != head {
		t.Fatalf("deleted fork: %+v, %v", res, err)
	}
	// Same repository, but the head branch was deleted after the merge: the
	// pull ref is used instead of creating a new branch from main.
	head9 := pushPullRef(t, o, 9, "main", "deleted-branch.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 9, HeadBranch: "deleted-branch", HeadOwner: "o", HeadRepo: "a"})
	res, err = f.addPRWorktree(t, "a", 9)
	if err != nil || res.Branch != "pr/9" || testutil.Git(t, res.Path, "rev-parse", "HEAD") != head9 || len(res.Warnings) == 0 {
		t.Fatalf("deleted head branch: %+v, %v", res, err)
	}
}

func TestPRWorktreeRefresh(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, o := f.clonedRepo(t, "a")
	head := pushPullRef(t, o, 7, "main", "fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "x", HeadOwner: "someone", HeadRepo: "a", IsCrossRepository: true})
	res, err := f.addPRWorktree(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	// Up to date.
	if res, err = f.addPRWorktree(t, "a", 7); err != nil || res.Created || !strings.Contains(res.Message, "up to date") {
		t.Fatalf("up to date: %+v, %v", res, err)
	}
	// New commits on the PR: fast-forwarded.
	next := pushPullRef(t, o, 7, head, "fork2.txt")
	if res, err = f.addPRWorktree(t, "a", 7); err != nil || res.Created || !strings.Contains(res.Message, "fast-forwarded") {
		t.Fatalf("fast-forward: %+v, %v", res, err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next {
		t.Fatalf("HEAD %s, want %s", got, next)
	}
	// Dirty worktree: refused, nothing moves.
	pushPullRef(t, o, 7, next, "fork3.txt")
	writeFile(t, filepath.Join(res.Path, "fork.txt"), "local change\n")
	if _, err = f.addPRWorktree(t, "a", 7); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty: %v", err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next {
		t.Fatalf("dirty: HEAD moved to %s", got)
	}
	testutil.Git(t, res.Path, "checkout", "--", "fork.txt")
	// Force-pushed PR (not a fast-forward): refused, nothing moves.
	pushPullRef(t, o, 7, "main", "rewritten.txt")
	if _, err = f.addPRWorktree(t, "a", 7); err == nil || !strings.Contains(err.Error(), "fast-forward") {
		t.Fatalf("force-pushed: %v", err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next {
		t.Fatalf("force-pushed: HEAD moved to %s", got)
	}
}

func TestPRWorktreeRefusesForeignLocalBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	pushPullRef(t, o, 7, "main", "fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "x", IsCrossRepository: true})
	testutil.Git(t, primary, "branch", "pr/7")
	if _, err := f.addPRWorktree(t, "a", 7); err == nil || !strings.Contains(err.Error(), "pr/7") {
		t.Fatalf("err = %v", err)
	}
}

func TestPRWorktreeErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	if _, err := f.addPRWorktree(t, "a", 42); !errors.Is(err, hosting.ErrNotFound) {
		t.Fatalf("unknown PR: %v", err)
	}
	if _, err := f.addPRWorktree(t, "nope", 1); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown repo: %v", err)
	}
	// The pull ref is missing on origin.
	f.fake.AddPR("o", "a", hosting.PR{Number: 5, HeadBranch: "x", IsCrossRepository: true})
	if _, err := f.addPRWorktree(t, "a", 5); err == nil || !strings.Contains(err.Error(), "refs/pull/5/head") {
		t.Fatalf("missing pull ref: %v", err)
	}
	if err := f.w.update(func(b *bundle.Bundle) error {
		b.Repositories[0].Host = "gitlab.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
	if _, err := f.addPRWorktree(t, "a", 3); err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("non-GitHub: %v", err)
	}
}

func TestUpdateReportsPullRequestBranches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, o := f.clonedRepo(t, "a")
	pushPullRef(t, o, 7, "main", "fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "x", IsCrossRepository: true})
	if _, err := f.addPRWorktree(t, "a", 7); err != nil {
		t.Fatal(err)
	}
	rep, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checkouts {
		if c.Dir != "pr-7" {
			continue
		}
		if !c.Skipped() || !strings.Contains(c.Reason, "sheaf wt pr a 7") {
			t.Fatalf("pr-7: %+v", c)
		}
		return
	}
	t.Fatalf("pr-7 not updated: %+v", rep)
}
