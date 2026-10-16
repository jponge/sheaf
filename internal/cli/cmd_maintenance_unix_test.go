//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
)

// stubGH writes an executable gh script running body and returns its path.
func stubGH(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGHAuthStatusStub(t *testing.T) {
	ctx := context.Background()
	args := filepath.Join(t.TempDir(), "args")
	ok := stubGH(t, `echo "$@" > '`+args+`'; exit 0`)
	if err := ghAuthStatus(ctx, ok, 5*time.Second); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if b, _ := os.ReadFile(args); strings.TrimSpace(string(b)) != "auth status --hostname github.com" {
		t.Fatalf("args %q", b)
	}
	bad := stubGH(t, `echo "You are not logged into any GitHub hosts. To log in, run: gh auth login" >&2; exit 1`)
	err := ghAuthStatus(ctx, bad, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not logged into any GitHub hosts") || errors.Is(err, hosting.ErrGHNotFound) {
		t.Fatalf("not logged in: %v", err)
	}
	// A hung gh (here with a grandchild holding the output pipe) is reported
	// as a timeout, promptly.
	hung := stubGH(t, `sleep 30 & sleep 30`)
	start := time.Now()
	err = ghAuthStatus(ctx, hung, 200*time.Millisecond)
	if !errors.Is(err, ops.ErrGHProbeTimeout) || !strings.Contains(err.Error(), "did not answer within 200ms") {
		t.Fatalf("hung: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestDefaultGHProbeWithoutGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := defaultGHProbe(context.Background()); !errors.Is(err, hosting.ErrGHNotFound) {
		t.Fatalf("err = %v", err)
	}
}
