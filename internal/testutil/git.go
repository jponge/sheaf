// Package testutil provides fixtures for tests that use real git against
// ephemeral local repositories.
package testutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// OriginOpts configures NewOrigin.
type OriginOpts struct {
	// DefaultBranch is the name of the default branch (default "main").
	DefaultBranch string
	// Commits is the number of commits on the default branch (default 1).
	Commits int
	// Branches lists extra branches, each created from the default branch tip
	// with one additional commit.
	Branches []string
}

// Origin describes a bare repository created by NewOrigin.
type Origin struct {
	// Path is the absolute path of the bare repository.
	Path string
	// URL is the file:// URL of the bare repository.
	URL string
}

// IsolateGit makes git ignore the user's and the system's configuration and
// any inherited GIT_* environment for the duration of the test. It is
// idempotent and pins the locale to C so git messages are stable. It must not be used in parallel tests (t.Setenv).
func IsolateGit(t *testing.T) {
	t.Helper()
	// Scrub every inherited GIT_* variable (git exports some of them to hooks,
	// e.g. GIT_DIR, GIT_INDEX_FILE, GIT_CONFIG_PARAMETERS). t.Setenv first so
	// the original value is restored at cleanup.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANGUAGE", "")
	os.Unsetenv("LANGUAGE")
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	const content = `[user]
	name = Sheaf Test
	email = sheaf-test@example.invalid
[init]
	defaultBranch = main
[protocol "file"]
	allow = always
[commit]
	gpgsign = false
[tag]
	gpgsign = false
[core]
	hooksPath = /dev/null
`
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// Git runs git in dir, fails the test on error and returns trimmed stdout.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// CommitFile writes content to path (relative to repoDir, parent directories
// are created) and commits it with msg.
func CommitFile(t *testing.T, repoDir, path, content, msg string) {
	t.Helper()
	full := filepath.Join(repoDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, repoDir, "add", "--", path)
	Git(t, repoDir, "commit", "-q", "-m", msg)
}

// NewOrigin builds a work repository according to opts, then clones it as a
// bare repository under t.TempDir() and returns the bare repository. It calls
// IsolateGit itself.
func NewOrigin(t *testing.T, opts OriginOpts) Origin {
	t.Helper()
	IsolateGit(t)
	if opts.DefaultBranch == "" {
		opts.DefaultBranch = "main"
	}
	if opts.Commits <= 0 {
		opts.Commits = 1
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, work, "init", "-q", "-b", opts.DefaultBranch)
	for i := 1; i <= opts.Commits; i++ {
		CommitFile(t, work, fmt.Sprintf("file-%d.txt", i), fmt.Sprintf("content %d\n", i), fmt.Sprintf("commit %d", i))
	}
	for _, b := range opts.Branches {
		Git(t, work, "checkout", "-q", "-b", b, opts.DefaultBranch)
		CommitFile(t, work, "branch-"+strings.ReplaceAll(b, "/", "_")+".txt", b+"\n", "work on "+b)
	}
	Git(t, work, "checkout", "-q", opts.DefaultBranch)

	bare := filepath.Join(root, "origin.git")
	Git(t, root, "clone", "-q", "--bare", work, bare)
	return Origin{Path: bare, URL: "file://" + bare}
}

// AdvanceOrigin adds a commit on branch of the bare origin: it clones the
// origin to a temp dir, commits path/content there and pushes back (local
// only). Use it to move an origin ahead after clones of it exist.
func AdvanceOrigin(t *testing.T, o Origin, branch, path, content, msg string) {
	t.Helper()
	IsolateGit(t)
	dir := filepath.Join(t.TempDir(), "advance")
	Git(t, filepath.Dir(dir), "clone", "-q", "--branch", branch, o.Path, dir)
	CommitFile(t, dir, path, content, msg)
	Git(t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}
