package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func adopt(t *testing.T, f *fixture, dir string, opts AdoptOptions) ([]Event, error) {
	t.Helper()
	var evs []Event
	_, err := Adopt(context.Background(), f.w, dir, opts, func(e Event) { evs = append(evs, e) })
	return evs, err
}

func noLeftovers(t *testing.T, root string) {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".sheaf-adopt") {
			t.Fatalf("temporary directory left behind: %s", e.Name())
		}
	}
}

// B1: switching the primary to the default branch must not overwrite an
// ignored file that the default branch tracks.
func TestAdoptSwitchNeverOverwritesIgnoredFiles(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	seed := filepath.Join(t.TempDir(), "seed")
	testutil.Git(t, filepath.Dir(seed), "clone", "-q", o.Path, seed)
	testutil.CommitFile(t, seed, "local.cfg", "tracked default\n", "track local.cfg on main")
	testutil.Git(t, seed, "checkout", "-q", "-b", "feat")
	testutil.Git(t, seed, "rm", "-q", "local.cfg")
	testutil.CommitFile(t, seed, ".gitignore", "local.cfg\n", "ignore local.cfg on feat")
	testutil.Git(t, seed, "push", "-q", "origin", "main", "feat")

	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	writeFile(t, filepath.Join(dir, "local.cfg"), "PRECIOUS USER DATA\n")
	_, err := adopt(t, f, dir, AdoptOptions{})
	if err == nil {
		t.Fatal("adopt succeeded although the switch would overwrite an ignored file")
	}
	if got := readFile(t, filepath.Join(dir, "local.cfg")); got != "PRECIOUS USER DATA\n" {
		t.Fatalf("local.cfg = %q after rollback", got)
	}
	if b := branchOf(t, dir); b != "feat" {
		t.Fatalf("branch after rollback = %q", b)
	}
	noLeftovers(t, f.w.Root)
}

func TestAdoptSwitchReportsIgnoredFilesStayInPrimary(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	writeFile(t, filepath.Join(dir, ".git", "info", "exclude"), "build/\n")
	if err := os.Mkdir(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "build", "out.bin"), "x")
	evs, err := adopt(t, f, dir, AdoptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "main", "build", "out.bin")) {
		t.Fatal("ignored file not kept in the primary")
	}
	if !slices.ContainsFunc(evs, func(e Event) bool {
		return strings.Contains(e.Message, "ignored") && strings.Contains(e.Message, filepath.Join(dir, "main"))
	}) {
		t.Fatalf("no event about ignored files staying in the primary: %+v", evs)
	}
}

// B1b: a laid-out repository is only recognized when the primary's
// directory is named after its branch.
func TestAdoptLaidOutChildMustMatchBranch(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	child := filepath.Join(f.w.Root, "docs", "somerepo")
	plainClone(t, o, child)
	testutil.Git(t, child, "checkout", "-q", "feat")
	_, err := adopt(t, f, filepath.Join(f.w.Root, "docs"), AdoptOptions{})
	if err == nil || !strings.Contains(err.Error(), "somerepo") || !strings.Contains(err.Error(), "feat") {
		t.Fatalf("err = %v, want refusal explaining that somerepo does not match branch feat", err)
	}
	if len(f.onDisk(t).Repositories) != 0 || !exists(filepath.Join(child, ".git")) {
		t.Fatal("something changed")
	}
	cands, err := Candidates(context.Background(), f.w.Root, &bundle.Bundle{Version: 1})
	if err != nil || len(cands) != 0 {
		t.Fatalf("candidates = %+v, %v; want none", cands, err)
	}
}

func TestCandidatesLaidOutOnFeatureBranch(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	child := filepath.Join(f.w.Root, "r", "Feat-X")
	plainClone(t, o, child)
	testutil.Git(t, child, "checkout", "-q", "feat/x")
	cands, err := Candidates(context.Background(), f.w.Root, &bundle.Bundle{Version: 1})
	if err != nil || len(cands) != 1 || cands[0].Kind != CandidateLaidOut || cands[0].Primary != child {
		t.Fatalf("candidates = %+v, %v", cands, err)
	}
}

