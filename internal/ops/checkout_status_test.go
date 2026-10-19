package ops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func TestCollectStatusCountsAndCleanliness(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	addRepoWith(t, f, "beta")
	wt := addWorktree(t, alpha, "feat", "feat", true)
	// alpha/main: 1 staged, 1 unstaged, 1 untracked, 1 stash (repo-wide).
	writeFile(t, filepath.Join(alpha, "stash.txt"), "s\n")
	testutil.Git(t, alpha, "stash", "push", "-q", "-u")
	writeFile(t, filepath.Join(alpha, "file-1.txt"), "staged\n")
	testutil.Git(t, alpha, "add", "file-1.txt")
	writeFile(t, filepath.Join(alpha, "file-1.txt"), "unstaged on top\n")
	writeFile(t, filepath.Join(alpha, "new.txt"), "u\n")
	// alpha/feat: only untracked -> not clean.
	writeFile(t, filepath.Join(wt, "junk"), "j\n")

	r, err := CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 4}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range r.Checkouts {
		names = append(names, c.Name())
		if c.Err != nil {
			t.Errorf("%s: %v", c.Name(), c.Err)
		}
	}
	if !slices.Equal(names, []string{"alpha/main", "alpha/feat", "beta/main"}) {
		t.Fatalf("checkouts = %v", names)
	}
	m := r.Checkouts[0].Status
	if m.Staged != 1 || m.Unstaged != 1 || m.Untracked != 1 || m.Stashes != 1 || m.Upstream != "origin/main" {
		t.Errorf("alpha/main status = %+v", m)
	}
	if r.Checkouts[0].Clean() || r.Checkouts[1].Clean() || !r.Checkouts[2].Clean() {
		t.Errorf("clean = %v %v %v, want false false true", r.Checkouts[0].Clean(), r.Checkouts[1].Clean(), r.Checkouts[2].Clean())
	}

	r, err = CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 2, DirtyOnly: true}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checkouts) != 2 || r.Checkouts[0].Name() != "alpha/main" || r.Checkouts[1].Name() != "alpha/feat" {
		t.Fatalf("dirty-only: %+v", r.Checkouts)
	}
}

// status never fetches unless asked to.
func TestCollectStatusFetchesOnlyWithFetchOption(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, _ := addRepoWith(t, f, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	ctx := context.Background()

	r, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 1}, Discard)
	if err != nil || len(r.Checkouts) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if b := r.Checkouts[0].Status.Behind; b != 0 {
		t.Fatalf("behind = %d without --fetch: status fetched", b)
	}
	r, err = CollectStatus(ctx, f.w, StatusOptions{Jobs: 1, Fetch: true}, Discard)
	if err != nil || len(r.Checkouts) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if b := r.Checkouts[0].Status.Behind; b != 1 {
		t.Fatalf("behind = %d with --fetch, want 1", b)
	}
}

// With HeadTimes, each checkout has the author date of its HEAD,
// detached or not; without it, none.
func TestCollectStatusHeadTimes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	_, beta := addRepoWith(t, f, "beta")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	t1 := time.Date(2020, 5, 17, 10, 30, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	t3 := time.Date(2025, 7, 8, 9, 10, 11, 0, time.UTC)
	c1 := testutil.CommitAt(t, alpha, "one", t1)
	testutil.CommitAt(t, feat, "two", t2)
	testutil.Git(t, alpha, "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(alpha), "pinned"), c1)
	testutil.CommitAt(t, beta, "three", t3)

	r, err := CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 4, HeadTimes: true}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]time.Time{}
	for _, c := range r.Checkouts {
		got[c.Name()] = c.HeadTime
	}
	want := map[string]time.Time{"alpha/main": t1, "alpha/feat": t2, "alpha/pinned": t1, "beta/main": t3}
	if len(got) != len(want) {
		t.Fatalf("head times = %v, want %v", got, want)
	}
	for name, w := range want {
		if !got[name].Equal(w) {
			t.Errorf("%s: head time %v, want %v", name, got[name], w)
		}
	}
	r, err = CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 4}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checkouts {
		if !c.HeadTime.IsZero() {
			t.Errorf("%s: head time %v without HeadTimes", c.Name(), c.HeadTime)
		}
	}
}

