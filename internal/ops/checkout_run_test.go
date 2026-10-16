//go:build unix

package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// coRecSink records the Begin/End calls; Begin returns a per-checkout buffer.
type coRecSink struct {
	mu     sync.Mutex
	calls  []string
	out    map[string]*bytes.Buffer
	ends   []RunResult
	before func(c Checkout)
}

func (s *coRecSink) Begin(c Checkout) io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.before != nil {
		s.before(c)
	}
	s.calls = append(s.calls, "begin "+c.Name())
	if s.out == nil {
		s.out = map[string]*bytes.Buffer{}
	}
	b := &bytes.Buffer{}
	s.out[c.Name()] = b
	return coLockedWriter{&s.mu, b}
}

func (s *coRecSink) End(r RunResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "end "+r.Name())
	s.ends = append(s.ends, r)
}

func (s *coRecSink) output(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.out[name]; b != nil {
		return b.String()
	}
	return ""
}

type coLockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l coLockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func coThreeCheckouts(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	_, alpha := coAddRepoWith(t, f, "alpha", "feat-a")
	coAddRepoWith(t, f, "beta")
	coAddWorktree(t, alpha, "feat-a", "feat-a", true)
	return f, alpha
}

func TestRunCommandOrderedBlocksAndExitCodes(t *testing.T) {
	t.Parallel()
	f, _ := coThreeCheckouts(t)
	sink := &coRecSink{}
	r, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs:  4,
		Shell: true,
		// Later checkouts finish first; one fails with 3.
		Args: []string{`case "$SHEAF_REPO/$SHEAF_DIR" in alpha/main) sleep 0.3;; alpha/feat-a) echo oops >&2; exit 3;; esac; echo "$(basename "$PWD") $SHEAF_BRANCH"`},
		Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"begin alpha/main", "end alpha/main", "begin alpha/feat-a", "end alpha/feat-a", "begin beta/main", "end beta/main"}
	if !slices.Equal(sink.calls, wantCalls) {
		t.Fatalf("sink calls = %v, want %v", sink.calls, wantCalls)
	}
	if got := sink.output("alpha/main"); got != "main main\n" {
		t.Errorf("alpha/main output %q", got)
	}
	if got := sink.output("alpha/feat-a"); got != "oops\n" {
		t.Errorf("alpha/feat-a output %q (stderr must be captured)", got)
	}
	if len(r.Results) != 3 {
		t.Fatalf("results %+v", r.Results)
	}
	for _, res := range r.Results {
		wantFail := res.Name() == "alpha/feat-a"
		if res.Failed() != wantFail || !res.Started {
			t.Errorf("%s: failed=%v started=%v (%+v)", res.Name(), res.Failed(), res.Started, res)
		}
	}
	if r.Results[1].ExitCode != 3 {
		t.Errorf("exit code %d, want 3", r.Results[1].ExitCode)
	}
}

func TestRunCommandWithoutShellAndFilters(t *testing.T) {
	t.Parallel()
	f, alpha := coThreeCheckouts(t)
	writeFile(t, filepath.Join(alpha, "file-1.txt"), "dirty\n")
	sink := &coRecSink{}
	r, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs: 2, Args: []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.output("alpha/feat-a"); got != "feat-a\n" {
		t.Errorf("output %q", got)
	}
	if len(r.Results) != 3 {
		t.Errorf("results %+v", r.Results)
	}

	sink = &coRecSink{}
	r, err = RunCommand(context.Background(), f.w, RunOptions{Jobs: 2, DirtyOnly: true, Args: []string{"true"}, Sink: sink})
	if err != nil || len(r.Results) != 1 || r.Results[0].Name() != "alpha/main" {
		t.Errorf("dirty-only: %v %+v", err, r.Results)
	}
	r, err = RunCommand(context.Background(), f.w, RunOptions{Jobs: 2, Filter: CheckoutFilter{PrimaryOnly: true}, Args: []string{"true"}, Sink: &coRecSink{}})
	if err != nil || len(r.Results) != 2 {
		t.Errorf("primary-only: %v %+v", err, r.Results)
	}
	// A missing executable is a failure of that checkout, not a crash.
	r, err = RunCommand(context.Background(), f.w, RunOptions{Jobs: 1, Args: []string{"sheaf-no-such-command"}, Sink: &coRecSink{}})
	if err != nil || len(r.Results) != 3 || !r.Results[0].Failed() || r.Results[0].Err == nil {
		t.Errorf("missing command: %v %+v", err, r.Results)
	}
	if _, err := RunCommand(context.Background(), f.w, RunOptions{Jobs: 1, Sink: &coRecSink{}}); err == nil {
		t.Error("empty command accepted")
	}
}

