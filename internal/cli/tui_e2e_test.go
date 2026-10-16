package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the
// bubbletea renderer and the reads of a test waiting for some output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// key is one Read result of a scripted TUI input (bubbletea parses each
// read on its own, so one key is one key press); wait, if set, runs first.
type key struct {
	keys string
	wait func()
}

// scriptIn is the standard input of a forced-TUI run: one segment of keys
// per bubbletea program the command runs, in order. The read loop of a
// program consumes its segment and then gets io.EOF (which also makes
// tui.Run quit when the program is still running), so a program never
// reads the keys meant for the next one. After the last segment, reads
// block until the end of the test when blockAtEnd is set (so that the last
// program, typically the progress view, is not ended by EOF), and return
// io.EOF otherwise.
type scriptIn struct {
	mu         sync.Mutex
	segs       [][]key
	blockAtEnd bool
	release    chan struct{}
}

func newScript(t *testing.T, blockAtEnd bool, segs ...[]key) *scriptIn {
	s := &scriptIn{segs: segs, blockAtEnd: blockAtEnd, release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *scriptIn) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.segs) == 0 {
		s.mu.Unlock()
		if s.blockAtEnd {
			<-s.release
		}
		return 0, io.EOF
	}
	seg := s.segs[0]
	if len(seg) == 0 {
		s.segs = s.segs[1:]
		s.mu.Unlock()
		return 0, io.EOF
	}
	k := seg[0]
	s.segs[0] = seg[1:]
	s.mu.Unlock()
	if k.wait != nil {
		k.wait()
	}
	return copy(p, k.keys), nil
}

// keys turns each argument into one key press.
func keys(ks ...string) []key {
	out := make([]key, 0, len(ks))
	for _, k := range ks {
		out = append(out, key{keys: k})
	}
	return out
}

