//go:build unix

package ops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// S1: a second Ctrl-C (KillCommands) during the grace period after SIGTERM
// must kill what is left of the command's process group, even after the
// group leader exited.
func TestKillCommandsDuringGraceKillsLeftovers(t *testing.T) {
	t.Cleanup(coResetCommands)
	f, _ := coThreeCheckouts(t)
	pidfile := filepath.Join(t.TempDir(), "pids")    // the TERM-immune child
	leaderfile := filepath.Join(t.TempDir(), "lead") // the group leader
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunCommand(ctx, f.w, RunOptions{
			Jobs: 1, Shell: true, Grace: 20 * time.Second, Filter: CheckoutFilter{Repos: []string{"beta"}}, Sink: &coRecSink{},
			// The child records its pid only once its trap is installed, so
			// a SIGTERM can never reach it before.
			Args: []string{"sh -c \"trap '' TERM; echo \\$\\$ >> '" + pidfile + "'; exec sleep 30\" >/dev/null 2>&1 </dev/null &\n" +
				"echo $$ >> '" + leaderfile + "'\nwait\n"},
		})
	}()
	coWaitPIDs(t, pidfile, 1)
	coWaitPIDs(t, leaderfile, 1)
	leader := coPidsIn(t, leaderfile)[0]
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for coAlive(leader) {
		if time.Now().After(deadline) {
			t.Fatal("leader survived SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // Wait has returned; the grace period runs
	if n := KillCommands(); n < 1 {
		t.Errorf("KillCommands = %d during the grace period, want 1", n)
	}
	coWantGone(t, pidfile, 2*time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand did not return after KillCommands")
	}
}

// S2: a command that exits 0 but leaves a background process holding its
// output succeeds, with a note; the leftover process group is terminated.
func TestRunCommandBackgroundChildHoldingOutput(t *testing.T) {
	for _, jobs := range []int{1, 2} {
		t.Run("jobs="+string(rune('0'+jobs)), func(t *testing.T) {
			f, _ := coThreeCheckouts(t)
			pidfile := filepath.Join(t.TempDir(), "pids")
			sink := &coRecSink{}
			start := time.Now()
			r, err := RunCommand(context.Background(), f.w, RunOptions{
				Jobs: jobs, Stream: jobs == 1, Shell: true, Grace: 300 * time.Millisecond, Sink: sink,
				Filter: CheckoutFilter{Repos: []string{"beta"}},
				Args:   []string{"sleep 30 &\necho $! >> '" + pidfile + "'\necho started-bg"},
			})
			if err != nil || len(r.Results) != 1 {
				t.Fatalf("%v %+v", err, r.Results)
			}
			res := r.Results[0]
			if res.Failed() || res.ExitCode != 0 || !strings.Contains(res.Note, "truncated") {
				t.Errorf("result = %+v, want success with a truncation note", res)
			}
			if got := sink.output("beta/main"); got != "started-bg\n" {
				t.Errorf("output %q", got)
			}
			coWantGone(t, pidfile, 2*time.Second)
			if el := time.Since(start); el > 5*time.Second {
				t.Errorf("took %v", el)
			}
		})
	}
}

// coSizeSink checks large outputs without keeping them.
type coSizeSink struct {
	mu       sync.Mutex
	cur      string
	sizes    map[string]int
	bad      map[string]bool
	maxWrite int
}

func (s *coSizeSink) Begin(c Checkout) io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = c.Name()
	return s
}

func (s *coSizeSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > s.maxWrite {
		s.maxWrite = len(p)
	}
	for _, b := range p {
		if b != 'x' {
			s.bad[s.cur] = true
			break
		}
	}
	s.sizes[s.cur] += len(p)
	return len(p), nil
}

func (s *coSizeSink) End(RunResult) {}

// S3: large captured outputs are spilled to temporary files (removed
// afterwards) instead of being held in memory, and come out intact.
func TestRunCommandSpillsLargeOutput(t *testing.T) {
	f, _ := coThreeCheckouts(t)
	var mu sync.Mutex
	var spilled []string
	old := coSpillHook
	t.Cleanup(func() { coSpillHook = old })
	coSpillHook = func(p string) { mu.Lock(); spilled = append(spilled, p); mu.Unlock() }
	const size = 5 << 20
	sink := &coSizeSink{sizes: map[string]int{}, bad: map[string]bool{}}
	r, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs: 3, Shell: true, Sink: sink,
		Args: []string{"head -c 5242880 /dev/zero | tr '\\0' x"},
	})
	if err != nil || len(r.Results) != 3 {
		t.Fatalf("%v %+v", err, r.Results)
	}
	for _, res := range r.Results {
		if res.Failed() || sink.sizes[res.Name()] != size || sink.bad[res.Name()] {
			t.Errorf("%s: failed=%v size=%d corrupt=%v", res.Name(), res.Failed(), sink.sizes[res.Name()], sink.bad[res.Name()])
		}
	}
	if sink.maxWrite > 1<<20 {
		t.Errorf("largest write %d bytes: the output was held in memory", sink.maxWrite)
	}
	if len(spilled) != 3 {
		t.Errorf("spilled %d outputs, want 3", len(spilled))
	}
	for _, p := range spilled {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("temporary file %s left behind", p)
		}
	}
}

