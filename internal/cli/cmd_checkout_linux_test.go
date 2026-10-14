//go:build linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

func coSessionOf(t *testing.T, pid string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[3]
}

// Interactive git mode is threaded from the CLI to gitx: with -j 1 and stdin
// on a terminal, the ssh spawned by `sheaf fetch` runs in sheaf's session
// (it can reach the terminal); otherwise in a new one.
func TestFetchInteractiveGitMode(t *testing.T) {
	e := coBundle(t)
	_, alpha := e.coAddRepo(t, "alpha")
	rec := filepath.Join(t.TempDir(), "sid")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nread -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + rec + "'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, alpha, "config", "remote.origin.url", "ssh://sheaf-test.invalid/repo.git")
	testutil.Git(t, alpha, "config", "ssh.variant", "ssh")
	t.Setenv("GIT_SSH_COMMAND", fake)
	self := coSessionOf(t, "self")
	sid := func(args ...string) string {
		t.Helper()
		os.Remove(rec)
		e.run(t, args...) // the fake ssh fails the fetch
		b, err := os.ReadFile(rec)
		if err != nil {
			t.Fatalf("sheaf %v: fake ssh never ran", args)
		}
		return strings.TrimSpace(string(b))
	}

	if got := sid("fetch", "-j", "1"); got == self {
		t.Error("stdin not a terminal: ssh ran in sheaf's session")
	}
	coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
	if got := sid("fetch", "-j", "1"); got != self {
		t.Errorf("-j 1, stdin terminal: ssh in session %s, want sheaf's %s", got, self)
	}
	if got := sid("fetch", "-j", "2"); got == self {
		t.Error("-j 2: ssh ran in sheaf's session")
	}
	if got := sid("update", "-j", "1"); got != self {
		t.Errorf("update -j 1, stdin terminal: ssh in session %s, want sheaf's %s", got, self)
	}
}

// `sheaf add` clones: in interactive git mode, the ssh spawned by the clone
// runs in sheaf's session.
func TestAddInteractiveGitMode(t *testing.T) {
	e := coBundle(t)
	rec := filepath.Join(t.TempDir(), "sid")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nread -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + rec + "'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fake)
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "alpha", CloneURL: "ssh://sheaf-test.invalid/alpha.git", DefaultBranch: "main"})
	coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
	e.run(t, "add", "-j", "1", "o/alpha") // the fake ssh fails the clone
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal("fake ssh never ran")
	}
	if got, self := strings.TrimSpace(string(b)), coSessionOf(t, "self"); got != self {
		t.Fatalf("add -j 1, stdin terminal: ssh in session %s, want sheaf's %s", got, self)
	}
}

// coFakeSSH installs a fake ssh recording its session id; it returns the
// record file.
func coFakeSSH(t *testing.T) string {
	t.Helper()
	rec := filepath.Join(t.TempDir(), "sid")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nread -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + rec + "'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fake)
	return rec
}

func coWantSelfSession(t *testing.T, what, rec string) {
	t.Helper()
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("%s: fake ssh never ran", what)
	}
	if got, self := strings.TrimSpace(string(b)), coSessionOf(t, "self"); got != self {
		t.Fatalf("%s: ssh in session %s, want sheaf's %s", what, got, self)
	}
}

// remove fetches before checking risks: interactive in the same conditions.
func TestRemoveInteractiveGitMode(t *testing.T) {
	e := coBundle(t)
	_, alpha := e.coAddRepo(t, "alpha")
	rec := coFakeSSH(t)
	testutil.Git(t, alpha, "config", "remote.origin.url", "ssh://sheaf-test.invalid/alpha.git")
	testutil.Git(t, alpha, "config", "ssh.variant", "ssh")
	coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
	e.run(t, "remove", "-j", "1", "--yes", "alpha") // the failing fetch refuses the removal
	coWantSelfSession(t, "remove -j 1", rec)
}

// init --adopt-all may ask origin for its default branch (ls-remote).
func TestInitAdoptAllInteractiveGitMode(t *testing.T) {
	e := newEnv(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := t.TempDir()
	testutil.Git(t, dir, "clone", "-q", o.URL, filepath.Join(dir, "alpha"))
	clone := filepath.Join(dir, "alpha")
	testutil.Git(t, clone, "remote", "set-head", "origin", "-d")
	testutil.Git(t, clone, "config", "remote.origin.url", "ssh://sheaf-test.invalid/o/alpha.git")
	testutil.Git(t, clone, "config", "ssh.variant", "ssh")
	rec := coFakeSSH(t)
	coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
	e.run(t, "init", "-j", "1", "--adopt-all", dir) // the fake ssh fails ls-remote
	coWantSelfSession(t, "init --adopt-all -j 1", rec)
}

// adopt may ask origin for its default branch (ls-remote).
func TestAdoptInteractiveGitMode(t *testing.T) {
	e := coBundle(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	clone := filepath.Join(e.cwd, "alpha")
	testutil.Git(t, e.cwd, "clone", "-q", o.URL, clone)
	testutil.Git(t, clone, "remote", "set-head", "origin", "-d")
	testutil.Git(t, clone, "config", "remote.origin.url", "ssh://sheaf-test.invalid/o/alpha.git")
	testutil.Git(t, clone, "config", "ssh.variant", "ssh")
	rec := coFakeSSH(t)
	coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
	e.run(t, "adopt", "-j", "1", "alpha") // the fake ssh fails ls-remote
	coWantSelfSession(t, "adopt -j 1", rec)
}

// execute wraps standard output to detect broken pipes; terminal detection
// (TUI choice, colors) must still see the terminal behind the wrapper.
func TestIsTerminalSeesThroughPipeWatch(t *testing.T) {
	_, slave := openPTY(t)
	if !isTerminal(&pipeWatch{w: slave}) {
		t.Fatal("isTerminal(pipeWatch{pty}) = false")
	}
	if isTerminal(&pipeWatch{w: io.Discard}) {
		t.Fatal("isTerminal(pipeWatch{io.Discard}) = true")
	}
}
