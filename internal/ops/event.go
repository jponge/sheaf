// Package ops implements sheaf's use cases (create, init, add, adopt, remove,
// ...) independently of the user interface. Long-running operations report
// progress as Events to a Reporter, so that a plain CLI renderer or a TUI can
// display them.
package ops

import "fmt"

// Kind is the kind of an Event.
type Kind int

const (
	KindStart    Kind = iota + 1 // an operation on a repository started
	KindProgress                 // intermediate information
	KindDone                     // the operation succeeded
	KindSkipped                  // the operation was not needed or not applicable
	KindFailed                   // the operation failed (Err is set)
)

func (k Kind) String() string {
	switch k {
	case KindStart:
		return "start"
	case KindProgress:
		return "progress"
	case KindDone:
		return "done"
	case KindSkipped:
		return "skipped"
	case KindFailed:
		return "failed"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Phases used in events.
const (
	PhaseResolve = "resolve"
	PhaseClone   = "clone"
	PhaseFetch   = "fetch"
	PhaseAdopt   = "adopt"
	PhaseCheck   = "check"
	PhaseRemove  = "remove"
)

// Event is a progress notification about one repository (and optionally one
// of its worktrees).
type Event struct {
	Repo     string
	Worktree string // empty when the event concerns the repository as a whole
	Phase    string
	Kind     Kind
	Message  string
	Err      error // set for KindFailed
}

// Reporter receives events. Implementations do not need to be safe for
// concurrent use: ops never calls Report concurrently (Execute serializes the
// calls of its tasks). Report should return quickly; it is called from the
// worker goroutines.
type Reporter interface {
	Report(Event)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Event)

// Report calls f(e).
func (f ReporterFunc) Report(e Event) { f(e) }

// Discard is a Reporter that drops every event.
var Discard Reporter = ReporterFunc(func(Event) {})
