package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestChildEnvStripsRepoVariablesAndPinsLocale(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/x",
		"GIT_DIR=/victim/.git",
		"GIT_WORK_TREE=/victim",
		"GIT_INDEX_FILE=/victim/.git/index",
		"GIT_OBJECT_DIRECTORY=/victim/.git/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/victim/alt",
		"GIT_COMMON_DIR=/victim/.git",
		"GIT_PREFIX=sub/",
		"GIT_NAMESPACE=ns",
		"GIT_CONFIG_GLOBAL=/cfg",
		"LC_ALL=fr_FR.UTF-8",
		"GIT_TERMINAL_PROMPT=1",
	}
	got := childEnv(in)
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE":
			t.Errorf("%s not stripped: %v", k, got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/x", "GIT_CONFIG_GLOBAL=/cfg", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q missing from %v", want, got)
		}
	}
	for _, bad := range []string{"LC_ALL=fr_FR.UTF-8", "GIT_TERMINAL_PROMPT=1"} {
		if slices.Contains(got, bad) {
			t.Errorf("%q should have been overridden: %v", bad, got)
		}
	}
}

func TestRunReturnsStdout(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	out, err := Run(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		t.Fatal(err)
	}
	if out != "true\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunReturnsTypedError(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	_, err := Run(context.Background(), dir, "rev-parse", "--verify", "no-such-ref")
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %#v, want *GitError", err)
	}
	if ge.Dir != dir || !slices.Equal(ge.Args, []string{"rev-parse", "--verify", "no-such-ref"}) {
		t.Errorf("dir/args = %q %q", ge.Dir, ge.Args)
	}
	if ge.ExitCode != 128 {
		t.Errorf("exit code = %d, want 128", ge.ExitCode)
	}
	if !strings.Contains(ge.Stderr, "not a git repository") {
		t.Errorf("stderr = %q", ge.Stderr)
	}
	if msg := ge.Error(); !strings.Contains(msg, "rev-parse") || !strings.Contains(msg, "not a git repository") {
		t.Errorf("Error() = %q", msg)
	}
}

func TestRunRejectsEmptyDir(t *testing.T) {
	t.Parallel()
	if _, err := Run(context.Background(), "", "version"); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestRunHonoursContext(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, t.TempDir(), "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestRunIgnoresInheritedRepoEnv reproduces sheaf being run from a git hook:
// GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE point at a sentinel repository and the
// operation must still target the repository given as dir.
func TestRunIgnoresInheritedRepoEnv(t *testing.T) {
	testutil.IsolateGit(t)
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	target := filepath.Join(t.TempDir(), "target")
	for _, d := range []string{sentinel, target} {
		testutil.Git(t, filepath.Dir(d), "init", "-q", "-b", filepath.Base(d), d)
		testutil.CommitFile(t, d, "f.txt", d, "init")
	}
	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))
	t.Setenv("GIT_WORK_TREE", sentinel)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(sentinel, ".git", "index"))

	out, err := Run(context.Background(), target, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "target" {
		t.Fatalf("branch = %q, want target (inherited GIT_DIR leaked)", got)
	}
}

// Every repository-local variable git knows about (git rev-parse
// --local-env-vars) is stripped, except the config-injection variables, which
// carry user configuration rather than a repository location.
func TestStrippedEnvCoversGitLocalEnvVars(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	out := testutil.Git(t, t.TempDir(), "rev-parse", "--local-env-vars")
	keep := map[string]bool{"GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true}
	for v := range strings.FieldsSeq(out) {
		if !keep[v] && !strippedEnv[v] {
			t.Errorf("%s is listed by git rev-parse --local-env-vars but not stripped", v)
		}
	}
}

// Review 2, item 7: a stale lock file (e.g. left by a killed fetch) is
// recognized and its path exposed.
func TestStaleLockPath(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{})
	dir = resolved(t, dir) // git reports lock paths with symlinks resolved
	ctx := context.Background()

	refLock := filepath.Join(dir, ".git", "refs", "remotes", "origin", "main.lock")
	writeFile(t, filepath.Dir(refLock), "main.lock", "")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	err := FetchRemote(ctx, dir, "origin")
	if err == nil {
		t.Fatal("precondition: Fetch succeeded despite a ref lock")
	}
	if p, ok := StaleLockPath(fmt.Errorf("wrapped: %w", err)); !ok || p != refLock {
		t.Errorf("StaleLockPath(fetch error) = %q, %v; want %q (err: %v)", p, ok, refLock, err)
	}
	if err := os.Remove(refLock); err != nil {
		t.Fatal(err)
	}

	indexLock := filepath.Join(dir, ".git", "index.lock")
	writeFile(t, filepath.Dir(indexLock), "index.lock", "")
	_, err = Run(ctx, dir, "commit", "--allow-empty", "-q", "-m", "x")
	if p, ok := StaleLockPath(err); !ok || p != indexLock {
		t.Errorf("StaleLockPath(commit error) = %q, %v; want %q (err: %v)", p, ok, indexLock, err)
	}

	_, other := Run(ctx, dir, "rev-parse", "--verify", "no-such-ref")
	for _, e := range []error{nil, errors.New("Unable to create '/x.lock': File exists."), other} {
		if p, ok := StaleLockPath(e); ok {
			t.Errorf("StaleLockPath(%v) = %q, true", e, p)
		}
	}
}
