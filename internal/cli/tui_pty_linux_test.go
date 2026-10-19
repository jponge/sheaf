//go:build linux

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// termReplies are the answers of the pty "terminal" to the queries that
// sheaf (bubbletea) may send.
var termReplies = []struct{ query, reply string }{
	{"\x1b[?2026$p", "\x1b[?2026;2$y"}, // DECRQM synchronized output
	{"\x1b[?2027$p", "\x1b[?2027;2$y"}, // DECRQM unicode core
	{"\x1b[?u", "\x1b[?1u"},            // kitty keyboard flags
	{"\x1b[c", "\x1b[?62;22c"},         // primary device attributes
	{"\x1b[6n", "\x1b[1;1R"},           // cursor position
	{"\x1b]11;?", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
}

// ptyShell is a shell script running on a pseudo-terminal (its controlling
// terminal) that answers terminal queries like an interactive terminal,
// after a delay.
type ptyShell struct {
	t      *testing.T
	master *os.File
	cmd    *exec.Cmd
	done   chan error

	mu      sync.Mutex
	out     bytes.Buffer
	seen    map[string]int // queries answered (or scheduled), by query
	pending sync.WaitGroup // replies scheduled but not written yet
	replies chan timedReply
}

// timedReply is a reply to write at a given time; replies are written in
// order, as a terminal answers queries.
type timedReply struct {
	at    time.Time
	reply string
}

// startPTYShell runs script with sh in dir; $SHEAF is the sheaf binary.
// Replies to terminal queries are written after delay (0: as soon as the
// query is read).
func startPTYShell(t *testing.T, dir, script string, delay time.Duration) *ptyShell {
	t.Helper()
	bin := filepath.Join(sheafBinary(t), "sheaf")
	master, slave := openPTY(t)
	// 24x100, like an interactive terminal.
	ws := struct{ row, col, x, y uint16 }{24, 100, 0, 0}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSWINSZ, uintptrOf(&ws)); e != 0 {
		t.Fatalf("TIOCSWINSZ: %v", e)
	}
	s := &ptyShell{t: t, master: master, seen: map[string]int{}, done: make(chan error, 1), replies: make(chan timedReply, 64)}
	go func() {
		for r := range s.replies {
			time.Sleep(time.Until(r.at))
			_, _ = s.master.Write([]byte(r.reply))
			s.pending.Done()
		}
	}()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "TERM", "TERM_PROGRAM", "SSH_TTY", "CI", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "COLORTERM":
		default:
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "TERM=xterm-256color", "SHEAF="+bin, "LC_ALL=C")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close() // the shell holds it: reading the master fails once it exits
	s.cmd = cmd
	go func() { s.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		s.pending.Wait()
	})
	go s.read(delay)
	return s
}

func (s *ptyShell) read(delay time.Duration) {
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		s.mu.Lock()
		s.out.Write(buf[:n])
		for _, r := range termReplies {
			for c := bytes.Count(s.out.Bytes(), []byte(r.query)); s.seen[r.query] < c; s.seen[r.query]++ {
				s.pending.Add(1)
				s.replies <- timedReply{time.Now().Add(delay), r.reply}
			}
		}
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *ptyShell) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

// waitFor waits until the output matches re and returns the submatches.
func (s *ptyShell) waitFor(re string) []string {
	s.t.Helper()
	rx := regexp.MustCompile(re)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if m := rx.FindStringSubmatch(s.output()); m != nil {
			return m
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("output never matched %q:\n%q", re, s.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *ptyShell) write(b string) {
	s.t.Helper()
	if _, err := s.master.Write([]byte(b)); err != nil {
		s.t.Fatal(err)
	}
}

// nextLine types a marker line once every scheduled reply has been written
// (like a user typing after sheaf exited) and returns the line the shell
// reads next: anything left by sheaf in the terminal input comes first.
func (s *ptyShell) nextLine() string {
	s.t.Helper()
	s.pending.Wait()
	s.write("zzz\n")
	return s.waitFor(`GOT=\[(.*?)\]END`)[1]
}

// readNext is the end of the scripts: the shell reads one line.
const readNext = `; IFS= read -r line; printf 'GOT=[%s]END\n' "$line"`

// Bubble Tea v2 queries the terminal at startup (DECRQM 2026/2027) and
// on the first frame (kitty keyboard flags). A command that ends quickly
// must not leave the replies in the terminal input, where the shell reads
// them as typed text.
func TestTUIFastExitLeavesNoQueryReplies(t *testing.T) {
	for _, delay := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			e := newBundleEnv(t)
			e.addRepo(t, "alpha")
			e.addRepo(t, "beta")
			s := startPTYShell(t, e.cwd, `"$SHEAF" fetch; echo SHEAF-EXIT=$?`+readNext, delay)
			s.waitFor(`SHEAF-EXIT=0`)
			if !strings.Contains(s.output(), "\x1b[?2026$p") {
				t.Log("note: no DECRQM query sent (the test does not exercise it)")
			}
			if got := s.nextLine(); got != "zzz" {
				t.Fatalf("the shell read %q after sheaf fetch, want zzz (query replies leaked)", got)
			}
		})
	}
}