// With Stream, Begin is called before the command starts and the output is
// written to the returned writer.
func TestRunCommandStreamBeginsBeforeStart(t *testing.T) {
	t.Parallel()
	f, _ := coThreeCheckouts(t)
	marker := filepath.Join(t.TempDir(), "ran")
	sink := &coRecSink{}
	sink.before = func(c Checkout) {
		if c.Name() == "alpha/main" {
			if _, err := os.Stat(marker); err == nil {
				t.Error("Begin called after the command ran")
			}
		}
	}
	_, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs: 4, Stream: true, Shell: true,
		Args: []string{`touch '` + marker + `'; echo "$SHEAF_DIR"`}, Sink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.output("beta/main"); got != "main\n" {
		t.Errorf("streamed output %q", got)
	}
	if want := []string{"begin alpha/main", "end alpha/main", "begin alpha/feat-a", "end alpha/feat-a", "begin beta/main", "end beta/main"}; !slices.Equal(sink.calls, want) {
		t.Errorf("calls %v", sink.calls)
	}
}

func coPidsIn(t *testing.T, pidfile string) []int {
	t.Helper()
	b, _ := os.ReadFile(pidfile)
	var pids []int
	for f := range strings.FieldsSeq(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

func coAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return !(i >= 0 && i+2 < len(s) && s[i+2] == 'Z')
}

func coWaitPIDs(t *testing.T, pidfile string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for len(coPidsIn(t, pidfile)) < n {
		if time.Now().After(deadline) {
			t.Fatal("command never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func coWantGone(t *testing.T, pidfile string, within time.Duration) {
	t.Helper()
	pids := coPidsIn(t, pidfile)
	deadline := time.Now().Add(within)
	for {
		var live []int
		for _, p := range pids {
			if coAlive(p) {
				live = append(live, p)
			}
		}
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, p := range live {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
			t.Fatalf("processes %v survived", live)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Cancellation sends SIGTERM to the command's process group (children
// included); commands ignoring it get SIGKILL after the grace period; the
// remaining checkouts are not started.
func TestRunCommandCancelTerminatesProcessGroup(t *testing.T) {
	for _, tc := range []struct {
		name, trap string
	}{{"sigterm", ""}, {"sigkill-after-grace", "trap '' TERM\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := coThreeCheckouts(t)
			pidfile := filepath.Join(t.TempDir(), "pids")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan RunReport, 1)
			go func() {
				r, _ := RunCommand(ctx, f.w, RunOptions{
					Jobs: 1, Shell: true, Grace: 300 * time.Millisecond, Sink: &coRecSink{},
					Args: []string{tc.trap + "sleep 30 &\necho $! >> '" + pidfile + "'\necho $$ >> '" + pidfile + "'\nwait\n"},
				})
				done <- r
			}()
			coWaitPIDs(t, pidfile, 2)
			start := time.Now()
			cancel()
			var r RunReport
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("RunCommand did not return after cancellation")
			}
			if el := time.Since(start); el > 5*time.Second {
				t.Errorf("returned after %v", el)
			}
			coWantGone(t, pidfile, 2*time.Second)
			if len(r.Results) != 3 || !r.Results[0].Started || !r.Results[0].Failed() {
				t.Fatalf("results %+v", r.Results)
			}
			for _, res := range r.Results[1:] {
				if res.Started || !errors.Is(res.Err, context.Canceled) || !res.Failed() {
					t.Errorf("%s: %+v, want not started (cancelled)", res.Name(), res)
				}
			}
		})
	}
}

func TestKillCommandsKillsRunningGroups(t *testing.T) {
	t.Cleanup(coResetCommands)
	f, _ := coThreeCheckouts(t)
	pidfile := filepath.Join(t.TempDir(), "pids")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunCommand(context.Background(), f.w, RunOptions{
			Jobs: 1, Shell: true, Filter: CheckoutFilter{Repos: []string{"beta"}}, Sink: &coRecSink{},
			Args: []string{"trap '' TERM\nsleep 30 &\necho $! >> '" + pidfile + "'\necho $$ >> '" + pidfile + "'\nwait\n"},
		})
	}()
	coWaitPIDs(t, pidfile, 2)
	if n := KillCommands(); n != 1 {
		t.Errorf("KillCommands = %d, want 1", n)
	}
	coWantGone(t, pidfile, 2*time.Second)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunCommand did not return")
	}
}

// After KillCommands, RunCommand starts nothing; the result's error is what
// the CLI shows for the checkout (as "[...]" after its block and in the
// "failed: ... (...)" summary line).
func TestRunCommandAfterKillCommandsDoesNotStart(t *testing.T) {
	t.Cleanup(coResetCommands)
	f, _ := coThreeCheckouts(t)
	KillCommands()
	r, err := RunCommand(context.Background(), f.w, RunOptions{
		Jobs: 1, Shell: true, Filter: CheckoutFilter{Repos: []string{"beta"}}, Sink: &coRecSink{},
		Args: []string{"true"},
	})
	if err != nil || len(r.Results) != 1 {
		t.Fatalf("%v %+v", err, r.Results)
	}
	res := r.Results[0]
	const want = "not started (killed on request)"
	if res.Started || res.Err == nil || res.Err.Error() != want {
		t.Errorf("started %v, err %v, want not started with error %q", res.Started, res.Err, want)
	}
}
