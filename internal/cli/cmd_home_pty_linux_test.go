//go:build linux

package cli

import (
	"os"
	"os/exec"
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

// dismissReport waits until the output of s contains the title of the
// report view of the action name n times, then for the grace period of the
// view, and presses a key.
func dismissReport(t *testing.T, s *ptyShell, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(s.output(), "sheaf "+name+": ") < n {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained the report view of %s %d times:\n%q", name, n, s.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(tui.ReportGrace)
	s.write("x")
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
	// The summary of the fetch is printed on the normal screen, below its
	// progress view, and shown in its report view on the alternate screen.
	waitScreenState(t, s, "the report view of fetch on the alternate screen", func(vt *vtScreen) bool {
		alt, normal := vt.altLines(), strings.Join(vt.normalLines(), "\n")
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf fetch: ") && slices.Contains(alt, "done     alpha: fetched origin") &&
			strings.Contains(normal, "Fetching: 1 done, 0 skipped, 0 failed") && strings.Contains(normal, "done     alpha: fetched origin")
	})
	time.Sleep(tui.ReportGrace)
	s.write("x")
	vt := waitScreenState(t, s, "the overview after fetch on the alternate screen", overviewShown("fetch: done"))
	if normal := strings.Join(vt.normalLines(), "\n"); !strings.Contains(normal, "Fetching: 1 done, 0 skipped, 0 failed") ||
		strings.Index(normal, "done     alpha: fetched origin") < strings.Index(normal, "Fetching: 1 done, 0 skipped, 0 failed") {
		t.Errorf("the normal screen lacks the progress view of the fetch and its summary below it:\n%s", normal)
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

// After a failed action, the error is printed on the normal screen and
// shown in the report view on the alternate screen until a key is pressed;
// the overview then comes back with the failure as message. ctrl+c in the
// view interrupts sheaf (exit 130).
func TestHomeOnPTYFailureShowsReportView(t *testing.T) {
	for _, tc := range []struct{ key, exit string }{{"x", "0"}, {"\x03", "130"}} {
		e, _, _, _, _ := homeBundle(t)
		s := startPTYShell(t, e.cwd, `B=$(stty -g); "$SHEAF"`+sttyCheck, 0)
		s.waitFor(`STATE`)
		s.write("w")
		s.waitFor(`New worktree of alpha`)
		s.write("main\r") // the primary's branch: wt add refuses it
		waitScreenState(t, s, "the error in the report view", func(vt *vtScreen) bool {
			alt := vt.altLines()
			isError := func(l string) bool { return strings.HasPrefix(l, "sheaf: ") && !strings.HasPrefix(l, "sheaf: /") }
			return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf add: ") && slices.ContainsFunc(alt, isError) &&
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
	s.write("x")
	reportShown := func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf add: ")
	}
	waitScreenState(t, s, "the report view", reportShown)
	time.Sleep(2 * tui.ReportGrace)
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	if !reportShown(vt) {
		t.Fatalf("the key typed ahead ended the report view: alternate screen %v\nalternate screen:\n%s", vt.altActive(), strings.Join(vt.altLines(), "\n"))
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
	dismissReport(t, s, "fetch", 1)
	waitCount("fetch: done", 1)
	s.write("u")
	dismissReport(t, s, "update", 1)
	waitCount("update: done", 1)
	s.write("j")
	s.write("r")
	waitCount("[y/N]", 1)
	s.write("n")
	waitCount("remove: cancelled", 1)
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

// doctorView waits until the 10-row terminal of s shows the report view of
// doctor (bundle root) with a status line, and ok accepts its status line
// (submatches of reportStatus) and its report lines; it returns the
// terminal.
func doctorView(t *testing.T, s *ptyShell, root, what string, ok func(status, lines []string) bool) *vtScreen {
	t.Helper()
	return waitScreenStateRows(t, s, 10, what, func(vt *vtScreen) bool {
		alt := vt.altLines()
		if !vt.altActive() || len(alt) == 0 || alt[0] != "sheaf doctor: "+root {
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
	s.write("x")
	waitScreenStateRows(t, s, 10, "the overview after doctor", func(vt *vtScreen) bool {
		alt := vt.altLines()
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf: ") && slices.Contains(alt, "doctor: done")
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
// way: j j k and home j both show lines 2-8.
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
	if !slices.ContainsFunc(a, func(l string) bool { return strings.HasPrefix(l, "lines 2-8 of ") }) {
		t.Errorf("no status line \"lines 2-8 of ...\":\n%s", strings.Join(a, "\n"))
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
		return vt.altActive() && len(alt) > 0 && strings.HasPrefix(alt[0], "sheaf add: ") && slices.ContainsFunc(alt, isProgress) &&
			slices.ContainsFunc(vt.normalLines(), isProgress)
	})
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
}

// An action interrupted with ctrl+c (exit 130) leaves what its command
// printed on the normal screen, as the command alone would: here the
// summary of a fetch whose git processes were killed.
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
	wantTerminalRestored(t, s, "130")
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	normal := vt.normalLines()
	n := 0
	for _, l := range normal {
		if l == "0 done, 0 skipped, 2 failed" {
			n++
		}
	}
	if n != 1 || !slices.ContainsFunc(normal, func(l string) bool { return strings.HasPrefix(l, "failed   alpha") }) {
		t.Errorf("the summary of the interrupted fetch is not on the normal screen once (%d times):\n%s", n, strings.Join(normal, "\n"))
	}
}
