package gitx

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// siblingCheckouts builds a primary clone plus n linked worktrees (branches
// b1..bn tracking origin) and a separate pusher clone that can advance every
// branch of the origin at once.
func siblingCheckouts(t *testing.T, n int) (primary string, checkouts, branches []string, advance func(round int)) {
	t.Helper()
	branches = []string{"main"}
	for i := 1; i <= n; i++ {
		branches = append(branches, fmt.Sprintf("b%d", i))
	}
	o, repo, primary := layout(t, testutil.OriginOpts{Branches: branches[1:]})
	checkouts = []string{primary}
	for _, b := range branches[1:] {
		p := filepath.Join(repo, b)
		mustAdd(t, primary, p, b, WorktreeAddOpts{}, WorktreeRemoteBranch)
		checkouts = append(checkouts, p)
	}
	pusher := filepath.Join(t.TempDir(), "pusher")
	testutil.Git(t, filepath.Dir(pusher), "clone", "-q", o.Path, pusher)
	advance = func(round int) {
		t.Helper()
		push := []string{"push", "-q", "origin"}
		for _, b := range branches {
			testutil.Git(t, pusher, "checkout", "-q", "-B", b, "origin/"+b)
			testutil.CommitFile(t, pusher, fmt.Sprintf("round-%d-%s.txt", round, b), "x\n", "round")
			push = append(push, b)
		}
		testutil.Git(t, pusher, push...)
		testutil.Git(t, pusher, "fetch", "-q")
	}
	return primary, checkouts, branches, advance
}

// B2: fetching once per repository, then fast-forwarding all sibling
// worktrees in parallel, never races on shared refs.
func TestFetchOnceThenParallelFastForwardOnSiblingWorktrees(t *testing.T) {
	t.Parallel()
	primary, checkouts, branches, advance := siblingCheckouts(t, 8)
	ctx := context.Background()

	common, err := CommonDir(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		t.Fatalf("CommonDir = %q, want an absolute path", common)
	}
	for _, c := range checkouts {
		if got, err := CommonDir(ctx, c); err != nil || got != common {
			t.Fatalf("CommonDir(%s) = %q, %v; want %q", c, got, err, common)
		}
	}

	for round := 1; round <= 5; round++ {
		advance(round)
		if err := Fetch(ctx, primary); err != nil {
			t.Fatal(err)
		}
		results := make([]PullResult, len(checkouts))
		var wg sync.WaitGroup
		for i, c := range checkouts {
			wg.Go(func() { results[i] = FastForward(ctx, c) })
		}
		wg.Wait()
		for i, r := range results {
			if r.Outcome != PullFastForwarded {
				t.Errorf("round %d, %s: outcome %v, err %v", round, branches[i], r.Outcome, r.Err)
				continue
			}
			if want := testutil.Git(t, primary, "rev-parse", "origin/"+branches[i]); r.New != want || head(t, checkouts[i]) != want {
				t.Errorf("round %d, %s: New = %s, HEAD = %s, want %s", round, branches[i], r.New, head(t, checkouts[i]), want)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
	}
}
