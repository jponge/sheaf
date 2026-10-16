package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellCmd returns the command running script with shell (bash, zsh or
// fish, without user configuration; zsh gets the test module path), the
// remaining arguments being the script's $1, $2... ($argv in fish). It
// skips the test when the shell is missing.
func shellCmd(t *testing.T, shell, script string, args ...string) *exec.Cmd {
	t.Helper()
	switch shell {
	case "bash":
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash is not installed")
		}
		return exec.Command(bash, append([]string{"--norc", "--noprofile", "-c", script, "bash"}, args...)...)
	case "zsh":
		script = "[[ -n $SHEAF_TEST_ZSH_MODULE_PATH ]] && module_path=(${(s.:.)SHEAF_TEST_ZSH_MODULE_PATH} $module_path)\n" + script
		return exec.Command(shellBinary(t, "zsh"), append([]string{"-f", "-c", script, "zsh"}, args...)...)
	default:
		return exec.Command(shellBinary(t, "fish"), append([]string{"--no-config", "-c", script}, args...)...)
	}
}

// S1: SHEAF_CD_FILE is meant for sheaf only: the commands of `sheaf run`
// (like git hooks or editors) must not see it, or they could make the
// shell function change the directory.
func TestShellFunctionIgnoresChildrenWritingTheCDFile(t *testing.T) {
	t.Parallel()
	bin := sheafBinary(t)
	e := coBundle(t)
	e.coAddRepo(t, "alpha")
	child := `if [ -n "${SHEAF_CD_FILE:-}" ]; then printf %s /etc > "$SHEAF_CD_FILE"; fi`
	scripts := map[string]string{
		"bash": `eval "$(sheaf shell-init bash)"; cd "$1"; sheaf --no-tui run -- sh -c "$2" >/dev/null 2>&1; echo "rc=$? PWD=$PWD"`,
		"zsh":  `eval "$(sheaf shell-init zsh)"; cd "$1"; sheaf --no-tui run -- sh -c "$2" >/dev/null 2>&1; echo "rc=$? PWD=$PWD"`,
		"fish": `sheaf shell-init fish | source; cd $argv[1]; sheaf --no-tui run -- sh -c $argv[2] >/dev/null 2>&1; echo "rc=$status PWD=$PWD"`,
	}
	// One shell at a time: concurrent runs in one bundle could fail and
	// then not change the directory whatever the fix.
	for _, sh := range []string{"bash", "zsh", "fish"} {
		t.Run(sh, func(t *testing.T) {
			cmd := shellCmd(t, sh, scripts[sh], e.cwd, child)
			cmd.Env = shellEnv(t, bin)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if want := "rc=0 PWD=" + e.cwd + "\n"; string(out) != want {
				t.Fatalf("got %q, want %q (a child of sheaf changed the directory)", out, want)
			}
		})
	}
}

// S1, in the process: Execute takes SHEAF_CD_FILE out of the environment
// before running anything, so children do not inherit it.
func TestExecuteHidesTheCDFileFromChildren(t *testing.T) {
	e := coBundle(t)
	e.coAddRepo(t, "alpha")
	t.Setenv(cdFileVar, filepath.Join(t.TempDir(), "cd"))
	var out, errb bytes.Buffer
	code := Execute([]string{"--bundle", e.cwd, "--no-tui", "run", "--shell", "--", `echo "CD=${SHEAF_CD_FILE-unset}"`},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errb})
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "CD=unset\n") {
		t.Fatalf("a child saw SHEAF_CD_FILE:\n%s", out.String())
	}
	if v, ok := os.LookupEnv(cdFileVar); ok {
		t.Fatalf("SHEAF_CD_FILE still in the environment: %q", v)
	}
}

// S3: with errexit (bash set -e, zsh -e), a failing sheaf must not end the
// shell before the function has removed its temporary file.
func TestShellFunctionCleansUpUnderErrexit(t *testing.T) {
	t.Parallel()
	for _, sh := range []string{"bash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			t.Parallel()
			env, initFile, _, tmp := wrapperFixture(t, sh)
			cmd := shellCmd(t, sh, `set -e; . "$1"; sheaf rc 3; echo unreachable`, initFile)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); code != 3 || strings.Contains(string(out), "unreachable") {
				t.Fatalf("exit %d (%v), want 3:\n%s", code, err, out)
			}
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Fatalf("files left in TMPDIR: %v", left)
			}
		})
	}
}

// N1: when the temporary file cannot be created, the function runs sheaf
// without it, silently, and keeps its exit status.
func TestShellFunctionFallsBackWithoutTempFile(t *testing.T) {
	t.Parallel()
	scripts := map[string]string{
		"bash": `. "$1"; cd /; sheaf args "a b"; sheaf file; sheaf rc 5; echo "rc=$? pwd=$PWD"`,
		"zsh":  `. "$1"; cd /; sheaf args "a b"; sheaf file; sheaf rc 5; echo "rc=$? pwd=$PWD"`,
		"fish": `source $argv[1]; cd /; sheaf args "a b"; sheaf file; sheaf rc 5; echo "rc=$status pwd=$PWD"`,
	}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		t.Run(sh, func(t *testing.T) {
			t.Parallel()
			env, initFile, _, _ := wrapperFixture(t, sh)
			cmd := shellCmd(t, sh, scripts[sh], initFile)
			cmd.Env = append(env, "TMPDIR=/nonexistent/sheaf-test")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if want := "<a b>\nfile=no\nrc=5 pwd=/\n"; string(out) != want {
				t.Fatalf("got:\n%s\nwant:\n%s", out, want)
			}
		})
	}
}