// waitOutput returns a wait function polling out until it contains want.
func waitOutput(t *testing.T, out *syncBuffer, want string) func() {
	return func() {
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Errorf("output never contained %q:\n%s", want, out.String())
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// runTUI executes the CLI with the TUI forced on and in as standard input.
func (e *env) runTUI(t *testing.T, in io.Reader, out *syncBuffer, args ...string) (stderr string, code int) {
	t.Helper()
	return e.runTUIWith(t, nil, in, out, args...)
}

// runTUIWith is runTUI with the interrupt controller ctl (nil: a new one).
func (e *env) runTUIWith(t *testing.T, ctl *interruptCtl, in io.Reader, out *syncBuffer, args ...string) (stderr string, code int) {
	t.Helper()
	var errb syncBuffer
	d := e.deps()
	d.ctl = ctl
	d.UseTUI = func(io.Reader, io.Writer, bool) bool { return true }
	code = executeDeps(Streams{In: in, Out: out, Err: &errb}, d, args)
	return errb.String(), code
}

func TestTUIDecisionIsInjectableAndPlainByDefault(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	// Not forced: the test streams are not terminals, remove needs --yes.
	_, errs, code := e.run(t, "remove", "alpha")
	if code != 1 || !strings.Contains(errs, "--yes") {
		t.Fatalf("plain mode: exit %d, stderr %q", code, errs)
	}
}

func TestTUIAddOrgMenuSelectsTwoOfThree(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.origin(t, "o", n, false)
	}
	var out syncBuffer
	// Menu: space (a), j, space (b), enter; then the progress view.
	in := newScript(t, true, keys(" ", "j", " ", "\r"))
	errs, code := e.runTUI(t, in, &out, "add", "--org", "o")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	b := load(t, e.cwd)
	if got := names(b); strings.Join(got, ",") != "a,b" {
		t.Fatalf("repositories = %v, want a,b\n%s", got, out.String())
	}
	if len(b.Sources) != 1 || b.Sources[0].Org != "o" {
		t.Fatalf("sources = %+v", b.Sources)
	}
	o := out.String()
	for _, want := range []string{"Add repositories of o", "2 done, 0 skipped, 0 failed"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
	if exists(filepath.Join(e.cwd, "c")) {
		t.Error("c cloned although not selected")
	}
}

func TestTUIAddOrgMenuShowsPresentAndCancelChangesNothing(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	for _, n := range []string{"a", "b"} {
		e.origin(t, "o", n, false)
	}
	e.mustRun(t, "add", "o/a")
	before := readYAML(t, e.cwd)
	var out syncBuffer
	in := newScript(t, false, keys(" ", "q"))
	errs, code := e.runTUI(t, in, &out, "add", "--org", "o")
	if code != 1 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("exit %d, stderr %q, want 1 and cancelled", code, errs)
	}
	if !strings.Contains(out.String(), "[present]") {
		t.Errorf("menu does not mark a as present:\n%s", out.String())
	}
	if readYAML(t, e.cwd) != before || exists(filepath.Join(e.cwd, "b")) {
		t.Fatal("cancel changed the bundle")
	}
}

func TestTUISyncAddsSelectedAndDeclinesExclusion(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	var out syncBuffer
	// Proposals: new b, new c. Select b; then decline excluding c.
	in := newScript(t, true, keys(" ", "\r"), keys("\r"))
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !exists(filepath.Join(root, "b", "main")) || exists(filepath.Join(root, "c")) {
		t.Fatalf("b not cloned or c cloned:\n%s", out.String())
	}
	if y := readYAML(t, root); strings.Contains(y, "exclude") {
		t.Fatalf("c excluded although declined:\n%s", y)
	}
	o := out.String()
	for _, want := range []string{"Exclude", "done     b", "1 done, 0 skipped, 0 failed"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
}

func TestTUISyncExcludesChosenNewRepository(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	var out syncBuffer
	// Select nothing; exclude c (second item of the exclusion menu).
	in := newScript(t, true, keys("\r"), keys("j", " ", "\r"))
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	y := readYAML(t, root)
	if !strings.Contains(y, "exclude") || !strings.Contains(y, "- c") || strings.Contains(y, "- b") {
		t.Fatalf("sheaf.yaml:\n%s", y)
	}
	if !strings.Contains(out.String(), "excluded o/c") {
		t.Errorf("stdout lacks the exclusion:\n%s", out.String())
	}
}

// goneB tracks a and b from o; the listing then lacks b (whose remote
// still answers) and offers c.
func goneB(t *testing.T) (*env, string) {
	t.Helper()
	e, root := syncBundle(t)
	e.mustRun(t, "add", "--org", "o", "--repo", "b")
	a, _ := load(t, root).Repository("a")
	e.fake = hostingtest.NewFake()
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "a", CloneURL: a.URL, DefaultBranch: "main"})
	e.origin(t, "o", "c", false)
	return e, root
}

// The TUI never implies --force: a gone repository whose remote still
// answers (gh token scope) is refused, and the refusal is displayed.
func TestTUISyncGoneRemovalRefusalIsDisplayed(t *testing.T) {
	e, root := goneB(t)
	var out syncBuffer
	// Proposals: new c, gone b. Select b (second), no exclusion of c,
	// confirm the removal.
	in := newScript(t, true, keys("j", " ", "\r"), keys("\r"), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !exists(filepath.Join(root, "b", "main")) {
		t.Fatal("b removed without --force")
	}
	o := out.String()
	for _, want := range []string{"Remove 1 repository gone from its organization?", "failed   b", "0 done, 0 skipped, 1 failed"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
	if !strings.Contains(errs, "authentication scope") {
		t.Errorf("stderr lacks the refusal details:\n%s", errs)
	}
}

func TestTUISyncDeclinedRemovalChangesNothing(t *testing.T) {
	e, root := goneB(t)
	before := readYAML(t, root)
	var out syncBuffer
	// Select c and b, no exclusion, decline the removal.
	in := newScript(t, false, keys(" ", "j", " ", "\r"), []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 1 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if !exists(filepath.Join(root, "b")) || exists(filepath.Join(root, "c")) || readYAML(t, root) != before {
		t.Fatal("declined sync changed the bundle")
	}
}

func TestTUIRemoveConfirmYes(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "remove", "alpha")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(filepath.Join(e.cwd, "alpha")) || len(load(t, e.cwd).Repositories) != 0 {
		t.Fatal("alpha not removed")
	}
	o := out.String()
	for _, want := range []string{"Remove alpha?", "will delete " + filepath.Join(e.cwd, "alpha"), "removed " + filepath.Join(e.cwd, "alpha")} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
}

func TestTUIRemoveConfirmNo(t *testing.T) {
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "remove", "alpha")
	if code != 1 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("exit %d, stderr %q, want 1 and cancelled", code, errs)
	}
	if !exists(filepath.Join(e.cwd, "alpha", "main")) || len(load(t, e.cwd).Repositories) != 1 {
		t.Fatal("alpha removed although declined")
	}
}

