package testutil

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

// cachedRoot returns the directory of the cache entry for key if it was
// built successfully, without building it. The calling test must have built
// it itself (through a fixture), so that reading the entry is synchronised
// with its build.
func cachedRoot(key string) (string, bool) {
	proc.cacheMu.Lock()
	e := proc.cache[key]
	proc.cacheMu.Unlock()
	if e == nil || e.err != nil || e.path == "" {
		return "", false
	}
	return e.path, true
}

// NewForkedOrigin builds the pair once per test process (like NewOrigin)
// and copies it per test.
func TestNewForkedOriginIsCopiedFromProcessCache(t *testing.T) {
	t.Parallel()
	opts := OriginOpts{Branches: []string{"fork-cache-probe"}}
	a, b := NewForkedOrigin(t, opts), NewForkedOrigin(t, opts)
	root, ok := cachedRoot(forkCacheKey(OriginOpts{DefaultBranch: "main", Commits: 1, Branches: opts.Branches}))
	if !ok {
		t.Fatal("NewForkedOrigin did not build the fixture in the process cache")
	}
	cache := map[string]string{
		"upstream": filepath.Join(root, "upstream", "repo.git"),
		"fork":     filepath.Join(root, "fork", "repo.git"),
	}
	for _, f := range []ForkedOrigin{a, b} {
		for name, path := range map[string]string{"upstream": f.Upstream.Path, "fork": f.Fork.Path} {
			if path == cache[name] {
				t.Fatalf("%s is the cached repository itself: %s", name, path)
			}
			if got, want := Git(t, path, "for-each-ref"), Git(t, cache[name], "for-each-ref"); got != want {
				t.Fatalf("%s refs differ from the cached ones:\n%s\nvs\n%s", name, got, want)
			}
		}
	}
	if a.Upstream.Path == b.Upstream.Path || a.Fork.Path == b.Fork.Path {
		t.Fatalf("copies share paths: %+v %+v", a, b)
	}
}

