package ops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// Characterization tests of the two listings of a repository's checkouts,
// ListWorktrees (wt commands) and ListCheckouts (checkout commands), on the
// cases where they differ today (backlog B3 kept the differences).

func wtListing(infos []WorktreeInfo) []string {
	var out []string
	for _, in := range infos {
		s := in.Repo + "/" + in.Dir + " " + in.Path
		if in.Primary {
			s += " primary"
		}
		if in.Outside {
			s += " outside"
		}
		out = append(out, s)
	}
	return out
}

func coListing(cs []Checkout) []string {
	var out []string
	for _, c := range cs {
		s := c.Name() + " " + c.Path
		if c.Primary {
			s += " primary"
		}
		if c.Outside {
			s += " outside"
		}
		out = append(out, s)
	}
	return out
}

// A worktree outside <repo>/ is listed last with an empty Dir by
// ListWorktrees, and among the others by its base name by ListCheckouts.
func TestListingsOutsideWorktreeOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "feat", "zz", "aaa")
	feat := coAddWorktree(t, alpha, "feat", "feat", true)
	zz := coAddWorktree(t, alpha, "zz", "zz", true)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(tmp, "aaa")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "aaa", far, "origin/aaa")

	infos, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/feat " + feat, "alpha/zz " + zz, "alpha/ " + far + " outside"}
	if got := wtListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	want = []string{"alpha/main " + alpha + " primary", "alpha/aaa " + far + " outside", "alpha/feat " + feat, "alpha/zz " + zz}
	if got := coListing(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A registered worktree directory now holding another repository is listed
// by ListWorktrees and reported as a problem by ListCheckouts.
func TestListingsWorktreeOfAnotherRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "feat")
	feat := coAddWorktree(t, alpha, "feat", "feat", true)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	other := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, filepath.Dir(feat), "clone", "-q", other.URL, feat)

	infos, fails, err := ListWorktrees(context.Background(), f.w, "alpha")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/feat " + feat}
	if got := wtListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := coListing(cs); !slices.Equal(got, []string{"alpha/main " + alpha + " primary"}) {
		t.Errorf("ListCheckouts = %q", got)
	}
	if len(probs) != 1 || probs[0].Path != feat || !strings.HasPrefix(probs[0].Err.Error(), "worktree of another repository (") {
		t.Errorf("problems = %v", probs)
	}
}

// A primary checkout whose .git is a file (separate git directory) is
// refused by both, for different reasons: ListWorktrees requires a .git
// directory; ListCheckouts accepts it but then finds that git lists the git
// directory, not the checkout, as the main work tree.
func TestListingsPrimaryWithGitFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(tmp, "alpha.git")
	if err := os.Rename(filepath.Join(alpha, ".git"), gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 1 || fails[0].Error() != "alpha: no primary checkout at "+alpha {
		t.Errorf("ListWorktrees: err %v, failures %v", err, fails)
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(cs) != 0 || len(probs) != 1 || probs[0].Error() != "alpha: "+alpha+": not the main work tree of its repository" {
		t.Errorf("ListCheckouts: err %v, checkouts %v, problems %v", err, cs, probs)
	}
}

// A worktree registered at a path whose parent directory is now a symbolic
// link to <repo>/ is outside for ListWorktrees (lexical parent) and inside
// for ListCheckouts (resolved parent).
func TestListingsWorktreeThroughSymlinkedParent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "x")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(tmp, "far")
	x := filepath.Join(far, "x")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "x", x, "origin/x")
	repoDir := filepath.Dir(alpha)
	if err := os.Rename(x, filepath.Join(repoDir, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(far); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, far); err != nil {
		t.Fatal(err)
	}

	infos, fails, err := ListWorktrees(context.Background(), f.w, "alpha")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/ " + x + " outside"}
	if got := wtListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	want = []string{"alpha/main " + alpha + " primary", "alpha/x " + x}
	if got := coListing(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
