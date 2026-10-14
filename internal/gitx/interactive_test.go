package gitx

import (
	"context"
	"slices"
	"testing"
)

func TestWithInteractiveMarksContext(t *testing.T) {
	ctx := context.Background()
	if IsInteractive(ctx) {
		t.Fatal("background context reported interactive")
	}
	if !IsInteractive(WithInteractive(ctx)) {
		t.Fatal("WithInteractive context not reported interactive")
	}
	c, cancel := context.WithCancel(WithInteractive(ctx))
	defer cancel()
	if !IsInteractive(c) {
		t.Fatal("derived context lost the interactive mark")
	}
}

// Interactive invocations may prompt (that is their purpose), so they must
// not force GIT_TERMINAL_PROMPT=0; the user's own value is kept.
func TestInteractiveEnvAllowsTerminalPrompts(t *testing.T) {
	in := []string{"PATH=/bin", "GIT_DIR=/elsewhere", "LC_ALL=fr_FR", "GIT_TERMINAL_PROMPT=1"}
	got := interactiveEnv(in)
	if slices.Contains(got, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("interactive env forces GIT_TERMINAL_PROMPT=0: %q", got)
	}
	if !slices.Contains(got, "GIT_TERMINAL_PROMPT=1") {
		t.Errorf("interactive env dropped the user's GIT_TERMINAL_PROMPT: %q", got)
	}
	if slices.Contains(got, "GIT_DIR=/elsewhere") || !slices.Contains(got, "LC_ALL=C") || slices.Contains(got, "LC_ALL=fr_FR") {
		t.Errorf("interactive env must still strip GIT_DIR and force LC_ALL=C: %q", got)
	}
}

func TestStripRepoEnv(t *testing.T) {
	in := []string{"PATH=/bin", "GIT_DIR=/x", "GIT_WORK_TREE=/y", "GIT_INDEX_FILE=/z", "LC_ALL=fr_FR", "GIT_TERMINAL_PROMPT=1", "GIT_CONFIG_COUNT=1"}
	got := StripRepoEnv(in)
	want := []string{"PATH=/bin", "LC_ALL=fr_FR", "GIT_TERMINAL_PROMPT=1", "GIT_CONFIG_COUNT=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("StripRepoEnv = %q, want %q", got, want)
	}
}
