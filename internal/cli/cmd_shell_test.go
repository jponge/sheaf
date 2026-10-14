package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestCompletionScriptsAndHelp(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out := e.mustRun(t, "completion", sh)
		if !strings.Contains(out, "sheaf") || len(out) < 500 {
			t.Fatalf("completion %s: implausible script (%d bytes)", sh, len(out))
		}
		help := e.mustRun(t, "completion", sh, "--help")
		if !strings.Contains(help, "sheaf completion "+sh) {
			t.Fatalf("completion %s --help lacks usage:\n%s", sh, help)
		}
	}
	h := e.mustRun(t, "completion", "fish", "--help")
	if !strings.Contains(h, "sheaf completion fish > ~/.config/fish/completions/sheaf.fish") {
		t.Fatalf("fish help lacks the install command:\n%s", h)
	}
	if h := e.mustRun(t, "completion", "bash", "--help"); !strings.Contains(h, "source <(sheaf completion bash)") {
		t.Fatalf("bash help:\n%s", h)
	}
	if h := e.mustRun(t, "completion", "zsh", "--help"); !strings.Contains(h, "compinit") {
		t.Fatalf("zsh help:\n%s", h)
	}
	if _, _, code := e.run(t, "completion", "tcsh"); code == 0 {
		t.Fatal("unknown shell accepted")
	}
	if _, _, code := e.run(t, "completion"); code == 0 {
		t.Fatal("missing shell accepted")
	}
}

func TestShellInitScripts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out := e.mustRun(t, "shell-init", sh)
		if !strings.Contains(out, "scd") || !strings.Contains(out, "sheaf path") {
			t.Fatalf("shell-init %s:\n%s", sh, out)
		}
	}
	// The empty word being completed must reach `sheaf __complete` as an
	// argument (fish drops an unquoted empty command substitution); fish is
	// not always available to check the syntax, so pin this detail.
	if out := e.mustRun(t, "shell-init", "fish"); !strings.Contains(out, `set -a args "$a"`) {
		t.Fatalf("fish completion must keep an empty current token:\n%s", out)
	}
	if _, _, code := e.run(t, "shell-init", "tcsh"); code == 0 {
		t.Fatal("unknown shell accepted")
	}
	if h := e.mustRun(t, "shell-init", "--help"); !strings.Contains(h, `eval "$(sheaf shell-init bash)"`) || !strings.Contains(h, "sheaf shell-init fish | source") {
		t.Fatalf("shell-init help:\n%s", h)
	}
}

// TestGeneratedScriptsParse syntax-checks every generated script with the
// shells installed on the machine; the others are skipped explicitly.
func TestGeneratedScriptsParse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	checks := map[string][]string{
		"bash": {"bash", "-n"},
		"zsh":  {"zsh", "-n"},
		"fish": {"fish", "--no-execute"},
	}
	for _, sh := range []string{"bash", "zsh", "fish"} {
		for _, gen := range []string{"completion", "shell-init"} {
			t.Run(gen+"/"+sh, func(t *testing.T) {
				bin, err := exec.LookPath(sh)
				if err != nil {
					t.Skipf("%s is not installed: generated %s script NOT syntax-checked", sh, gen)
				}
				script := filepath.Join(t.TempDir(), "script")
				if err := os.WriteFile(script, []byte(e.mustRun(t, gen, sh)), 0o644); err != nil {
					t.Fatal(err)
				}
				args := append(checks[sh][1:], script)
				if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
					t.Fatalf("%s %v: %v\n%s", sh, args, err, out)
				}
			})
		}
	}
}

// sheafBin is the sheaf binary built by sheafBinary.
var sheafBin struct {
	once sync.Once
	dir  string
	out  []byte
	err  error
}

// sheafBinary builds the sheaf binary once per test process, into a
// directory of the process (callers must not write into it), and returns the
// directory. The Go toolchain runs with the environment the process started
// with (see testutil.ToolchainEnv): its caches are under the real HOME.
func sheafBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH: cannot build the sheaf binary")
	}
	dir := filepath.Join(testutil.ProcessTempDir(t), "sheaf-bin")
	sheafBin.once.Do(func() {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "sheaf"), "github.com/jponge/sheaf/cmd/sheaf")
		cmd.Env = testutil.ToolchainEnv()
		sheafBin.out, sheafBin.err = cmd.CombinedOutput()
		sheafBin.dir = dir
	})
	if sheafBin.err != nil {
		t.Fatalf("go build: %v\n%s", sheafBin.err, sheafBin.out)
	}
	return sheafBin.dir
}

