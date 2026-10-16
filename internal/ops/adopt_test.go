package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func noEmit(Event) {}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func originSlug(t *testing.T, o testutil.Origin) string {
	t.Helper()
	_, s, err := ParseRemoteURL(o.URL)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAdoptPlainCloneMovesAndRegisters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	// Uncommitted and untracked data must survive the move.
	writeFile(t, filepath.Join(dir, "file-1.txt"), "modified\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "keep me\n")
	// A linked worktree outside the bundle must keep working.
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat/x")

	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(dir, "main")
	if got := readFile(t, filepath.Join(primary, "untracked.txt")); got != "keep me\n" {
		t.Fatalf("untracked file = %q", got)
	}
	if got := readFile(t, filepath.Join(primary, "file-1.txt")); got != "modified\n" {
		t.Fatalf("modified file = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s/.git still exists: %v", dir, err)
	}
	if b, err := gitx.CurrentBranch(context.Background(), wt); err != nil || b != "feat/x" {
		t.Fatalf("linked worktree broken after adopt: %q, %v", b, err)
	}
	wts, err := gitx.WorktreeList(context.Background(), wt)
	if err != nil || len(wts) != 2 || wts[0].Path != primary {
		t.Fatalf("worktrees seen from the linked one = %+v, %v; want primary %s first", wts, err, primary)
	}
	entries, err := os.ReadDir(f.w.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sheaf-adopt") {
			t.Fatalf("temporary directory left behind: %s", e.Name())
		}
	}
	r, ok := f.onDisk(t).Repository("p")
	want := bundle.Repository{Name: "p", Slug: originSlug(t, o), URL: o.URL, DefaultBranch: "main", Origin: "adopted", Host: bundle.HostLocal}
	if !ok || *r != want {
		t.Fatalf("entry = %+v, want %+v", r, want)
	}
}

func TestAdoptLaidOutOnlyRegisters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	primary := filepath.Join(f.w.Root, "p", "main")
	plainClone(t, o, primary)
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "feat", "../feat")
	if _, err := Adopt(context.Background(), f.w, filepath.Join(f.w.Root, "p"), AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(primary, ".git")); err != nil {
		t.Fatalf("primary moved: %v", err)
	}
	if r, ok := f.onDisk(t).Repository("p"); !ok || r.Origin != "adopted" || r.DefaultBranch != "main" {
		t.Fatalf("entry = %+v", r)
	}
}

func TestAdoptRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	// Already registered: skipped.
	plainClone(t, o, filepath.Join(f.w.Root, "p"))
	if _, err := Adopt(context.Background(), f.w, filepath.Join(f.w.Root, "p"), AdoptOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	_, err := Adopt(context.Background(), f.w, filepath.Join(f.w.Root, "p"), AdoptOptions{}, noEmit)
	var skip *SkipError
	if !errors.As(err, &skip) {
		t.Fatalf("re-adopt err = %v, want skip", err)
	}

	cases := map[string]func(t *testing.T) string{
		"same origin as a registered repo": func(t *testing.T) string {
			d := filepath.Join(f.w.Root, "dup")
			plainClone(t, o, d)
			return d
		},
		"no origin remote": func(t *testing.T) string {
			d := filepath.Join(f.w.Root, "noorigin")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), d)
			testutil.Git(t, d, "remote", "remove", "origin")
			return d
		},
		"outside the bundle": func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "elsewhere")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), d)
			return d
		},
		"nested below a repo dir": func(t *testing.T) string {
			d := filepath.Join(f.w.Root, "group", "deep")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), d)
			return d
		},
		"not a checkout": func(t *testing.T) string {
			d := filepath.Join(f.w.Root, "plain-dir")
			if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		},
		"symlink": func(t *testing.T) string {
			target := filepath.Join(t.TempDir(), "target")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), target)
			d := filepath.Join(f.w.Root, "link")
			if err := os.Symlink(target, d); err != nil {
				t.Fatal(err)
			}
			return d
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			d := mk(t)
			before := snapshotGitDir(d)
			_, err := Adopt(context.Background(), f.w, d, AdoptOptions{}, noEmit)
			if err == nil || errors.As(err, &skip) {
				t.Fatalf("Adopt(%s) err = %v, want failure", d, err)
			}
			if after := snapshotGitDir(d); after != before {
				t.Fatalf("directory changed: %q -> %q", before, after)
			}
		})
	}
	if got := repoNames(f.onDisk(t)); len(got) != 1 {
		t.Fatalf("registered = %v, want only p", got)
	}
}

