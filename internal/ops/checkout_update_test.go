package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func coUpdatesByName(r UpdateReport) map[string]CheckoutUpdate {
	m := map[string]CheckoutUpdate{}
	for _, c := range r.Checkouts {
		m[c.Name()] = c
	}
	return m
}

func TestUpdateClassifiesEveryCheckout(t *testing.T) {
	f := newFixture(t)
	o, alpha := coAddRepoWith(t, f, "alpha", "clean", "dirty", "gone", "div")
	coAddRepoWith(t, f, "beta")
	coAddWorktree(t, alpha, "clean", "clean", true)
	dirty := coAddWorktree(t, alpha, "dirty", "dirty", true)
	coAddWorktree(t, alpha, "gone", "gone", true)
	div := coAddWorktree(t, alpha, "div", "div", true)
	coAddWorktree(t, alpha, "local", "local", false)
	testutil.Git(t, alpha, "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(alpha), "det"), "HEAD")

	oldMain := testutil.Git(t, alpha, "rev-parse", "HEAD")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote main")
	testutil.AdvanceOrigin(t, o, "dirty", "r.txt", "r\n", "remote dirty")
	testutil.AdvanceOrigin(t, o, "div", "r.txt", "r\n", "remote div")
	testutil.Git(t, o.Path, "branch", "-D", "gone")
	newMain := testutil.Git(t, o.Path, "rev-parse", "main")
	// dirty: 1 staged, 2 unstaged.
	writeFile(t, filepath.Join(dirty, "file-1.txt"), "staged\n")
	testutil.Git(t, dirty, "add", "file-1.txt")
	writeFile(t, filepath.Join(dirty, "file-1.txt"), "more\n")
	writeFile(t, filepath.Join(dirty, "branch-dirty.txt"), "changed\n")
	testutil.CommitFile(t, div, "local.txt", "l\n", "local div")

	var mu sync.Mutex
	var events []Event
	rep := ReporterFunc(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 4}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 0 {
		t.Fatalf("problems: %v", r.Problems)
	}
	m := coUpdatesByName(r)
	if len(m) != 8 {
		t.Fatalf("got %d checkouts: %+v", len(m), r.Checkouts)
	}
	if u := m["alpha/main"]; u.Outcome != gitx.PullFastForwarded || u.Old != oldMain || u.New != newMain {
		t.Errorf("alpha/main = %+v, want fast-forwarded %s..%s", u, oldMain, newMain)
	}
	if u := m["beta/main"]; u.Outcome != gitx.PullUpToDate {
		t.Errorf("beta/main = %+v", u)
	}
	if u := m["alpha/clean"]; u.Outcome != gitx.PullUpToDate {
		t.Errorf("alpha/clean = %+v", u)
	}
	u := m["alpha/dirty"]
	if u.Outcome != gitx.PullSkippedDirty || !strings.Contains(u.Reason, "1 staged") || !strings.Contains(u.Reason, "2 unstaged") {
		t.Errorf("alpha/dirty = %+v, want skipped with counts", u)
	}
	if u := m["alpha/gone"]; u.Outcome != gitx.PullSkippedNoUpstream || !strings.Contains(u.Reason, "gone") {
		t.Errorf("alpha/gone = %+v", u)
	}
	if u := m["alpha/local"]; u.Outcome != gitx.PullSkippedNoUpstream || !strings.Contains(u.Reason, "no upstream") {
		t.Errorf("alpha/local = %+v", u)
	}
	if u := m["alpha/div"]; u.Outcome != gitx.PullSkippedDiverged {
		t.Errorf("alpha/div = %+v", u)
	}
	if u := m["alpha/det"]; u.Outcome != gitx.PullSkippedDetached {
		t.Errorf("alpha/det = %+v", u)
	}
	// Order: repo, primary first, then by dir.
	var order []string
	for _, c := range r.Checkouts {
		order = append(order, c.Name())
	}
	want := "alpha/main alpha/clean alpha/det alpha/dirty alpha/div alpha/gone alpha/local beta/main"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
	// One event per checkout outcome carries the worktree name.
	found := false
	for _, e := range events {
		if e.Repo == "alpha" && e.Worktree == "dirty" && e.Kind == KindSkipped && e.Phase == PhaseUpdate {
			found = true
		}
	}
	if !found {
		t.Errorf("no skipped update event for alpha/dirty in %+v", events)
	}
}

func TestUpdateFetchFailureFailsTheRepositoryCheckouts(t *testing.T) {
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha")
	testutil.Git(t, alpha, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "missing.git"))
	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checkouts) != 1 || r.Checkouts[0].Outcome != gitx.PullFailed || r.Checkouts[0].Err == nil || !strings.Contains(r.Checkouts[0].Reason, "fetch") {
		t.Fatalf("report = %+v", r)
	}
}

