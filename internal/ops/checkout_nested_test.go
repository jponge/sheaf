package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// coNestedBundle builds a bundle inside the work tree of an outer clone
// whose upstream is ahead (fetched), with repository r1 whose primary lost
// its .git. It returns the fixture, the outer clone and r1's primary.
func coNestedBundle(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f, outer, primary := coNestedBundleIntact(t, "r1")
	if err := os.RemoveAll(filepath.Join(primary, ".git")); err != nil {
		t.Fatal(err)
	}
	return f, outer, primary
}

// coNestedBundleIntact is coNestedBundle without breaking the primary; the
// repository has an extra branch "feat".
func coNestedBundleIntact(t *testing.T, name string) (*fixture, string, string) {
	t.Helper()
	testutil.IsolateGit(t)
	oo := testutil.NewOrigin(t, testutil.OriginOpts{})
	outer := filepath.Join(t.TempDir(), "outer")
	testutil.Git(t, filepath.Dir(outer), "clone", "-q", oo.URL, outer)
	testutil.AdvanceOrigin(t, oo, "main", "o.txt", "o\n", "outer remote")
	testutil.Git(t, outer, "fetch", "-q")
	root, err := Create(filepath.Join(outer, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{w: w, fake: hosting.NewFake()}
	_, primary := coAddRepoWith(t, f, name, "feat")
	return f, outer, primary
}

func coWantNotWorkTree(t *testing.T, what string, probs []Problem, primary string) {
	t.Helper()
	if len(probs) != 1 || probs[0].Repo != "r1" || probs[0].Path != primary || !strings.Contains(probs[0].Error(), "not a git work tree") {
		t.Errorf("%s: problems = %v, want r1 not a git work tree", what, probs)
	}
}

// B1: a primary without .git inside an enclosing work tree must not be
// mistaken for the enclosing repository.
func TestCheckoutsPrimaryWithoutGitInsideOuterRepo(t *testing.T) {
	t.Parallel()
	f, outer, primary := coNestedBundle(t)
	ctx := context.Background()
	before := testutil.Git(t, outer, "rev-parse", "HEAD")

	cs, probs, err := ListCheckouts(ctx, f.w, CheckoutFilter{}, 1)
	if err != nil || len(cs) != 0 {
		t.Errorf("list: %v %v", err, checkoutNamesOf(cs))
	}
	coWantNotWorkTree(t, "list", probs, primary)

	u, err := Update(ctx, f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil || len(u.Checkouts) != 0 {
		t.Errorf("update: %v %+v", err, u.Checkouts)
	}
	coWantNotWorkTree(t, "update", u.Problems, primary)

	s, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 1}, Discard)
	if err != nil || len(s.Checkouts) != 0 {
		t.Errorf("status: %v %+v", err, s.Checkouts)
	}
	coWantNotWorkTree(t, "status", s.Problems, primary)

	marker := filepath.Join(t.TempDir(), "ran")
	r, err := RunCommand(ctx, f.w, RunOptions{Jobs: 1, Shell: true, Args: []string{"touch '" + marker + "'"}})
	if err != nil || len(r.Results) != 0 {
		t.Errorf("run: %v %+v", err, r.Results)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("run executed the command in the outer repository")
	}

	sum, err := FetchRepos(ctx, f.w, FetchOptions{Jobs: 1}, Discard)
	if err != nil || len(sum.Results) != 1 || sum.Results[0].Status != StatusFailed ||
		!strings.Contains(sum.Results[0].Err.Error(), "not a git work tree") {
		t.Errorf("fetch: %v %+v", err, sum.Results)
	}

	if h := testutil.Git(t, outer, "rev-parse", "HEAD"); h != before {
		t.Fatal("the outer repository was fast-forwarded")
	}
}

// A primary that is a linked worktree of another checkout is not a primary.
func TestCheckoutsPrimaryThatIsALinkedWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, primary := coAddRepoWith(t, f, "r1", "dev")
	repoDir := filepath.Dir(primary)
	dev := filepath.Join(repoDir, "dev")
	// Make dev the main work tree and main a linked worktree of it.
	if err := os.Rename(primary, dev); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dev, "checkout", "-q", "dev")
	testutil.Git(t, dev, "worktree", "add", "-q", primary, "main")
	_, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 1 || !strings.Contains(probs[0].Error(), "linked worktree") {
		t.Fatalf("problems = %v (%v)", probs, err)
	}
	sum, _ := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 1}, Discard)
	if len(sum.Results) != 1 || sum.Results[0].Status != StatusFailed {
		t.Fatalf("fetch: %+v", sum.Results)
	}
}

func checkoutNamesOf(cs []Checkout) []string { return coCheckoutNames(cs) }

// B1': a linked worktree that lost its .git file inside an enclosing work
// tree is a problem, never the enclosing repository; the primary is still
// processed.
func TestCheckoutsLinkedWorktreeWithoutGitInsideOuterRepo(t *testing.T) {
	t.Parallel()
	f, outer, primary := coNestedBundleIntact(t, "r1")
	wt := coAddWorktree(t, primary, "feat", "feat", true)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before := testutil.Git(t, outer, "rev-parse", "HEAD")
	wantProblem := func(what string, probs []Problem) {
		t.Helper()
		if len(probs) != 1 || probs[0].Path != wt || !strings.Contains(probs[0].Error(), "not a git work tree") {
			t.Errorf("%s: problems = %v, want feat not a git work tree", what, probs)
		}
	}

	s, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 2}, Discard)
	if err != nil || len(s.Checkouts) != 1 || s.Checkouts[0].Name() != "r1/main" {
		t.Errorf("status: %v %+v", err, s.Checkouts)
	}
	wantProblem("status", s.Problems)

	u, err := Update(ctx, f.w, UpdateOptions{Jobs: 2}, Discard)
	if err != nil || len(u.Checkouts) != 1 || u.Checkouts[0].Name() != "r1/main" {
		t.Errorf("update: %v %+v", err, u.Checkouts)
	}
	wantProblem("update", u.Problems)

	marker := filepath.Join(t.TempDir(), "ran-in-feat")
	r, err := RunCommand(ctx, f.w, RunOptions{Jobs: 2, Shell: true,
		Args: []string{`[ "$SHEAF_DIR" = feat ] && touch '` + marker + `'; true`}})
	if err != nil || len(r.Results) != 1 {
		t.Errorf("run: %v %+v", err, r.Results)
	}
	wantProblem("run", r.Problems)
	if _, err := os.Stat(marker); err == nil {
		t.Error("run executed the command in the broken worktree")
	}
	if h := testutil.Git(t, outer, "rev-parse", "HEAD"); h != before {
		t.Fatal("the outer repository was fast-forwarded")
	}
}
