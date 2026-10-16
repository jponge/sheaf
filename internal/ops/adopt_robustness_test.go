package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Linked worktrees nested inside the moved clone are repaired.
func TestAdoptRepairsWorktreesNestedInTheClone(t *testing.T) {
	t.Parallel()
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

// An undeterminable default branch is refused unless given.
func TestAdoptUnknownDefaultBranch(t *testing.T) {
	t.Parallel()
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
	if b := branchOf(t, filepath.Join(dir, "main")); b != "feat" {
		t.Fatalf("primary branch = %q, want feat (moved as is)", b)
	}
	if r, _ := f.onDisk(t).Repository("x"); r == nil || r.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", r)
	}
}

// The same directory given twice is adopted once.
func TestAdoptAllDeduplicatesDirectories(t *testing.T) {
	t.Parallel()
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

// Adopt records the host of the origin.
func TestAdoptRecordsHost(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A panic during adoption is reported, and the clone restored.
func TestAdoptAllRecoversPanicAndRestores(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "checkout", "-q", "feat")
	s := AdoptAll(context.Background(), f.w, []string{dir}, AdoptOptions{afterMove: func() error { panic("boom") }}, 1, Discard)
	if len(s.Results) != 1 || s.Results[0].Status != StatusFailed || !strings.Contains(fmt.Sprint(s.Results[0].Err), "boom") {
		t.Fatalf("results = %+v, want failure mentioning the panic", s.Results)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() || branchOf(t, dir) != "feat" {
		t.Fatalf("clone not restored: %v", err)
	}
	noLeftovers(t, f.w.Root)
}

// Failure branches of moveIntoSubdir.
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
			fsys := moveFS{
				rename: func(a, b string) error {
					calls++
					if calls == c.failRename {
						return boom
					}
					return os.Rename(a, b)
				},
				mkdir: func(p string, m os.FileMode) error {
					if c.failMkdirCall {
						return boom
					}
					return os.Mkdir(p, m)
				},
			}
			if _, err := moveIntoSubdir(fsys, root, dir, "main"); !errors.Is(err, boom) {
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

// A failure
// after renaming p/dev to p/main restores p/dev and its sibling worktree,
// and a second attempt leaves no prunable worktree.
func TestAdoptLaidOutRenameRollbackWithSibling(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"dev", "feat"}})
	dev := filepath.Join(f.w.Root, "p", "dev")
	plainClone(t, o, dev)
	testutil.Git(t, dev, "checkout", "-q", "dev")
	testutil.Git(t, dev, "worktree", "add", "-q", "../feat", "feat")
	writeFile(t, filepath.Join(f.w.Root, "p", "feat", "wip.txt"), "wip\n")
	boom := errors.New("injected")
	if _, err := adopt(t, f, filepath.Join(f.w.Root, "p"), AdoptOptions{afterMove: func() error { return boom }}); !errors.Is(err, boom) {
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
	if _, err := adopt(t, f, filepath.Join(f.w.Root, "p"), AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if out := testutil.Git(t, filepath.Join(f.w.Root, "p", "main"), "worktree", "list", "--porcelain"); strings.Contains(out, "prunable") {
		t.Fatalf("prunable worktree after laid-out switch:\n%s", out)
	}
}
