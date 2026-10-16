package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
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
				var bin string
				if sh == "bash" {
					var err error
					if bin, err = exec.LookPath(sh); err != nil {
						t.Skipf("%s is not installed: generated %s script NOT syntax-checked", sh, gen)
					}
				} else {
					bin = shellBinary(t, sh)
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

// ptyDriver runs an interactive bash or zsh (the binary given as first
// argument; rc, then PS1 set to recognise prompts) on a pty and types each
// argument (backslash escapes decoded),
// waiting for events instead of fixed delays: the first prompt; after keys
// ending with a newline, the next prompt; after other keys (e.g. ending with
// a tab), the echo of their printable part, the rest (the completion) being
// processed by readline before the next keys, like typeahead. A wait that
// times out (per step, and 20 s overall) is noted in the output and the
// driver goes on; the test's assertions report it.
const ptyDriver = `
import os, pty, select, shlex, sys, time
shell, bindir, cwd, rc = sys.argv[1:5]
zsh = os.path.basename(shell).startswith("zsh")
# The system bashrc may set PS1: set it after rc, to recognise prompts.
wrapper = rc + "-driver"
if zsh:  # zsh reads $ZDOTDIR/.zshrc
    os.mkdir(wrapper)
    wrapper = os.path.join(wrapper, ".zshrc")
with open(wrapper, "w") as f:
    f.write(". %s\nPS1='PROMPT$ '\n" % shlex.quote(rc))
pid, fd = pty.fork()
if pid == 0:
    os.chdir(cwd)
    env = {"PATH": bindir + ":/usr/bin:/bin", "HOME": cwd, "TERM": "dumb", "PS1": "PROMPT$ ", "LC_ALL": "C"}
    if zsh:  # -d: no global rc files; see shellBinary for the passed-through variables
        env["ZDOTDIR"] = os.path.dirname(wrapper)
        for k in ("LD_LIBRARY_PATH", "FPATH", "SHEAF_TEST_ZSH_MODULE_PATH"):
            if k in os.environ:
                env[k] = os.environ[k]
        os.execve(shell, ["zsh", "-d", "-i"], env)
    os.execve(shell, ["bash", "--noprofile", "--rcfile", wrapper, "-i"], env)
overall = time.time() + 20
out = b""
eof = False
def wait(what, cond, step=10):
    global out, eof
    end = min(time.time() + step, overall)
    while not cond(out.decode(errors="replace")):
        if eof or time.time() >= end:
            out += ("\nDRIVER: gave up waiting for %s\n" % what).encode()
            return
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try:
                b = os.read(fd, 65536)
            except OSError:
                b = b""
            if not b:
                eof = True
            out += b
prompts = lambda s: s.count("PROMPT$ ")
wait("the first prompt", lambda s: prompts(s) >= 1)
for keys in sys.argv[5:]:
    k = keys.encode().decode("unicode_escape")
    if k.endswith("\n"):
        n = prompts(out.decode(errors="replace"))
        os.write(fd, k.encode())
        wait("the prompt after %r" % keys, lambda s, n=n: prompts(s) > n)
    else:
        start = len(out.decode(errors="replace"))
        os.write(fd, k.encode())
        echo = k.rstrip("".join(map(chr, range(32))))  # without the tab
        wait("the echo of %r" % keys, lambda s, start=start, echo=echo: echo in s[start:])
os.write(fd, b"exit\n")
wait("the shell to exit", lambda s: eof, step=5)
if not eof:
    os.kill(pid, 9)
os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
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
	out, err := exec.Command(py, "-I", script, "/bin/bash", bin, root, rc, "args a w\\\\&\\t", "\\n", "args a x\\\\$\\t", "\\n").CombinedOutput()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	for _, want := range []string{"<a><w&z>", "<a><x$y>"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// shellBinary returns the fish or zsh binary used by the shell tests: the
// path in SHEAF_TEST_FISH or SHEAF_TEST_ZSH when set, else the shell found on
// PATH; the test is skipped, saying so, when there is none. A shell unpacked
// outside its install prefix may need more environment, which the tests pass
// through to it: LD_LIBRARY_PATH (its libraries), FPATH (zsh: the function
// directories, for compinit) and SHEAF_TEST_ZSH_MODULE_PATH (zsh: the module
// directory, prepended to module_path; zsh does not import MODULE_PATH). fish
// finds its data relative to the binary when <prefix>/share/fish and
// <prefix>/etc/fish both exist for <prefix>/bin/fish. With
// SHEAF_TEST_REQUIRE_SHELLS set, a missing shell fails the test instead
// (`make test-shells` sets it, and sets up shells unpacked from packages).
func shellBinary(t *testing.T, name string) string {
	t.Helper()
	v := "SHEAF_TEST_" + strings.ToUpper(name)
	if p := os.Getenv(v); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil && os.Getenv("SHEAF_TEST_REQUIRE_SHELLS") != "" {
		t.Fatalf("%s is not installed (nor %s set) and SHEAF_TEST_REQUIRE_SHELLS is set", name, v)
	}
	if err != nil {
		t.Skipf("%s is not installed (nor %s set): %s integration NOT tested", name, v, name)
	}
	return p
}

// shellEnv is the environment of the shells run by the tests: the sheaf
// binary first on PATH, a HOME and XDG directories of the test, and the
// variables documented at shellBinary.
func shellEnv(t *testing.T, binDir string) []string {
	home := t.TempDir()
	env := []string{"PATH=" + binDir + ":" + os.Getenv("PATH"), "HOME=" + home, "LC_ALL=C",
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache")}
	for _, k := range []string{"LD_LIBRARY_PATH", "FPATH", "SHEAF_TEST_ZSH_MODULE_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// runFish runs script in fish (no user configuration) with the sheaf
// completion and scd loaded, from dir; t prints the candidates of `complete
// -C <line>` without descriptions, sorted, then "==".
func runFish(t *testing.T, binDir, dir, script string) string {
	t.Helper()
	fish := shellBinary(t, "fish")
	pre := "sheaf completion fish | source; sheaf shell-init fish | source\n" +
		"function t; complete -C $argv[1] | string replace -r -- '\\t.*' '' | sort; echo ==; end\n"
	cmd := exec.Command(fish, "--no-config", "-c", pre+script)
	cmd.Dir = dir
	cmd.Env = shellEnv(t, binDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fish: %v\n%s", err, out)
	}
	return string(out)
}

// shellBundle, like complBundle, has repositories a and b, in a directory
// whose path has a space; a has worktrees feat/x, q&r and x$y (shell
// metacharacters), a tag worktree v1 (tag v2 is free) and branch dev; the
// organization o is tracked.
func shellBundle(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	root := filepath.Join(t.TempDir(), "my bundle")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x", "dev", "q&r", "x$y"}})
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "a", CloneURL: o.URL, DefaultBranch: "main"})
	e.origin(t, "o", "b", false)
	e.mustRun(t, "add", "o/a", "o/b")
	e.mustRun(t, "add", "--org", "o", "--all") // tracks the organization o
	primary := filepath.Join(root, "a", "main")
	testutil.Git(t, primary, "tag", "v1")
	testutil.Git(t, primary, "tag", "v2")
	for _, args := range [][]string{{"feat/x"}, {"q&r"}, {"x$y"}, {"--tag", "v1"}} {
		e.mustRun(t, append(append([]string{"wt", "add", "a"}, args...), "--no-fetch")...)
	}
	return e, root
}

