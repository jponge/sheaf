package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// isolatedHomeVar marks an environment isolated by Main; its value is the
// isolated HOME. A test binary re-executed by a test (a helper process)
// inherits it and keeps the environment it was given (see Main).
const isolatedHomeVar = "SHEAF_TEST_ISOLATED_HOME"

// proc is the isolation state of the test process, set by Main before any
// test runs and read-only afterwards (except the fixture cache).
var proc struct {
	isolated bool
	home     string // isolated HOME
	cfg      string // isolated global git configuration (read-only)
	ownsHome bool   // home was created by this process
	root     string // temporary directory removed when Main returns, see rootDir
	rootOnce sync.Once
	rootErr  error
	baseEnv  []string
	toolEnv  []string

	cacheMu sync.Mutex
	cache   map[string]*cacheEntry
	cacheN  int
}

type cacheEntry struct {
	once sync.Once
	n    int // index of the entry's directory, set when it is created
	path string
	err  error
}

// Main isolates git for the whole test process, runs the tests and exits.
// Every package whose tests run git (directly or through the fixtures of
// this package) must call it from TestMain:
//
//	func TestMain(m *testing.M) { testutil.Main(m) }
//
// Before any test runs, Main removes every inherited GIT_* variable (git
// exports some of them to hooks, e.g. GIT_DIR, GIT_INDEX_FILE,
// GIT_CONFIG_PARAMETERS) and LANGUAGE, and sets HOME and XDG_CONFIG_HOME to a
// fresh temporary directory, GIT_CONFIG_GLOBAL to a read-only configuration
// in it (so a test cannot change the configuration other tests see),
// GIT_CONFIG_NOSYSTEM=1, GIT_TERMINAL_PROMPT=0 and LC_ALL=C. Tests then need
// no per-test isolation (t.Setenv) and can run in parallel. The fixtures of
// this package fail the test if Main was not called.
//
// A test binary started by a test of the same process (a helper process
// re-executing os.Args[0]) inherits an isolated environment, recognised by
// isolatedHomeVar and the isolated configuration it names: it is kept as
// given, including variables the parent test set on purpose (as the
// per-test isolation did). A marker that does not match an isolated
// configuration is ignored and the environment isolated again.
func Main(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := isolateProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "testutil.Main:", err)
		removeAll(proc.root)
		return 2
	}
	code := m.Run()
	if proc.ownsHome {
		// The configuration is read-only; check anyway (e.g. tests running
		// as root ignore file modes).
		if b, err := os.ReadFile(proc.cfg); err != nil || string(b) != gitConfig {
			fmt.Fprintf(os.Stderr, "testutil.Main: a test changed the global git configuration of the test process %s (err %v):\n%s\n", proc.cfg, err, b)
			if code == 0 {
				code = 1
			}
		}
	}
	removeAll(proc.root)
	return code
}

// isolatedConfigPath is the global git configuration of the isolated home.
func isolatedConfigPath(home string) string {
	return filepath.Join(home, ".sheaf-test", "gitconfig")
}

func isolateProcess() error {
	// The Go toolchain keeps its caches and settings under the real HOME; see
	// ToolchainEnv.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "GIT_") {
			proc.toolEnv = append(proc.toolEnv, kv)
		}
	}
	if home, ok := inheritedIsolation(); ok {
		proc.home, proc.cfg = home, isolatedConfigPath(home)
	} else {
		root, err := rootDir()
		if err != nil {
			return err
		}
		home := filepath.Join(root, "home")
		cfg := isolatedConfigPath(home)
		if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(cfg, []byte(gitConfig), 0o444); err != nil {
			return err
		}
		// git writes a configuration through a lock file next to it: a
		// read-only directory makes `git config --global` fail.
		if err := os.Chmod(filepath.Dir(cfg), 0o555); err != nil {
			return err
		}
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
				if err := os.Unsetenv(k); err != nil {
					return err
				}
			}
		}
		if err := os.Unsetenv("LANGUAGE"); err != nil {
			return err
		}
		for k, v := range isolatedVars(home) {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
		if err := os.Setenv(isolatedHomeVar, home); err != nil {
			return err
		}
		proc.home, proc.cfg, proc.ownsHome = home, cfg, true
	}
	// Fixtures are built with the isolated environment without the GIT_*
	// variables a helper process may have been given.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, managed := isolatedVars(proc.home)[k]; managed || !strings.HasPrefix(k, "GIT_") {
			proc.baseEnv = append(proc.baseEnv, kv)
		}
	}
	proc.isolated = true
	return nil
}

