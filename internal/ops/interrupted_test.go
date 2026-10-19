package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// Work stopped by an interruption is Interrupted, unless its cleanup or
// rollback failed too: that failure must be fixed by hand and is reported
// as a failure, with its full text (B52 U2 review).

func TestInterrupted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"other", errors.New("boom"), false},
		{"cancelled", context.Canceled, true},
		{"cancelled git", &gitx.GitError{Args: []string{"fetch"}, Err: context.Canceled}, true},
		{"deadline", context.DeadlineExceeded, false},
	} {
		if got := Interrupted(tc.err); got != tc.want {
			t.Errorf("%s: Interrupted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An adoption interrupted after the move, whose rollback fails (the bundle
// root became read-only), is not merely interrupted.
func TestAdoptInterruptedRollbackFailureIsNotInterrupted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := AdoptOptions{afterMove: func() error {
		if err := os.Chmod(dir, 0o555); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	}}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := Adopt(ctx, f.w, dir, opts, noEmit)
	if err == nil || !strings.Contains(err.Error(), "rolling back also failed, fix manually") {
		t.Fatalf("err = %v, want a rollback failure", err)
	}
	if Interrupted(err) {
		t.Errorf("Interrupted(%q) = true: the rollback failure would be hidden", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v no longer wraps the cancellation", err)
	}
}

// A clone interrupted before git ran, whose directory cannot be removed,
// is not merely interrupted.
func TestCloneInterruptedCleanupFailureIsNotInterrupted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	repoDir := bundle.RepoDir(f.w.Root, "x")
	locked := filepath.Join(repoDir, "locked")
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	emit := func(Event) { // runs before the clone, once repoDir exists
		if err := os.MkdirAll(filepath.Join(locked, "f"), 0o755); err == nil {
			_ = os.Chmod(locked, 0o555)
		}
	}
	entry := bundle.Repository{Name: "x", Slug: "o/x", URL: "file:///nonexistent/x.git", DefaultBranch: "main"}
	_, err := cloneRepo(ctx, f.w, gitx.Net{}, entry, "main", "", false, nil, emit)
	if err == nil || !strings.Contains(err.Error(), "also failed") {
		t.Fatalf("err = %v, want a cleanup failure", err)
	}
	if Interrupted(err) {
		t.Errorf("Interrupted(%q) = true: the partial clone left would be hidden", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v no longer wraps the cancellation", err)
	}
}