func TestAdoptLaidOutOnFeatureBranchRestoresInvariant(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	repo := filepath.Join(f.w.Root, "r")
	child := filepath.Join(repo, "feat-x")
	plainClone(t, o, child)
	testutil.Git(t, child, "checkout", "-q", "feat/x")
	if _, err := adopt(t, f, repo, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if b := branchOf(t, filepath.Join(repo, "main")); b != "main" {
		t.Fatalf("primary branch = %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(repo, "main", ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("r/main is not the primary: %v", err)
	}
	if b := branchOf(t, child); b != "feat/x" {
		t.Fatalf("worktree branch = %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(child, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("r/feat-x is not a linked worktree: %v", err)
	}
	if r, _ := f.onDisk(t).Repository("r"); r == nil || r.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", r)
	}
}

func TestAdoptLaidOutRefusesDirtyOrDetached(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, child string){
		"dirty": func(t *testing.T, child string) {
			testutil.Git(t, child, "checkout", "-q", "feat")
			writeFile(t, filepath.Join(child, "wip.txt"), "wip\n")
		},
		"detached": func(t *testing.T, child string) {
			testutil.Git(t, child, "checkout", "-q", "--detach", "origin/feat")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
			child := filepath.Join(f.w.Root, "r", "feat")
			plainClone(t, o, child)
			setup(t, child)
			if _, err := adopt(t, f, filepath.Join(f.w.Root, "r"), AdoptOptions{}); err == nil {
				t.Fatal("adopt succeeded")
			}
			if !exists(filepath.Join(child, ".git")) || exists(filepath.Join(f.w.Root, "r", "main")) {
				t.Fatal("something moved")
			}
		})
	}
}

