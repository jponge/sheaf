//go:build unix

package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

// holdLock takes the bundle lock like another sheaf process would.
func holdLock(t *testing.T, root string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) }
	t.Cleanup(release)
	return release
}

type eventLog struct {
	mu  sync.Mutex
	evs []Event
}

func (l *eventLog) emit(e Event) { l.mu.Lock(); l.evs = append(l.evs, e); l.mu.Unlock() }

func (l *eventLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.evs {
		if strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}

func TestUpdateWaitsForLockAndReports(t *testing.T) {
	f := newFixture(t)
	release := holdLock(t, f.w.Root)
	log := &eventLog{}
	done := make(chan error, 1)
	go func() {
		done <- f.w.updateCtx(context.Background(), log.emit, func(b *bundle.Bundle) error { return b.AddSource("o") })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !log.has("waiting for sheaf.yaml.lock") {
		if time.Now().After(deadline) {
			t.Fatal("no waiting event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("update finished while the lock was held: %v", err)
	default:
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(f.onDisk(t).Sources) != 1 {
		t.Fatal("update not saved")
	}
}

func TestUpdateLockWaitHonoursCancellation(t *testing.T) {
	f := newFixture(t)
	holdLock(t, f.w.Root)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.w.updateCtx(ctx, nil, func(b *bundle.Bundle) error { return b.AddSource("o") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v", d)
	}
}

// Adopt's rollback runs when the registration is cancelled while waiting
// for the lock.
func TestAdoptRollsBackWhenRegistrationCancelled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	holdLock(t, f.w.Root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Adopt(ctx, f.w, dir, AdoptOptions{}, func(e Event) {
		if strings.Contains(e.Message, "waiting for sheaf.yaml.lock") {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("x/ not restored: %v", err)
	}
	if exists(filepath.Join(dir, "main")) {
		t.Fatal("x/main left behind")
	}
	noLeftovers(t, f.w.Root)
}