func TestScdEndToEndInFish(t *testing.T) {
	t.Parallel()
	shellBinary(t, "fish")
	bin := sheafBinary(t)
	_, root := shellBundle(t)
	got := runFish(t, bin, root, `scd a; pwd; scd a feat/x; pwd; scd a 'q&r'; pwd; scd a 'x$y'; pwd; scd a v1; pwd; scd; pwd
scd nosuch; echo "status=$status"; pwd
cd a/main; scd feat-x; pwd; scd b; pwd`)
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	want := strings.Join([]string{j("a", "main"), j("a", "feat-x"), j("a", "q-r"), j("a", "x-y"), j("a", "v1"), root,
		`sheaf: no repository named "nosuch" in the bundle (repositories: a, b)`, "status=1", root,
		j("a", "feat-x"), j("b", "main")}, "\n") + "\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFishCompletion(t *testing.T) {
	t.Parallel()
	shellBinary(t, "fish")
	bin := sheafBinary(t)
	_, root := shellBundle(t)
	parent, name := filepath.Dir(root), filepath.Base(root)
	all := "feat-x\nfeat/x\nmain\nq&r\nq-r\nv1\nx$y\nx-y\n==\n"
	for _, c := range []struct{ dir, line, want string }{
		{root, "scd ", "a\nb\n==\n"},
		{root, "scd a ", all},
		{root, "scd a f", "feat-x\nfeat/x\n==\n"},
		{parent, `scd --bundle "` + name + `" a `, all},
		{parent, `scd --bundle ` + strings.ReplaceAll(name, " ", `\ `) + ` a `, all},
		{filepath.Join(root, "a", "main"), "scd ", "a\nb\n" + all},
		{root, "sheaf wt rm a ", "feat-x\nfeat/x\nq&r\nq-r\nv1\nx$y\nx-y\n==\n"},
		{root, "sheaf wt add a ", "dev\n==\n"},
		{root, "sheaf wt add a --tag ", "v2\n==\n"},
		{root, "sheaf add --org ", "o\n==\n"},
		// the current token as typed, with quotes or escapes
		{root, `scd a q\&`, "q&r\n==\n"},
		{root, `scd a 'x$`, "x$y\n==\n"},
		{parent, `scd --bundle my\ bu`, name + "/\n==\n"},
		{root, `sheaf path a q\&`, "q&r\n==\n"},
		{root, `sheaf path a 'x$`, "x$y\n==\n"},
		// previous words with spaces (fish passes them unescaped)
		{parent, `sheaf --bundle "` + name + `" path a `, all},
		{parent, `sheaf --bundle ` + strings.ReplaceAll(name, " ", `\ `) + ` path a `, all},
	} {
		line := strings.ReplaceAll(c.line, `\`, `\\`)
		line = strings.ReplaceAll(line, `'`, `\'`)
		if got := runFish(t, bin, c.dir, "t '"+line+"'"); got != c.want {
			t.Errorf("complete -C %q in %s:\ngot:\n%s\nwant:\n%s", c.line, c.dir, got, c.want)
		}
	}
	// ~ in previous words is expanded (HOME is the shell's own)
	got := runFish(t, bin, parent, `ln -s "`+root+`" ~/b; t 'sheaf --bundle ~/b path a '; t 'scd --bundle ~/b a '`)
	if got != all+all {
		t.Errorf("--bundle ~/b:\ngot:\n%s\nwant:\n%s", got, all+all)
	}
}

// TestFishCompletionDoesNotEvaluateWords: completing must not run code in
// the words of the command line (fish does not expand (...) inside quotes).
func TestFishCompletionDoesNotEvaluateWords(t *testing.T) {
	t.Parallel()
	shellBinary(t, "fish")
	bin := sheafBinary(t)
	_, root := shellBundle(t)
	mark := filepath.Join(t.TempDir(), "evaluated")
	for _, line := range []string{
		`sheaf path "(touch ` + mark + `)" `,
		`sheaf path "$(touch ` + mark + `)" `,
		`scd "(touch ` + mark + `)" `,
		`scd "$(touch ` + mark + `)" `,
	} {
		runFish(t, bin, root, "t '"+line+"'")
		if _, err := os.Stat(mark); err == nil {
			t.Fatalf("completing %q ran the command substitution", line)
		}
	}
}

// TestZshCompletionInteractive drives zsh's completion system through a pty:
// the inserted word must be the candidate, escaped, for the words as typed.
func TestZshCompletionInteractive(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed: no pty-driven completion test")
	}
	zsh := shellBinary(t, "zsh")
	bin := sheafBinary(t)
	_, root := shellBundle(t)
	parent, name := filepath.Dir(root), filepath.Base(root)
	dir := t.TempDir()
	rc := filepath.Join(dir, "rc")
	script := filepath.Join(dir, "drive.py")
	// args stands for scd and shows the arguments it receives
	rcText := `[[ -n $SHEAF_TEST_ZSH_MODULE_PATH ]] && module_path=(${(s.:.)SHEAF_TEST_ZSH_MODULE_PATH} $module_path)
autoload -U compinit && compinit -u -D
source <(sheaf completion zsh)
eval "$(sheaf shell-init zsh)"
args() { printf '<%s>' "$@"; echo; }
compdef _sheaf_scd args
`
	for f, c := range map[string]string{rc: rcText, script: ptyDriver} {
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	esc := strings.ReplaceAll(name, " ", `\\ `) // decoded by the driver: "my\ bundle"
	cases := []struct{ keys, want string }{
		{`cd ` + esc + `\n`, ""},
		{`args a ma\t`, "<a><main>"},
		{`args a q\\&\t`, "<a><q&r>"},
		{`args a x\\$\t`, "<a><x$y>"},
		{`cd ..\n`, ""},
		{`args --bundle ` + esc + ` a ma\t`, "<--bundle><" + name + "><a><main>"},
		{`args --bundle '` + name + `' a ma\t`, "<--bundle><" + name + "><a><main>"},
		{`sheaf path --bundle ` + esc + ` a q\\&\t`, filepath.Join(root, "a", "q-r")},
		{`args --bundle ` + esc + ` a 'x$\t`, "<--bundle><" + name + "><a><x$y>"}, // last: if the quote is left open, the next lines are a continuation
	}
	args := []string{"-I", script, zsh, bin, parent, rc}
	for _, c := range cases {
		args = append(args, c.keys, `\n`)
		if strings.HasSuffix(c.keys, `\n`) {
			args = args[:len(args)-1]
		}
	}
	out, err := exec.Command(py, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	// parts[i+1]: from the prompt where case i is typed to the next one
	parts := strings.Split(string(out), "PROMPT$ ")
	for i, c := range cases {
		if i+1 >= len(parts) {
			t.Errorf("%q: no prompt", c.keys)
		} else if c.want != "" && !strings.Contains(parts[i+1], c.want+"\r\n") {
			t.Errorf("%q: missing %q", c.keys, c.want)
		}
	}
	if strings.Contains(string(out), "DRIVER:") {
		t.Error("the driver timed out")
	}
	if t.Failed() {
		t.Logf("pty output:\n%s", out)
	}
}

func TestScdEndToEndInZsh(t *testing.T) {
	t.Parallel()
	zsh := shellBinary(t, "zsh")
	bin := sheafBinary(t)
	_, root := shellBundle(t)
	cmd := exec.Command(zsh, "-f", "-c", `[[ -n $SHEAF_TEST_ZSH_MODULE_PATH ]] && module_path=(${(s.:.)SHEAF_TEST_ZSH_MODULE_PATH} $module_path)
eval "$(sheaf shell-init zsh)"
scd a; pwd; scd a feat/x; pwd; scd a 'q&r'; pwd; scd a 'x$y'; pwd; scd a v1; pwd; scd; pwd
scd nosuch; echo "status=$?"; pwd
cd a/main; scd feat-x; pwd; scd b; pwd`)
	cmd.Dir = root
	cmd.Env = shellEnv(t, bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zsh: %v\n%s", err, out)
	}
	j := func(p ...string) string { return filepath.Join(append([]string{root}, p...)...) }
	want := strings.Join([]string{j("a", "main"), j("a", "feat-x"), j("a", "q-r"), j("a", "x-y"), j("a", "v1"), root,
		`sheaf: no repository named "nosuch" in the bundle (repositories: a, b)`, "status=1", root,
		j("a", "feat-x"), j("b", "main")}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}
