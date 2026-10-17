//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/tui"
)

// sttyCheck ends a pty script: sheaf's exit status, whether the terminal
// settings saved in $B are back, and the line the shell reads next.
const sttyCheck = `; echo SHEAF-EXIT=$?; if [ "$(stty -g)" = "$B" ]; then echo STTY-SAME; else echo STTY-CHANGED; fi` + readNext

// wantTerminalRestored checks the end of a pty session: exit status,
// terminal settings and modes restored, nothing left in the input.
func wantTerminalRestored(t *testing.T, s *ptyShell, exit string) {
	t.Helper()
	s.waitFor(`STTY-(SAME|CHANGED)`)
	out := s.output()
	if !strings.Contains(out, "SHEAF-EXIT="+exit+"\r\n") {
		t.Errorf("want exit %s:\n%q", exit, out)
	}
	if !strings.Contains(out, "STTY-SAME") {
		t.Errorf("terminal settings not restored:\n%q", out)
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
	if last("\x1b[?1049h", "\x1b[?1049l") {
		t.Error("alternate screen left on")
	}
	if got := s.nextLine(); got != "zzz" {
		t.Fatalf("the shell read %q after sheaf, want zzz", got)
	}
}

func TestHomeOnPTYQuits(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
}

func TestHomeOnPTYCtrlCExits130(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
	if !strings.Contains(s.output(), "sheaf: interrupted") {
		t.Errorf("no interruption message:\n%q", s.output())
	}
}

// An action runs its own TUI (progress view) and the overview comes back,
// refreshed, below the summary.
func TestHomeOnPTYActionReturnsToOverview(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	s.waitFor(`fetch: done`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	out := s.output()
	if i, j := strings.Index(out, "1 done, 0 skipped, 0 failed"), strings.LastIndex(out, "behind 2"); i < 0 || j < i {
		t.Errorf("summary, then the refreshed overview expected:\n%q", out)
	}
}

func TestHomeOnPTYEnterWithoutShellFunctionPrintsPath(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("j\r")
	wantTerminalRestored(t, s, "0")
	if !strings.Contains(s.output(), feat+"\r\n") {
		t.Errorf("path not printed:\n%q", s.output())
	}
}

// homeCD runs the home screen through the shell function of shell-init
// in the shell started by run (a command line reading the shell code from
// sheaf and running `sheaf`, then printing RC and PWD), presses enter on
// the second row and checks that the shell is now in that checkout.
func homeCD(t *testing.T, run string) {
	t.Helper()
	e, _, feat, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `PATH="$(dirname "$SHEAF"):$PATH"; export PATH; B=$(stty -g); `+run+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("j\r")
	wantTerminalRestored(t, s, "0")
	if !strings.Contains(s.output(), "RC=0 PWD="+feat+"\r\n") {
		t.Errorf("the shell did not change to %s:\n%q", feat, s.output())
	}
	if strings.Contains(s.output(), feat+"\r\nRC=") {
		t.Errorf("the path was printed:\n%q", s.output())
	}
}

func TestHomeEnterCDsThroughShellFunctionBash(t *testing.T) {
	homeCD(t, `bash --norc --noprofile -c 'eval "$(sheaf shell-init bash)"; sheaf; echo "RC=$? PWD=$PWD"'`)
}

func TestHomeEnterCDsThroughShellFunctionZsh(t *testing.T) {
	zsh := shellBinary(t, "zsh")
	homeCD(t, `'`+zsh+`' -f -c '[[ -n $SHEAF_TEST_ZSH_MODULE_PATH ]] && module_path=(${(s.:.)SHEAF_TEST_ZSH_MODULE_PATH} $module_path)
eval "$(sheaf shell-init zsh)"; sheaf; echo "RC=$? PWD=$PWD"'`)
}

func TestHomeEnterCDsThroughShellFunctionFish(t *testing.T) {
	fish := shellBinary(t, "fish")
	homeCD(t, `'`+fish+`' --no-config -c 'sheaf shell-init fish | source; sheaf; echo "RC=$status PWD=$PWD"'`)
}

// waitScreenState waits until the terminal, emulated from the output of s,
// satisfies ok, and returns it.
func waitScreenState(t *testing.T, s *ptyShell, what string, ok func(vt *vtScreen) bool) *vtScreen {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		vt := newVTScreen(24, 100)
		vt.write(s.output())
		if ok(vt) {
			if len(vt.unknown) > 0 {
				t.Errorf("sequences not emulated: %v", vt.unknown)
			}
			return vt
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never showed %s: alternate screen %v\nnormal screen:\n%s\nalternate screen:\n%s",
				what, vt.altActive(), strings.Join(vt.normalLines(), "\n"), strings.Join(vt.altLines(), "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The overview is on the alternate screen, from its top, and actions run on
// the normal screen, where their output stays: after an action the next
// overview starts on a clear alternate screen, and enter ends sheaf with
// the normal screen back and the path printed on it.
func TestHomeOnPTYOverviewOnAlternateScreen(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); echo before-sheaf; "$SHEAF"`+sttyCheck, 0)
	// overviewShown tells whether the alternate screen shows the overview
	// from its top, with the message line message unless empty, and the
	// normal screen no part of it.
	overviewShown := func(message string) func(vt *vtScreen) bool {
		return func(vt *vtScreen) bool {
			alt, normal := vt.altLines(), strings.Join(vt.normalLines(), "\n")
			return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") &&
				strings.Count(strings.Join(alt, "\n"), "STATE") == 1 && (message == "" || slices.Contains(alt, message)) &&
				strings.Contains(normal, "before-sheaf") && !strings.Contains(normal, "STATE")
		}
	}
	waitScreenState(t, s, "the first overview on the alternate screen", overviewShown(""))
	s.write("f")
	vt := waitScreenState(t, s, "the overview after fetch on the alternate screen", overviewShown("fetch: done"))
	if normal := strings.Join(vt.normalLines(), "\n"); !strings.Contains(normal, "Fetching: 1 done, 0 skipped, 0 failed") {
		t.Errorf("the summary of the fetch is not on the normal screen:\n%s", normal)
	}
	s.write("j\r")
	wantTerminalRestored(t, s, "0")
	vt = newVTScreen(24, 100)
	vt.write(s.output())
	normal := vt.normalLines()
	if vt.altActive() || !slices.Contains(normal, feat) || !slices.Contains(normal, "before-sheaf") || slices.ContainsFunc(normal, func(l string) bool { return strings.Contains(l, "STATE") }) {
		t.Errorf("after enter: alternate screen %v, want the normal screen with the path %s and no overview:\n%s", vt.altActive(), feat, strings.Join(normal, "\n"))
	}
}

// After a failed action, the normal screen shows the error and a prompt
// until a key is pressed; the overview then comes back with the failure as
// message. ctrl+c at the prompt interrupts sheaf (exit 130).
func TestHomeOnPTYFailurePausesOnNormalScreen(t *testing.T) {
	for _, tc := range []struct{ key, exit string }{{"x", "0"}, {"\x03", "130"}} {
		e, _, _, _, _ := homeBundle(t)
		s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
		s.waitFor(`STATE`)
		s.write("w")
		s.waitFor(`New worktree of alpha`)
		s.write("main\r") // the primary's branch: wt add refuses it
		waitScreenState(t, s, "the error and the pause prompt on the normal screen", func(vt *vtScreen) bool {
			normal := vt.normalLines()
			i := slices.IndexFunc(normal, func(l string) bool { return strings.HasPrefix(l, "sheaf: ") && !strings.HasPrefix(l, "sheaf: /") })
			return !vt.altActive() && i >= 0 && slices.Contains(normal[i:], homePauseText)
		})
		if tc.exit == "0" {
			time.Sleep(tui.PauseGrace) // keys are ignored during the grace period
		}
		s.write(tc.key)
		if tc.exit == "0" {
			waitScreenState(t, s, "the overview with the failure", func(vt *vtScreen) bool {
				alt := vt.altLines()
				return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") &&
					slices.ContainsFunc(alt, func(l string) bool { return strings.HasPrefix(l, "add failed: ") })
			})
			s.write("q")
		}
		wantTerminalRestored(t, s, tc.exit)
		if tc.exit == "130" && !strings.Contains(s.output(), "sheaf: interrupted") {
			t.Errorf("no interruption message:\n%q", s.output())
		}
	}
}

// A key typed while the failing action runs does not skip the pause: it
// still shows the error and the prompt after the grace period, and a key
// pressed then returns to the overview.
func TestHomeOnPTYTypeAheadDoesNotSkipPause(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	s.write("main\r") // the primary's branch: wt add refuses it
	time.Sleep(50 * time.Millisecond)
	s.write("x")
	pauseShown := func(vt *vtScreen) bool {
		return !vt.altActive() && slices.Contains(vt.normalLines(), homePauseText)
	}
	waitScreenState(t, s, "the pause prompt", pauseShown)
	time.Sleep(2 * tui.PauseGrace)
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	if !pauseShown(vt) {
		t.Fatalf("the key typed ahead ended the pause: alternate screen %v\nnormal screen:\n%s", vt.altActive(), strings.Join(vt.normalLines(), "\n"))
	}
	s.write("x")
	waitScreenState(t, s, "the overview with the failure", func(vt *vtScreen) bool {
		return vt.altActive() && slices.ContainsFunc(vt.altLines(), func(l string) bool { return strings.HasPrefix(l, "add failed: ") })
	})
	s.write("q")
	wantTerminalRestored(t, s, "0")
}

// The overview leaves no copy of itself in the terminal (screen and
// scrollback, as a terminal emulator shows them) when an action runs or
// sheaf quits: after several actions only the last overview is visible,
// and none after quitting. The session starts low on the screen, so that
// the outputs scroll.
func TestHomeOnPTYLeavesNoStaleOverviews(t *testing.T) {
	e, _, feat, _, _ := homeBundle(t)
	if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil {
		t.Fatal(err)
	}
	s := startPTYShell(t, e.cwd, `B=$(stty -g); i=0; while [ $i -lt 20 ]; do echo line$i; i=$((i+1)); done; "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	waitCount := func(marker string, n int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for strings.Count(s.output(), marker) < n {
			if time.Now().After(deadline) {
				t.Fatalf("output never contained %q %d times:\n%q", marker, n, s.output())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// counts returns how many lines of each part of an overview (header,
	// rows, status, help) the terminal shows, screen and scrollback, and
	// checks that the scrollback holds what scrolled off (the lines printed
	// before sheaf, the summaries of the actions so far).
	rowLine := regexp.MustCompile(`^(> |  )\[[ x]\] `)
	statusLine := regexp.MustCompile(`^\d+/\d+ selected`)
	counts := func(summaries ...string) [4]int {
		t.Helper()
		vt := newVTScreen(24, 100)
		vt.write(s.output())
		if len(vt.unknown) > 0 {
			t.Errorf("sequences not emulated: %v", vt.unknown)
		}
		var n [4]int
		text := strings.Join(vt.lines(), "\n")
		for _, l := range vt.lines() {
			switch {
			case strings.Contains(l, "REPO") && strings.Contains(l, "STATE"):
				n[0]++
			case rowLine.MatchString(l):
				n[1]++
			case statusLine.MatchString(l):
				n[2]++
			case strings.Contains(l, "esc/q quit"):
				n[3]++
			}
		}
		at := 0
		for _, want := range append([]string{"line0\n", "line19\n"}, summaries...) {
			i := strings.Index(text[at:], want)
			if i < 0 {
				t.Errorf("the terminal lacks %q after offset %d:\n%s", want, at, text)
				break
			}
			at += i + len(want)
		}
		return n
	}
	summaries := []string{"Fetching: 1 done", "Updating: 1 done", "Checking worktree feat of alpha for removal: 0 done, 0 skipped, 0 failed, 1 cancelled", "Fetching: 1 done"}
	s.write("f")
	waitCount("fetch: done", 1)
	s.write("u")
	waitCount("update: done", 1)
	s.write("j")
	s.write("r")
	waitCount("[y/N]", 1)
	s.write("n")
	waitCount("remove: cancelled", 1)
	s.write("f")
	waitCount("fetch: done", 2)
	// One overview: its header, 3 rows, status and help lines.
	if n := counts(summaries...); n != [4]int{1, 3, 1, 1} {
		t.Errorf("after 4 actions the terminal shows %d header, %d row, %d status and %d help lines of overviews, want 1, 3, 1, 1", n[0], n[1], n[2], n[3])
	}
	s.write("q")
	wantTerminalRestored(t, s, "0")
	if n := counts(summaries...); n != [4]int{} {
		t.Errorf("after quitting the terminal shows %d header, %d row, %d status and %d help lines of overviews, want none", n[0], n[1], n[2], n[3])
	}
}
