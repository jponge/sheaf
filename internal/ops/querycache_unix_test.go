//go:build unix

package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// AddWorktree re-reads the worktrees under the bundle lock afresh: a
// worktree created for the branch while the add waited for the lock is
// refused by sheaf's own check (not by git).
func TestAddWorktreeRecheckUnderLockSeesWorktreeAddedMeanwhile(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	release := holdLock(t, f.w.Root)
	log := &eventLog{}
	done := make(chan error, 1)
	go func() {
		_, err := AddWorktree(context.Background(), f.w, "a", "feat", WorktreeAddOptions{NoFetch: true}, log.emit)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !log.has("waiting for .sheaf.lock") {
		if time.Now().After(deadline) {
			t.Fatal("no waiting event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	elsewhere := filepath.Join(filepath.Dir(primary), "elsewhere")
	testutil.Git(t, primary, "worktree", "add", "-q", elsewhere, "feat")
	release()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), `branch "feat" is already checked out at `+elsewhere) {
		t.Fatalf("err = %v", err)
	}
}

// AddTagWorktree re-reads the worktrees under the bundle lock afresh: a
// worktree pinned at the tag created while the add waited for the lock is
// refused by sheaf's own check.
func TestAddTagWorktreeRecheckUnderLockSeesWorktreeAddedMeanwhile(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1")
	release := holdLock(t, f.w.Root)
	log := &eventLog{}
	done := make(chan error, 1)
	go func() {
		_, err := AddTagWorktree(context.Background(), f.w, "a", "v1", WorktreeAddOptions{NoFetch: true}, log.emit)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !log.has("waiting for .sheaf.lock") {
		if time.Now().After(deadline) {
			t.Fatal("no waiting event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	elsewhere := filepath.Join(filepath.Dir(primary), "elsewhere")
	if _, err := gitx.WorktreeAddTag(context.Background(), primary, elsewhere, "v1"); err != nil {
		t.Fatal(err)
	}
	release()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "tag v1 already has a worktree at "+elsewhere) {
		t.Fatalf("err = %v", err)
	}
}
