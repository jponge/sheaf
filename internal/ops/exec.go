package ops

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
)

// Task is one unit of work of Execute, about one repository.
type Task struct {
	// Repo names the repository; results are ordered by it.
	Repo string
	// Phase is used in the start and final events.
	Phase string
	// Run performs the work. emit sends progress events; Repo and Phase
	// default to the task's and Kind to KindProgress. Run returns a short
	// message for the final event, and an error: nil (done), an error made by
	// Skip (skipped) or any other error (failed).
	Run func(ctx context.Context, emit func(Event)) (string, error)
}

// Status is the final state of a task.
type Status int

// Final states of a task.
const (
	StatusDone Status = iota + 1
	StatusSkipped
	StatusFailed
)

// Result is the outcome of one task.
type Result struct {
	Repo    string
	Phase   string
	Status  Status
	Message string // the message returned by Run, or the skip reason
	Err     error  // set when Status is StatusFailed
}

// Summary holds the results of Execute, sorted by Repo.
type Summary struct {
	Results []Result
}

// SkipError marks a task as skipped rather than failed.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return "skipped: " + e.Reason }

// Skip returns an error that makes Execute report the task as skipped.
func Skip(reason string) error { return &SkipError{Reason: reason} }

// Execute runs tasks with at most jobs (minimum 1) of them in parallel and
// returns their results sorted by Repo (stable for equal names), whatever the
// completion order. A failing task does not stop the others.
//
// For each task it reports a KindStart event, the task's progress events and
// a final KindDone, KindSkipped or KindFailed event. Calls to rep are
// serialized. Once ctx is done no further task is started; the tasks not
// started are reported (KindFailed only, no start) and returned as failed
// with ctx.Err(). Running tasks are expected to honour ctx themselves.
func Execute(ctx context.Context, jobs int, tasks []Task, rep Reporter) Summary {
	if jobs < 1 {
		jobs = 1
	}
	if rep == nil {
		rep = Discard
	}
	var mu sync.Mutex
	report := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		rep.Report(e)
	}
	results := make([]Result, len(tasks))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i, t := range tasks {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		// Check again: when both cases were ready, select picks at random.
		// A slot taken here is never released, which no longer matters.
		if err := ctx.Err(); err != nil {
			results[i] = Result{Repo: t.Repo, Phase: t.Phase, Status: StatusFailed, Err: err}
			report(Event{Repo: t.Repo, Phase: t.Phase, Kind: KindFailed, Message: "not started", Err: err})
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = runTask(ctx, t, report)
		})
	}
	wg.Wait()
	slices.SortStableFunc(results, func(a, b Result) int { return strings.Compare(a.Repo, b.Repo) })
	return Summary{Results: results}
}

func runTask(ctx context.Context, t Task, report func(Event)) Result {
	report(Event{Repo: t.Repo, Phase: t.Phase, Kind: KindStart})
	emit := func(e Event) {
		if e.Repo == "" {
			e.Repo = t.Repo
		}
		if e.Phase == "" {
			e.Phase = t.Phase
		}
		if e.Kind == 0 {
			e.Kind = KindProgress
		}
		report(e)
	}
	msg, err := runRecovering(ctx, t, emit)
	res := Result{Repo: t.Repo, Phase: t.Phase, Message: msg}
	var skip *SkipError
	switch {
	case err == nil:
		res.Status = StatusDone
		report(Event{Repo: t.Repo, Phase: t.Phase, Kind: KindDone, Message: msg})
	case errors.As(err, &skip):
		res.Status, res.Message = StatusSkipped, skip.Reason
		report(Event{Repo: t.Repo, Phase: t.Phase, Kind: KindSkipped, Message: skip.Reason})
	default:
		res.Status, res.Err = StatusFailed, err
		report(Event{Repo: t.Repo, Phase: t.Phase, Kind: KindFailed, Message: msg, Err: err})
	}
	return res
}

// Count returns the number of results with status st.
func (s Summary) Count(st Status) int {
	n := 0
	for _, r := range s.Results {
		if r.Status == st {
			n++
		}
	}
	return n
}

// Err returns nil when no task failed, and an error counting the failures
// otherwise.
func (s Summary) Err() error {
	if n := s.Count(StatusFailed); n > 0 {
		return fmt.Errorf("%d of %d repositories failed", n, len(s.Results))
	}
	return nil
}

// runRecovering runs t, turning a panic into an error carrying the panic
// value and the stack, so one task cannot crash the process.
func runRecovering(ctx context.Context, t Task, emit func(Event)) (msg string, err error) {
	defer func() {
		if r := recover(); r != nil {
			msg, err = "", fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return t.Run(ctx, emit)
}
