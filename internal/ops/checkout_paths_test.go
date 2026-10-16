package ops

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestCheckoutFilterPaths(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, alpha := addRepoWith(t, f, "alpha", "feat", "gone")
	addRepoWith(t, f, "beta")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	gone := addWorktree(t, alpha, "gone", "gone", true)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cs, probs, err := ListCheckouts(ctx, f.w, CheckoutFilter{Paths: []string{feat}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutNames(cs); !slices.Equal(got, []string{"alpha/feat"}) || len(probs) != 0 {
		t.Fatalf("checkouts = %v, problems = %v", got, probs)
	}
	cs, probs, err = ListCheckouts(ctx, f.w, CheckoutFilter{Repos: []string{"alpha"}, Paths: []string{alpha, gone}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutNames(cs); !slices.Equal(got, []string{"alpha/main"}) || len(probs) != 1 || probs[0].Path != gone {
		t.Fatalf("checkouts = %v, problems = %v", got, probs)
	}
	if _, _, err := ListCheckouts(ctx, f.w, CheckoutFilter{Repos: []string{"beta"}, Paths: []string{feat}}, 2); err == nil {
		t.Error("a path outside the selected repositories selected nothing without error")
	}

	// update only fast-forwards the selected checkouts.
	oldMain := testutil.Git(t, alpha, "rev-parse", "HEAD")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote main")
	testutil.AdvanceOrigin(t, o, "feat", "r.txt", "r\n", "remote feat")
	r, err := Update(ctx, f.w, UpdateOptions{Filter: CheckoutFilter{Repos: []string{"alpha"}, Paths: []string{feat}}, Jobs: 2}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checkouts) != 1 || r.Checkouts[0].Path != feat || len(r.Problems) != 0 {
		t.Fatalf("update report = %+v", r)
	}
	if got := testutil.Git(t, alpha, "rev-parse", "HEAD"); got != oldMain {
		t.Errorf("the primary was moved from %s to %s", oldMain, got)
	}
	if got, want := testutil.Git(t, feat, "rev-parse", "HEAD"), testutil.Git(t, o.Path, "rev-parse", "feat"); got != want {
		t.Errorf("feat at %s, want %s", got, want)
	}
}
