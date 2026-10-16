//go:build linux

package cli

import (
	"regexp"
	"strings"
	"testing"
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
