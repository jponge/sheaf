package gitx

import (
	"context"
	"slices"
	"sync"
)

// Call is one git invocation recorded by a CallCounter.
type Call struct {
	Dir  string   // working directory
	Args []string // arguments passed to git (without "git")
}

// CallCounter records the git invocations made under a context returned by
// WithCallCounter. It is safe for concurrent use. It exists for tests and
// diagnostics (git-call budgets); production code never installs one.
type CallCounter struct {
	mu    sync.Mutex
	calls []Call
}

type callCounterKey struct{}

// WithCallCounter returns a context under which every git process started by
// gitx (through Run or any other function of this package) is recorded in
// the returned counter, before it starts. Counting is scoped to the context,
// so parallel tests each count only their own invocations. A counter
// already installed in a parent context no longer sees the calls made under
// the returned context.
func WithCallCounter(ctx context.Context) (context.Context, *CallCounter) {
	c := &CallCounter{}
	return context.WithValue(ctx, callCounterKey{}, c), c
}

// Calls returns a copy of the recorded invocations, in start order.
func (c *CallCounter) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Call, len(c.calls))
	for i, k := range c.calls {
		out[i] = Call{Dir: k.Dir, Args: slices.Clone(k.Args)}
	}
	return out
}

// Count returns the number of recorded invocations.
func (c *CallCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// Reset forgets the recorded invocations.
func (c *CallCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

// recordCall records an invocation in the counter of ctx, if any.
func recordCall(ctx context.Context, dir string, args []string) {
	c, _ := ctx.Value(callCounterKey{}).(*CallCounter)
	if c == nil {
		return
	}
	c.mu.Lock()
	c.calls = append(c.calls, Call{Dir: dir, Args: slices.Clone(args)})
	c.mu.Unlock()
}