// End to end on a pty: the removal dialog answered n restores the terminal
// (modes, raw mode, kitty keyboard stack) and leaves nothing in its input.
func TestTUIRemoveDialogOnPTY(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF" remove alpha; echo SHEAF-EXIT=$?; `+
		`if [ "$(stty -g)" = "$B" ]; then echo STTY-SAME; else echo STTY-CHANGED; fi`+readNext, 0)
	s.waitFor(`\[y/N\]`)
	s.write("n")
	s.waitFor(`STTY-(SAME|CHANGED)`)
	out := s.output()
	if !strings.Contains(out, "SHEAF-EXIT=1") || !strings.Contains(out, "cancelled") {
		t.Errorf("want exit 1 and cancelled:\n%q", out)
	}
	if !strings.Contains(out, "STTY-SAME") {
		t.Errorf("terminal settings not restored:\n%q", out)
	}
	if !exists(filepath.Join(e.cwd, "alpha", "main")) {
		t.Error("alpha removed")
	}
	count := func(re string) int { return len(regexp.MustCompile(re).FindAllString(out, -1)) }
	if push, pop := count(`\x1b\[>\d*u`), count(`\x1b\[<\d*u`); push != pop {
		t.Errorf("kitty keyboard: %d push, %d pop", push, pop)
	}
	last := func(on, off string) bool { return strings.LastIndex(out, on) > strings.LastIndex(out, off) }
	if last("\x1b[?2004h", "\x1b[?2004l") {
		t.Error("bracketed paste left on")
	}
	if last("\x1b[?25l", "\x1b[?25h") {
		t.Error("cursor left hidden")
	}
	if got := s.nextLine(); got != "zzz" {
		t.Fatalf("the shell read %q after the dialog, want zzz", got)
	}
}

// fakeFetchGit returns a directory holding a git wrapper running fetch as
// the shell code fetch says (in the checkout's directory, with the
// arguments in "$@"), other commands as git.
func fakeFetchGit(t *testing.T, fetch string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = fetch ]; then\n"+fetch+"\nfi; done\nexec '"+git+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// terminalProgress matches the OSC 9;4 sequences (terminal progress).
var terminalProgress = regexp.MustCompile(`\x1b\]9;4;[^\x07\x1b]*(?:\x07|\x1b\\)`)

// wantTerminalProgressReset checks that out sets the terminal's progress
// indicator, and resets it last.
func wantTerminalProgressReset(t *testing.T, out string) {
	t.Helper()
	all := terminalProgress.FindAllString(out, -1)
	if len(all) < 2 || all[len(all)-1] != "\x1b]9;4;0\x07" {
		t.Errorf("terminal progress sequences %q: want some, the last one the reset \\x1b]9;4;0\\x07", all)
	}
}

// On a terminal, the progress view drives the terminal's own progress
// indicator (OSC 9;4, B43): here the error state at 50% once one of two
// fetches failed while the other runs; the indicator is reset when the
// program ends.
func TestTUIProgressDrivesTerminalProgress(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	e.addRepo(t, "beta")
	bin := fakeFetchGit(t, `case "$PWD" in */alpha*) echo boom >&2; exit 1;; esac; sleep 30`)
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" fetch -j 2`+sttyCheck, 0)
	s.waitFor(`\x1b\]9;4;2;50\x07`)
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
	wantTerminalProgressReset(t, s.output())
	// Only the home screen sets the window title (B42).
	if titles := windowTitles(s.output()); len(titles) > 0 {
		t.Errorf("window titles %q set by a command run alone", titles)
	}
}

// The emergency stop (a second ctrl+c while git ignores the first) kills
// the progress view: the terminal's progress indicator is reset too.
func TestTUIEmergencyStopResetsTerminalProgress(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	bin := fakeFetchGit(t, `trap '' TERM INT HUP; while :; do sleep 1; done`)
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" fetch`+sttyCheck, 0)
	s.waitFor(`\x1b\]9;4;1;0\x07`)
	s.write("\x03")
	s.waitFor(`Interrupted: no new work`)
	s.write("\x03")
	s.waitFor(`interrupted again`)
	wantTerminalRestored(t, s, "130")
	wantTerminalProgressReset(t, s.output())
}

// Without the TUI (--no-tui) nothing drives the terminal's progress
// indicator nor sets the window title.
func TestPlainFetchOnPTYLeavesTerminalProgressAlone(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	s := startPTYShell(t, e.cwd, `"$SHEAF" --no-tui fetch; echo SHEAF-EXIT=$?`+readNext, 0)
	s.waitFor(`SHEAF-EXIT=0`)
	if all := terminalProgress.FindAllString(s.output(), -1); len(all) > 0 {
		t.Errorf("terminal progress sequences %q in plain mode", all)
	}
	if titles := windowTitles(s.output()); len(titles) > 0 {
		t.Errorf("window titles %q in plain mode", titles)
	}
}

func uintptrOf[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }
