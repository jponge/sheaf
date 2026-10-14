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

// gitConfig is the global git configuration of isolated tests.
const gitConfig = `[user]
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
[maintenance]
	auto = false
[gc]
	auto = 0
`

// IsolateGit makes sure git ignores the user's and the system's
// configuration and any inherited GIT_* environment, and that the locale is
// pinned to C so git messages are stable. It fails the test unless TestMain
// calls Main.
//
// Main isolates the whole test process, so in a test that did not change the
// environment IsolateGit only checks that and returns: it is then safe in
// parallel tests. If the test changed the environment (t.Setenv of GIT_*,
// HOME, LC_ALL...), it isolates again as IsolateGitPerTest does, which
// cannot be used in parallel tests (t.Setenv).
func IsolateGit(t *testing.T) {
	t.Helper()
	requireMain(t, "IsolateGit")
	if !isolatedNow() {
		isolatePerTest(t)
	}
}

// IsolateGitPerTest isolates git for the duration of the test with a fresh
// HOME and a writable global configuration of its own (the one of the test
// process is read-only), for tests that change the global git configuration.
// It must not be used in parallel tests (t.Setenv).
func IsolateGitPerTest(t *testing.T) {
	t.Helper()
	requireMain(t, "IsolateGitPerTest")
	isolatePerTest(t)
}

func isolatePerTest(t *testing.T) {
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
	if err := os.WriteFile(cfg, []byte(gitConfig), 0o600); err != nil {
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
	requireMain(t, "Git")
	out, err := gitEnv(nil, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gitEnv runs git in dir with env (nil: the process environment) and returns
// trimmed stdout.
func gitEnv(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
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
//
// In a test that did not change the environment, the bare repository is a
// copy of one built once per test process for the same opts (in Main's
// temporary directory, with the isolated environment), so it is also safe in
// parallel tests; repositories built for equal opts have the same commits.
func NewOrigin(t *testing.T, opts OriginOpts) Origin {
	t.Helper()
	requireMain(t, "NewOrigin")
	cached := isolatedNow()
	if !cached {
		isolatePerTest(t)
	}
	if opts.DefaultBranch == "" {
		opts.DefaultBranch = "main"
	}
	if opts.Commits <= 0 {
		opts.Commits = 1
	}
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	if cached {
		src, err := cachedOrigin(opts)
		if err == nil {
			err = copyTree(src, bare)
		}
		if err != nil {
			t.Fatal(err)
		}
	} else if err := buildOrigin(nil, root, opts); err != nil {
		t.Fatal(err)
	}
	return Origin{Path: bare, URL: "file://" + bare}
}

// buildOrigin creates root/work according to opts and its bare clone
// root/origin.git, running git with env (nil: the process environment).
func buildOrigin(env []string, root string, opts OriginOpts) error {
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	git := func(dir string, args ...string) error {
		_, err := gitEnv(env, dir, args...)
		return err
	}
	commit := func(path, content, msg string) error {
		if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o644); err != nil {
			return err
		}
		if err := git(work, "add", "--", path); err != nil {
			return err
		}
		return git(work, "commit", "-q", "-m", msg)
	}
	if err := git(work, "init", "-q", "-b", opts.DefaultBranch); err != nil {
		return err
	}
	for i := 1; i <= opts.Commits; i++ {
		if err := commit(fmt.Sprintf("file-%d.txt", i), fmt.Sprintf("content %d\n", i), fmt.Sprintf("commit %d", i)); err != nil {
			return err
		}
	}
	for _, b := range opts.Branches {
		if err := git(work, "checkout", "-q", "-b", b, opts.DefaultBranch); err != nil {
			return err
		}
		if err := commit("branch-"+strings.ReplaceAll(b, "/", "_")+".txt", b+"\n", "work on "+b); err != nil {
			return err
		}
	}
	if err := git(work, "checkout", "-q", opts.DefaultBranch); err != nil {
		return err
	}
	return git(root, "clone", "-q", "--bare", work, filepath.Join(root, "origin.git"))
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
