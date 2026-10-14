package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// cloneOrigin creates an origin with opts and clones it with Clone into a
// fresh directory, returning both.
func cloneOrigin(t *testing.T, opts testutil.OriginOpts) (testutil.Origin, string) {
	t.Helper()
	o := testutil.NewOrigin(t, opts)
	dir := filepath.Join(t.TempDir(), "clone")
	if err := Clone(context.Background(), o.URL, dir, ""); err != nil {
		t.Fatal(err)
	}
	return o, dir
}

func TestCloneDefaultBranch(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{DefaultBranch: "trunk", Branches: []string{"dev"}})
	if got := testutil.Git(t, dir, "symbolic-ref", "--short", "HEAD"); got != "trunk" {
		t.Fatalf("HEAD = %q, want trunk", got)
	}
	if got := testutil.Git(t, dir, "remote", "get-url", "origin"); got != o.URL {
		t.Fatalf("origin = %q, want %q", got, o.URL)
	}
}

func TestCloneExplicitBranchIntoMissingParents(t *testing.T) {
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x"}})
	dir := filepath.Join(t.TempDir(), "a", "b", "clone")
	if err := Clone(context.Background(), o.URL, dir, "feat/x"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, dir, "symbolic-ref", "--short", "HEAD"); got != "feat/x" {
		t.Fatalf("HEAD = %q, want feat/x", got)
	}
}

func TestCloneFailureIsGitError(t *testing.T) {
	testutil.IsolateGit(t)
	missing := "file://" + filepath.Join(t.TempDir(), "nope.git")
	err := Clone(context.Background(), missing, filepath.Join(t.TempDir(), "c"), "")
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *GitError", err)
	}
}

func TestDefaultBranchFromOriginHEAD(t *testing.T) {
	for _, b := range []string{"master", "trunk"} {
		t.Run(b, func(t *testing.T) {
			_, dir := cloneOrigin(t, testutil.OriginOpts{DefaultBranch: b, Branches: []string{"other"}})
			// Check out another branch so HEAD cannot be mistaken for the default.
			testutil.Git(t, dir, "checkout", "-q", "other")
			got, err := DefaultBranch(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != b {
				t.Fatalf("DefaultBranch = %q, want %q", got, b)
			}
		})
	}
}

func TestDefaultBranchFallsBackToLsRemote(t *testing.T) {
	for _, b := range []string{"master", "trunk"} {
		t.Run(b, func(t *testing.T) {
			_, dir := cloneOrigin(t, testutil.OriginOpts{DefaultBranch: b, Branches: []string{"other"}})
			testutil.Git(t, dir, "checkout", "-q", "other")
			testutil.Git(t, dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
			got, err := DefaultBranch(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != b {
				t.Fatalf("DefaultBranch = %q, want %q", got, b)
			}
		})
	}
}

func TestDefaultBranchNoOrigin(t *testing.T) {
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	if _, err := DefaultBranch(context.Background(), dir); err == nil {
		t.Fatal("expected an error without origin")
	}
}

func TestCurrentBranch(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat/cli/rework-ergonomics"}})
	testutil.Git(t, dir, "checkout", "-q", "feat/cli/rework-ergonomics")
	got, err := CurrentBranch(context.Background(), dir)
	if err != nil || got != "feat/cli/rework-ergonomics" {
		t.Fatalf("CurrentBranch = %q, %v", got, err)
	}
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	if _, err := CurrentBranch(context.Background(), dir); !errors.Is(err, ErrDetachedHead) {
		t.Fatalf("detached: err = %v, want ErrDetachedHead", err)
	}
}

func TestCurrentBranchUnborn(t *testing.T) {
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q", "-b", "fresh")
	got, err := CurrentBranch(context.Background(), dir)
	if err != nil || got != "fresh" {
		t.Fatalf("CurrentBranch = %q, %v", got, err)
	}
}

func TestRemoteURL(t *testing.T) {
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	got, err := RemoteURL(context.Background(), dir, "origin")
	if err != nil || got != o.URL {
		t.Fatalf("RemoteURL = %q, %v; want %q", got, err, o.URL)
	}
	if _, err := RemoteURL(context.Background(), dir, "nope"); err == nil {
		t.Fatal("expected error for unknown remote")
	}
}

func TestTopLevelAndIsRepo(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	sub := filepath.Join(dir, "sub", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := TopLevel(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Fatalf("TopLevel = %q, want %q", got, want)
	}
	if !IsRepo(ctx, dir) || !IsRepo(ctx, sub) {
		t.Fatal("IsRepo = false inside a work tree")
	}
	plain := t.TempDir()
	if IsRepo(ctx, plain) {
		t.Fatal("IsRepo = true outside a repository")
	}
	if _, err := TopLevel(ctx, plain); err == nil {
		t.Fatal("TopLevel outside a repository: expected error")
	}
	if IsRepo(ctx, filepath.Join(plain, "missing")) {
		t.Fatal("IsRepo = true for a missing directory")
	}
}
