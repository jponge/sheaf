package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is deliberately not synchronized: the race detector flags
// concurrent Report calls.
type recorder struct {
	events  []Event
	inside  atomic.Int32
	overlap bool
}

func (r *recorder) Report(e Event) {
	if r.inside.Add(1) > 1 {
		r.overlap = true
	}
	time.Sleep(100 * time.Microsecond)
	r.events = append(r.events, e)
	r.inside.Add(-1)
}

func (r *recorder) kinds(repo string) []Kind {
	var ks []Kind
	for _, e := range r.events {
		if e.Repo == repo {
			ks = append(ks, e.Kind)
		}
	}
	return ks
}

func TestExecuteBoundedParallelism(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	var tasks []Task
	for i := range 6 {
		tasks = append(tasks, Task{Repo: fmt.Sprint("r", i), Phase: "p", Run: func(ctx context.Context, _ func(Event)) (string, error) {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			cur.Add(-1)
			return "", nil
		}})
	}
	done := make(chan Summary)
	go func() { done <- Execute(context.Background(), 2, tasks, Discard) }()
	deadline := time.Now().Add(5 * time.Second)
	for peak.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // give a third worker a chance to (wrongly) start
	close(release)
	s := <-done
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak parallelism = %d, want 2", p)
	}
	if got := s.Count(StatusDone); got != 6 {
		t.Fatalf("done = %d, want 6", got)
	}
}

func TestExecuteResultsSortedIndependentOfCompletionOrder(t *testing.T) {
	t.Parallel()
	// c finishes first, a last.
	order := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
	var finished sync.WaitGroup
	mk := func(name, next string) Task {
		return Task{Repo: name, Phase: "p", Run: func(context.Context, func(Event)) (string, error) {
			<-order[name]
			if next != "" {
				close(order[next])
			}
			return "msg-" + name, nil
		}}
	}
	finished.Add(1)
	tasks := []Task{mk("b", "a"), mk("a", ""), mk("c", "b")}
	go func() { close(order["c"]); finished.Done() }()
	s := Execute(context.Background(), 3, tasks, Discard)
	finished.Wait()
	var names []string
	for _, r := range s.Results {
		names = append(names, r.Repo)
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("result order = %v, want [a b c]", names)
	}
	if s.Results[0].Message != "msg-a" || s.Results[0].Status != StatusDone {
		t.Fatalf("result[0] = %+v", s.Results[0])
	}
}

func TestExecuteCollectsErrorsAndSkips(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	rec := &recorder{}
	tasks := []Task{
		{Repo: "ok", Phase: "clone", Run: func(_ context.Context, emit func(Event)) (string, error) {
			emit(Event{Message: "halfway"})
			return "cloned", nil
		}},
		{Repo: "bad", Phase: "clone", Run: func(context.Context, func(Event)) (string, error) { return "", boom }},
		{Repo: "skip", Phase: "clone", Run: func(context.Context, func(Event)) (string, error) { return "", Skip("already there") }},
	}
	s := Execute(context.Background(), 1, tasks, rec)
	if s.Count(StatusDone) != 1 || s.Count(StatusFailed) != 1 || s.Count(StatusSkipped) != 1 {
		t.Fatalf("summary = %+v", s)
	}
	bad := s.Results[0]
	if bad.Repo != "bad" || !errors.Is(bad.Err, boom) {
		t.Fatalf("bad result = %+v", bad)
	}
	if sk := s.Results[2]; sk.Repo != "skip" || sk.Message != "already there" || sk.Err != nil {
		t.Fatalf("skip result = %+v", sk)
	}
	if err := s.Err(); err == nil {
		t.Fatal("Summary.Err() = nil, want failure")
	}
	if got := rec.kinds("ok"); !slices.Equal(got, []Kind{KindStart, KindProgress, KindDone}) {
		t.Fatalf("ok events = %v", got)
	}
	if got := rec.kinds("bad"); !slices.Equal(got, []Kind{KindStart, KindFailed}) {
		t.Fatalf("bad events = %v", got)
	}
	if got := rec.kinds("skip"); !slices.Equal(got, []Kind{KindStart, KindSkipped}) {
		t.Fatalf("skip events = %v", got)
	}
	for _, e := range rec.events {
		if e.Phase != "clone" {
			t.Fatalf("event without task phase: %+v", e)
		}
		if e.Kind == KindFailed && !errors.Is(e.Err, boom) {
			t.Fatalf("failed event without error: %+v", e)
		}
	}
}

