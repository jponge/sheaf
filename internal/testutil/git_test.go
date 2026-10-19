package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsolateGit(t *testing.T) {
	t.Parallel()
	// Main isolated HOME before the tests run: the real one is the one the
	// process started with.
	var realHome string
	for _, kv := range ToolchainEnv() {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			realHome = v
		}
	}
	IsolateGit(t)
	if h := os.Getenv("HOME"); h == realHome || h == "" {
		t.Fatalf("HOME not isolated: %q", h)
	}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") != "1" {
		t.Fatal("GIT_CONFIG_NOSYSTEM not set")
	}
	dir := t.TempDir()
	if got := Git(t, dir, "config", "--global", "user.name"); got == "" {
		t.Fatal("user.name empty")
	}
	if got := Git(t, dir, "config", "--global", "init.defaultBranch"); got != "main" {
		t.Fatalf("init.defaultBranch = %q", got)
	}
	Git(t, dir, "init")
	if got := Git(t, dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("initial branch = %q, want main", got)
	}
}

func TestCommitFileAndGit(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	dir := t.TempDir()
	Git(t, dir, "init")
	CommitFile(t, dir, "a/b.txt", "hello", "add b")
	b, err := os.ReadFile(filepath.Join(dir, "a", "b.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("file content = %q, err = %v", b, err)
	}
	if got := Git(t, dir, "log", "-1", "--format=%s"); got != "add b" {
		t.Fatalf("subject = %q", got)
	}
	if got := Git(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("tree dirty: %q", got)
	}
}

func TestCommitAt(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	dir := t.TempDir()
	Git(t, dir, "init")
	when := time.Date(2020, 5, 17, 10, 30, 0, 0, time.UTC)
	id := CommitAt(t, dir, "dated", when)
	if got := Git(t, dir, "log", "-1", "--format=%H %at %s"); got != fmt.Sprintf("%s %d dated", id, when.Unix()) {
		t.Fatalf("commit = %q, want id %s authored at %d", got, id, when.Unix())
	}
	if got := Git(t, dir, "log", "-1", "--format=%ct"); got == fmt.Sprint(when.Unix()) {
		t.Fatalf("committer date = %s, want the current time", got)
	}
}

func TestNewOriginDefaults(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	o := NewOrigin(t, OriginOpts{})
	if !strings.HasPrefix(o.URL, "file://") || strings.TrimPrefix(o.URL, "file://") != o.Path {
		t.Fatalf("URL = %q, Path = %q", o.URL, o.Path)
	}
	if got := Git(t, o.Path, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("is-bare = %q", got)
	}
	if got := Git(t, o.Path, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("HEAD = %q, want main", got)
	}
	if got := Git(t, o.Path, "rev-list", "--count", "main"); got != "1" {
		t.Fatalf("commits = %q, want 1", got)
	}
}

func TestNewOriginCustomBranchesAndClone(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	o := NewOrigin(t, OriginOpts{DefaultBranch: "master", Commits: 3, Branches: []string{"feat/x", "dev"}})
	if got := Git(t, o.Path, "symbolic-ref", "--short", "HEAD"); got != "master" {
		t.Fatalf("HEAD = %q, want master", got)
	}
	if got := Git(t, o.Path, "rev-list", "--count", "master"); got != "3" {
		t.Fatalf("commits = %q, want 3", got)
	}
	branches := Git(t, o.Path, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	for _, want := range []string{"master", "feat/x", "dev"} {
		if !strings.Contains("\n"+branches+"\n", "\n"+want+"\n") {
			t.Fatalf("branch %q missing in %q", want, branches)
		}
	}
	// file:// clone works (protocol.file.allow) and yields a clean checkout.
	dst := filepath.Join(t.TempDir(), "clone")
	Git(t, filepath.Dir(dst), "clone", o.URL, dst)
	if got := Git(t, dst, "symbolic-ref", "--short", "HEAD"); got != "master" {
		t.Fatalf("clone HEAD = %q", got)
	}
}

func TestNewOriginsAreIndependent(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	a, b := NewOrigin(t, OriginOpts{}), NewOrigin(t, OriginOpts{})
	if a.Path == b.Path {
		t.Fatal("origins share a path")
	}
}

func TestIsolateGitScrubsInheritedGitEnv(t *testing.T) {
	IsolateGit(t)
	sentinel := t.TempDir()
	Git(t, sentinel, "init", "-q")
	CommitFile(t, sentinel, "s.txt", "s", "sentinel")
	refsBefore := Git(t, sentinel, "for-each-ref")
	logBefore := Git(t, sentinel, "log", "--oneline")

	strayIndex := filepath.Join(t.TempDir(), "stray-index")
	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))
	t.Setenv("GIT_WORK_TREE", sentinel)
	t.Setenv("GIT_INDEX_FILE", strayIndex)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'init.defaultBranch=trunk' 'core.hooksPath=/nonexistent'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "commit.gpgsign")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_ALLOW_PROTOCOL", "https")

	IsolateGit(t)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") && k != "GIT_CONFIG_GLOBAL" && k != "GIT_CONFIG_NOSYSTEM" && k != "GIT_TERMINAL_PROMPT" {
			t.Errorf("inherited %s still set", k)
		}
	}

	o := NewOrigin(t, OriginOpts{})
	if got := Git(t, o.Path, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("default branch = %q", got)
	}
	if got := Git(t, sentinel, "for-each-ref"); got != refsBefore {
		t.Fatalf("sentinel refs changed:\n%s\nvs\n%s", refsBefore, got)
	}
	if got := Git(t, sentinel, "log", "--oneline"); got != logBefore {
		t.Fatalf("sentinel log changed: %q", got)
	}
	if _, err := os.Stat(strayIndex); err == nil {
		t.Fatal("stray index file created")
	}

	fresh := t.TempDir()
	Git(t, fresh, "init", "-q")
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	local := ".git/config" // git reports the repo-local file relative to cwd
	for _, line := range strings.Split(Git(t, fresh, "config", "--list", "--show-origin"), "\n") {
		origin, _, _ := strings.Cut(line, "\t")
		if origin != "file:"+global && origin != "file:"+local {
			t.Errorf("unexpected config origin: %q", line)
		}
	}
}

func TestIsolateGitPinsLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	IsolateGit(t)
	if got := os.Getenv("LC_ALL"); got != "C" {
		t.Fatalf("LC_ALL = %q", got)
	}
	if got := os.Getenv("LANGUAGE"); got != "" {
		t.Fatalf("LANGUAGE = %q", got)
	}
}

func TestNewOriginIsolatesGitItself(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "devconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tname = Real Dev\n\temail = dev@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	o := NewOrigin(t, OriginOpts{})
	if got := Git(t, o.Path, "log", "-1", "--format=%an"); got != "Sheaf Test" {
		t.Fatalf("author = %q, developer config leaked", got)
	}
}

func TestAdvanceOrigin(t *testing.T) {
	t.Parallel()
	IsolateGit(t)
	o := NewOrigin(t, OriginOpts{Branches: []string{"feat/x"}})
	before := Git(t, t.TempDir(), "ls-remote", o.URL, "refs/heads/main", "refs/heads/feat/x")
	AdvanceOrigin(t, o, "main", "new.txt", "n", "advance main")
	AdvanceOrigin(t, o, "feat/x", "new2.txt", "n2", "advance feat")
	if got := Git(t, o.Path, "log", "-1", "--format=%s", "main"); got != "advance main" {
		t.Fatalf("main tip = %q", got)
	}
	if got := Git(t, o.Path, "log", "-1", "--format=%s", "feat/x"); got != "advance feat" {
		t.Fatalf("feat/x tip = %q", got)
	}
	after := Git(t, t.TempDir(), "ls-remote", o.URL, "refs/heads/main", "refs/heads/feat/x")
	if before == after {
		t.Fatal("ls-remote unchanged")
	}
}
