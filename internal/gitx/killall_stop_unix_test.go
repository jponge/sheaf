//go:build unix

package gitx

import (
	"context"
	"errors"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// After KillAll no new git process starts: a Run racing with the emergency
// stop must not leave a git process behind once sheaf exits.
func TestRunAfterKillAllDoesNotStart(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	t.Cleanup(resetKillAll)
	KillAll()
	_, err := Run(context.Background(), dir, "status")
	if !errors.Is(err, ErrForceKilled) {
		t.Fatalf("err = %v, want ErrForceKilled without starting git", err)
	}
}
