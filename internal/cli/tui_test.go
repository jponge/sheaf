package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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

// stubEmergency replaces the emergency-stop seams; it returns channels
// receiving the killAll calls and the exit codes.
func stubEmergency(t *testing.T) (killed chan struct{}, exited chan int) {
	t.Helper()
	killed, exited = make(chan struct{}, 4), make(chan int, 4)
	oldKill, oldExit := killAll, exit
	t.Cleanup(func() { killAll, exit = oldKill, oldExit })
	killAll = func() int { killed <- struct{}{}; return 0 }
	exit = func(code int) { exited <- code }
	return killed, exited
}

// The TUI's ctrl+c key goes through the same function as the signals: the
// first one cancels the command's context, the second one releases the
// terminal, closes the force channel, kills the git process groups and
// exits with 130.
func TestInterruptControllerKeyPath(t *testing.T) {
	killed, exited := stubEmergency(t)
	var errb bytes.Buffer
	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		ctx := c.Context()
		ctl := interruptFrom(ctx)
		if ctl == nil {
			return errors.New("no interrupt controller in the command context")
		}
		var order []string
		ctl.setRelease(func() { order = append(order, "release") })
		ctl.interrupt()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return errors.New("first interrupt did not cancel the context")
		}
		select {
		case <-forceFrom(ctx):
			return errors.New("force channel closed by the first interrupt")
		default:
		}
		if len(exited) > 0 || len(killed) > 0 {
			return errors.New("first interrupt stopped everything")
		}
		ctl.interrupt()
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d), want 130", code)
			}
		default:
			return errors.New("second interrupt did not exit")
		}
		select {
		case <-killed:
		default:
			return errors.New("git process groups not killed")
		}
		select {
		case <-forceFrom(ctx):
		default:
			return errors.New("force channel not closed")
		}
		if len(order) != 1 {
			return fmt.Errorf("release hook called %d times, want 1", len(order))
		}
		ctl.interrupt() // a third one does nothing more
		if len(exited) > 0 {
			return errors.New("third interrupt exited again")
		}
		return nil
	}})
	if code := execute(root, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "killing git processes") {
		t.Errorf("emergency message missing:\n%s", errb.String())
	}
}
