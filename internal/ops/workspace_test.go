package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateMakesDirectoriesAndBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	root, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root != resolved(t, dir) {
		t.Fatalf("root = %q, want %q", root, resolved(t, dir))
	}
	if _, err := bundle.Load(dir); err != nil {
		t.Fatalf("bundle not loadable: %v", err)
	}
	if _, err := Create(dir); !errors.Is(err, bundle.ErrExists) {
		t.Fatalf("second Create err = %v, want ErrExists", err)
	}
}

func TestFindBundle(t *testing.T) {
	root, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := FindBundle("", sub); err != nil || got != root {
		t.Fatalf("FindBundle from cwd = %q, %v; want %q", got, err, root)
	}
	other := t.TempDir()
	if got, err := FindBundle(root, other); err != nil || got != root {
		t.Fatalf("FindBundle with flag = %q, %v; want %q", got, err, root)
	}
	// --bundle must point at a bundle root, not somewhere inside it.
	if _, err := FindBundle(sub, other); err == nil {
		t.Fatal("FindBundle(--bundle without sheaf.yaml) succeeded")
	}
	if _, err := FindBundle("", other); !errors.Is(err, bundle.ErrNotFound) {
		t.Fatalf("FindBundle outside bundle err = %v, want ErrNotFound", err)
	}
}

func TestWorkspaceUpdatePersistsAndIsolatesOnError(t *testing.T) {
	root, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.update(func(b *bundle.Bundle) error { return b.AddSource("o") }); err != nil {
		t.Fatal(err)
	}
	b, err := bundle.Load(root)
	if err != nil || len(b.Sources) != 1 {
		t.Fatalf("saved bundle = %+v, %v", b, err)
	}
	boom := errors.New("boom")
	err = w.update(func(b *bundle.Bundle) error {
		_ = b.AddSource("p")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := len(w.Bundle().Sources); got != 1 {
		t.Fatalf("in-memory sources after failed update = %d, want 1", got)
	}
	// Bundle() returns a copy.
	w.Bundle().Sources[0].Org = "mutated"
	if w.Bundle().Sources[0].Org != "o" {
		t.Fatal("Bundle() aliases the workspace state")
	}
}

func plainClone(t *testing.T, o testutil.Origin, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, filepath.Dir(dest), "clone", "-q", o.URL, dest)
}

func TestInitDetectsCandidates(t *testing.T) {
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := t.TempDir()
	plainClone(t, o, filepath.Join(dir, "plain"))
	plainClone(t, o, filepath.Join(dir, "laid", "main"))
	testutil.Git(t, filepath.Join(dir, "laid", "main"), "worktree", "add", "-q", "-b", "feat", "../feat")
	// Not candidates: an empty dir, a file, a hidden dir, a dir whose only
	// child is a linked worktree, an invalid name.
	for _, d := range []string{"empty", ".hidden/x"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plainClone(t, o, filepath.Join(dir, ".hidden", "x", "c"))
	plainClone(t, o, filepath.Join(dir, "bad name", "main"))
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "plain"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	root, cands, err := Init(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Load(root); err != nil {
		t.Fatalf("Init did not create sheaf.yaml: %v", err)
	}
	want := []Candidate{
		{Name: "laid", Path: filepath.Join(root, "laid"), Kind: CandidateLaidOut, Primary: filepath.Join(root, "laid", "main")},
		{Name: "plain", Path: filepath.Join(root, "plain"), Kind: CandidatePlainClone, Primary: filepath.Join(root, "plain")},
	}
	if !slices.Equal(cands, want) {
		t.Fatalf("candidates =\n%+v\nwant\n%+v", cands, want)
	}
	// Nothing was moved.
	if _, err := os.Stat(filepath.Join(dir, "plain", ".git")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Init(context.Background(), dir); !errors.Is(err, bundle.ErrExists) {
		t.Fatalf("second Init err = %v, want ErrExists", err)
	}
	if _, _, err := Init(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("Init on a missing directory succeeded")
	}
}

func TestCandidatesExcludeRegistered(t *testing.T) {
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	root, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plainClone(t, o, filepath.Join(root, "plain"))
	b := &bundle.Bundle{Version: 1, Repositories: []bundle.Repository{{Name: "plain", Slug: "o/plain", URL: o.URL, DefaultBranch: "main", Origin: "adopted"}}}
	cands, err := Candidates(context.Background(), root, b)
	if err != nil || len(cands) != 0 {
		t.Fatalf("candidates = %+v, %v; want none", cands, err)
	}
}