// snapshotGitDir describes where .git is below d, to detect moves.
func snapshotGitDir(d string) string {
	if fi, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
		return "top:" + fi.Mode().Type().String()
	}
	m, _ := filepath.Glob(filepath.Join(d, "*", ".git"))
	return strings.Join(m, ",")
}

func TestAdoptRestoresOnFailureAfterMove(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	dir := filepath.Join(f.w.Root, "p")
	plainClone(t, o, dir)
	writeFile(t, filepath.Join(dir, "untracked.txt"), "keep me\n")
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat")

	boom := errors.New("injected")
	opts := AdoptOptions{afterMove: func() error { return boom }}

	if _, err := Adopt(context.Background(), f.w, dir, opts, noEmit); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want injected", err)
	}
	if got := readFile(t, filepath.Join(dir, "untracked.txt")); got != "keep me\n" {
		t.Fatalf("not restored: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("%s/.git not restored: %v", dir, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "main")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("p/main still exists: %v", err)
	}
	if b, err := gitx.CurrentBranch(context.Background(), wt); err != nil || b != "feat" {
		t.Fatalf("linked worktree broken after rollback: %q, %v", b, err)
	}
	wts, err := gitx.WorktreeList(context.Background(), wt)
	if err != nil || wts[0].Path != dir {
		t.Fatalf("worktrees = %+v, %v; want primary %s", wts, err, dir)
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("failed adoption was recorded")
	}
}

func TestAdoptAllRunsTasks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := filepath.Join(f.w.Root, "a")
	b := filepath.Join(f.w.Root, "b")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), a)
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), b)
	s := AdoptAll(context.Background(), f.w, []string{b, a, filepath.Join(f.w.Root, "missing")}, AdoptOptions{}, 2, Discard)
	if want := map[string]Status{"a": StatusDone, "b": StatusDone, "missing": StatusFailed}; !mapsEqual(statuses(s), want) {
		t.Fatalf("statuses = %v, want %v", statuses(s), want)
	}
	if got := repoNames(f.onDisk(t)); len(got) != 2 {
		t.Fatalf("registered = %v", got)
	}
}

func TestAdoptHostAliasAndOverride(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	alias := filepath.Join(f.w.Root, "alias")
	plainClone(t, o, alias)
	testutil.Git(t, alias, "remote", "set-url", "origin", "git@github_work:o/alias.git")
	if _, err := adopt(t, f, alias, AdoptOptions{DefaultBranch: "main"}); err != nil {
		t.Fatalf("adopt with an SSH alias: %v", err)
	}
	over := filepath.Join(f.w.Root, "over")
	plainClone(t, o, over)
	testutil.Git(t, over, "remote", "set-url", "origin", "git@github_work:o/over.git")
	if _, err := adopt(t, f, over, AdoptOptions{DefaultBranch: "main", Host: "github.com"}); err != nil {
		t.Fatalf("adopt --host github.com: %v", err)
	}
	b := f.onDisk(t)
	if r, _ := b.Repository("alias"); r == nil || r.Host != "github_work" || r.IsGitHub() {
		t.Fatalf("alias = %+v, want host github_work", r)
	}
	if r, _ := b.Repository("over"); r == nil || r.Host != "" || !r.IsGitHub() {
		t.Fatalf("over = %+v, want github.com (stored empty)", r)
	}

	bad := filepath.Join(f.w.Root, "bad")
	plainClone(t, o, bad)
	_, err := adopt(t, f, bad, AdoptOptions{Host: "bad host"})
	if err == nil || !strings.Contains(err.Error(), "bad host") {
		t.Fatalf("err = %v, want invalid host refusal", err)
	}
	if !exists(filepath.Join(bad, ".git")) {
		t.Fatal("moved despite an invalid host")
	}
}
