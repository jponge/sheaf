package gitx

import (
	"context"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestMergedBranches(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "branch", "merged/one")
	testutil.Git(t, dir, "checkout", "-q", "-b", "unmerged")
	testutil.CommitFile(t, dir, "u.txt", "u\n", "unmerged work")
	testutil.Git(t, dir, "checkout", "-q", "main")
	got, err := MergedBranches(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"main", "merged/one"}) {
		t.Fatalf("MergedBranches = %q", got)
	}
	if _, err := MergedBranches(context.Background(), dir, "no-such-ref"); err == nil {
		t.Fatal("unknown target: expected error")
	}
}