// Cancelled while fetching: no merge starts, every checkout of the
// repository is reported as failed with the context's error.
func TestUpdateCancelledDuringFetchStartsNoMerge(t *testing.T) {
	f := newFixture(t)
	o, alpha := coAddRepoWith(t, f, "alpha", "feat")
	coAddWorktree(t, alpha, "feat", "feat", true)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	before := testutil.Git(t, alpha, "rev-parse", "HEAD")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := coFetchHook
	t.Cleanup(func() { coFetchHook = old })
	coFetchHook = func(string) { cancel() }
	r, _ := Update(ctx, f.w, UpdateOptions{Jobs: 1}, Discard)
	if len(r.Checkouts) != 2 {
		t.Fatalf("report = %+v", r)
	}
	for _, u := range r.Checkouts {
		if u.Outcome != gitx.PullFailed || !errors.Is(u.Err, context.Canceled) {
			t.Errorf("%s = %+v, want failed with context.Canceled", u.Name(), u)
		}
	}
	if h := testutil.Git(t, alpha, "rev-parse", "HEAD"); h != before {
		t.Fatal("HEAD moved although cancelled before the merge")
	}
}

// coBlockingMergeRepo returns a bundle whose alpha/main fast-forward runs a
// post-merge hook that signals started and then waits for release to exist.
// alpha also has a worktree "feat" that is behind.
func coBlockingMergeRepo(t *testing.T) (f *fixture, alpha, started, release string) {
	t.Helper()
	f = newFixture(t)
	o, alpha := coAddRepoWith(t, f, "alpha", "feat")
	coAddWorktree(t, alpha, "feat", "feat", true)
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote main")
	testutil.AdvanceOrigin(t, o, "feat", "r.txt", "r\n", "remote feat")
	hooks := t.TempDir()
	started = filepath.Join(hooks, "started")
	release = filepath.Join(hooks, "release")
	script := "#!/bin/sh\ncase \"$PWD\" in */feat) exit 0;; esac\ntouch '" + started + "'\nwhile [ ! -e '" + release + "' ]; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, alpha, "config", "core.hooksPath", hooks)
	return f, alpha, started, release
}

func coWaitFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// First Ctrl-C: the in-progress fast-forward completes, a waiting event is
// reported for it, and the next checkout is not started.
func TestUpdateCancelLetsInProgressFastForwardFinish(t *testing.T) {
	f, alpha, started, release := coBlockingMergeRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan Event, 4)
	rep := ReporterFunc(func(e Event) {
		if e.Phase == PhaseWaitFastForward {
			waiting <- e
		}
	})
	done := make(chan UpdateReport, 1)
	go func() {
		r, _ := Update(ctx, f.w, UpdateOptions{Jobs: 1}, rep)
		done <- r
	}()
	coWaitFile(t, started)
	cancel()
	select {
	case e := <-waiting:
		if e.Repo != "alpha" || e.Worktree != "main" {
			t.Errorf("waiting event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no waiting event after cancellation")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var r UpdateReport
	select {
	case r = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("update did not return")
	}
	if n := len(waiting); n != 0 {
		t.Errorf("%d extra waiting events, want exactly one (for the started merge)", n)
	}
	m := coUpdatesByName(r)
	if u := m["alpha/main"]; u.Outcome != gitx.PullFastForwarded {
		t.Errorf("alpha/main = %+v, want fast-forwarded despite cancellation", u)
	}
	if u := m["alpha/feat"]; u.Outcome != gitx.PullFailed || !errors.Is(u.Err, context.Canceled) {
		t.Errorf("alpha/feat = %+v, want not started (cancelled)", u)
	}
	_ = alpha
}

// Second Ctrl-C: the force channel kills the in-progress merge.
func TestUpdateForceKillsInProgressFastForward(t *testing.T) {
	f, _, started, _ := coBlockingMergeRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	force := make(chan struct{})
	done := make(chan UpdateReport, 1)
	go func() {
		r, _ := Update(ctx, f.w, UpdateOptions{Jobs: 1, Force: force}, Discard)
		done <- r
	}()
	coWaitFile(t, started)
	cancel()
	close(force)
	select {
	case r := <-done:
		u := coUpdatesByName(r)["alpha/main"]
		if u.Outcome != gitx.PullFailed || !errors.Is(u.Err, gitx.ErrForceKilled) {
			t.Fatalf("alpha/main = %+v, want failed with ErrForceKilled", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("force did not stop the merge")
	}
}
