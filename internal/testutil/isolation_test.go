package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Variables of the child processes run by the tests below (re-executions of
// this test binary).
const (
	childVar    = "SHEAF_TESTUTIL_CHILD"     // which child test body to run
	hostileVar  = "SHEAF_TESTUTIL_HOSTILE"   // directory of the hostile configuration
	skipMainVar = "SHEAF_TESTUTIL_SKIP_MAIN" // TestMain runs the tests without Main
)

// runChild runs the test binary with only the test named test selected, in
// exactly env, and returns its combined output and error.
func runChild(t *testing.T, test string, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1", "-test.v")
	cmd.Env = WithoutRaceExitDelay(env)
	cmd.Dir = t.TempDir()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// cleanBaseEnv is the environment of this process without the variables that
// git isolation manages, i.e. what a developer shell provides besides them.
func cleanBaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "SHEAF_") {
			continue
		}
		switch k {
		case "HOME", "XDG_CONFIG_HOME", "LC_ALL", "LANGUAGE":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// The test process is isolated before any test runs: a test binary started
// from a hostile environment (as from a git hook, with a developer's
// configuration and locale) runs its tests with none of it, without any
// per-test call, and git run by tests cannot touch the repository the
// inherited GIT_DIR points at. A forged isolation marker is not trusted.
func TestMainIsolatesHostileEnvironment(t *testing.T) {
	t.Parallel()
	for _, forgeMarker := range []bool{false, true} {
		name := "plain"
		if forgeMarker {
			name = "forged-marker"
		}
		t.Run(name, func(t *testing.T) {
			sentinel := t.TempDir()
			// Set up without relying on the isolation under test.
			Git(t, sentinel, "init", "-q")
			if err := os.WriteFile(filepath.Join(sentinel, "s.txt"), []byte("s"), 0o644); err != nil {
				t.Fatal(err)
			}
			Git(t, sentinel, "add", "s.txt")
			Git(t, sentinel, "-c", "user.name=Sentinel", "-c", "user.email=sentinel@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "sentinel")
			refsBefore := Git(t, sentinel, "for-each-ref")
			logBefore := Git(t, sentinel, "log", "--oneline")
			configBefore, err := os.ReadFile(filepath.Join(sentinel, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}

			hostile := t.TempDir()
			home := filepath.Join(hostile, "home")
			xdg := filepath.Join(hostile, "xdg")
			for p, c := range map[string]string{
				filepath.Join(home, ".gitconfig"):      "[user]\n\tname = Real Dev\n[init]\n\tdefaultBranch = trunk\n",
				filepath.Join(xdg, "git", "config"):    "[user]\n\temail = dev@example.invalid\n",
				filepath.Join(hostile, "devconfig"):    "[user]\n\tname = Real Dev\n[commit]\n\tgpgsign = true\n",
				filepath.Join(hostile, "systemconfig"): "[init]\n\tdefaultBranch = trunk\n",
			} {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			strayIndex := filepath.Join(hostile, "stray-index")
			env := append(cleanBaseEnv(),
				childVar+"=hostile",
				hostileVar+"="+hostile,
				"HOME="+home,
				"XDG_CONFIG_HOME="+xdg,
				"LC_ALL=de_DE.UTF-8",
				"LANGUAGE=de",
				"GIT_DIR="+filepath.Join(sentinel, ".git"),
				"GIT_WORK_TREE="+sentinel,
				"GIT_INDEX_FILE="+strayIndex,
				"GIT_CONFIG_GLOBAL="+filepath.Join(hostile, "devconfig"),
				"GIT_CONFIG_SYSTEM="+filepath.Join(hostile, "systemconfig"),
				"GIT_CONFIG_PARAMETERS='init.defaultBranch=trunk' 'core.hooksPath=/nonexistent'",
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=commit.gpgsign",
				"GIT_CONFIG_VALUE_0=true",
				"GIT_ALLOW_PROTOCOL=https",
				"GIT_TERMINAL_PROMPT=1",
			)
			if forgeMarker {
				env = append(env, isolatedHomeVar+"="+home)
			}
			out, err := runChild(t, "TestMainIsolationChild", env)
			if err != nil || !strings.Contains(out, "--- PASS: TestMainIsolationChild") {
				t.Errorf("child test in a hostile environment: %v\n%s", err, out)
			}

			if got := Git(t, sentinel, "for-each-ref"); got != refsBefore {
				t.Errorf("sentinel refs changed:\n%s\nvs\n%s", refsBefore, got)
			}
			if got := Git(t, sentinel, "log", "--oneline"); got != logBefore {
				t.Errorf("sentinel log changed: %q", got)
			}
			if got, _ := os.ReadFile(filepath.Join(sentinel, ".git", "config")); !bytes.Equal(got, configBefore) {
				t.Errorf("sentinel config changed:\n%s", got)
			}
			if _, err := os.Stat(strayIndex); err == nil {
				t.Error("stray index file created")
			}
		})
	}
}

// TestMainIsolationChild is the body of TestMainIsolatesHostileEnvironment,
// run in a child process started from a hostile environment.
func TestMainIsolationChild(t *testing.T) {
	if os.Getenv(childVar) != "hostile" {
		t.Skip("child process of TestMainIsolatesHostileEnvironment only")
	}
	allowed := map[string]bool{"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_TERMINAL_PROMPT": true}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") && !allowed[k] {
			t.Errorf("inherited %s still set", kv)
		}
	}
	for k, want := range map[string]string{"LC_ALL": "C", "LANGUAGE": "", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := os.LookupEnv("LANGUAGE"); ok {
		t.Error("LANGUAGE still set")
	}
	hostile := os.Getenv(hostileVar)
	if hostile == "" {
		t.Fatal(hostileVar + " not set")
	}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL"} {
		if v := os.Getenv(k); v == "" || strings.HasPrefix(v, hostile) {
			t.Errorf("%s not isolated: %q", k, v)
		}
	}
	if t.Failed() {
		return // do not run git in a hostile environment
	}

	// Without any per-test call, git sees only the isolated global
	// configuration and the repository's own.
	fresh := t.TempDir()
	Git(t, fresh, "init", "-q")
	if got := Git(t, fresh, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("initial branch = %q, want main", got)
	}
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	for _, line := range strings.Split(Git(t, fresh, "config", "--list", "--show-origin"), "\n") {
		origin, _, _ := strings.Cut(line, "\t")
		if origin != "file:"+global && origin != "file:.git/config" {
			t.Errorf("unexpected config origin: %q", line)
		}
	}
	CommitFile(t, fresh, "f.txt", "f", "fresh")
	if got := Git(t, fresh, "log", "-1", "--format=%an <%ae>"); got != "Sheaf Test <sheaf-test@example.invalid>" {
		t.Errorf("author = %q", got)
	}
	o := NewOrigin(t, OriginOpts{Branches: []string{"feat/x"}})
	if got := Git(t, o.Path, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("origin default branch = %q", got)
	}
	if got := Git(t, o.Path, "log", "-1", "--format=%an", "feat/x"); got != "Sheaf Test" {
		t.Errorf("origin author = %q", got)
	}
	// Parallel tests can use the fixtures: nothing per test touches the
	// process environment (t.Setenv would panic in a parallel test).
	t.Run("parallel", func(t *testing.T) {
		t.Parallel()
		IsolateGit(t)
		o := NewOrigin(t, OriginOpts{})
		AdvanceOrigin(t, o, "main", "n.txt", "n", "advance")
		if got := Git(t, o.Path, "log", "-1", "--format=%s", "main"); got != "advance" {
			t.Errorf("main tip = %q", got)
		}
	})
}

// Using the git fixtures in a test binary whose TestMain does not call Main
// fails the test instead of running git with the developer's environment.
func TestFixturesRequireMain(t *testing.T) {
	t.Parallel()
	env := append(cleanBaseEnv(), childVar+"=no-main", skipMainVar+"=1", "HOME="+t.TempDir())
	out, err := runChild(t, "TestNoMainChild", env)
	if err == nil || !strings.Contains(out, "--- FAIL: TestNoMainChild") {
		t.Fatalf("child without Main: err = %v, want a failed test\n%s", err, out)
	}
	for _, want := range []string{"IsolateGit: TestMain must call testutil.Main", "Git: TestMain must call testutil.Main"} {
		if !strings.Contains(out, want) {
			t.Errorf("child output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "UNREACHED") {
		t.Errorf("child went on after the failure:\n%s", out)
	}
}

// TestNoMainChild is the body of TestFixturesRequireMain.
func TestNoMainChild(t *testing.T) {
	if os.Getenv(childVar) != "no-main" {
		t.Skip("child process of TestFixturesRequireMain only")
	}
	t.Run("IsolateGit", func(t *testing.T) {
		IsolateGit(t)
		t.Log("UNREACHED")
	})
	t.Run("Git", func(t *testing.T) {
		Git(t, t.TempDir(), "init", "-q")
		t.Log("UNREACHED")
	})
}

// The global git configuration is shared by the tests of the process: a test
// cannot change it (IsolateGitPerTest gives a test a writable one).
func TestGlobalConfigIsReadOnly(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("git", "config", "--global", "pull.rebase", "true")
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("git config --global succeeded:\n%s", out)
	}
	if b, err := os.ReadFile(os.Getenv("GIT_CONFIG_GLOBAL")); err != nil || string(b) != gitConfig {
		t.Errorf("global configuration changed (%v):\n%s", err, b)
	}
}

func TestWithoutRaceExitDelay(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		env  []string
		want []string
	}{
		{[]string{"A=1"}, []string{"A=1", "GORACE=atexit_sleep_ms=0"}},
		{[]string{"GORACE=halt_on_error=1", "A=1"}, []string{"A=1", "GORACE=halt_on_error=1 atexit_sleep_ms=0"}},
		{[]string{"GORACE=", "A=1"}, []string{"A=1", "GORACE=atexit_sleep_ms=0"}},
	} {
		in := append([]string(nil), c.env...)
		if got := WithoutRaceExitDelay(c.env); !slices.Equal(got, c.want) {
			t.Errorf("WithoutRaceExitDelay(%q) = %q, want %q", in, got, c.want)
		}
		if !slices.Equal(c.env, in) {
			t.Errorf("argument modified: %q", c.env)
		}
	}
}