// runBash runs script in bash with the sheaf binary on PATH, from dir.
func runBash(t *testing.T, binDir, dir, script string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	cmd := exec.Command(bash, "--norc", "--noprofile", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + binDir + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

func TestScdEndToEndInBash(t *testing.T) {
	t.Parallel()
	bin := sheafBinary(t)
	_, root := complBundle(t)
	pre := `eval "$(sheaf shell-init bash)"; `

	got := runBash(t, bin, root, pre+`scd a; echo "PWD=$PWD"`)
	if want := "PWD=" + filepath.Join(root, "a", "main") + "\n"; got != want {
		t.Fatalf("scd a: got %q, want %q", got, want)
	}
	got = runBash(t, bin, root, pre+`scd a feat/x; echo "PWD=$PWD"; scd; echo "PWD=$PWD"`)
	want := "PWD=" + filepath.Join(root, "a", "feat-x") + "\nPWD=" + root + "\n"
	if got != want {
		t.Fatalf("scd a feat/x: got %q, want %q", got, want)
	}
	// inside a checkout one argument is a worktree of that repository
	got = runBash(t, bin, filepath.Join(root, "a", "main"), pre+`scd feat-x; echo "PWD=$PWD"`)
	if want := "PWD=" + filepath.Join(root, "a", "feat-x") + "\n"; got != want {
		t.Fatalf("scd feat-x: got %q, want %q", got, want)
	}
	// failure: no cd, status propagated
	got = runBash(t, bin, root, pre+`scd nosuch; echo "rc=$? PWD=$PWD"`)
	if !strings.Contains(got, `no repository named "nosuch"`) || !strings.HasSuffix(got, "rc=1 PWD="+root+"\n") {
		t.Fatalf("scd nosuch: got %q", got)
	}
	// completion of scd mirrors sheaf path
	got = runBash(t, bin, root, pre+`COMP_WORDS=(scd a ""); COMP_CWORD=2; _sheaf_scd; printf '%s\n' "${COMPREPLY[@]}"; `+
		`COMP_WORDS=(scd ""); COMP_CWORD=1; _sheaf_scd; printf '%s\n' "${COMPREPLY[@]}"; `+
		`COMP_WORDS=(scd a f); COMP_CWORD=2; _sheaf_scd; printf '%s\n' "${COMPREPLY[@]}"; complete -p scd`)
	if want := "feat-x\nfeat/x\nmain\na\nb\nfeat-x\nfeat/x\ncomplete -F _sheaf_scd scd\n"; got != want {
		t.Fatalf("scd completion: got %q, want %q", got, want)
	}
}

// complete is the bash completion of scd for the given words; the last one is
// the word being completed. It prints one candidate per line in brackets.
func scdComplete(words ...string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = "'" + w + "'"
	}
	return fmt.Sprintf(`COMP_WORDS=(%s); COMP_CWORD=%d; _sheaf_scd; printf '[%%s]\n' "${COMPREPLY[@]}"; `, strings.Join(q, " "), len(words)-1)
}

func TestScdBashCompletionEscapesCandidates(t *testing.T) {
	t.Parallel()
	bin := sheafBinary(t)
	e, root := complBundle(t)
	e.mustRun(t, "wt", "add", "a", "q&r", "--no-fetch")
	e.mustRun(t, "wt", "add", "a", "x$y", "--no-fetch")
	pre := `eval "$(sheaf shell-init bash)"; `
	got := runBash(t, bin, root, pre+scdComplete("scd", "a", "q")+scdComplete("scd", "a", "x"))
	if want := "[q\\&r]\n[q-r]\n[x\\$y]\n[x-y]\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestScdBashCompletionBundleFlag(t *testing.T) {
	t.Parallel()
	bin := sheafBinary(t)
	_, root := complBundle(t)
	home, name := filepath.Dir(root), filepath.Base(root)
	elsewhere := t.TempDir()
	pre := `eval "$(sheaf shell-init bash)"; HOME=` + home + `; `
	want := "[feat-x]\n[feat/x]\n[main]\n"
	for desc, words := range map[string][]string{
		"separate":      {"scd", "--bundle", root, "a", ""},
		"equals":        {"scd", "--bundle=" + root, "a", ""},
		"split equals":  {"scd", "--bundle", "=", root, "a", ""},
		"tilde":         {"scd", "--bundle", "~/" + name, "a", ""},
		"tilde equals":  {"scd", "--bundle", "=", "~/" + name, "a", ""},
		"tilde at word": {"scd", "--bundle=~/" + name, "a", ""},
	} {
		if got := runBash(t, bin, elsewhere, pre+scdComplete(words...)); got != want {
			t.Errorf("%s: got %q, want %q", desc, got, want)
		}
	}
	// the value of --bundle is completed with directories
	got := runBash(t, bin, home, pre+scdComplete("scd", "--bundle", name[:3])+`complete -p scd >/dev/null`)
	if want := "[" + name + "]\n"; got != want {
		t.Errorf("--bundle directories: got %q, want %q", got, want)
	}
	got = runBash(t, bin, elsewhere, pre+scdComplete("scd", "--bundle", "=", "~/"+name[:3]))
	if want := "[" + home + "/" + name + "]\n"; got != want && got != "[~/"+name+"]\n" {
		t.Errorf("--bundle=~ directories: got %q", got)
	}
}

func TestScdBashCompletionHidesGlobalFlags(t *testing.T) {
	t.Parallel()
	bin := sheafBinary(t)
	_, root := complBundle(t)
	got := runBash(t, bin, root, `eval "$(sheaf shell-init bash)"; `+scdComplete("scd", "-"))
	for _, bad := range []string{"--jobs", "--no-tui", "-j"} {
		if strings.Contains(got, "["+bad+"]") {
			t.Errorf("%s offered:\n%s", bad, got)
		}
	}
	for _, good := range []string{"--bundle", "--repo", "--branch", "--dir"} {
		if !strings.Contains(got, "["+good+"]") {
			t.Errorf("%s not offered:\n%s", good, got)
		}
	}
}

const ptyDriver = `
import os, pty, select, sys, time
bindir, cwd, rc = sys.argv[1:4]
pid, fd = pty.fork()
if pid == 0:
    os.chdir(cwd)
    env = {"PATH": bindir + ":/usr/bin:/bin", "HOME": cwd, "TERM": "dumb", "PS1": "PROMPT$ ", "LC_ALL": "C"}
    os.execve("/bin/bash", ["bash", "--noprofile", "--rcfile", rc, "-i"], env)
def read(t):
    out = b""; end = time.time() + t
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try: out += os.read(fd, 65536)
            except OSError: break
    return out.decode(errors="replace")
res = read(1.5)
for keys in sys.argv[4:]:
    os.write(fd, keys.encode().decode("unicode_escape").encode())
    res += read(1.0)
os.write(fd, b"exit\n"); res += read(0.3)
sys.stdout.write(res)
`

// TestScdBashCompletionInteractive drives a real readline through a pty: the
// inserted word must be escaped so that the shell passes it as one argument.
func TestScdBashCompletionInteractive(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed: no pty-driven completion test")
	}
	bin := sheafBinary(t)
	e, root := complBundle(t)
	e.mustRun(t, "wt", "add", "a", "w&z", "--no-fetch")
	e.mustRun(t, "wt", "add", "a", "x$y", "--no-fetch")
	dir := t.TempDir()
	rc := filepath.Join(dir, "rc")
	script := filepath.Join(dir, "drive.py")
	// args stands for scd and shows the arguments it receives
	rcText := "eval \"$(sheaf shell-init bash)\"\nargs() { printf '<%s>' \"$@\"; echo; }\ncomplete -F _sheaf_scd args\n"
	for f, c := range map[string]string{rc: rcText, script: ptyDriver} {
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(py, "-I", script, bin, root, rc, "args a w\\\\&\\t", "\\n", "args a x\\\\$\\t", "\\n").CombinedOutput()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	for _, want := range []string{"<a><w&z>", "<a><x$y>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
