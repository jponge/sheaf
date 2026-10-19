//go:build linux

package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// spinnerLine matches a frame of the working indicator followed by label.
func spinnerLine(label string) *regexp.Regexp {
	return regexp.MustCompile("[" + strings.Join(spinnerFrames, "") + "] " + regexp.QuoteMeta(label))
}

// hasSpinnerFrame reports whether a line of the screen holds a frame of
// the working indicator.
func hasSpinnerFrame(lines []string) bool {
	for _, l := range lines {
		if strings.ContainsAny(l, strings.Join(spinnerFrames, "")) {
			return true
		}
	}
	return false
}

// slowGit returns a directory holding a git that sleeps secs seconds
// before a fetch, ahead of the real one on PATH.
func slowGit(t *testing.T, secs string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = fetch ] && sleep "+secs+"; done\nexec '"+git+"' \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A root command on a terminal's standard error gets a spinner and writes
// through it; the commands of the home screen, whose standard error is the
// home screen's tee, share the home screen's spinner (so that whatever is
// written to the terminal, e.g. by the emergency stop, erases it) rather
// than wrap the tee in a spinner of their own.
func TestRootSpinnerOnTerminal(t *testing.T) {
	t.Parallel()
	_, slave := openPTY(t)
	errOf := func(s Streams) io.Writer {
		var got io.Writer
		root := NewRootCmdWithDeps(s, Deps{})
		root.AddCommand(&cobra.Command{Use: "probe", Run: func(c *cobra.Command, _ []string) { got = c.ErrOrStderr() }})
		root.SetArgs([]string{"probe"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	w, ok := errOf(Streams{Out: io.Discard, Err: slave}).(*spinWriter)
	if !ok || w.w != io.Writer(slave) || w.s == nil || w.s.term != io.Writer(slave) {
		t.Fatalf("standard error on a terminal: %#v", w)
	}
	var buf bytes.Buffer
	tee := teeWriter{io.MultiWriter(w, &buf), w}
	if got := errOf(Streams{Out: io.Discard, Err: tee}); got != io.Writer(tee) {
		t.Fatalf("home screen command: standard error %#v, want the tee", got)
	}
	if got := errOf(Streams{Out: io.Discard, Err: &buf}); got != io.Writer(&buf) {
		t.Fatalf("standard error not a terminal: %#v, want it unchanged", got)
	}
}

// During a slow fetch of wt add, the working indicator shows on the
// terminal below the progress line, and is erased when the fetch ends:
// the final screen has no trace of it (B34).
func TestWorktreeAddSpinnerOnPTY(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin := slowGit(t, "1")
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" wt add alpha spin-x </dev/null`+sttyCheck, 0)
	wantTerminalRestored(t, s, "0")
	out := s.output()
	if !spinnerLine("fetching origin").MatchString(out) {
		t.Errorf("no working indicator during the fetch:\n%q", out)
	}
	vt := newVTScreen(24, 100)
	vt.write(out)
	lines := vt.normalLines()
	if hasSpinnerFrame(lines) {
		t.Errorf("working indicator left on the screen:\n%s", strings.Join(lines, "\n"))
	}
	joined := strings.Join(vt.normalLogicalLines(), "\n") // paths may be wider than the terminal (B55)
	if !strings.Contains(joined, "alpha: fetch: fetching origin\n") || !strings.Contains(joined, filepath.Join(e.cwd, "alpha", "spin-x")) {
		t.Errorf("output lines missing or damaged:\n%s", joined)
	}
}

// Ctrl-C during the phase interrupts the command and leaves the line
// clean.
func TestWorktreeAddSpinnerCtrlCOnPTY(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin := slowGit(t, "30")
	// The shell survives the SIGINT of the terminal (trap); sheaf handles it.
	s := startPTYShell(t, e.cwd, `trap : INT; PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" wt add alpha spin-y </dev/null`+sttyCheck, 0)
	s.waitFor(spinnerLine("fetching origin").String())
	s.write("\x03")
	wantTerminalRestored(t, s, "130")
	vt := newVTScreen(24, 100)
	vt.write(s.output())
	if lines := vt.normalLines(); hasSpinnerFrame(lines) || !strings.Contains(strings.Join(lines, "\n"), "sheaf: interrupted") {
		t.Errorf("after ctrl+c:\n%s", strings.Join(lines, "\n"))
	}
}

// With git interactive (standard input on the terminal), git may prompt
// there: no working indicator. With standard error not a terminal, the
// output is unchanged.
func TestWorktreeAddNoSpinnerOnPTY(t *testing.T) {
	e, _, _, _, _ := homeBundle(t)
	bin := slowGit(t, "1")
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" wt add alpha spin-i`+sttyCheck, 0)
	wantTerminalRestored(t, s, "0")
	if out := s.output(); strings.ContainsAny(out, strings.Join(spinnerFrames, "")) {
		t.Errorf("working indicator with git interactive:\n%q", out)
	}
	errFile := filepath.Join(t.TempDir(), "err")
	s = startPTYShell(t, e.cwd, `PATH='`+bin+`':$PATH; export PATH; B=$(stty -g); "$SHEAF" wt add alpha spin-n </dev/null 2>'`+errFile+`'`+sttyCheck, 0)
	wantTerminalRestored(t, s, "0")
	b, err := os.ReadFile(errFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha: fetch: fetching origin\n"; !strings.HasPrefix(string(b), want) || strings.ContainsAny(string(b), "\x1b\r"+strings.Join(spinnerFrames, "")) {
		t.Errorf("standard error (a file):\n%q", b)
	}
}

// The listing of sync shows the working indicator below its static
// "listing repositories of o..." line, erased before the result is printed.
func TestSyncListingSpinnerOnPTY(t *testing.T) {
	e := newBundleEnv(t)
	yaml := filepath.Join(e.cwd, "sheaf.yaml")
	b, err := os.ReadFile(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "sources: []") {
		t.Fatalf("fixture: unexpected sheaf.yaml:\n%s", b)
	}
	writeFile(t, yaml, strings.Replace(string(b), "sources: []", "sources:\n  - org: o", 1))
	bin := toolsDir(t, "git", "stty", "sleep")
	writeFile(t, filepath.Join(bin, "gh"), "#!/bin/sh\ncase \"$*\" in *\"repo list\"*) sleep 1; echo '[]';; *) echo https;; esac\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := startPTYShell(t, e.cwd, `PATH='`+bin+`'; export PATH; B=$(stty -g); "$SHEAF" sync`+sttyCheck, 0)
	wantTerminalRestored(t, s, "0")
	out := s.output()
	// The static line names the phase: the indicator is only the frame.
	frameOnly := regexp.MustCompile("\r[" + strings.Join(spinnerFrames, "") + "]\x1b\\[K")
	if !frameOnly.MatchString(out) || spinnerLine("listing repositories of o").MatchString(out) {
		t.Errorf("want a frame alone as working indicator during the listing:\n%q", out)
	}
	vt := newVTScreen(24, 100)
	vt.write(out)
	lines := vt.normalLines()
	if joined := strings.Join(lines, "\n"); hasSpinnerFrame(lines) || !strings.Contains(joined, "listing repositories of o...\nnothing to sync") {
		t.Errorf("screen:\n%s", joined)
	}
}
