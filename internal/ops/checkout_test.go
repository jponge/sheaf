package ops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// addRepoWith adds o/<name> to the bundle, backed by a fresh origin with the
// given extra branches, and returns the origin and the primary checkout.
func addRepoWith(t *testing.T, f *fixture, name string, branches ...string) (testutil.Origin, string) {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: branches})
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: name, CloneURL: o.URL, DefaultBranch: "main"})
	s, err := f.add(t, AddRequest{Slugs: []string{"o/" + name}})
	if err != nil || s.Err() != nil {
		t.Fatalf("add %s: %v %v", name, err, s.Err())
	}
	return o, filepath.Join(f.w.Root, name, "main")
}

// addWorktree creates the sibling worktree <repo>/<dir> of primary. With
// track, branch is created tracking origin/<branch>; otherwise it is a new
// local branch without upstream.
func addWorktree(t *testing.T, primary, dir, branch string, track bool) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(primary), dir)
	if track {
		testutil.Git(t, primary, "worktree", "add", "-q", "--track", "-b", branch, p, "origin/"+branch)
	} else {
		testutil.Git(t, primary, "worktree", "add", "-q", "-b", branch, p)
	}
	return p
}

func checkoutNames(cs []Checkout) []string {
	var n []string
	for _, c := range cs {
		n = append(n, c.Name())
	}
	return n
}

func TestListCheckoutsPrimaryFirstThenWorktreesByDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, beta := addRepoWith(t, f, "beta")
	_, alpha := addRepoWith(t, f, "alpha", "feat/b", "feat/a")
	addWorktree(t, alpha, "feat-b", "feat/b", true)
	addWorktree(t, alpha, "feat-a", "feat/a", true)
	det := filepath.Join(filepath.Dir(alpha), "det")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--detach", det, "HEAD")
	_ = beta

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 4)
	if err != nil || len(probs) != 0 {
		t.Fatalf("err %v, problems %v", err, probs)
	}
	want := []string{"alpha/main", "alpha/det", "alpha/feat-a", "alpha/feat-b", "beta/main"}
	if got := checkoutNames(cs); !slices.Equal(got, want) {
		t.Fatalf("checkouts = %v, want %v", got, want)
	}
	if !cs[0].Primary || cs[1].Primary || cs[0].Branch != "main" || cs[0].Path != alpha {
		t.Errorf("primary entry = %+v", cs[0])
	}
	if !cs[1].Detached || cs[1].Branch != "" {
		t.Errorf("detached entry = %+v", cs[1])
	}
	if cs[2].Branch != "feat/a" || cs[2].Dir != "feat-a" || cs[2].Repo != "alpha" {
		t.Errorf("worktree entry = %+v", cs[2])
	}
}

func TestListCheckoutsReportsMissingDirectoriesAsProblems(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	_, beta := addRepoWith(t, f, "beta")
	addRepoWith(t, f, "gamma")
	wt := addWorktree(t, alpha, "feat", "feat", true)
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(beta)); err != nil {
		t.Fatal(err)
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutNames(cs); !slices.Equal(got, []string{"alpha/main", "gamma/main"}) {
		t.Errorf("checkouts = %v", got)
	}
	if len(probs) != 2 || probs[0].Repo != "alpha" || probs[0].Path != wt || probs[1].Repo != "beta" || probs[1].Path != filepath.Dir(beta) {
		t.Fatalf("problems = %+v", probs)
	}
	for _, p := range probs {
		if !strings.Contains(p.Error(), "missing") || !strings.Contains(p.Error(), p.Repo) {
			t.Errorf("problem message %q lacks repo or \"missing\"", p.Error())
		}
	}
}

func TestListCheckoutsFilters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	addRepoWith(t, f, "alps")
	addRepoWith(t, f, "beta")
	addWorktree(t, alpha, "feat", "feat", true)
	ctx := context.Background()

	cs, _, err := ListCheckouts(ctx, f.w, CheckoutFilter{Repos: []string{"al*"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutNames(cs); !slices.Equal(got, []string{"alpha/main", "alpha/feat", "alps/main"}) {
		t.Errorf("glob filter: %v", got)
	}
	cs, _, err = ListCheckouts(ctx, f.w, CheckoutFilter{Repos: []string{"beta", "alpha"}, PrimaryOnly: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutNames(cs); !slices.Equal(got, []string{"alpha/main", "beta/main"}) {
		t.Errorf("names + primary-only: %v", got)
	}
	for _, bad := range []string{"nope", "z*", "["} {
		if _, _, err := ListCheckouts(ctx, f.w, CheckoutFilter{Repos: []string{bad}}, 1); err == nil {
			t.Errorf("--repo %q: no error", bad)
		}
	}
}