// Background processes a command leaves in its process group are terminated
// whatever its exit status, even when they do not hold the output.
func TestRunCommandTerminatesLeftoverGroupAfterExit(t *testing.T) {
	for _, code := range []int{0, 3} {
		t.Run("exit="+string(rune('0'+code)), func(t *testing.T) {
			f, _ := coThreeCheckouts(t)
			pidfile := filepath.Join(t.TempDir(), "pids")
			r, err := RunCommand(context.Background(), f.w, RunOptions{
				Jobs: 1, Shell: true, Grace: 300 * time.Millisecond, Sink: &coRecSink{},
				Filter: CheckoutFilter{Repos: []string{"beta"}},
				Args:   []string{"sleep 30 >/dev/null 2>&1 </dev/null &\necho $! >> '" + pidfile + "'\nexit " + string(rune('0'+code))},
			})
			if err != nil || len(r.Results) != 1 || r.Results[0].ExitCode != code || r.Results[0].Err != nil {
				t.Fatalf("%v %+v", err, r.Results)
			}
			if !strings.Contains(r.Results[0].Note, "background") {
				t.Errorf("note %q does not mention the terminated background processes", r.Results[0].Note)
			}
			coWantGone(t, pidfile, 2*time.Second)
		})
	}
}

// N2: an emergency exit (second Ctrl-C: os.Exit, no deferred cleanup) must
// not leave spilled outputs behind: on Unix the temporary files are unlinked
// as soon as they are created (the open descriptor keeps the data).
func TestRunCommandSpillFilesNeverVisible(t *testing.T) {
	f, _ := coThreeCheckouts(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var mu sync.Mutex
	var visible []string
	spills := 0
	old := coSpillHook
	t.Cleanup(func() { coSpillHook = old })
	coSpillHook = func(string) {
		mu.Lock()
		defer mu.Unlock()
		spills++
		_ = filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
			if err == nil && p != tmp {
				visible = append(visible, p)
			}
			return nil
		})
	}
	sink := &coSizeSink{sizes: map[string]int{}, bad: map[string]bool{}}
	r, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs: 3, Shell: true, Sink: sink, Args: []string{"head -c 3145728 /dev/zero | tr '\\0' x"},
	})
	if err != nil || len(r.Results) != 3 || spills != 3 {
		t.Fatalf("%v %+v, %d spills", err, r.Results, spills)
	}
	for _, res := range r.Results {
		if sink.sizes[res.Name()] != 3<<20 || sink.bad[res.Name()] {
			t.Errorf("%s: size %d corrupt %v", res.Name(), sink.sizes[res.Name()], sink.bad[res.Name()])
		}
	}
	if len(visible) != 0 {
		t.Errorf("spill files visible on disk while running: %v", visible)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in TMPDIR: %v", left)
	}
}
