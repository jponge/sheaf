package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

// The fetches preceding update's fast-forwards and prune's plan (aside
// work) are announced to an AsidePlanner before the first one starts, so
// that a progress view can show their real total (B52 U2 review).

// asideRecorder records the PlanAside calls (as events of Phase "aside"
// whose Message is the number of units) and the events, in order.
type asideRecorder struct {
	mu     sync.Mutex
	events []Event
	onPlan func() // called by Plan
}

func (r *asideRecorder) Plan(int) {
	if r.onPlan != nil {
		r.onPlan()
	}
}

func (r *asideRecorder) Report(e Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func (r *asideRecorder) PlanAside(units int) {
	r.mu.Lock()
	r.events = append(r.events, Event{Phase: "aside", Message: fmt.Sprint(units)})
	r.mu.Unlock()
}

// planned returns the units announced before the first fetch event, and
// whether one was announced after it; starts counts the fetch starts.
func (r *asideRecorder) planned() (before int, late bool, starts int) {
	fetched := false
	for _, e := range r.events {
		switch {
		case e.Phase == "aside":
			var n int
			fmt.Sscan(e.Message, &n)
			if fetched {
				late = true
			}
			before += n
		case e.Phase == PhaseFetch:
			fetched = true
			if e.Kind == KindStart {
				starts++
			}
		}
	}
	return before, late, starts
}

func TestUpdatePlansOneFetchPerRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addRepoWith(t, f, "alpha")
	addRepoWith(t, f, "beta")
	addRepoWith(t, f, "gamma")
	rec := &asideRecorder{}
	if _, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, rec); err != nil {
		t.Fatal(err)
	}
	if n, late, starts := rec.planned(); n != 3 || late || starts != 3 {
		t.Fatalf("planned %d fetches (late: %v) for %d started, want 3 before the first: %+v", n, late, starts, rec.events)
	}
}

// An update interrupted before a repository is fetched reports that fetch
// as not started, so that every announced fetch ends.
func TestUpdateInterruptedBeforeFetchReportsItNotStarted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addRepoWith(t, f, "alpha")
	addRepoWith(t, f, "beta")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &asideRecorder{onPlan: cancel} // once the checkouts are listed
	if _, err := Update(ctx, f.w, UpdateOptions{Jobs: 1}, rec); err != nil {
		t.Fatal(err)
	}
	n, _, _ := rec.planned()
	ended := 0
	for _, e := range rec.events {
		if e.Phase == PhaseFetch && e.Kind == KindFailed && e.Message == "not started" && errors.Is(e.Err, context.Canceled) {
			ended++
		}
	}
	if n != 2 || ended != 2 {
		t.Fatalf("planned %d fetches, %d reported not started, want 2 and 2: %+v", n, ended, rec.events)
	}
}

// prune announces the fetches of the repositories it can check (not those
// with a problem), and none offline.
func TestPlanPrunePlansTheFetchesItRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addRepoWith(t, f, "alpha")
	_, beta := addRepoWith(t, f, "beta")
	addRepoWith(t, f, "gamma")
	if err := os.RemoveAll(beta); err != nil {
		t.Fatal(err)
	}
	rec := &asideRecorder{}
	if _, err := PlanPrune(context.Background(), f.w, PruneOptions{Jobs: 1, WorkingDir: f.w.Root}, rec); err != nil {
		t.Fatal(err)
	}
	if n, late, starts := rec.planned(); n != 2 || late || starts != 2 {
		t.Fatalf("planned %d fetches (late: %v) for %d started, want 2 before the first: %+v", n, late, starts, rec.events)
	}
	rec = &asideRecorder{}
	if _, err := PlanPrune(context.Background(), f.w, PruneOptions{Jobs: 1, WorkingDir: f.w.Root, Offline: true}, rec); err != nil {
		t.Fatal(err)
	}
	if n, _, starts := rec.planned(); n != 0 || starts != 0 {
		t.Fatalf("offline: planned %d fetches, %d started, want none: %+v", n, starts, rec.events)
	}
}