// noCacheReference fails the test if a file of the repository dir names the
// process cache (alternates, remote URLs...): git run in a copy must never
// read or write the cached repositories.
func noCacheReference(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "objects", "info", "alternates")); err == nil {
		t.Errorf("%s borrows objects (objects/info/alternates)", dir)
	}
	roots := [][]byte{[]byte(proc.root)}
	if r, err := filepath.EvalSymlinks(proc.root); err == nil && r != proc.root {
		roots = append(roots, []byte(r))
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, r := range roots {
			if bytes.Contains(b, r) {
				t.Errorf("%s names the process cache %s:\n%s", p, r, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Copies of cached fixtures are independent of the cache: no shared objects,
// no remote pointing into it, and the remotes of the bare repositories are
// those an uncached build has (origin.git: <dir>/work; the fork: its
// upstream). Changing a copy leaves the cache as it was.
func TestCachedFixtureCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	opts := OriginOpts{Branches: []string{"independence-probe"}}
	o := NewOrigin(t, opts)
	f := NewForkedOrigin(t, opts)
	for _, dir := range []string{o.Path, f.Upstream.Path, f.Fork.Path} {
		noCacheReference(t, dir)
	}
	if got, want := Git(t, o.Path, "config", "remote.origin.url"), filepath.Join(filepath.Dir(o.Path), "work"); got != want {
		t.Errorf("origin.git remote = %q, want %q", got, want)
	}
	if got := Git(t, f.Fork.Path, "config", "remote.origin.url"); got != f.Upstream.Path {
		t.Errorf("fork remote = %q, want the upstream %q", got, f.Upstream.Path)
	}

	full := OriginOpts{DefaultBranch: "main", Commits: 1, Branches: opts.Branches}
	src, err := cachedOrigin(full)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := cachedRoot(forkCacheKey(full))
	if !ok {
		t.Fatal("forked fixture not cached")
	}
	cached := []string{src, filepath.Join(root, "upstream", "repo.git"), filepath.Join(root, "fork", "repo.git")}
	var before []string
	for _, c := range cached {
		before = append(before, Git(t, c, "for-each-ref"))
	}
	for _, x := range []Origin{o, f.Upstream, f.Fork} {
		AdvanceOrigin(t, x, "main", "copy.txt", "copy\n", "copy only")
		Git(t, x.Path, "branch", "-D", "independence-probe")
	}
	Git(t, f.Fork.Path, "push", "-q", "origin", "main:refs/heads/from-fork")
	for i, c := range cached {
		if got := Git(t, c, "for-each-ref"); got != before[i] {
			t.Errorf("cached %s changed:\n%s\nvs\n%s", c, before[i], got)
		}
	}
}

// copyBare also retargets a remote URL that git writes quoted or escaped in
// its configuration (directories with spaces, ';', '#'...).
func TestCopyBareRetargetsQuotedRemoteURLs(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	srcRoot, dstRoot := filepath.Join(base, `src "a;b"#c`), filepath.Join(base, `dst \d e`)
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(srcRoot, "work")
	Git(t, base, "init", "-q", work)
	CommitFile(t, work, "f.txt", "f\n", "f")
	Git(t, base, "clone", "-q", "--bare", work, filepath.Join(srcRoot, "origin.git"))
	dst := filepath.Join(dstRoot, "origin.git")
	if err := copyBare(filepath.Join(srcRoot, "origin.git"), dst, srcRoot, dstRoot); err != nil {
		t.Fatal(err)
	}
	if got, want := Git(t, dst, "config", "remote.origin.url"), filepath.Join(dstRoot, "work"); got != want {
		t.Fatalf("remote = %q, want %q", got, want)
	}
}

var cacheRaceRun atomic.Int64

// Each cache entry is built in a directory of its own, whoever builds it: a
// caller of an existing entry that builds it before the caller that created
// it must not use the directory of an entry created in between. Not
// parallel: it sets testHookCacheEntry.
func TestCachedEntryBuildsInItsOwnDirectory(t *testing.T) {
	run := cacheRaceRun.Add(1)
	k1, k2 := fmt.Sprintf("test\x00own-dir-1\x00%d", run), fmt.Sprintf("test\x00own-dir-2\x00%d", run)
	created, release := make(chan struct{}), make(chan struct{})
	var k1Calls atomic.Int64
	testHookCacheEntry = func(key string) {
		if key == k1 && k1Calls.Add(1) == 1 {
			close(created) // the creator of k1's entry waits before building it
			<-release
		}
	}
	t.Cleanup(func() { testHookCacheEntry = nil })
	build := func(name string) func(string) error {
		return func(root string) error { return os.WriteFile(filepath.Join(root, name), nil, 0o644) }
	}
	creator := make(chan error, 1)
	go func() { _, err := cached(k1, build("k1")); creator <- err }()
	<-created
	root2, err := cached(k2, build("k2")) // created after k1's entry
	if err != nil {
		t.Fatal(err)
	}
	root1, err := cached(k1, build("k1")) // builds k1 before its creator does
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-creator; err != nil {
		t.Fatal(err)
	}
	if root1 == root2 {
		t.Fatalf("entries %q and %q share the directory %s", k1, k2, root1)
	}
	for _, f := range []string{filepath.Join(root1, "k1"), filepath.Join(root2, "k2")} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root1, "k2")); err == nil {
		t.Errorf("%s holds the build of another entry", root1)
	}
}

// A build that does not return (CachedTree's build failing its test with
// t.Fatal exits the goroutine inside the entry's once) leaves an error for
// the later callers, not an empty directory without error.
func TestCachedEntryBuildThatDoesNotReturnIsAnError(t *testing.T) {
	t.Parallel()
	key := fmt.Sprintf("test\x00goexit\x00%d", cacheRaceRun.Add(1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cached(key, func(string) error { runtime.Goexit(); return nil })
	}()
	<-done
	root, err := cached(key, func(string) error { t.Error("built twice"); return nil })
	if err == nil || root != "" {
		t.Fatalf("cached = %q, %v; want an error", root, err)
	}
}