// An unreadable HEAD commit (here removed between the status and the
// dates, as when HEAD moves and the old commit is pruned meanwhile) only
// loses the date of its own checkout, not those of the other checkouts of
// the repository. A checkout whose HEAD commit is already missing has a
// failed status (git status needs it), hence no date either.
func TestHeadTimesKeepsSiblingsOfAnUnreadableCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	gone := addWorktree(t, alpha, "gone", "gone", false)
	t1 := time.Date(2020, 5, 17, 10, 30, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	testutil.CommitAt(t, alpha, "one", t1)
	testutil.CommitAt(t, feat, "two", t2)
	lost := testutil.CommitAt(t, gone, "three", time.Date(2025, 7, 8, 9, 10, 11, 0, time.UTC))
	removeObject := func() {
		t.Helper()
		if err := os.Remove(filepath.Join(alpha, ".git", "objects", lost[:2], lost[2:])); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	sets, _, err := enumerate(ctx, f.w, CheckoutFilter{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var cs []CheckoutStatus
	for _, s := range sets {
		for _, c := range s.checkouts {
			st, err := gitx.Status(ctx, c.Path)
			if err != nil {
				t.Fatal(err)
			}
			cs = append(cs, CheckoutStatus{Checkout: c, Status: st})
		}
	}
	removeObject()
	headTimes(ctx, sets, cs, 1)
	want := map[string]time.Time{"alpha/main": t1, "alpha/feat": t2, "alpha/gone": {}}
	for _, c := range cs {
		if w, ok := want[c.Name()]; !ok || !c.HeadTime.Equal(w) {
			t.Errorf("%s: head time %v, want %v", c.Name(), c.HeadTime, w)
		}
	}

	r, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 1, HeadTimes: true}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checkouts {
		if w := want[c.Name()]; !c.HeadTime.Equal(w) || (c.Name() == "alpha/gone") != (c.Err != nil) {
			t.Errorf("CollectStatus: %s: head time %v, error %v; want %v", c.Name(), c.HeadTime, c.Err, w)
		}
	}
}

func TestCollectStatusReportsProblems(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	if err := os.RemoveAll(alpha); err != nil {
		t.Fatal(err)
	}
	r, err := CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checkouts) != 0 || len(r.Problems) != 1 || r.Problems[0].Repo != "alpha" {
		t.Fatalf("report = %+v", r)
	}
}

func TestFetchReposOneTaskPerRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, alpha := addRepoWith(t, f, "alpha", "feat")
	addRepoWith(t, f, "beta")
	addWorktree(t, alpha, "feat", "feat", true)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	want := testutil.Git(t, o.Path, "rev-parse", "main")

	s, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 4}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Results) != 2 || s.Results[0].Repo != "alpha" || s.Results[1].Repo != "beta" || s.Err() != nil {
		t.Fatalf("summary = %+v", s)
	}
	if got := testutil.Git(t, alpha, "rev-parse", "origin/main"); got != want {
		t.Fatalf("origin/main = %s, want %s (not fetched)", got, want)
	}
	if _, err := FetchRepos(context.Background(), f.w, FetchOptions{Repos: []string{"nope"}, Jobs: 1}, Discard); err == nil {
		t.Fatal("unknown --repo accepted")
	}
}

func TestFetchReposMissingPrimaryFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	addRepoWith(t, f, "beta")
	if err := os.RemoveAll(alpha); err != nil {
		t.Fatal(err)
	}
	s, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 2}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if st := statuses(s); st["alpha"] != StatusFailed || st["beta"] != StatusDone {
		t.Fatalf("statuses = %v", st)
	}
}

// Two bundle entries resolving to the same repository (here through a
// symlinked repository directory) cannot be fetched concurrently: the
// symlinked one is refused (see checkPrimary), the other is fetched.
// TestFetchSerializedPerCommonDirWorktrees covers the per-repository lock.
func TestFetchSerializedPerCommonDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	addRepoWith(t, f, "beta")
	beta := filepath.Join(f.w.Root, "beta")
	if err := os.RemoveAll(beta); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(alpha), beta); err != nil {
		t.Fatal(err)
	}
	s, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 2}, Discard)
	if err != nil || len(s.Results) != 2 {
		t.Fatalf("%v %+v", err, s.Results)
	}
	if r := s.Results[0]; r.Repo != "alpha" || r.Status != StatusDone {
		t.Errorf("alpha: %+v", r)
	}
	if r := s.Results[1]; r.Repo != "beta" || r.Status != StatusFailed || r.Err == nil || r.Err.Error() != beta+": repository directory is a symbolic link" {
		t.Errorf("beta: %+v", r)
	}
}

// Fetches from two work trees of one repository (its primary checkout and a
// linked worktree) are serialized: they share the remote-tracking refs.
func TestFetchSerializedPerCommonDirWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	var mu sync.Mutex
	active, maxActive := 0, 0
	hook := func(string) {
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}
	errs := make(chan error, 2)
	for _, dir := range []string{alpha, feat} {
		go func() { errs <- fetchRepo(context.Background(), gitx.Net{}, dir, gitx.OriginRemotes, hook) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if maxActive != 1 {
		t.Fatalf("%d concurrent fetches into the same repository, want 1", maxActive)
	}
}