// S1: linked worktrees nested inside the moved clone are repaired.
func TestAdoptRepairsWorktreesNestedInTheClone(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	writeFile(t, filepath.Join(dir, ".git", "info", "exclude"), ".wt/\n")
	testutil.Git(t, dir, "worktree", "add", "-q", ".wt/feat", "feat")
	writeFile(t, filepath.Join(dir, ".wt", "feat", "wip.txt"), "wip\n")
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "main", ".wt", "feat")
	wts, err := gitx.WorktreeList(context.Background(), filepath.Join(dir, "main"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || wts[1].Path != moved || wts[1].Prunable {
		t.Fatalf("worktrees = %+v, want %s registered and not prunable", wts, moved)
	}
	if b := branchOf(t, moved); b != "feat" {
		t.Fatalf("nested worktree broken: %q", b)
	}
}

// S5: an undeterminable default branch is refused unless given.
func TestAdoptUnknownDefaultBranch(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	testutil.Git(t, dir, "remote", "set-head", "origin", "-d")
	testutil.Git(t, dir, "remote", "set-url", "origin", "file:///nonexistent/o/x.git")
	_, err := adopt(t, f, dir, AdoptOptions{})
	if err == nil || !strings.Contains(err.Error(), "--default-branch") {
		t.Fatalf("err = %v, want refusal suggesting --default-branch", err)
	}
	if len(f.onDisk(t).Repositories) != 0 || !exists(filepath.Join(dir, ".git")) {
		t.Fatal("something changed")
	}
	if _, err := adopt(t, f, dir, AdoptOptions{DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if b := branchOf(t, filepath.Join(dir, "main")); b != "main" {
		t.Fatalf("primary branch = %q", b)
	}
	if r, _ := f.onDisk(t).Repository("x"); r == nil || r.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", r)
	}
}

// R7: the same directory given twice is adopted once.
func TestAdoptAllDeduplicatesDirectories(t *testing.T) {
	for i := range 5 {
		f := newFixture(t)
		dir := filepath.Join(f.w.Root, "x")
		plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
		s := AdoptAll(context.Background(), f.w, []string{dir, dir + "/", filepath.Join(dir, "..", "x")}, AdoptOptions{}, 3, Discard)
		if len(s.Results) != 1 || s.Results[0].Status != StatusDone {
			t.Fatalf("iteration %d: results = %+v, want one success", i, s.Results)
		}
		if !exists(filepath.Join(dir, "main", ".git")) || len(f.onDisk(t).Repositories) != 1 {
			t.Fatalf("iteration %d: inconsistent state", i)
		}
		noLeftovers(t, f.w.Root)
	}
}

// S7: adopt records the host of the origin.
func TestAdoptRecordsHost(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	local := filepath.Join(f.w.Root, "l")
	plainClone(t, o, local)
	gl := filepath.Join(f.w.Root, "g")
	plainClone(t, o, gl)
	testutil.Git(t, gl, "remote", "set-url", "origin", "https://gitlab.example.com/group/sub/proj.git")
	for _, d := range []string{local, gl} {
		if _, err := adopt(t, f, d, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	b := f.onDisk(t)
	if r, _ := b.Repository("l"); r == nil || r.Host != bundle.HostLocal || r.IsGitHub() {
		t.Fatalf("l = %+v, want host local", r)
	}
	if r, _ := b.Repository("g"); r == nil || r.Host != "gitlab.example.com" || r.Slug != "sub/proj" || r.IsGitHub() {
		t.Fatalf("g = %+v, want gitlab.example.com sub/proj", r)
	}
}

func TestParseRemoteURLHost(t *testing.T) {
	for in, want := range map[string][2]string{
		"git@github.com:o/n.git":                        {"github.com", "o/n"},
		"https://GitHub.com/o/n":                        {"github.com", "o/n"},
		"ssh://git@github.com:22/o/n.git":               {"github.com", "o/n"},
		"https://gitlab.example.com/group/sub/proj.git": {"gitlab.example.com", "sub/proj"},
		"git@gitlab.example.com:group/sub/proj.git":     {"gitlab.example.com", "sub/proj"},
		"file:///tmp/x/001/origin.git":                  {"local", "001/origin"},
		"/srv/git/team/repo.git":                        {"local", "team/repo"},
	} {
		h, s, err := ParseRemoteURL(in)
		if err != nil || h != want[0] || s != want[1] {
			t.Errorf("ParseRemoteURL(%q) = %q, %q, %v; want %v", in, h, s, err, want)
		}
	}
}

// NIT: a panic during adoption is reported, and the clone restored.
func TestAdoptAllRecoversPanicAndRestores(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	old := adoptAfterSwitch
	adoptAfterSwitch = func() error { panic("boom") }
	t.Cleanup(func() { adoptAfterSwitch = old })
	s := AdoptAll(context.Background(), f.w, []string{dir}, AdoptOptions{}, 1, Discard)
	if len(s.Results) != 1 || s.Results[0].Status != StatusFailed || !strings.Contains(fmt.Sprint(s.Results[0].Err), "boom") {
		t.Fatalf("results = %+v, want failure mentioning the panic", s.Results)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() || branchOf(t, dir) != "feat" {
		t.Fatalf("clone not restored: %v", err)
	}
	noLeftovers(t, f.w.Root)
}

// Test gap: failure branches of moveIntoSubdir.
func TestMoveIntoSubdirFailuresRestore(t *testing.T) {
	boom := errors.New("injected")
	for _, c := range []struct {
		name          string
		failRename    int // 1-based call to fail, 0 = none
		failMkdirCall bool
	}{
		{name: "first rename", failRename: 1},
		{name: "mkdir", failMkdirCall: true},
		{name: "second rename", failRename: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			testutil.IsolateGit(t)
			root := t.TempDir()
			dir := filepath.Join(root, "x")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
			calls := 0
			oldR, oldM := osRename, osMkdir
			t.Cleanup(func() { osRename, osMkdir = oldR, oldM })
			osRename = func(a, b string) error {
				calls++
				if calls == c.failRename {
					return boom
				}
				return os.Rename(a, b)
			}
			osMkdir = func(p string, m os.FileMode) error {
				if c.failMkdirCall {
					return boom
				}
				return os.Mkdir(p, m)
			}
			if _, _, err := moveIntoSubdir(root, dir, "main"); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want injected", err)
			}
			if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
				t.Fatalf("clone not restored at %s: %v", dir, err)
			}
			if exists(filepath.Join(dir, "main")) {
				t.Fatal("target left behind")
			}
			noLeftovers(t, root)
		})
	}
}

// Review 5, N12: laid-out switch with a sibling linked worktree, failure
// after the switch: everything restored, and a second attempt leaves no
// prunable worktree.
func TestAdoptLaidOutSwitchRollbackWithSibling(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"dev", "feat"}})
	dev := filepath.Join(f.w.Root, "p", "dev")
	plainClone(t, o, dev)
	testutil.Git(t, dev, "checkout", "-q", "dev")
	testutil.Git(t, dev, "worktree", "add", "-q", "../feat", "feat")
	writeFile(t, filepath.Join(f.w.Root, "p", "feat", "wip.txt"), "wip\n")
	boom := errors.New("injected")
	old := adoptAfterSwitch
	adoptAfterSwitch = func() error { return boom }
	t.Cleanup(func() { adoptAfterSwitch = old })
	if _, err := adopt(t, f, filepath.Join(f.w.Root, "p"), AdoptOptions{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want injected", err)
	}
	if b := branchOf(t, dev); b != "dev" {
		t.Fatalf("dev not restored: %s", b)
	}
	if exists(filepath.Join(f.w.Root, "p", "main")) {
		t.Fatal("p/main left")
	}
	if b := branchOf(t, filepath.Join(f.w.Root, "p", "feat")); b != "feat" {
		t.Fatalf("sibling worktree broken: %s", b)
	}
	adoptAfterSwitch = old
	if _, err := adopt(t, f, filepath.Join(f.w.Root, "p"), AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if out := testutil.Git(t, filepath.Join(f.w.Root, "p", "main"), "worktree", "list", "--porcelain"); strings.Contains(out, "prunable") {
		t.Fatalf("prunable worktree after laid-out switch:\n%s", out)
	}
}

// Review 5, N4: an unreadable directory in an ignored tree cannot be
// checked; even a forced remove must fail rather than delete unchecked.
func TestRemoveUnreadableIgnoredDirFailsSafely(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("SKIP: root can read everything")
	}
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "build/\n")
	locked := filepath.Join(primary, "build", "locked")
	if err := os.MkdirAll(filepath.Join(locked, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	_, err := Remove(context.Background(), f.w, "a", RemoveOptions{Offline: true, Force: true, Confirm: yes}, noEmit)
	if err == nil {
		t.Fatal("forced remove succeeded although part of the tree could not be checked")
	}
	if !exists(primary) {
		t.Fatal("deleted")
	}
}
