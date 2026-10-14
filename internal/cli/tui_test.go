package cli

import (
	"bytes"
	"os"
	"testing"
)

func TestUseTUIDecision(t *testing.T) {
	for _, c := range []struct {
		in, out, noTUI, want bool
	}{
		{true, true, false, true},
		{true, true, true, false},
		{false, true, false, false},
		{true, false, false, false},
		{false, false, false, false},
	} {
		if got := useTUI(c.in, c.out, c.noTUI); got != c.want {
			t.Errorf("useTUI(stdinTTY=%v, stdoutTTY=%v, noTUI=%v) = %v, want %v", c.in, c.out, c.noTUI, got, c.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for name, x := range map[string]any{"pipe reader": r, "pipe writer": w, "buffer": &bytes.Buffer{}, "nil": nil} {
		if isTerminal(x) {
			t.Errorf("isTerminal(%s) = true", name)
		}
	}
	if UseTUI(r, w, false) {
		t.Error("UseTUI on pipes = true")
	}
}
