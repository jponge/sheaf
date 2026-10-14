package ops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestCollectStatusCountsAndCleanliness(t *testing.T) {
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "feat")
	coAddRepoWith(t, f, "beta")
	wt := coAddWorktree(t, alpha, "feat", "feat", true)
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
	f := newFixture(t)
	o, _ := coAddRepoWith(t, f, "alpha")
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

func TestCollectStatusReportsProblems(t *testing.T) {
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha")
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
	f := newFixture(t)
	o, alpha := coAddRepoWith(t, f, "alpha", "feat")
	coAddRepoWith(t, f, "beta")
	coAddWorktree(t, alpha, "feat", "feat", true)
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
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha")
	coAddRepoWith(t, f, "beta")
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
// symlinked repository directory) must not be fetched concurrently: the
// remote-tracking ref locks would collide.
func TestFetchSerializedPerCommonDir(t *testing.T) {
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha")
	coAddRepoWith(t, f, "beta")
	beta := filepath.Join(f.w.Root, "beta")
	if err := os.RemoveAll(beta); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(alpha), beta); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := map[string]int{}
	var maxActive atomic.Int32
	old := coFetchHook
	t.Cleanup(func() { coFetchHook = old })
	coFetchHook = func(cd string) {
		mu.Lock()
		active[cd]++
		if n := int32(active[cd]); n > maxActive.Load() {
			maxActive.Store(n)
		}
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		active[cd]--
		mu.Unlock()
	}
	s, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 2}, Discard)
	if err != nil || s.Err() != nil {
		t.Fatalf("%v %v", err, s.Err())
	}
	if n := maxActive.Load(); n != 1 {
		t.Fatalf("%d concurrent fetches into the same repository, want 1", n)
	}
}
