//go:build unix

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The context given to commands must be cancelled by SIGINT, SIGTERM and
// SIGHUP: git runs in its own session and no longer receives the terminal's
// SIGINT, so cancellation is the only way to stop it. The test sends the
// signal to its own process from inside a command, i.e. after execute has
// installed its handler; a safety-net signal.Notify keeps the test process
// alive if the handler is missing, so that case fails on the assertion.
func TestExecuteCancelsCommandContextOnSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			net := make(chan os.Signal, 1)
			signal.Notify(net, sig)
			defer signal.Stop(net)

			var errb bytes.Buffer
			root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
			root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
				if err := syscall.Kill(os.Getpid(), sig); err != nil {
					return err
				}
				select {
				case <-c.Context().Done():
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("command context not cancelled by " + sig.String())
				}
			}})
			if code := execute(root, []string{"probe"}); code != 0 {
				t.Fatalf("exit code %d: %s", code, errb.String())
			}
		})
	}
}

func TestInterruptedCommandExits130(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	var errb bytes.Buffer
	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &errb})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-c.Context().Done():
			return c.Context().Err()
		case <-time.After(5 * time.Second):
			return errors.New("not cancelled")
		}
	}})
	if code := execute(root, []string{"probe"}); code != 130 || !strings.Contains(errb.String(), "interrupted") {
		t.Fatalf("exit %d, stderr %q; want 130 and \"interrupted\"", code, errb.String())
	}
}

// A second signal kills the running git process groups and exits at once.
func TestSecondSignalKillsGitAndExits(t *testing.T) {
	net := make(chan os.Signal, 2)
	signal.Notify(net, syscall.SIGINT)
	defer signal.Stop(net)
	killed := make(chan struct{}, 1)
	exited := make(chan int, 1)
	oldKill, oldExit := killAll, exit
	t.Cleanup(func() { killAll, exit = oldKill, oldExit })
	killAll = func() int { killed <- struct{}{}; return 0 }
	exit = func(code int) { exited <- code }

	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-c.Context().Done()
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d), want 130", code)
			}
		case <-time.After(5 * time.Second):
			return errors.New("second signal did not exit")
		}
		select {
		case <-killed:
		default:
			return errors.New("git process groups not killed before exiting")
		}
		return nil
	}})
	var errb bytes.Buffer
	root.SetErr(&errb)
	// The probe checks exit(130) and killAll itself and returns nil (the
	// stubbed exit returns, unlike os.Exit).
	if code := execute(root, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"sheaf adopt", ".sheaf-adopt-"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("emergency stop message lacks %q:\n%s", want, errb.String())
		}
	}
}

// A signal and a TUI ctrl+c count together: signal first, key second is
// the emergency stop.
func TestSignalThenKeyIsEmergencyStop(t *testing.T) {
	net := make(chan os.Signal, 1)
	signal.Notify(net, syscall.SIGTERM)
	defer signal.Stop(net)
	killed, exited := stubEmergency(t)
	root := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-c.Context().Done():
		case <-time.After(5 * time.Second):
			return errors.New("signal did not cancel")
		}
		ctl := interruptFrom(c.Context())
		if ctl == nil {
			return errors.New("no interrupt controller")
		}
		ctl.interrupt()
		select {
		case code := <-exited:
			if code != 130 {
				return fmt.Errorf("exit(%d)", code)
			}
		case <-time.After(5 * time.Second):
			return errors.New("key after signal did not exit")
		}
		if len(killed) != 1 {
			return errors.New("not killed")
		}
		return nil
	}})
	var errb bytes.Buffer
	root.SetErr(&errb)
	if code := execute(root, []string{"probe"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}
