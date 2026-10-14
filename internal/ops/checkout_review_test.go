package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// An untracked file in the way of the fast-forward is a skip with the file
// list, not a failure; the full git error is kept.
func TestUpdateUntrackedFilesInTheWayIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, alpha := coAddRepoWith(t, f, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "remote\n", "remote")
	writeFile(t, filepath.Join(alpha, "r.txt"), "local untracked\n")
	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil || len(r.Checkouts) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	u := r.Checkouts[0]
	if !u.Skipped() || !strings.Contains(u.Reason, "untracked files in the way") || !strings.Contains(u.Reason, "r.txt") {
		t.Fatalf("update = %+v, want skipped: untracked files in the way (r.txt)", u)
	}
	if u.Err == nil || !strings.Contains(u.Err.Error(), "would be overwritten") {
		t.Errorf("full git error not kept: %v", u.Err)
	}
	if readFile(t, filepath.Join(alpha, "r.txt")) != "local untracked\n" {
		t.Error("untracked file changed")
	}
}

// Linked worktrees outside <bundle>/<repo>/ are listed, marked Outside.
func TestListCheckoutsMarksOutsideWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "feat", "far")
	coAddWorktree(t, alpha, "feat", "feat", true)
	far := filepath.Join(t.TempDir(), "far")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "far", far, "origin/far")
	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 || len(cs) != 3 {
		t.Fatalf("%v %v %+v", err, probs, cs)
	}
	for _, c := range cs {
		if want := c.Dir == "far"; c.Outside != want {
			t.Errorf("%s: Outside = %v, want %v", c.Name(), c.Outside, want)
		}
	}
	_ = gitx.PullFailed
}
