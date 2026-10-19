package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/ops"
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
// per bubbletea program the command runs, in order. tui.NewProgram claims
// the next segment when it creates a program (NextSegment), so a program
// reads its own segment and then io.EOF (which also makes tui.Run quit when
// the program is still running), whatever the scheduling of the readers:
// the reader of a program that has quit may still be reading when the next
// program starts reading. A program created after the last segment reads
// io.EOF, or blocks until the end of the test when blockAtEnd is set (so
// that the last program, typically the progress view, is not ended by
// EOF). Read, for other readers, consumes the segments in turn.
type scriptIn struct {
	mu         sync.Mutex
	segs       [][]key
	blockAtEnd bool
	release    chan struct{}

	// holdFirstEnd, when set, holds the read returning the end (io.EOF) of
	// the first segment until another read starts, forcing the interleaving
	// in which the program owning the first segment quits on its last key
	// before its reader reaches the end of the segment, and the reader of
	// the next program reads first.
	holdFirstEnd bool
	popped       int           // segments consumed
	holding      bool          // a read of the first end is held
	arrived      chan struct{} // closed by the read releasing it
}

func newScript(t *testing.T, blockAtEnd bool, segs ...[]key) *scriptIn {
	s := &scriptIn{segs: segs, blockAtEnd: blockAtEnd, release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

// hold implements holdFirstEnd for a read; firstEnd tells whether it
// returns the end of the first segment. Called with s.mu held, which it
// releases while holding.
func (s *scriptIn) hold(firstEnd bool) {
	if s.holding {
		s.holding = false
		close(s.arrived)
		return
	}
	if firstEnd && s.holdFirstEnd && s.arrived == nil {
		s.holding, s.arrived = true, make(chan struct{})
		arrived := s.arrived
		s.mu.Unlock()
		<-arrived
		s.mu.Lock()
	}
}

func (s *scriptIn) Read(p []byte) (int, error) {
	s.mu.Lock()
	s.hold(s.popped == 0 && len(s.segs) > 0 && len(s.segs[0]) == 0)
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
		s.popped++
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

// NextSegment claims the next segment for a program being created (see
// tui.NewProgram).
func (s *scriptIn) NextSegment() io.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segs) == 0 {
		return &segmentIn{s: s, past: true}
	}
	r := &segmentIn{s: s, keys: s.segs[0], first: s.popped == 0}
	s.segs = s.segs[1:]
	s.popped++
	return r
}

// segmentIn reads one segment of a scriptIn: its keys, then io.EOF.
type segmentIn struct {
	s     *scriptIn
	keys  []key
	first bool // the first segment
	past  bool // claimed after the last segment
}

func (r *segmentIn) Read(p []byte) (int, error) {
	s := r.s
	s.mu.Lock()
	s.hold(r.first && len(r.keys) == 0)
	if len(r.keys) == 0 {
		s.mu.Unlock()
		if r.past && s.blockAtEnd {
			<-s.release
		}
		return 0, io.EOF
	}
	k := r.keys[0]
	r.keys = r.keys[1:]
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

// screenText returns what a terminal shows after out (screen and
// scrollback, see vtScreen), the output of a program writing to a buffer:
// the renderer only rewrites the changed parts of a frame, so a line of
// the final frame may be absent from out as is.
func screenText(out string) string {
	vt := newVTScreen(500, 200)
	vt.newline = true
	vt.write(out)
	return strings.Join(vt.lines(), "\n")
}

// waitScreen returns a wait function polling out until the terminal shows
// want (see screenText).
func waitScreen(t *testing.T, out *syncBuffer, want string) func() {
	return func() {
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(screenText(out.String()), want) {
			if time.Now().After(deadline) {
				t.Errorf("the terminal never showed %q:\n%s", want, screenText(out.String()))
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

// After the menu there is no further question about the new repositories
// left unselected (B19): the next program is the progress view, and
// nothing is recorded as excluded.
func TestTUISyncAddsSelectedWithoutExclusionStep(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	var out syncBuffer
	// Proposals: new b, new c. Select b; then the progress view, which
	// quits by itself once done. The second segment, read by the progress
	// view, waits for its final header; it only makes a second menu, if
	// one reads it, fail fast instead of blocking.
	in := newScript(t, true, keys(" ", "\r"), []key{{wait: func() {
		deadline := time.Now().Add(20 * time.Second)
		for o := out.String(); !strings.Contains(o, "Syncing: finished in"); o = out.String() {
			if strings.Contains(o, "Exclude") || time.Now().After(deadline) {
				t.Errorf("no progress summary after the menu:\n%s", o)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}}})
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !exists(filepath.Join(root, "b", "main")) || exists(filepath.Join(root, "c")) {
		t.Fatalf("b not cloned or c cloned:\n%s", out.String())
	}
	if y := readYAML(t, root); strings.Contains(y, "exclude") {
		t.Fatalf("c excluded although never asked:\n%s", y)
	}
	o := out.String()
	if strings.Contains(o, "Exclude") {
		t.Errorf("an exclusion step was shown:\n%s", o)
	}
	for _, want := range []string{"done     b", "1 done, 0 skipped, 0 failed"} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o)
		}
	}
}

// Selecting nothing changes nothing: the unselected new repositories are
// not excluded and sync proposes them again.
func TestTUISyncNothingSelectedExcludesNothing(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	before := readYAML(t, root)
	var out syncBuffer
	in := newScript(t, false, keys("\r"))
	errs, code := e.runTUI(t, in, &out, "sync")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if readYAML(t, root) != before {
		t.Fatalf("sheaf.yaml changed:\n%s", readYAML(t, root))
	}
	if o := out.String(); strings.Contains(o, "Exclude") || !strings.Contains(o, "nothing selected; nothing changed") {
		t.Errorf("stdout:\n%s", o)
	}
	// The listing, before the menu, is announced as with add --org.
	if !strings.Contains(errs, "listing repositories of o...\n") {
		t.Errorf("stderr:\n%s", errs)
	}
	again := e.mustRun(t, "sync", "--json")
	for _, k := range []string{`"new:o/b"`, `"new:o/c"`} {
		if !strings.Contains(again, k) {
			t.Errorf("next sync does not propose %s:\n%s", k, again)
		}
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

// ratioRecorder is a planning reporter that records the ratio of final
// events over planned units after each call, as the progress bars show it.
type ratioRecorder struct {
	plans    []int
	planned  int
	finished int
	ratios   []float64
}

func (r *ratioRecorder) Plan(units int) {
	r.plans = append(r.plans, units)
	r.planned += units
	r.record()
}

func (r *ratioRecorder) Report(e ops.Event) {
	if e.Kind == ops.KindDone || e.Kind == ops.KindSkipped || e.Kind == ops.KindFailed {
		r.finished++
	}
	r.record()
}

func (r *ratioRecorder) record() {
	if r.planned > 0 {
		r.ratios = append(r.ratios, float64(r.finished)/float64(r.planned))
	}
}

// A TUI sync adding new repositories and removing gone ones announces all
// of its work up front: its progress ratio never goes backwards.
func TestTUISyncPlansAllItsWorkUpFront(t *testing.T) {
	e, root := goneB(t)
	w, err := ops.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	d := e.deps()
	rep, err := ops.Sync(context.Background(), w, d.hosting(), ops.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var add, gone []ops.Proposal
	for _, p := range rep.Proposals {
		switch p.Kind {
		case ops.ProposalNew:
			add = append(add, p)
		case ops.ProposalGone:
			gone = append(gone, p)
		}
	}
	if len(add) != 1 || len(gone) != 1 {
		t.Fatalf("proposals %+v, want new c and gone b", rep.Proposals)
	}
	r := &ratioRecorder{}
	syncApply(context.Background(), d, w, add, gone, false, root, 1, r)
	if !slices.Equal(r.plans, []int{2}) {
		t.Errorf("plans %v, want [2]", r.plans)
	}
	if !slices.IsSorted(r.ratios) || r.finished != 2 {
		t.Errorf("ratios %v (%d finished), want them non-decreasing up to 2 finished", r.ratios, r.finished)
	}
}

// The TUI never implies --force: a gone repository whose remote still
// answers (gh token scope) is refused, and the refusal is displayed.
func TestTUISyncGoneRemovalRefusalIsDisplayed(t *testing.T) {
	e, root := goneB(t)
	var out syncBuffer
	// Proposals: new c, gone b. Select b (second), confirm the removal.
	in := newScript(t, true, keys("j", " ", "\r"), []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
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
	// Select c and b, decline the removal.
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
	if strings.Contains(out.String(), "is kept") {
		t.Errorf("dialog says the deleted branch is kept:\n%s", out.String())
	}
}

// Without -d/-D the dialog says that the branch, with its commits, stays.
func TestTUIWorktreeRemoveSaysBranchIsKept(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, primary := e.addRepo(t, "alpha")
	wt := filepath.Join(e.cwd, "alpha", "feat-y")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "feat/y", wt)
	writeFile(t, filepath.Join(wt, "y.txt"), "y\n")
	testutil.Git(t, wt, "add", "y.txt")
	testutil.Git(t, wt, "commit", "-q", "-m", "local only")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "wt", "rm", "alpha", "feat/y")
	if code != 1 || !exists(wt) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if !strings.Contains(out.String(), "branch feat/y is kept") {
		t.Errorf("dialog does not say the branch is kept:\n%s", out.String())
	}
}

func TestTUIPruneConfirms(t *testing.T) {
	e := newBundleEnv(t)
	o, primary := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, primary, "feat", "feat")
	testutil.Git(t, o.Path, "branch", "-D", "feat") // gone upstream
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}, {keys: "x", wait: waitScreen(t, &out, "Pruning: finished in")}})
	errs, code := e.runTUI(t, in, &out, "prune")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(wt) {
		t.Fatalf("worktree not pruned:\n%s", out.String())
	}
	if asking := out.String()[:strings.Index(out.String(), "[y/N]")]; !strings.Contains(asking, "Checking prune candidates ·") || strings.Contains(asking, "Pruning") {
		t.Errorf("title while asking:\n%s", asking)
	}
	for _, want := range []string{"Prune 1 candidate?", "will remove worktree alpha/feat", "1 removed, 0 kept, 0 failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

// The final line of the progress view does not repeat the counts of the
// summary that follows it, nor contradicts them (B52 B2): update counted
// pinned checkouts as skipped there.
func TestTUIProgressFinalLineHasNoCounts(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, primary := e.addRepo(t, "alpha")
	testutil.Git(t, primary, "tag", "v1")
	e.mustRun(t, "wt", "add", "alpha", "--tag", "v1", "--no-fetch")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	for _, tc := range []struct {
		args        []string
		final, sums string
	}{
		{[]string{"fetch"}, "Fetching: finished in ", "1 done, 0 skipped, 0 failed"},
		{[]string{"update"}, "Updating: finished in ", "1 fast-forwarded, 0 up to date, 1 pinned, 0 skipped, 0 failed"},
	} {
		var out syncBuffer
		errs, code := e.runTUI(t, newScript(t, true), &out, tc.args...)
		o := out.String()
		// From the final line on (earlier frames of the view have counts).
		final := o[max(0, strings.Index(o, tc.final)):]
		if code != 0 || !strings.Contains(o, tc.final) || !strings.Contains(final, tc.sums) || strings.Count(final, " failed") != 1 {
			t.Errorf("%v: exit %d, want the final line %q and one count line %q\nstdout:\n%s\nstderr:\n%s", tc.args, code, tc.final, tc.sums, o, errs)
		}
	}
}

// The header counts the fetch preceding the plan (aside rows) apart, so
// that it does not read "0 running" next to the fetch rows (B52 B1).
func TestTUIPruneHeaderCountsTheFetch(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, primary := e.addRepo(t, "alpha", "feat")
	addWorktree(t, primary, "feat", "feat")
	testutil.Git(t, o.Path, "branch", "-D", "feat") // gone upstream
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "prune")
	if code != 1 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if asking := out.String()[:strings.Index(out.String(), "[y/N]")]; !strings.Contains(asking, "Checking prune candidates · fetch 1/1 · 0 running, 0 done, 0 skipped, 0 failed ·") {
		t.Errorf("header while asking does not count the fetch:\n%s", asking)
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
	// The deselected checkout is not forgotten.
	if want := "not adopted: beta; adopt later with: sheaf adopt beta\n"; !strings.Contains(out.String(), want) {
		t.Errorf("stdout lacks %q:\n%s", want, out.String())
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

// The confirmation states what will happen, including the condition under
// which a branch without worktree is deleted (git branch -d rules): a
// merged branch is deleted, a gone one only if merged.
func TestTUIPruneConfirmationStatesBranchConditions(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, primary := e.addRepo(t, "alpha", "gone", "done", "shipped")
	testutil.Git(t, primary, "branch", "-q", "--track", "gone", "origin/gone")
	testutil.Git(t, primary, "branch", "-q", "--track", "done", "origin/done")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "gone")
	// done, and shipped (checked out in a worktree), are merged into main
	// with merge commits (first-parent rule).
	testutil.AdvanceOrigin(t, o, "done", "d.txt", "d\n", "done work")
	testutil.AdvanceOrigin(t, o, "shipped", "s.txt", "s\n", "shipped work")
	testutil.Git(t, primary, "fetch", "-q", "origin")
	testutil.Git(t, primary, "branch", "-q", "-f", "done", "origin/done")
	addWorktree(t, primary, "shipped", "shipped")
	work := t.TempDir()
	testutil.Git(t, work, "clone", "-q", o.URL, "w")
	testutil.Git(t, filepath.Join(work, "w"), "merge", "-q", "--no-ff", "-m", "merge done", "origin/done")
	testutil.Git(t, filepath.Join(work, "w"), "merge", "-q", "--no-ff", "-m", "merge shipped", "origin/shipped")
	testutil.Git(t, filepath.Join(work, "w"), "push", "-q", "origin", "main")
	var out syncBuffer
	in := newScript(t, true, []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code := e.runTUI(t, in, &out, "prune", "--include-branches", "--delete-branches")
	if code != 1 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	o2 := out.String()
	for _, want := range []string{
		"will delete branch alpha/done: merged into origin/main",
		"will delete branch alpha/gone only if merged (git branch -d rules), otherwise keep it: upstream origin/gone is gone",
		"will remove worktree alpha/shipped [shipped]",
		"will delete branch shipped",
	} {
		if !strings.Contains(o2, want) {
			t.Errorf("dialog lacks %q:\n%s", want, o2)
		}
	}
	if strings.Contains(o2, "branch shipped if merged") {
		t.Errorf("the merged worktree branch is stated as conditional:\n%s", o2)
	}
	if strings.Contains(o2, "would ") {
		t.Errorf("dialog uses the conditional:\n%s", o2)
	}
}

// Until the removal is confirmed the progress view says it is checking,
// not removing; declined, the operation is reported cancelled, not skipped.
func TestTUIDeletionTitlesCheckingThenRemovingOrCancelled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		args               []string
		checking, removing string
	}{
		{"remove", []string{"remove", "alpha"}, "Checking alpha for removal", "Removing alpha"},
		{"wt rm", []string{"wt", "rm", "alpha", "feat"}, "Checking worktree feat of alpha for removal", "Removing worktree feat of alpha"},
	} {
		for _, answer := range []string{"n", "y"} {
			e := newBundleEnv(t)
			_, primary := e.addRepo(t, "alpha")
			testutil.Git(t, primary, "worktree", "add", "-q", "-b", "feat", filepath.Join(e.cwd, "alpha", "feat"))
			var out syncBuffer
			final := tc.checking + ": cancelled after "
			if answer == "y" {
				final = tc.removing + ": finished in "
			}
			// The second key keeps the input open until the final view.
			in := newScript(t, true, []key{{keys: answer, wait: waitOutput(t, &out, "[y/N]")}, {keys: "x", wait: waitScreen(t, &out, final)}})
			errs, code := e.runTUI(t, in, &out, tc.args...)
			if want := map[string]int{"n": 1, "y": 0}[answer]; code != want {
				t.Fatalf("%s %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", tc.name, answer, code, want, out.String(), errs)
			}
			o := out.String()
			asking := o[:strings.Index(o, "[y/N]")]
			if !strings.Contains(asking, tc.checking+" · 1 running") || strings.Contains(asking, tc.removing) {
				t.Errorf("%s %s: title while asking:\n%s", tc.name, answer, asking)
			}
			if !strings.Contains(screenText(o), final) {
				t.Errorf("%s %s: final view lacks %q:\n%s", tc.name, answer, final, screenText(o))
			}
			if strings.Contains(o, "skipped: cancelled") || strings.Contains(o, "1 skipped") {
				t.Errorf("%s %s: reported skipped:\n%s", tc.name, answer, o)
			}
		}
	}
}

// The confirmation says what happens to the branch of each worktree: kept,
// or with --delete-branches deleted when merged (git branch -d rules, as
// wt rm -d), kept otherwise.
func TestTUIPruneConfirmationStatesWorktreeBranchFate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"prune"}, "branch feat is kept"},
		{[]string{"prune", "--delete-branches"}, "will delete branch feat if merged (git branch -d rules), otherwise keep it"},
	} {
		e := newBundleEnv(t)
		o, primary := e.addRepo(t, "alpha", "feat")
		addWorktree(t, primary, "feat", "feat")
		testutil.Git(t, o.Path, "branch", "-D", "feat") // gone upstream
		var out syncBuffer
		in := newScript(t, true, []key{{keys: "n", wait: waitOutput(t, &out, "[y/N]")}})
		errs, code := e.runTUI(t, in, &out, tc.args...)
		if code != 1 {
			t.Fatalf("%v: exit %d\nstdout:\n%s\nstderr:\n%s", tc.args, code, out.String(), errs)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%v: dialog lacks %q:\n%s", tc.args, tc.want, out.String())
		}
	}
}
