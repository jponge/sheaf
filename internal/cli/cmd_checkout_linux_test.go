//go:build linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

func sessionOf(t *testing.T, pid string) string {
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
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	rec := filepath.Join(t.TempDir(), "sid")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nread -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + rec + "'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, alpha, "config", "remote.origin.url", "ssh://sheaf-test.invalid/repo.git")
	testutil.Git(t, alpha, "config", "ssh.variant", "ssh")
	t.Setenv("GIT_SSH_COMMAND", fake)
	self := sessionOf(t, "self")
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
	e.stdinTTY = func(io.Reader) bool { return true }
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
	e := newBundleEnv(t)
	rec := filepath.Join(t.TempDir(), "sid")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nread -r line < /proc/$$/stat\nset -- ${line##*) }\necho $4 > '" + rec + "'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fake)
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "alpha", CloneURL: "ssh://sheaf-test.invalid/alpha.git", DefaultBranch: "main"})
	e.stdinTTY = func(io.Reader) bool { return true }
	e.run(t, "add", "-j", "1", "o/alpha") // the fake ssh fails the clone
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal("fake ssh never ran")
	}
	if got, self := strings.TrimSpace(string(b)), sessionOf(t, "self"); got != self {
		t.Fatalf("add -j 1, stdin terminal: ssh in session %s, want sheaf's %s", got, self)
	}
}

// fakeSSH installs a fake ssh recording its session id; it returns the
// record file.
func fakeSSH(t *testing.T) string {
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

func wantSelfSession(t *testing.T, what, rec string) {
	t.Helper()
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("%s: fake ssh never ran", what)
	}
	if got, self := strings.TrimSpace(string(b)), sessionOf(t, "self"); got != self {
		t.Fatalf("%s: ssh in session %s, want sheaf's %s", what, got, self)
	}
}

// remove fetches before checking risks: interactive in the same conditions.
func TestRemoveInteractiveGitMode(t *testing.T) {
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	rec := fakeSSH(t)
	testutil.Git(t, alpha, "config", "remote.origin.url", "ssh://sheaf-test.invalid/alpha.git")
	testutil.Git(t, alpha, "config", "ssh.variant", "ssh")
	e.stdinTTY = func(io.Reader) bool { return true }
	e.run(t, "remove", "-j", "1", "--yes", "alpha") // the failing fetch refuses the removal
	wantSelfSession(t, "remove -j 1", rec)
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
	rec := fakeSSH(t)
	e.stdinTTY = func(io.Reader) bool { return true }
	e.run(t, "init", "-j", "1", "--adopt-all", dir) // the fake ssh fails ls-remote
	wantSelfSession(t, "init --adopt-all -j 1", rec)
}

// adopt may ask origin for its default branch (ls-remote).
func TestAdoptInteractiveGitMode(t *testing.T) {
	e := newBundleEnv(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	clone := filepath.Join(e.cwd, "alpha")
	testutil.Git(t, e.cwd, "clone", "-q", o.URL, clone)
	testutil.Git(t, clone, "remote", "set-head", "origin", "-d")
	testutil.Git(t, clone, "config", "remote.origin.url", "ssh://sheaf-test.invalid/o/alpha.git")
	testutil.Git(t, clone, "config", "ssh.variant", "ssh")
	rec := fakeSSH(t)
	e.stdinTTY = func(io.Reader) bool { return true }
	e.run(t, "adopt", "-j", "1", "alpha") // the fake ssh fails ls-remote
	wantSelfSession(t, "adopt -j 1", rec)
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

// sshProxy installs a fake ssh that appends its session id to the returned
// record file, then runs the remote command locally (git-upload-pack on the
// path of the ssh:// URL): a working ssh remote whose every connection is
// observable.
func sshProxy(t *testing.T) string {
	t.Helper()
	rec := filepath.Join(t.TempDir(), "sids")
	fake := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nfor cmd; do :; done\nread -r line < /proc/$$/stat\nset -- ${line##*) }\n" +
		"echo $4 >> '" + rec + "'\neval \"exec $cmd\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", fake)
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	return rec
}

// sshBundle is a bundle tracking organization o, with repository a (pull
// requests 7 from a fork and 3 from feat, tag v1, worktrees feat and pr-7)
// reached through the fake ssh of sshProxy, whose record file is returned;
// the hosting fake also lists b, returned.
func sshBundle(t *testing.T) (e *env, b hosting.Repo, rec string) {
	t.Helper()
	e = newEnv(t)
	for _, n := range []string{"a", "b"} {
		o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		testutil.Git(t, o.Path, "update-ref", "refs/pull/7/head", "refs/heads/feat")
		testutil.Git(t, o.Path, "tag", "v1", "main")
		b = hosting.Repo{Owner: "o", Name: n, CloneURL: "ssh://sheaf-test.invalid" + o.Path, DefaultBranch: "main"}
		e.fake.AddRepo(b)
	}
	e.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "patch", HeadOwner: "fork", HeadRepo: "a", IsCrossRepository: true})
	e.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
	rec = sshProxy(t) // after newEnv and NewOrigin, which scrub GIT_* variables
	root := filepath.Join(e.cwd, "bundle")
	e.mustRun(t, "create", root)
	e.cwd = root
	e.mustRun(t, "add", "--org", "o", "--repo", "a")
	e.mustRun(t, "wt", "add", "a", "feat")
	e.mustRun(t, "wt", "pr", "a", "7")
	return e, b, rec
}