func TestTUIRemoveWithYesDoesNotAsk(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.addRepo(t, "alpha")
	var out syncBuffer
	in := newScript(t, true)
	errs, code := e.runTUI(t, in, &out, "remove", "--yes", "alpha")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if strings.Contains(out.String(), "[y/N]") || exists(filepath.Join(e.cwd, "alpha")) {
		t.Fatalf("asked or not removed:\n%s", out.String())
	}
}

func TestTUIWorktreeRemoveShowsLostCommits(t *testing.T) {
	e := newBundleEnv(t)
	_, primary := e.addRepo(t, "alpha")
	wt := filepath.Join(e.cwd, "alpha", "feat")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "feat", wt)
	writeFile(t, filepath.Join(wt, "x.txt"), "x\n")
	testutil.Git(t, wt, "add", "x.txt")
	testutil.Git(t, wt, "commit", "-q", "-m", "local only")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "wt", "rm", "-D", "alpha", "feat")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(wt) {
		t.Fatal("worktree not removed")
	}
	if !strings.Contains(out.String(), "will delete unmerged branch feat: 1 commit will be lost (-D)") {
		t.Errorf("dialog lacks the lost commits:\n%s", out.String())
	}
}

func TestTUIPruneConfirms(t *testing.T) {
	e := newBundleEnv(t)
	o, primary := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, primary, "feat", "feat")
	testutil.Git(t, o.Path, "branch", "-D", "feat") // gone upstream
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "prune")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(wt) {
		t.Fatalf("worktree not pruned:\n%s", out.String())
	}
	for _, want := range []string{"Prune 1 candidate?", "would remove worktree alpha/feat", "1 removed, 0 kept, 0 failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTUIInitAdoptsSelectedCandidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dir := t.TempDir()
	for _, n := range []string{"alpha", "beta"} {
		o := testutil.NewOrigin(t, testutil.OriginOpts{})
		testutil.Git(t, dir, "clone", "-q", o.URL, filepath.Join(dir, n))
	}
	var out syncBuffer
	// Both preselected: deselect beta (second), confirm.
	in := newScript(t, true, keys("j", " ", "\r"))
	errs, code := e.runTUI(t, in, &out, "init", dir)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	root, _ := filepath.EvalSymlinks(dir)
	if got := names(load(t, root)); strings.Join(got, ",") != "alpha" {
		t.Fatalf("adopted %v, want alpha\n%s", got, out.String())
	}
	if !exists(filepath.Join(dir, "alpha", "main", ".git")) || !exists(filepath.Join(dir, "beta", ".git")) {
		t.Fatal("unexpected layout after adoption")
	}
}

func TestTUIUpdateProgressAndSummary(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, _ := e.addRepo(t, "alpha")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	var out syncBuffer
	in := newScript(t, true)
	errs, code := e.runTUI(t, in, &out, "update")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o2 := out.String()
	for _, want := range []string{"Updating", "fast-forwarded  alpha/main", "1 fast-forwarded, 0 up to date, 0 skipped, 0 failed"} {
		if !strings.Contains(o2, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o2)
		}
	}
	if strings.Contains(errs, "started") {
		t.Errorf("plain progress lines on stderr in TUI mode:\n%s", errs)
	}
}
