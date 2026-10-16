package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubSheaf is a fake sheaf for the tests of the shell function `sheaf`:
// "cd DIR [STATUS]" writes DIR to $SHEAF_CD_FILE and exits with STATUS
// (default 0); "args ..." prints its arguments in angle brackets; "file"
// tells whether SHEAF_CD_FILE is set; "rc N" exits with N.
const stubSheaf = `#!/bin/sh
case "$1" in
cd) printf '%s' "$2" > "$SHEAF_CD_FILE"; exit "${3:-0}" ;;
args) shift; printf '<%s>' "$@"; echo ;;
file) if [ -n "$SHEAF_CD_FILE" ] && [ -f "$SHEAF_CD_FILE" ]; then echo file=yes; else echo file=no; fi ;;
rc) exit "$2" ;;
esac
`

// wrapperFixture writes the stub sheaf into a directory for PATH, the
// shell-init code of shell into a file, and creates the target directory
// (with a space, a leading dash and a dollar sign in its name) and an empty
// TMPDIR. It returns the environment of the shell and those paths.
func wrapperFixture(t *testing.T, shell string) (env []string, initFile, target, tmp string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	target = filepath.Join(dir, "-a b$c")
	tmp = filepath.Join(dir, "tmp")
	for _, d := range []string{bin, target, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "sheaf"), []byte(stubSheaf), 0o755); err != nil {
		t.Fatal(err)
	}
	initFile = filepath.Join(dir, "init")
	if err := os.WriteFile(initFile, []byte(shellInit[shell]), 0o644); err != nil {
		t.Fatal(err)
	}
	target, _ = filepath.EvalSymlinks(target)
	return append(shellEnv(t, bin), "TMPDIR="+tmp), initFile, target, tmp
}

// checkWrapper runs script with the shell and compares its output.
func checkWrapper(t *testing.T, cmd *exec.Cmd, tmp, want string) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("files left in TMPDIR: %v", left)
	}
}

// wrapperWant is the expected output of the wrapper scripts below.
func wrapperWant(target string) string {
	return strings.Join([]string{
		"rc=0 pwd=" + target, // cd on success
		"rc=3 pwd=/",         // no cd on failure, status kept
		"<a b><c$d><>",       // arguments passed unchanged
		"file=yes",           // the file exists while sheaf runs
		"rc=7",               // status of other commands kept
		"rc=0 pwd=/",         // nothing written: no cd
	}, "\n") + "\n"
}

func TestSheafShellFunctionBash(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	env, initFile, target, tmp := wrapperFixture(t, "bash")
	cmd := exec.Command(bash, "--norc", "--noprofile", "-c", `. "$1"; T=$2
cd /; sheaf cd "$T"; echo "rc=$? pwd=$PWD"
cd /; sheaf cd "$T" 3; echo "rc=$? pwd=$PWD"
sheaf args "a b" 'c$d' ''
sheaf file
sheaf rc 7; echo "rc=$?"
cd /; sheaf args >/dev/null; echo "rc=$? pwd=$PWD"`, "bash", initFile, target)
	cmd.Env = env
	checkWrapper(t, cmd, tmp, wrapperWant(target))
}

func TestSheafShellFunctionZsh(t *testing.T) {
	t.Parallel()
	zsh := shellBinary(t, "zsh")
	env, initFile, target, tmp := wrapperFixture(t, "zsh")
	cmd := exec.Command(zsh, "-f", "-c", `[[ -n $SHEAF_TEST_ZSH_MODULE_PATH ]] && module_path=(${(s.:.)SHEAF_TEST_ZSH_MODULE_PATH} $module_path)
. "$1"; T=$2
cd /; sheaf cd "$T"; echo "rc=$? pwd=$PWD"
cd /; sheaf cd "$T" 3; echo "rc=$? pwd=$PWD"
sheaf args "a b" 'c$d' ''
sheaf file
sheaf rc 7; echo "rc=$?"
cd /; sheaf args >/dev/null; echo "rc=$? pwd=$PWD"`, "zsh", initFile, target)
	cmd.Env = env
	checkWrapper(t, cmd, tmp, wrapperWant(target))
}

func TestSheafShellFunctionFish(t *testing.T) {
	t.Parallel()
	fish := shellBinary(t, "fish")
	env, initFile, target, tmp := wrapperFixture(t, "fish")
	cmd := exec.Command(fish, "--no-config", "-c", `source $argv[1]; set T $argv[2]
cd /; sheaf cd "$T"; echo "rc=$status pwd=$PWD"
cd /; sheaf cd "$T" 3; echo "rc=$status pwd=$PWD"
sheaf args "a b" 'c$d' ''
sheaf file
sheaf rc 7; echo "rc=$status"
cd /; sheaf args >/dev/null; echo "rc=$status pwd=$PWD"`, initFile, target)
	cmd.Env = env
	checkWrapper(t, cmd, tmp, wrapperWant(target))
}