// isolatedVars are the variables Main sets (LANGUAGE is unset, and no other
// GIT_* variable is set).
func isolatedVars(home string) map[string]string {
	return map[string]string{
		"HOME":                home,
		"XDG_CONFIG_HOME":     filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":   isolatedConfigPath(home),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
		"LC_ALL":              "C",
	}
}

// inheritedIsolation reports whether the environment was isolated by the
// Main of a parent test process: the marker names a home whose variables are
// set and whose configuration is the isolated one.
func inheritedIsolation() (string, bool) {
	home := os.Getenv(isolatedHomeVar)
	if home == "" || !filepath.IsAbs(home) {
		return "", false
	}
	for k, v := range isolatedVars(home) {
		if got, ok := os.LookupEnv(k); !ok || got != v {
			return "", false
		}
	}
	if _, ok := os.LookupEnv("LANGUAGE"); ok {
		return "", false
	}
	b, err := os.ReadFile(isolatedConfigPath(home))
	if err != nil || string(b) != gitConfig {
		return "", false
	}
	return home, true
}

// isolatedNow reports whether the environment is the isolated one of the
// process: the variables Main sets have their values, LANGUAGE is unset and
// no other GIT_* variable is set.
func isolatedNow() bool {
	want := isolatedVars(proc.home)
	for k, v := range want {
		if got, ok := os.LookupEnv(k); !ok || got != v {
			return false
		}
	}
	if _, ok := os.LookupEnv("LANGUAGE"); ok {
		return false
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, managed := want[k]; strings.HasPrefix(k, "GIT_") && !managed {
			return false
		}
	}
	return true
}

func requireMain(t *testing.T, fn string) {
	t.Helper()
	if !proc.isolated {
		t.Fatalf("testutil.%s: TestMain must call testutil.Main, which isolates git for the test process", fn)
	}
}

// ToolchainEnv is the environment the test process started with, without
// its GIT_* variables: for running the Go toolchain (e.g. go build), whose
// caches and settings are under the real HOME. Never use it to run git.
func ToolchainEnv() []string {
	return append([]string(nil), proc.toolEnv...)
}