// sshForkBundle is a bundle with the forked repository a (worktree feat)
// whose two remotes are reached through the fake ssh of sshProxy, whose
// record file is returned; the hosting fake also lists the forked b.
func sshForkBundle(t *testing.T) (e *env, rec string) {
	t.Helper()
	e = newBundleEnv(t)
	e.fake.User = "me"
	for _, n := range []string{"a", "b"} {
		fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		e.fake.AddRepo(hosting.Repo{Owner: "o", Name: n, CloneURL: "ssh://sheaf-test.invalid" + fo.Upstream.Path, DefaultBranch: "main"})
		e.fake.AddFork(hosting.Fork{Repo: hosting.Repo{Owner: "me", Name: n, CloneURL: "ssh://sheaf-test.invalid" + fo.Fork.Path, Fork: true}, ParentOwner: "o", ParentName: n})
	}
	rec = sshProxy(t) // after NewForkedOrigin, which scrubs GIT_* variables
	e.mustRun(t, "add", "o/a")
	e.mustRun(t, "wt", "add", "a", "feat")
	return e, rec
}

// Every git network call of the plain commands follows interactive git mode
// (-j 1 where it applies, stdin on a terminal): each ssh connection runs in
// sheaf's session when interactive, in another one otherwise, and both modes
// make the same number of connections.
func TestInteractiveGitModeCoversEveryNetworkCall(t *testing.T) {
	for _, tc := range []struct {
		args []string
		fork bool // in sshForkBundle rather than sshBundle
	}{
		{args: []string{"add", "-j", "1", "o/b"}, fork: true},
		{args: []string{"fetch", "-j", "1"}, fork: true},
		{args: []string{"update", "-j", "1"}, fork: true},
		{args: []string{"prune", "-j", "1"}, fork: true},
		{args: []string{"remove", "--yes", "a"}, fork: true},
		{args: []string{"wt", "add", "a", "newbr"}, fork: true},
		{args: []string{"wt", "rm", "--yes", "a", "feat"}, fork: true},
	} {
		testInteractiveGitMode(t, tc.args, tc.fork)
	}
	for _, args := range [][]string{
		{"status", "--fetch", "-j", "1"},
		{"fetch", "-j", "1"},
		{"update", "-j", "1"},
		{"prune", "-j", "1"},
		{"remove", "--yes", "a"},
		{"wt", "add", "a", "newbr"},
		{"wt", "add", "a", "--tag", "v1"},
		{"wt", "rm", "--yes", "a", "feat"},
		{"wt", "pr", "a", "7"},
		{"wt", "pr", "a", "3"},
		{"sync", "--add-new", "-j", "1"},
		{"sync", "--remove-gone", "--yes", "--allow-mass-removal", "-j", "1"},
	} {
		testInteractiveGitMode(t, args, false)
	}
}

func testInteractiveGitMode(t *testing.T, args []string, fork bool) {
	name := strings.Join(args, " ")
	if fork {
		name = "fork " + name
	}
	t.Run(name, func(t *testing.T) {
		self := sessionOf(t, "self")
		sessions := func(tty bool) []string {
			t.Helper()
			var e *env
			var rec string
			if fork {
				e, rec = sshForkBundle(t)
			} else {
				var b hosting.Repo
				e, b, rec = sshBundle(t)
				if args[0] == "sync" && args[1] == "--remove-gone" {
					e.fake = hostingtest.NewFake() // a is gone
					e.fake.AddRepo(b)
				}
			}
			os.Remove(rec)
			e.stdinTTY = func(io.Reader) bool { return tty }
			e.run(t, args...)
			out, _ := os.ReadFile(rec)
			return strings.Fields(string(out))
		}
		on, off := sessions(true), sessions(false)
		if len(on) == 0 || len(on) != len(off) {
			t.Fatalf("%d ssh connections interactive, %d otherwise; want the same, at least 1", len(on), len(off))
		}
		for i := range on {
			if on[i] != self || off[i] == self {
				t.Fatalf("connection %d: session %s interactive, %s otherwise; sheaf's is %s", i+1, on[i], off[i], self)
			}
		}
		t.Logf("%d ssh connections", len(on))
	})
}
