package gitx

import (
	"context"
	"slices"
	"testing"
)

func TestCallCounterRecordsInvocationsOfItsContextOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, c := WithCallCounter(context.Background())
	if _, err := Run(ctx, dir, "version"); err != nil {
		t.Fatal(err)
	}
	_, _ = Run(ctx, dir, "rev-parse", "--show-toplevel")     // fails outside a repository: still counted
	if _, err := Version(context.Background()); err != nil { // another context: not counted
		t.Fatal(err)
	}
	calls := c.Calls()
	if len(calls) != 2 || c.Count() != 2 {
		t.Fatalf("calls = %v, want 2", calls)
	}
	if calls[0].Dir != dir || !slices.Equal(calls[0].Args, []string{"version"}) {
		t.Errorf("first call = %+v", calls[0])
	}
	if !slices.Equal(calls[1].Args, []string{"rev-parse", "--show-toplevel"}) {
		t.Errorf("second call = %+v", calls[1])
	}
	// Derived contexts (including WithoutCancel) keep counting.
	if _, err := Run(context.WithoutCancel(ctx), dir, "version"); err != nil {
		t.Fatal(err)
	}
	if c.Count() != 3 {
		t.Errorf("count after derived context = %d, want 3", c.Count())
	}
	c.Reset()
	if c.Count() != 0 {
		t.Errorf("count after reset = %d", c.Count())
	}
}