func TestExecuteNoFailureMeansNilErr(t *testing.T) {
	t.Parallel()
	s := Execute(context.Background(), 4, []Task{
		{Repo: "a", Run: func(context.Context, func(Event)) (string, error) { return "", nil }},
		{Repo: "b", Run: func(context.Context, func(Event)) (string, error) { return "", Skip("x") }},
	}, Discard)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if len(s.Results) != 2 {
		t.Fatalf("results = %+v", s.Results)
	}
}

func TestExecuteReporterCallsAreSerialized(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	var tasks []Task
	for i := range 16 {
		tasks = append(tasks, Task{Repo: fmt.Sprint(i), Run: func(_ context.Context, emit func(Event)) (string, error) {
			for range 5 {
				emit(Event{Message: "tick"})
			}
			return "", nil
		}})
	}
	Execute(context.Background(), 8, tasks, rec)
	if rec.overlap {
		t.Fatal("Report was called concurrently")
	}
	if len(rec.events) != 16*7 {
		t.Fatalf("events = %d, want %d", len(rec.events), 16*7)
	}
}

// planRecorder records the Plan calls and events it receives, in order:
// a Plan call as an event of Kind 0 whose Message is the number of units.
type planRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *planRecorder) Report(e Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func (r *planRecorder) Plan(units int) {
	r.mu.Lock()
	r.events = append(r.events, Event{Message: fmt.Sprint(units)})
	r.mu.Unlock()
}

// A Reporter that is a Planner is told the number of tasks before the
// first event.
func TestExecutePlansTasksBeforeTheFirstEvent(t *testing.T) {
	t.Parallel()
	rec := &planRecorder{}
	var tasks []Task
	for i := range 3 {
		tasks = append(tasks, Task{Repo: fmt.Sprint(i), Run: func(context.Context, func(Event)) (string, error) { return "", nil }})
	}
	Execute(context.Background(), 2, tasks, rec)
	if len(rec.events) != 1+3*2 || rec.events[0] != (Event{Message: "3"}) {
		t.Fatalf("events = %+v, want Plan(3) first, then 2 events per task", rec.events)
	}
	for _, e := range rec.events[1:] {
		if e.Kind == 0 {
			t.Fatalf("Plan called again: %+v", rec.events)
		}
	}
}

func TestExecuteCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var ran atomic.Int32
	tasks := []Task{{Repo: "a", Run: func(ctx context.Context, _ func(Event)) (string, error) {
		ran.Add(1)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}}
	for i := range 4 {
		tasks = append(tasks, Task{Repo: fmt.Sprint("z", i), Run: func(context.Context, func(Event)) (string, error) {
			ran.Add(1)
			return "", nil
		}})
	}
	go func() { <-started; cancel() }()
	rec := &recorder{}
	s := Execute(ctx, 1, tasks, rec)
	if n := ran.Load(); n != 1 {
		t.Fatalf("%d tasks ran, want 1 (the others must not start after cancellation)", n)
	}
	if len(s.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(s.Results))
	}
	for _, r := range s.Results {
		if r.Status != StatusFailed || !errors.Is(r.Err, context.Canceled) {
			t.Fatalf("result %+v, want failed with context.Canceled", r)
		}
	}
	if got := rec.kinds("z0"); !slices.Equal(got, []Kind{KindFailed}) {
		t.Fatalf("unstarted task events = %v, want [failed]", got)
	}
}

func TestExecuteRecoversPanics(t *testing.T) {
	t.Parallel()
	s := Execute(context.Background(), 2, []Task{
		{Repo: "a", Run: func(context.Context, func(Event)) (string, error) { panic("kaboom") }},
		{Repo: "b", Run: func(context.Context, func(Event)) (string, error) { return "", nil }},
	}, Discard)
	if s.Results[0].Status != StatusFailed || !strings.Contains(s.Results[0].Err.Error(), "kaboom") || !strings.Contains(s.Results[0].Err.Error(), "goroutine") {
		t.Fatalf("a = %+v, want failure with panic value and stack", s.Results[0])
	}
	if s.Results[1].Status != StatusDone {
		t.Fatalf("b = %+v", s.Results[1])
	}
}
