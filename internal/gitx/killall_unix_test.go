//go:build unix

package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKillAllKillsRunningGitProcessGroups(t *testing.T) {
	t.Cleanup(resetKillAll)
	pidfile := filepath.Join(t.TempDir(), "pids")
	dir := sshFakeClone(t, hangingScript(pidfile))
	done := make(chan error, 1)
	go func() { done <- FetchRemote(context.Background(), dir, "origin") }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(pidfile)
		if len(strings.Fields(string(b))) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hanging ssh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if n := KillAll(); n < 1 {
		t.Fatalf("KillAll() = %d, want at least 1", n)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrForceKilled) {
			t.Fatalf("err = %v, want ErrForceKilled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Fetch still running 3s after KillAll")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	wantProcessesGone(t, pidfile)
	if n := KillAll(); n != 0 {
		t.Fatalf("KillAll() after completion = %d, want 0", n)
	}
	resetKillAll()
}