// ProcessTempDir is a directory of the test process, removed when Main returns, for
// fixtures shared by the tests of a package. It fails the test unless
// TestMain calls Main.
func ProcessTempDir(t *testing.T) string {
	t.Helper()
	requireMain(t, "ProcessTempDir")
	root, err := rootDir()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// rootDir returns the temporary directory of the process, creating it on
// first use. A helper process (see Main) creates it inside the one of its
// parent, which removes it even if the helper does not return from Main
// (os.Exit, killed).
func rootDir() (string, error) {
	proc.rootOnce.Do(func() {
		parent := ""
		if !proc.ownsHome && proc.home != "" {
			parent = filepath.Dir(proc.home)
		}
		proc.root, proc.rootErr = os.MkdirTemp(parent, "sheaf-test-")
	})
	return proc.root, proc.rootErr
}

// originCacheKey is the cache key of the bare repository built for opts.
func originCacheKey(opts OriginOpts) string {
	return fmt.Sprintf("origin\x00%s\x00%d\x00%s", opts.DefaultBranch, opts.Commits, strings.Join(opts.Branches, "\x00"))
}

// forkCacheKey is the cache key of the forked fixture built for opts.
func forkCacheKey(opts OriginOpts) string {
	return "fork\x00" + originCacheKey(opts)
}

// testHookCacheEntry, if set, is called by cached between looking up the
// entry for key and building it (tests only, never in parallel tests).
var testHookCacheEntry func(key string)

// cached returns the directory of the cache entry for key, creating it and
// calling build on it on first use (with the isolated environment of the
// process, see proc.baseEnv).
func cached(key string, build func(root string) error) (string, error) {
	proc.cacheMu.Lock()
	if proc.cache == nil {
		proc.cache = map[string]*cacheEntry{}
	}
	e := proc.cache[key]
	if e == nil {
		proc.cacheN++
		e = &cacheEntry{n: proc.cacheN}
		proc.cache[key] = e
	}
	proc.cacheMu.Unlock()
	if testHookCacheEntry != nil {
		testHookCacheEntry(key)
	}
	e.once.Do(func() {
		// Kept if build does not return (e.g. CachedTree's build failing its
		// test with t.Fatal, which exits the goroutine): once is then done.
		e.err = fmt.Errorf("building the cache entry %q did not complete", key)
		base, err := rootDir()
		if err != nil {
			e.err = err
			return
		}
		root := filepath.Join(base, "origins", fmt.Sprint(e.n))
		if err := os.MkdirAll(root, 0o755); err != nil {
			e.err = err
			return
		}
		if err := build(root); err != nil {
			e.err = err
			return
		}
		e.path, e.err = root, nil
	})
	return e.path, e.err
}

// cachedOrigin returns the bare repository built for opts in this process
// (<root>/origin.git, whose origin remote is <root>/work), building it on
// first use.
func cachedOrigin(opts OriginOpts) (string, error) {
	root, err := cached(originCacheKey(opts), func(root string) error {
		if err := buildOrigin(proc.baseEnv, root, opts); err != nil {
			return fmt.Errorf("building cached origin %+v: %w", opts, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "origin.git"), nil
}

// cachedForkedOrigin returns the directory holding the forked fixture built
// for opts in this process (<root>/upstream/repo.git, a copy of the cached
// origin, and <root>/fork/repo.git, a bare clone of it that borrows no
// objects), building it on first use.
func cachedForkedOrigin(opts OriginOpts) (string, error) {
	return cached(forkCacheKey(opts), func(root string) error {
		src, err := cachedOrigin(opts)
		if err != nil {
			return err
		}
		up := filepath.Join(root, "upstream", "repo.git")
		if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
			return err
		}
		if err := copyBare(src, up, filepath.Dir(src), root); err != nil {
			return err
		}
		fork := filepath.Join(root, "fork", "repo.git")
		if _, err := gitEnv(proc.baseEnv, root, "clone", "-q", "--bare", "--no-local", up, fork); err != nil {
			return fmt.Errorf("building cached fork %+v: %w", opts, err)
		}
		return nil
	})
}

// copyBare copies the bare repository src, built under the directory
// srcRoot, to dst and moves its origin remote from under srcRoot to the same
// place under dstRoot, so that git run in the copy never reaches srcRoot.
func copyBare(src, dst, srcRoot, dstRoot string) error {
	if err := copyTree(src, dst); err != nil {
		return err
	}
	retarget := func(url string) (string, error) {
		rel, err := filepath.Rel(srcRoot, url)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("%s: remote URL %q is not under %s", dst, url, srcRoot)
		}
		return filepath.Join(dstRoot, rel), nil
	}
	// git may quote or escape a value with these characters: let git read
	// and write it then (two more processes).
	if strings.ContainsAny(srcRoot+dstRoot, "\"\\;# \t\n") {
		url, err := gitEnv(proc.baseEnv, dst, "config", "remote.origin.url")
		if err != nil {
			return err
		}
		newURL, err := retarget(url)
		if err != nil {
			return err
		}
		_, err = gitEnv(proc.baseEnv, dst, "config", "remote.origin.url", newURL)
		return err
	}
	// Otherwise git wrote the value verbatim: rewrite it without running git.
	cfg := filepath.Join(dst, "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	const key = "\n\turl = "
	i := bytes.Index(b, []byte(key))
	if i < 0 || bytes.Count(b, []byte(key)) != 1 || !bytes.Contains(b[:i], []byte("\n[remote \"origin\"]")) {
		return fmt.Errorf("%s: want exactly one remote, origin, with a URL:\n%s", cfg, b)
	}
	start := i + len(key)
	end := bytes.IndexByte(b[start:], '\n')
	if end < 0 {
		return fmt.Errorf("%s: unterminated remote URL", cfg)
	}
	end += start
	newURL, err := retarget(string(b[start:end]))
	if err != nil {
		return err
	}
	info, err := os.Stat(cfg)
	if err != nil {
		return err
	}
	out := append(append(append([]byte(nil), b[:start]...), newURL...), b[end:]...)
	return os.WriteFile(cfg, out, info.Mode().Perm())
}

// copyTree copies the directory src to dst (which must not exist), keeping
// file modes and symbolic links.
func copyTree(src, dst string) error {
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, mode.Perm()})
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsRegular():
			return copyFile(p, target, mode.Perm())
		default:
			return fmt.Errorf("copy %s: unsupported file type %v", p, mode.Type())
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Directory modes last: a read-only directory is filled first.
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

// removeAll removes dir, making its directories writable first.
func removeAll(dir string) {
	if dir == "" {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}

// WithoutRaceExitDelay returns env (e.g. for a helper process re-executing
// the test binary) with atexit_sleep_ms=0 appended to its GORACE options:
// under the race detector, a process otherwise sleeps 1 s before exiting.
func WithoutRaceExitDelay(env []string) []string {
	var out []string
	opts := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GORACE="); ok {
			opts = v // the last one wins, as for exec.Cmd.Env
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GORACE="+strings.TrimSpace(opts+" atexit_sleep_ms=0"))
}
