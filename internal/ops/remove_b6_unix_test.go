//go:build unix

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

	"github.com/jponge/sheaf/internal/bundle"
)

// B6 S6: the re-check after confirmation and the deletion run under
// .sheaf.lock, like wt rm and prune: while another sheaf process holds it,
// nothing is deleted.
func TestRemoveRechecksAndDeletesUnderTheBundleLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	dir := filepath.Join(f.w.Root, "a")
	var release func()
	waited, existedWhileLocked := false, false
	_, err := Remove(context.Background(), f.w, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: func(RemovePlan) bool {
		release = holdLock(t, f.w.Root) // another process takes the lock
		return true
	}}, func(e Event) {
		if !waited && strings.Contains(e.Message, "waiting for .sheaf.lock") {
			waited, existedWhileLocked = true, exists(dir)
			release()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !waited || !existedWhileLocked {
		t.Fatalf("waited for the lock: %v; %s still there while it was held: %v", waited, dir, existedWhileLocked)
	}
	if exists(dir) || len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("removal incomplete once the lock was released")
	}
}

// Review of B6: a directory of an unknown entry that cannot be read cannot
// be checked for worktrees of other repositories: a hard refusal that says
// so (not a raw "permission denied").
func TestRemoveUnreadableDirectoryInUnknownEntry(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	f := newFixture(t)
	f.clonedRepo(t, "a")
	locked := filepath.Join(f.w.Root, "a", "unk", "closed")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	_, err := f.remove(t, "a", true, yes)
	var re *RisksError
	if !errors.As(err, &re) || !re.Plan.Refused(true) ||
		!strings.Contains(err.Error(), locked+": cannot be checked for worktrees of other repositories") || !strings.Contains(err.Error(), "fix its permissions") {
		t.Fatalf("err = %v", err)
	}
	if !exists(locked) {
		t.Fatal("deleted")
	}
}

// Review of B6 S6: the bundle lock is always taken before Workspace.mu.
// While Remove holds the lock (deleting), another task's sheaf.yaml update
// must wait for the lock without holding mu, or Remove's own final update
// (which needs mu) would deadlock with it. Bounded: the update gives up
// after 3 s.
func TestUpdateWaitsForTheBundleLockWithoutHoldingTheMutex(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	waiting := make(chan struct{})
	updated := make(chan error, 1)
	started := false
	done := make(chan error, 1)
	go func() {
		_, err := Remove(context.Background(), f.w, "a", RemoveOptions{WorkingDir: f.w.Root, Confirm: yes}, func(e Event) {
			if started || !strings.HasPrefix(e.Message, "deleting ") {
				return
			}
			started = true // Remove holds the lock: another task updates sheaf.yaml
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var once sync.Once
				updated <- f.w.updateCtx(ctx, func(Event) { once.Do(func() { close(waiting) }) }, func(b *bundle.Bundle) error { return b.AddSource("o") })
			}()
			select { // let it reach the lock wait
			case <-waiting:
			case <-time.After(3 * time.Second):
			}
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("remove did not finish")
	}
	if err := <-updated; err != nil {
		t.Fatalf("the concurrent update failed (deadlock until its timeout?): %v", err)
	}
	if b := f.onDisk(t); len(b.Repositories) != 0 || len(b.Sources) != 1 {
		t.Fatalf("sheaf.yaml = %+v", b)
	}
}
