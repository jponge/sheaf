//go:build linux

package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
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
	for _, mode := range []string{"1000", "1002", "1003", "1006"} {
		if last("\x1b[?"+mode+"h", "\x1b[?"+mode+"l") {
			t.Errorf("mouse mode %s left on", mode)
		}
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

// dismissReport waits until the output of s contains the title of the
// report view of the action name n times (window titles, which name the
// action too, do not count), then for the grace period of the view, and
// presses a key.
func dismissReport(t *testing.T, s *ptyShell, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(windowTitleSeq.ReplaceAllString(s.output(), ""), "sheaf "+name+": ") < n {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained the report view of %s %d times:\n%q", name, n, s.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(tui.ReportGrace)
	s.write("q")
}

// An action runs its own TUI (progress view), then its report view, and the
// overview comes back, refreshed, below the summary.
func TestHomeOnPTYActionReturnsToOverview(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	dismissReport(t, s, "fetch", 1)
	s.waitFor(`fetch: done`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	out := s.output()
	if i, j := strings.Index(out, "1 done, 0 skipped, 0 failed"), strings.LastIndex(out, "↓2"); i < 0 || j < i {
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
	return waitScreenStateRows(t, s, 24, what, ok)
}

// waitScreenStateRows is waitScreenState for a terminal of rows rows.
func waitScreenStateRows(t *testing.T, s *ptyShell, rows int, what string, ok func(vt *vtScreen) bool) *vtScreen {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		vt := newVTScreen(rows, 100)
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
	// from its top, with the message message on its status line unless
	// empty, and the normal screen no part of it.
	overviewShown := func(message string) func(vt *vtScreen) bool {
		return func(vt *vtScreen) bool {
			alt, normal := vt.altLines(), strings.Join(vt.normalLines(), "\n")
			return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") &&
				strings.Count(strings.Join(alt, "\n"), "STATE") == 1 && (message == "" || slices.ContainsFunc(alt, func(l string) bool { return overviewMessage(l) == message })) &&
				strings.Contains(normal, "before-sheaf") && !strings.Contains(normal, "STATE")
		}
	}
	waitScreenState(t, s, "the first overview on the alternate screen", overviewShown(""))
	s.write("f")
	// The summary of the fetch is printed on the normal screen, below its
	// progress view, and shown in its report view on the alternate screen.
	waitScreenState(t, s, "the report view of fetch on the alternate screen", func(vt *vtScreen) bool {
		alt, normal := vt.altLines(), strings.Join(vt.normalLines(), "\n")
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf fetch: ") && slices.Contains(alt, "done     alpha: fetched origin") &&
			strings.Contains(normal, "Fetching: finished in") && strings.Contains(normal, "done     alpha: fetched origin")
	})
	time.Sleep(tui.ReportGrace)
	s.write("q")
	vt := waitScreenState(t, s, "the overview after fetch on the alternate screen", overviewShown("fetch: done"))
	if normal := strings.Join(vt.normalLines(), "\n"); !strings.Contains(normal, "Fetching: finished in") ||
		strings.Index(normal, "done     alpha: fetched origin") < strings.Index(normal, "Fetching: finished in") {
		t.Errorf("the normal screen lacks the progress view of the fetch and its summary below it:\n%s", normal)
	}
	s.write("j\r")
	wantTerminalRestored(t, s, "0")
	vt = newVTScreen(24, 100)
	vt.write(s.output())
	normal := vt.normalLogicalLines() // the path may be wider than the terminal (B55)
	if vt.altActive() || !slices.Contains(normal, feat) || !slices.Contains(normal, "before-sheaf") || slices.ContainsFunc(normal, func(l string) bool { return strings.Contains(l, "STATE") }) {
		t.Errorf("after enter: alternate screen %v, want the normal screen with the path %s and no overview:\n%s", vt.altActive(), feat, strings.Join(normal, "\n"))
	}
}

// After a failed action, the error is printed on the normal screen and
// shown in the report view on the alternate screen until a key is pressed;
// the overview then comes back with the failure as message. ctrl+c in the
// view interrupts sheaf (exit 130).
func TestHomeOnPTYFailureShowsReportView(t *testing.T) {
	for _, tc := range []struct{ key, exit string }{{"q", "0"}, {"\x03", "130"}} {
		e, _, _, _, _ := homeBundle(t)
		s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
		s.waitFor(`STATE`)
		s.write("w")
		s.waitFor(`New worktree of alpha`)
		s.write("main\r") // the primary's branch: wt add refuses it
		waitScreenState(t, s, "the error in the report view", func(vt *vtScreen) bool {
			alt := vt.altLines()
			isError := func(l string) bool { return strings.HasPrefix(l, "sheaf: ") && !strings.HasPrefix(l, "sheaf: /") }
			return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf wt add: ") && slices.ContainsFunc(alt, isError) &&
				slices.ContainsFunc(vt.normalLines(), isError)
		})
		if tc.exit == "0" {
			time.Sleep(tui.ReportGrace) // keys are ignored during the grace period
		}
		s.write(tc.key)
		if tc.exit == "0" {
			waitScreenState(t, s, "the overview with the failure", func(vt *vtScreen) bool {
				alt := vt.altLines()
				return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") &&
					slices.ContainsFunc(alt, func(l string) bool { return strings.HasPrefix(overviewMessage(l), "wt add failed: ") })
			})
			s.write("q")
		}
		wantTerminalRestored(t, s, tc.exit)
		if tc.exit == "130" && !strings.Contains(s.output(), "sheaf: interrupted") {
			t.Errorf("no interruption message:\n%q", s.output())
		}
	}
}

// A key typed while the failing action runs does not skip the report view:
// it still shows the error after the grace period, and a key pressed then
// returns to the overview.
func TestHomeOnPTYTypeAheadDoesNotSkipReport(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	s.write("main\r") // the primary's branch: wt add refuses it
	time.Sleep(50 * time.Millisecond)
	s.write("q")
	reportShown := func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf wt add: ")
	}
	waitScreenState(t, s, "the report view", reportShown)
	time.Sleep(2 * tui.ReportGrace)
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	if !reportShown(vt) {
		t.Fatalf("the key typed ahead ended the report view: alternate screen %v\nalternate screen:\n%s", vt.altActive(), strings.Join(vt.altLines(), "\n"))
	}
	s.write("q")
	waitScreenState(t, s, "the overview with the failure", func(vt *vtScreen) bool {
		return vt.altActive() && slices.ContainsFunc(vt.altLines(), func(l string) bool { return strings.HasPrefix(overviewMessage(l), "wt add failed: ") })
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
			case strings.Contains(l, "? more"):
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
	summaries := []string{"Fetching: finished in", "Updating: finished in", "Checking worktree feat of alpha for removal: cancelled after", "Fetching: finished in"}
	s.write("f")
	dismissReport(t, s, "fetch", 1)
	waitCount("fetch: done", 1)
	s.write("u")
	dismissReport(t, s, "update", 1)
	waitCount("update: done", 1)
	s.write("j")
	s.write("r")
	waitCount("[y/N]", 1)
	s.write("n")
	waitCount("wt rm: cancelled", 1)
	s.write("f")
	dismissReport(t, s, "fetch", 2)
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

// toolsDir returns a directory holding links to the given tools, to be
// used as PATH (e.g. without gh, so that the real gh never runs).
func toolsDir(t *testing.T, tools ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// doctorReport runs the home screen of e on a 10-row terminal without gh on
// PATH (doctor warns) and presses d.
func doctorReport(t *testing.T, e *env) *ptyShell {
	t.Helper()
	bin := toolsDir(t, "git", "stty")
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`'; export PATH; stty rows 10; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("d")
	return s
}

var (
	doctorLine   = regexp.MustCompile(`^(OK|INFO|WARN|FAIL) +\S`)
	reportStatus = regexp.MustCompile(`^lines (\d+)-(\d+) of (\d+)$`)
)

func isDoctorLine(l string) bool { return doctorLine.MatchString(l) }

// isTitle reports whether the line l of a view is the title title, whole
// or, when wider than the terminal, cut with an ellipsis (B52 E2, B55).
func isTitle(l, title string) bool {
	head, cut := strings.CutSuffix(l, "…")
	return l == title || cut && head != "" && strings.HasPrefix(title, head)
}

func TestIsTitle(t *testing.T) {
	t.Parallel()
	title := "sheaf doctor: /tmp/b"
	for l, want := range map[string]bool{
		title: true, "sheaf doctor: /t…": true, "sheaf doctor: /tmp/b2": false,
		"sheaf doctor: /x…": false, "…": false, "sheaf doctor: /t": false, "sheaf: /tmp/b": false,
	} {
		if got := isTitle(l, title); got != want {
			t.Errorf("isTitle(%q) = %v, want %v", l, got, want)
		}
	}
}

// overviewStatus matches the status line of an overview (filtered and
// scrolled or not) followed by a message.
var overviewStatus = regexp.MustCompile(`^\d+/\d+ selected(?: · \d+ shown)?(?: · rows \d+-\d+ of \d+)? · (.+)$`)

// overviewMessage returns the message of the overview status line l, ""
// when l is not such a line.
func overviewMessage(l string) string {
	if m := overviewStatus.FindStringSubmatch(l); m != nil {
		return m[1]
	}
	return ""
}

// doctorView waits until the 10-row terminal of s shows the report view of
// doctor (bundle root) with a status line, and ok accepts its status line
// (submatches of reportStatus) and its report lines; it returns the
// terminal.
func doctorView(t *testing.T, s *ptyShell, root, what string, ok func(status, lines []string) bool) *vtScreen {
	t.Helper()
	return waitScreenStateRows(t, s, 10, what, func(vt *vtScreen) bool {
		alt := vt.altLines()
		if !vt.altActive() || len(alt) == 0 || !isTitle(alt[0], "sheaf doctor: "+root) {
			return false
		}
		var st, lines []string
		for _, l := range alt {
			if m := reportStatus.FindStringSubmatch(l); m != nil {
				st = m
			}
			if isDoctorLine(l) || strings.HasPrefix(l, "      hint: ") {
				lines = append(lines, l)
			}
		}
		return st != nil && ok(st, lines)
	})
}

// The report of doctor is printed on the normal screen as it runs (as the
// command prints it) and shown in the report view on the alternate screen,
// scrolled when longer than the window (here 10 rows): a key returns to
// the overview, and after quitting the printed report is still on the
// normal screen.
func TestHomeOnPTYDoctorReportScrolls(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := doctorReport(t, e)
	var first string
	vt := doctorView(t, s, e.cwd, "the top of the report", func(st, lines []string) bool {
		if st[1] != "1" || st[2] == st[3] || len(lines) == 0 {
			return false
		}
		first = lines[0]
		return true
	})
	if normal := vt.normalLines(); !slices.Contains(normal, first) {
		t.Errorf("the normal screen lacks the report line %q:\n%s", first, strings.Join(normal, "\n"))
	}
	s.write("\x1b[F") // end
	doctorView(t, s, e.cwd, "the end of the report", func(st, lines []string) bool {
		return st[1] != "1" && st[2] == st[3] && !slices.Contains(lines, first)
	})
	s.write("\x1b[H") // home
	doctorView(t, s, e.cwd, "the top of the report again", func(st, lines []string) bool {
		return st[1] == "1" && len(lines) > 0 && lines[0] == first
	})
	time.Sleep(tui.ReportGrace)
	s.write("q")
	waitScreenStateRows(t, s, 10, "the overview after doctor", func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") && slices.ContainsFunc(alt, func(l string) bool { return overviewMessage(l) == "doctor: done" })
	})
	s.write("q")
	wantTerminalRestored(t, s, "0")
	vt = newVTScreen(10, 100)
	vt.write(s.output())
	if !slices.Contains(vt.lines(), first) {
		t.Errorf("the printed report is not left in the terminal after quitting:\n%s", strings.Join(vt.lines(), "\n"))
	}
}

// Scrolling the report view line by line gives the same screen whatever the
// way: j j k and home j both show lines 2-5.
func TestHomeOnPTYReportScrollsLineByLine(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := doctorReport(t, e)
	from := func(n string) func(st, _ []string) bool {
		return func(st, _ []string) bool { return st[1] == n }
	}
	doctorView(t, s, e.cwd, "the top of the report", from("1"))
	s.write("j")
	doctorView(t, s, e.cwd, "lines 2-", from("2"))
	s.write("j")
	doctorView(t, s, e.cwd, "lines 3-", from("3"))
	s.write("k")
	a := doctorView(t, s, e.cwd, "lines 2- after j j k", from("2")).altLines()
	s.write("\x1b[H") // home
	doctorView(t, s, e.cwd, "lines 1- after home", from("1"))
	s.write("j")
	b := doctorView(t, s, e.cwd, "lines 2- after home j", from("2")).altLines()
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("j j k and home j show different screens:\n%s\n---\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if !slices.ContainsFunc(a, func(l string) bool { return strings.HasPrefix(l, "lines 2-5 of ") }) {
		t.Errorf("no status line \"lines 2-5 of ...\":\n%s", strings.Join(a, "\n"))
	}
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
}

// The progress lines that wt add prints as it runs appear on the normal
// screen (live, as the command prints them), then in the report view.
func TestHomeOnPTYAddProgressIsLive(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	s.write("topic/live\r")
	progress := regexp.MustCompile(`^alpha: \w+: \S`)
	isProgress := func(l string) bool { return progress.MatchString(l) }
	waitScreenState(t, s, "the progress lines on the normal screen and in the report view", func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf wt add: ") && slices.ContainsFunc(alt, isProgress) &&
			slices.ContainsFunc(vt.normalLines(), isProgress)
	})
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
}

// An action interrupted with ctrl+c leaves what its command printed on the
// normal screen, as the command alone would: here the summary of a fetch
// whose git processes were killed, interrupted (B52 A6), still there after
// the report view and quitting (B62 H4: the first ctrl+c cancels the
// action only).
func TestHomeOnPTYInterruptedActionLeavesItsOutput(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = fetch ] && sleep 30; done\nexec '"+git+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("a")
	s.write("f")
	s.waitFor(`Fetching`)
	time.Sleep(200 * time.Millisecond)
	s.write("\x03")
	dismissReport(t, s, "fetch", 1)
	s.waitFor(`fetch: interrupted`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	normal := vt.normalLines()
	n := 0
	for _, l := range normal {
		if l == "0 done, 0 skipped, 0 failed, 2 interrupted" {
			n++
		}
	}
	if n != 1 || !slices.ContainsFunc(normal, func(l string) bool { return l == "interrupted alpha" }) {
		t.Errorf("the summary of the interrupted fetch is not on the normal screen once (%d times):\n%s", n, strings.Join(normal, "\n"))
	}
}

// windowTitleSeq matches the sequences setting the window title (OSC 0
// and 2); the submatch is the title.
var windowTitleSeq = regexp.MustCompile(`\x1b\][02];([^\x07\x1b]*)(?:\x07|\x1b\\)`)

// windowTitles returns the window titles that out sets, in order.
func windowTitles(out string) []string {
	var titles []string
	for _, m := range windowTitleSeq.FindAllStringSubmatch(out, -1) {
		titles = append(titles, m[1])
	}
	return titles
}

// The home screen sets the window title (B42): "sheaf: <bundle>" in the
// overview and the report views, "sheaf <action>: <bundle>" in the programs
// of an action (here the progress views of fetch and update, the prompt of
// add); bubbletea clears it when each program ends, hence last.
func TestHomeOnPTYSetsWindowTitle(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	b := filepath.Base(e.cwd)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	dismissReport(t, s, "fetch", 1)
	s.waitFor(`fetch: done`)
	s.write("u")
	dismissReport(t, s, "update", 1)
	s.waitFor(`update: done`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	s.write("\x1b")
	s.waitFor(`wt add: cancelled`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	titles := windowTitles(s.output())
	for _, want := range []string{"sheaf: " + b, "sheaf fetch: " + b, "sheaf update: " + b, "sheaf wt add: " + b} {
		if !slices.Contains(titles, want) {
			t.Errorf("window titles %q lack %q", titles, want)
		}
	}
	for _, got := range titles {
		if got != "" && !strings.HasPrefix(got, "sheaf") || !strings.HasSuffix(got, ": "+b) && got != "" {
			t.Errorf("unexpected window title %q in %q", got, titles)
		}
	}
	if len(titles) == 0 || titles[len(titles)-1] != "" {
		t.Errorf("window titles %q: want the last one cleared", titles)
	}
}

// titleControls matches the sequences about the window title: setting it
// (OSC 0, 2), saving and restoring it (XTWINOPS 22, 23).
var titleControls = regexp.MustCompile(`\x1b\][02];[^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[2[23];\d*t`)

// wantWindowTitleRestored checks that out saves the window title before
// setting it the first time, and restores it last.
func wantWindowTitleRestored(t *testing.T, out string) {
	t.Helper()
	all := titleControls.FindAllString(out, -1)
	if len(all) < 3 || all[0] != "\x1b[22;0t" || all[len(all)-1] != "\x1b[23;0t" || !strings.HasPrefix(all[1], "\x1b]2;sheaf") {
		t.Errorf("title sequences %q: want the save first, then titles, the restore last", all)
	}
	if strings.Count(out, "\x1b[22;0t") != 1 || strings.Count(out, "\x1b[23;0t") != 1 {
		t.Errorf("title sequences %q: want one save and one restore", all)
	}
}

// The home screen saves the window title of the terminal before its
// programs set theirs, and restores it once the last one has ended (D2),
// on every way out: quit, enter (cd), ctrl+c (130), an error, the
// emergency stop.
func TestHomeOnPTYRestoresWindowTitle(t *testing.T) {
	for _, c := range []struct {
		name, exit string
		path       string // shell code of a git wrapper (see fakeFetchGit), if any
		drive      func(t *testing.T, s *ptyShell, root string)
	}{
		{"quit", "0", "", func(t *testing.T, s *ptyShell, _ string) { s.write("q") }},
		{"cd", "0", "", func(t *testing.T, s *ptyShell, _ string) { s.write("\r") }},
		{"ctrl+c", "130", "", func(t *testing.T, s *ptyShell, _ string) { s.write("\x03") }},
		{"error", "1", "", func(t *testing.T, s *ptyShell, root string) {
			// The next overview cannot read the bundle.
			if err := os.Rename(filepath.Join(root, "sheaf.yaml"), filepath.Join(root, "moved.yaml")); err != nil {
				t.Fatal(err)
			}
			s.write("f")
			dismissReport(t, s, "fetch", 1)
		}},
		{"emergency stop", "130", `trap '' TERM INT HUP; while :; do sleep 1; done`, func(t *testing.T, s *ptyShell, _ string) {
			s.write("f")
			s.waitFor(`Fetching`)
			s.write("\x03")
			s.waitFor(`Interrupted: no new work`)
			s.write("\x03")
			s.waitFor(`interrupted again`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _, _ := homeBundle(t)
			path := ""
			if c.path != "" {
				path = `PATH='` + fakeFetchGit(t, c.path) + `':$PATH; export PATH; `
			}
			s := startPTYShell(t, e.cwd, path+`B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			c.drive(t, s, e.cwd)
			wantTerminalRestored(t, s, c.exit)
			wantWindowTitleRestored(t, s.output())
		})
	}
}

// With TERM=dumb, nothing touches the window title nor the progress
// indicator (F2).
func TestHomeOnPTYDumbTerminalSendsNoWindowControls(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin := fakeFetchGit(t, `sleep 30`)
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); TERM=dumb "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	s.waitFor(`Fetching · 1 running`)
	s.write("\x03")
	dismissReport(t, s, "fetch", 1)
	s.waitFor(`fetch: interrupted`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	if all := titleControls.FindAllString(s.output(), -1); len(all) > 0 {
		t.Errorf("title sequences %q with TERM=dumb", all)
	}
	if all := terminalProgress.FindAllString(s.output(), -1); len(all) > 0 {
		t.Errorf("terminal progress sequences %q with TERM=dumb", all)
	}
}

// y copies the path of the current checkout to the clipboard with OSC 52
// (B44) and says so in the overview's message; with TERM=dumb it is
// refused and nothing is sent.
func TestHomeOnPTYCopiesPathWithOSC52(t *testing.T) {
	for _, c := range []struct {
		term, message string
		sent          bool
	}{
		{"xterm-256color", "sent to the clipboard: ", true},
		{"dumb", "copy not available: the output is not a terminal, or TERM=dumb", false},
	} {
		t.Run(c.term, func(t *testing.T) {
			e, alpha, _, _, _ := homeBundle(t)
			s := startPTYShell(t, e.cwd, `B=$(stty -g); TERM=`+c.term+` "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			s.write("y")
			s.waitFor(regexp.QuoteMeta(c.message))
			s.write("q")
			wantTerminalRestored(t, s, "0")
			want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(alpha)) + "\x07"
			if got := regexp.MustCompile(`\x1b\]52;[^\x07\x1b]*(?:\x07|\x1b\\)`).FindAllString(s.output(), -1); c.sent != (len(got) == 1 && got[0] == want) || !c.sent && len(got) > 0 {
				t.Errorf("OSC 52 sequences %q, want %q sent: %v", got, want, c.sent)
			}
		})
	}
}

// On a terminal, the repository names of the overview are hyperlinks to
// their GitHub page (OSC 8, B46), through bubbletea's renderer, and the
// last hyperlink sequence ends the link; with TERM=dumb, none is sent.
func TestHomeOnPTYLinksRepositories(t *testing.T) {
	for _, c := range []struct {
		term   string
		linked bool
	}{{"xterm-256color", true}, {"dumb", false}} {
		t.Run(c.term, func(t *testing.T) {
			e, _, _, _, _ := homeBundle(t)
			s := startPTYShell(t, e.cwd, `B=$(stty -g); TERM=`+c.term+` "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			s.write("q")
			wantTerminalRestored(t, s, "0")
			out := s.output()
			all := regexp.MustCompile(`\x1b\]8;[^;\x07\x1b]*;([^\x07\x1b]*)(?:\x07|\x1b\\)`).FindAllStringSubmatch(out, -1)
			if !c.linked {
				if len(all) > 0 {
					t.Errorf("hyperlinks with TERM=dumb: %q", all)
				}
				return
			}
			for _, repo := range []string{"alpha", "beta"} {
				if !strings.Contains(out, "\x1b]8;;https://github.com/o/"+repo+"\x07"+repo) {
					t.Errorf("no hyperlink on %s:\n%q", repo, out)
				}
			}
			if len(all) == 0 || all[len(all)-1][1] != "" {
				t.Errorf("hyperlink left open at the end: %q", all)
			}
		})
	}
}

// The w prompt opens, takes input and is cancelled while the git process
// reading its suggestions (B47) still runs: they are loaded in the
// background.
func TestHomeOnPTYPromptDoesNotWaitForSuggestions(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// The suggestions are the only for-each-ref of refs/tags and
	// refs/remotes/origin.
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\ntags=; origin=\nfor a in \"$@\"; do case $a in refs/tags) tags=1;; refs/remotes/origin) origin=1;; esac; done\nif [ \"$tags$origin\" = 11 ]; then sleep 60; fi\nexec '"+git+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	s.write("f")
	waitScreenState(t, s, "the prompt with f typed", func(vt *vtScreen) bool {
		return slices.Contains(vt.normalLines(), "branch: f█")
	})
	s.write("\x1b")
	s.waitFor(`wt add: cancelled`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	if d := time.Since(start); d > 45*time.Second {
		t.Errorf("the session took %v: the prompt waited for its suggestions", d)
	}
}

// The git process reading the suggestions of the w prompt (B47) is stopped
// once the prompt is answered, before wt add runs: when wt add checks the
// branch name, the process (still loading, here) is gone (waited for up
// to 5 seconds, as stopping it takes a moment).
func TestHomeOnPTYSuggestionsStoppedBeforeWtAdd(t *testing.T) {
	e, alpha, _, _, _ := homeBundle(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, state := t.TempDir(), t.TempDir()
	pid, seen := filepath.Join(state, "suggest.pid"), filepath.Join(state, "seen")
	// The suggestions are the only for-each-ref of refs/tags and
	// refs/remotes/origin; wt add starts with check-ref-format --branch.
	writeFile(t, filepath.Join(bin, "git"), `#!/bin/sh
tags=; origin=; check=
for a in "$@"; do case $a in refs/tags) tags=1;; refs/remotes/origin) origin=1;; --branch) check=1;; esac; done
if [ "$tags$origin" = 11 ]; then echo $$ > '`+pid+`'; exec sleep 60; fi
if [ -n "$check" ] && [ -s '`+pid+`' ] && [ ! -e '`+seen+`' ]; then
	i=0
	while kill -0 "$(cat '`+pid+`')" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
	if kill -0 "$(cat '`+pid+`')" 2>/dev/null; then echo running > '`+seen+`'; else echo stopped > '`+seen+`'; fi
fi
exec '`+git+`' "$@"
`)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	for deadline := time.Now().Add(30 * time.Second); !exists(pid); { // the suggestions are loading
		if time.Now().After(deadline) {
			t.Fatal("the suggestions were never read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.write("xy\r")
	dismissReport(t, s, "wt add", 1)
	s.waitFor(`wt add: done`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	if !exists(filepath.Join(filepath.Dir(alpha), "xy", ".git")) {
		t.Fatalf("worktree not added:\n%q", s.output())
	}
	got, err := os.ReadFile(seen)
	if err != nil || string(got) != "stopped\n" {
		t.Errorf("suggestion process when wt add started: %q, %v", got, err)
	}
}

// Mouse wheel reports (SGR, as the terminal sends them once asked) move the
// cursor of the overview by up to 3 rows (B49); mouse reporting is on while
// the overview is displayed, as cell motion with SGR encoding (DECSET 1002
// and 1006, not 1003), and off while an action's prompt is.
func TestHomeOnPTYWheelMovesTheCursor(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	current := func(repo string) func(vt *vtScreen) bool {
		return func(vt *vtScreen) bool {
			return vt.altActive() && slices.ContainsFunc(vt.altLines(), func(l string) bool { return strings.HasPrefix(l, "> [ ] "+repo) })
		}
	}
	waitScreenState(t, s, "the cursor on alpha", current("alpha"))
	s.write("\x1b[<65;20;6M") // wheel down: 3 rows, the last of the 3 rows
	waitScreenState(t, s, "the cursor on beta after a wheel turn down", current("beta"))
	s.write("\x1b[<64;20;6M") // wheel up
	waitScreenState(t, s, "the cursor on alpha after a wheel turn up", current("alpha"))
	out := s.output()
	if !strings.Contains(out, "\x1b[?1002h") || !strings.Contains(out, "\x1b[?1006h") || strings.Contains(out, "\x1b[?1003h") {
		t.Errorf("want mouse modes 1002 and 1006 on, not 1003:\n%q", out)
	}
	s.write("w")
	s.waitFor(`New worktree of alpha`)
	out = s.output()
	if prompt := strings.LastIndex(out, "New worktree of alpha"); strings.LastIndex(out[:prompt], "\x1b[?1002h") > strings.LastIndex(out[:prompt], "\x1b[?1002l") {
		t.Errorf("mouse reporting on while the prompt is displayed:\n%q", out)
	}
	s.write("\x1b")
	s.waitFor(`wt add: cancelled`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
}

// Mouse wheel reports scroll the report view by 3 lines (B49).
func TestHomeOnPTYWheelScrollsTheReport(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := doctorReport(t, e)
	from := func(n string) func(st, _ []string) bool {
		return func(st, _ []string) bool { return st[1] == n }
	}
	doctorView(t, s, e.cwd, "the top of the report", from("1"))
	s.write("\x1b[<65;20;6M")
	doctorView(t, s, e.cwd, "lines 4- after a wheel turn down", from("4"))
	s.write("\x1b[<64;20;6M")
	doctorView(t, s, e.cwd, "lines 1- after a wheel turn up", from("1"))
	time.Sleep(tui.ReportGrace)
	s.write("\x1b[<65;20;6M\x1b[<0;20;6M\x1b[<0;20;6m") // wheel and click: the report stays
	doctorView(t, s, e.cwd, "lines 4- after the grace period", from("4"))
	s.write("q")
	waitScreenStateRows(t, s, 10, "the overview after doctor", func(vt *vtScreen) bool {
		return vt.altActive() && slices.ContainsFunc(vt.altLines(), func(l string) bool { return overviewMessage(l) == "doctor: done" })
	})
	s.write("q")
	wantTerminalRestored(t, s, "0")
}

// childPID returns the PID of the only child of process pid.
func childPID(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
		if err != nil {
			t.Fatal(err)
		}
		if f := strings.Fields(string(b)); len(f) == 1 {
			n, err := strconv.Atoi(f[0])
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("children of %d: %q", pid, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Mouse reporting, turned on by the overview and the report view, is off
// when sheaf ends, on every way out (wantTerminalRestored checks it): quit,
// enter (cd), ctrl+c in the overview and in the report view (130), an
// error, SIGTERM (130).
func TestHomeOnPTYMouseOffOnEveryExit(t *testing.T) {
	for _, c := range []struct {
		name, exit string
		drive      func(t *testing.T, s *ptyShell, root string)
	}{
		{"quit", "0", func(t *testing.T, s *ptyShell, _ string) { s.write("q") }},
		{"cd", "0", func(t *testing.T, s *ptyShell, _ string) { s.write("\r") }},
		{"ctrl+c", "130", func(t *testing.T, s *ptyShell, _ string) { s.write("\x03") }},
		{"ctrl+c in the report", "130", func(t *testing.T, s *ptyShell, _ string) {
			s.write("f")
			waitScreenState(t, s, "the report view of fetch", func(vt *vtScreen) bool {
				alt := vt.altLines()
				return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf fetch: ")
			})
			if n := strings.Count(s.output(), "\x1b[?1002h"); n != 2 {
				t.Errorf("mouse reporting turned on %d times, want 2 (overview, report view)", n)
			}
			s.write("\x03")
		}},
		{"error", "1", func(t *testing.T, s *ptyShell, root string) {
			if err := os.Rename(filepath.Join(root, "sheaf.yaml"), filepath.Join(root, "moved.yaml")); err != nil {
				t.Fatal(err)
			}
			s.write("f")
			dismissReport(t, s, "fetch", 1)
		}},
		{"SIGTERM", "130", func(t *testing.T, s *ptyShell, _ string) {
			if err := syscall.Kill(childPID(t, s.cmd.Process.Pid), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _, _ := homeBundle(t)
			s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			if !strings.Contains(s.output(), "\x1b[?1002h") {
				t.Fatalf("mouse reporting not on in the overview:\n%q", s.output())
			}
			c.drive(t, s, e.cwd)
			wantTerminalRestored(t, s, c.exit)
		})
	}
}

// Two signals close together while a program of the home screen is open
// (the overview, the prompt of an action) end sheaf with the terminal
// restored: the second one may arrive while the program quits after the
// first one (waiting up to drainTimeout for the terminal's answers), and
// the emergency stop must then release the program (review of B62 H4).
// The terminal answers its queries at once or after 100 ms (a quit drains
// the answers for at most 300 ms, two rounds: slower terminals get the
// last answer after sheaf ends, as when q quits).
func TestHomeOnPTYTwoSignalsRestoreTheTerminal(t *testing.T) {
	for _, c := range []struct {
		name  string
		keys  string // typed in the overview first, if any
		wait  string // shown after the keys
		sig   syscall.Signal
		delay time.Duration
	}{
		{"overview, one SIGTERM, slow terminal", "", "", syscall.SIGTERM, 100 * time.Millisecond},
		{"overview, SIGTERM", "", "", syscall.SIGTERM, 0},
		{"overview, SIGTERM, slow terminal", "", "", syscall.SIGTERM, 100 * time.Millisecond},
		{"overview, SIGINT", "", "", syscall.SIGINT, 0},
		{"overview, SIGINT, slow terminal", "", "", syscall.SIGINT, 100 * time.Millisecond},
		{"prompt, SIGINT", "w", `New worktree of alpha`, syscall.SIGINT, 0},
		{"prompt, SIGINT, slow terminal", "w", `New worktree of alpha`, syscall.SIGINT, 100 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _, _ := homeBundle(t)
			s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, c.delay)
			s.waitFor(`STATE`)
			if c.keys != "" {
				s.write(c.keys)
				s.waitFor(c.wait)
			}
			time.Sleep(c.delay + 50*time.Millisecond) // the startup queries answered
			pid := childPID(t, s.cmd.Process.Pid)
			if err := syscall.Kill(pid, c.sig); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(c.name, "overview, one ") {
				time.Sleep(30 * time.Millisecond)
				_ = syscall.Kill(pid, c.sig) // sheaf may have ended already
			}
			wantTerminalRestored(t, s, "130")
		})
	}
}

// With TERM=dumb, the home screen never turns mouse reporting on (B49).
func TestHomeOnPTYDumbTerminalEnablesNoMouse(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); TERM=dumb "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	dismissReport(t, s, "fetch", 1)
	s.waitFor(`fetch: done`)
	s.write("q")
	wantTerminalRestored(t, s, "0")
	if m := regexp.MustCompile(`\x1b\[\?100[0236][hl]`).FindAllString(s.output(), -1); len(m) > 0 {
		t.Errorf("mouse mode sequences %q with TERM=dumb", m)
	}
}

// On a terminal, the STATE column of the overview and the level words of
// doctor's report are colored; with NO_COLOR, nothing is (B32, B33).
func TestHomeOnPTYColorsStatesAndLevels(t *testing.T) {
	green := regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?32m(?:\x1b\\[[0-9;]*m)*clean")
	yellow := regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?33m(?:\x1b\\[[0-9;]*m)*WARN")
	for _, noColor := range []bool{false, true} {
		e, _, _, _, _ := homeBundle(t)
		env := ""
		if noColor {
			env = "NO_COLOR=1; export NO_COLOR; "
		}
		bin := toolsDir(t, "git", "stty") // no gh: doctor warns
		s := startPTYShell(t, e.cwd, env+`PATH='`+bin+`'; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
		s.waitFor(`STATE`)
		s.write("d")
		dismissReport(t, s, "doctor", 1)
		waitScreenState(t, s, "the overview after doctor", func(vt *vtScreen) bool {
			return vt.altActive() && slices.ContainsFunc(vt.altLines(), func(l string) bool { return strings.HasPrefix(overviewMessage(l), "doctor") })
		})
		s.write("q")
		wantTerminalRestored(t, s, "0")
		out := s.output()
		if got := green.MatchString(out); got == noColor {
			t.Errorf("NO_COLOR %v: green clean state %v:\n%q", noColor, got, out)
		}
		if got := yellow.MatchString(out); got == noColor {
			t.Errorf("NO_COLOR %v: yellow WARN %v:\n%q", noColor, got, out)
		}
		if noColor && colorSGR.MatchString(out) {
			t.Errorf("NO_COLOR: colors in\n%q", out)
		}
	}
}

// pidGit returns a directory holding a git wrapper (see fakeFetchGit) whose
// fetches append their PID to the file pids, then run the shell code fetch.
func pidGit(t *testing.T, fetch string) (bin, pids string) {
	t.Helper()
	pids = filepath.Join(t.TempDir(), "pids")
	return fakeFetchGit(t, `echo $$ >> '`+pids+`'; `+fetch), pids
}

// waitPIDs waits until the file pids lists at least n processes, and
// returns the PIDs it lists.
func waitPIDs(t *testing.T, pids string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, _ := os.ReadFile(pids)
		var got []int
		for _, f := range strings.Fields(string(b)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, pid)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d processes recorded in %s, want %d", len(got), pids, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantProcessesGone checks that none of the processes listed in pids (see
// pidGit) is left, within a few seconds (killing a process group and
// reaping it is not instantaneous).
func wantProcessesGone(t *testing.T, pids string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range waitPIDs(t, pids, 1) {
		for exists(fmt.Sprintf("/proc/%d", pid)) {
			if time.Now().After(deadline) {
				stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				t.Errorf("process %d (fake git) left behind: %s", pid, stat)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// waitOverviewMessage waits until the overview shows a status line (see
// overviewStatus) matching re.
func waitOverviewMessage(t *testing.T, s *ptyShell, re string) {
	t.Helper()
	rx := regexp.MustCompile(re)
	waitScreenState(t, s, "the overview with a status line matching "+re, func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") &&
			slices.ContainsFunc(alt, func(l string) bool { return overviewMessage(l) != "" && rx.MatchString(l) })
	})
}

// dismissReportOf waits until the terminal shows the report view of the
// command name (e.g. "wt add") or the overview with the message, and
// dismisses the report view if that is what it shows: an action cancelled
// before printing anything has none.
func dismissReportOf(t *testing.T, s *ptyShell, name, message string) {
	t.Helper()
	isReport := func(vt *vtScreen) bool {
		alt := vt.altLines()
		return len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf "+name+": ")
	}
	vt := waitScreenState(t, s, "the report view of "+name+" or the overview", func(vt *vtScreen) bool {
		return vt.altActive() && (isReport(vt) || slices.ContainsFunc(vt.altLines(), func(l string) bool { return overviewMessage(l) == message }))
	})
	if isReport(vt) {
		time.Sleep(tui.ReportGrace)
		s.write("q")
	}
}

// The first ctrl+c during an action of the home screen cancels the action
// only (B62 H4): its git processes are stopped as for the command alone,
// the report view shows what the action printed (the interrupted rows),
// and the overview comes back with the selection kept and the message
// "fetch: interrupted"; sheaf goes on (here it quits with q, status 0).
// The progress view's hint says "^C cancel".
func TestHomeOnPTYCtrlCCancelsTheAction(t *testing.T) {
	for _, c := range []struct {
		key, name, summary string // summary: regular expression of the summary line
	}{
		{"f", "fetch", `^0 done, 0 skipped, 0 failed, 2 interrupted$`}, // a command (see home.command)
		{"u", "update", `, \d+ interrupted$`},                          // inline (see home.inline)
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _, _ := homeBundle(t)
			bin, pids := pidGit(t, `exec sleep 30`)
			s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			s.write("a")
			s.write(c.key)
			s.waitFor(`\^C cancel`)
			waitPIDs(t, pids, 1)
			s.write("\x03")
			summary := regexp.MustCompile(c.summary)
			waitScreenState(t, s, "the report view of the interrupted "+c.name, func(vt *vtScreen) bool {
				alt := vt.altLines()
				return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf "+c.name+": ") &&
					slices.ContainsFunc(alt, summary.MatchString)
			})
			wantProcessesGone(t, pids)
			time.Sleep(tui.ReportGrace)
			s.write("q")
			waitOverviewMessage(t, s, `^3/3 selected · `+c.name+`: interrupted$`)
			if out := s.output(); strings.Contains(out, "SHEAF-EXIT=") || strings.Contains(out, "sheaf: interrupted") {
				t.Fatalf("sheaf ended or reported its own interruption after the first ctrl+c:\n%q", out)
			}
			s.write("q")
			wantTerminalRestored(t, s, "0")
			wantWindowTitleRestored(t, s.output())
			wantTerminalProgressReset(t, s.output())
		})
	}
}

// A second ctrl+c during the same action is the emergency stop: sheaf
// exits with 130 at once, with the terminal restored (modes, window
// title, progress indicator) and the git processes killed, even those
// that ignore SIGTERM.
func TestHomeOnPTYSecondCtrlCInTheActionIsTheEmergencyStop(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin, pids := pidGit(t, `trap '' TERM INT HUP; while :; do sleep 1; done`)
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	s.write("f")
	s.waitFor(`\^C cancel`)
	waitPIDs(t, pids, 1)
	s.write("\x03")
	s.waitFor(`Interrupted: no new work .*\(Ctrl-C again to stop sheaf\)`)
	s.write("\x03")
	s.waitFor(`interrupted again`)
	wantTerminalRestored(t, s, "130")
	wantWindowTitleRestored(t, s.output())
	wantTerminalProgressReset(t, s.output())
	wantProcessesGone(t, pids)
}

// Each action counts its own interruptions: after an action cancelled by
// ctrl+c, the first ctrl+c of the next one cancels it too (no emergency
// stop), and ctrl+c in the overview still exits with 130.
func TestHomeOnPTYEachActionCountsItsInterruptions(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin, pids := pidGit(t, `exec sleep 30`)
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	for n := 1; n <= 2; n++ {
		s.write("f")
		waitPIDs(t, pids, n)
		s.write("\x03")
		dismissReport(t, s, "fetch", n)
		deadline := time.Now().Add(30 * time.Second)
		for strings.Count(s.output(), "fetch: interrupted") < n {
			if time.Now().After(deadline) {
				t.Fatalf("overview %d after an interrupted fetch never shown:\n%q", n, s.output())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wantProcessesGone(t, pids)
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
	if out := s.output(); strings.Contains(out, "interrupted again") || !strings.Contains(out, "sheaf: interrupted") {
		t.Errorf("want a plain interruption in the overview, no emergency stop:\n%q", out)
	}
}

// ctrl+c in the prompt or in the question of an action cancels that action
// only: the overview comes back with "<command>: interrupted", nothing was
// changed, and sheaf goes on.
func TestHomeOnPTYCtrlCInActionDialogs(t *testing.T) {
	for _, c := range []struct {
		name, keys, wait, command string
		sigint                    bool // a SIGINT (kill -INT) rather than the ctrl+c key
	}{
		{"prompt", "w", `New worktree of alpha`, "wt add", false},
		{"prompt, SIGINT", "w", `New worktree of alpha`, "wt add", true},
		{"question", "jr", `\[y/N\]`, "wt rm", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, feat, _, _ := homeBundle(t)
			if err := os.Remove(filepath.Join(feat, "new.txt")); err != nil { // removable without --force
				t.Fatal(err)
			}
			s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			s.write(c.keys)
			s.waitFor(c.wait)
			if c.sigint {
				if err := syscall.Kill(childPID(t, s.cmd.Process.Pid), syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
			} else {
				s.write("\x03")
			}
			dismissReportOf(t, s, c.command, c.command+": interrupted")
			waitOverviewMessage(t, s, " · "+c.command+`: interrupted$`)
			out := s.output()
			if strings.Contains(out, "SHEAF-EXIT=") {
				t.Fatalf("sheaf ended:\n%q", out)
			}
			if c.command == "wt add" && strings.Contains(windowTitleSeq.ReplaceAllString(out, ""), "sheaf wt add: ") {
				t.Errorf("a report view after the prompt interrupted before anything was printed:\n%q", out)
			}
			if !exists(feat) {
				t.Error("the worktree feat was removed")
			}
			s.write("q")
			wantTerminalRestored(t, s, "0")
		})
	}
}

// After a prompt ended by a SIGINT, the next key reaches the overview that
// follows. A program killed (rather than quit) leaves its input reader
// running, which may take that key: a race, hence rounds, each waiting
// for the next overview, then typing w, which must open the prompt again
// (with the program killed, 8 rounds lost a key in 2 runs out of 5, 30
// rounds in 10 out of 10).
func TestHomeOnPTYKeysReachTheOverviewAfterInterruptedPrompts(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
	s.waitFor(`STATE`)
	count := func(what string, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for strings.Count(s.output(), what) < n {
			if time.Now().After(deadline) {
				t.Fatalf("%q shown %d times, want %d: the key was lost\n%q", what, strings.Count(s.output(), what), n, s.output())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	pid := childPID(t, s.cmd.Process.Pid)
	const rounds = 30
	for i := 1; i <= rounds; i++ {
		count("\x1b[?1049h", i) // overview i on the alternate screen
		s.write("w")
		count("\x1b]2;sheaf wt add: ", i) // prompt i (its window title, set once)
		if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	count("\x1b[?1049h", rounds+1)
	s.write("q")
	wantTerminalRestored(t, s, "0")
}

// A SIGINT while an action prints on the normal screen (the terminal is
// not in raw mode there, so ctrl+c is a SIGINT; `kill -INT` is the same)
// cancels the action like the ctrl+c key: here the fetch of wt add, whose
// git process is stopped; nothing is added. SIGTERM still ends sheaf
// (exit 130).
func TestHomeOnPTYSignalsDuringAnAction(t *testing.T) {
	for _, c := range []struct {
		name string
		sig  syscall.Signal
	}{{"SIGINT", syscall.SIGINT}, {"SIGTERM", syscall.SIGTERM}} {
		t.Run(c.name, func(t *testing.T) {
			e, alpha, _, _, _ := homeBundle(t)
			bin, pids := pidGit(t, `exec sleep 30`)
			s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
			s.waitFor(`STATE`)
			s.write("w")
			s.waitFor(`New worktree of alpha`)
			s.write("topic/sig\r")
			waitPIDs(t, pids, 1)
			if err := syscall.Kill(childPID(t, s.cmd.Process.Pid), c.sig); err != nil {
				t.Fatal(err)
			}
			if c.sig == syscall.SIGTERM {
				wantTerminalRestored(t, s, "130")
				wantProcessesGone(t, pids)
				return
			}
			dismissReportOf(t, s, "wt add", "wt add: interrupted")
			waitOverviewMessage(t, s, ` · wt add: interrupted$`)
			wantProcessesGone(t, pids)
			if wt := testutil.Git(t, alpha, "worktree", "list", "--porcelain"); strings.Contains(wt, "topic/sig") {
				t.Errorf("a worktree was added:\n%s", wt)
			}
			s.write("q")
			wantTerminalRestored(t, s, "0")
		})
	}
}
