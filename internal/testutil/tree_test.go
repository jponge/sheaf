package testutil

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var treeRun atomic.Int64

// buildTreeProbe builds a small bundle-like fixture: a bare origin (its own
// directory), and a bundle directory holding a sheaf.yaml that names the
// origin's URL, a primary clone a/main with a branch old tracking
// origin/feat, and a linked worktree a/feat.
func buildTreeProbe(t *testing.T) TreeFixture {
	t.Helper()
	o := NewOrigin(t, OriginOpts{Branches: []string{"feat"}})
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "sheaf.yaml"), []byte("repositories:\n  - name: a\n    url: "+o.URL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(bundle, "a", "main")
	Git(t, bundle, "clone", "-q", o.URL, primary)
	Git(t, primary, "fetch", "-q", "origin")
	Git(t, primary, "branch", "-q", "--track", "old", "origin/feat")
	Git(t, primary, "worktree", "add", "-q", filepath.Join(bundle, "a", "feat"), "feat")
	return TreeFixture{
		Dirs:   []string{bundle, filepath.Dir(o.Path)},
		Values: map[string]string{"bundle": bundle, "primary": primary, "origin": o.Path, "url": o.URL, "other": "not a path"},
	}
}

// noTreeReference fails the test if a file under dirs (outside object
// directories) names one of roots, or if a repository borrows objects.
func noTreeReference(t *testing.T, dirs []string, roots []string) {
	t.Helper()
	var forbidden [][]byte
	for _, r := range roots {
		for _, f := range pathForms(r) {
			forbidden = append(forbidden, []byte(f))
		}
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "objects" && isGitDir(filepath.Dir(p)) {
					if _, err := os.Stat(filepath.Join(p, "info", "alternates")); err == nil {
						t.Errorf("%s borrows objects (alternates)", filepath.Dir(p))
					}
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, f := range forbidden {
				if bytes.Contains(b, f) {
					t.Errorf("%s names %s:\n%s", p, f, b)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// normalizeDirs replaces the directories of tf (and their resolved forms)
// in s with placeholders.
func normalizeDirs(s string, tf TreeFixture) string {
	for i, d := range tf.Dirs {
		forms := pathForms(d)
		for j := len(forms) - 1; j >= 0; j-- { // resolved form first: it may contain the other
			s = strings.ReplaceAll(s, forms[j], fmt.Sprintf("<dir %d>", i))
		}
	}
	return s
}

// treeState describes the git state of a probe fixture with read-only
// commands (no optional index refresh).
func treeState(t *testing.T, tf TreeFixture) string {
	t.Helper()
	p := tf.Values["primary"]
	var b strings.Builder
	b.WriteString(Git(t, p, "for-each-ref") + "\n")
	b.WriteString(Git(t, p, "worktree", "list", "--porcelain") + "\n")
	b.WriteString(Git(t, tf.Values["origin"], "for-each-ref") + "\n")
	feat := filepath.Join(tf.Values["bundle"], "a", "feat")
	b.WriteString(Git(t, feat, "--no-optional-locks", "status", "--porcelain=v2", "--branch") + "\n")
	y, err := os.ReadFile(filepath.Join(tf.Values["bundle"], "sheaf.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b.Write(y)
	return b.String()
}

// CachedTree builds a fixture once per test process and key, and gives each
// caller an independent copy: every path into the fixture (worktree links,
// remote URLs, files such as sheaf.yaml) is moved into the copy, and
// changing a copy leaves the cached template and the other copies as they
// were.
func TestCachedTreeCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	key := fmt.Sprintf("tree-probe-%d", treeRun.Add(1))
	var builds atomic.Int64
	var orig []string
	build := func(t *testing.T) TreeFixture {
		builds.Add(1)
		tf := buildTreeProbe(t)
		orig = tf.Dirs
		return tf
	}
	a := CachedTree(t, key, build)
	b := CachedTree(t, key, build)
	if n := builds.Load(); n != 1 {
		t.Fatalf("built %d times, want once per process", n)
	}
	tmpl, ok := cachedTreeTemplate(key)
	if !ok {
		t.Fatal("no cached template")
	}
	for _, c := range []TreeFixture{a, b, tmpl} {
		if len(c.Dirs) != 2 || len(c.Values) != 5 || c.Values["other"] != "not a path" {
			t.Fatalf("fixture %+v", c)
		}
	}
	seen := map[string]bool{}
	for _, c := range []TreeFixture{a, b, tmpl} {
		for _, d := range c.Dirs {
			for _, o := range orig {
				if d == o {
					t.Fatalf("%s is a directory of the build", d)
				}
			}
			if seen[d] {
				t.Fatalf("%s shared between copies", d)
			}
			seen[d] = true
		}
	}

	// Paths are moved into each copy.
	for _, c := range []TreeFixture{a, b} {
		want := map[string]string{
			"bundle":  c.Dirs[0],
			"primary": filepath.Join(c.Dirs[0], "a", "main"),
			"origin":  filepath.Join(c.Dirs[1], "origin.git"),
			"url":     "file://" + filepath.Join(c.Dirs[1], "origin.git"),
		}
		for k, v := range want {
			if c.Values[k] != v {
				t.Errorf("value %s = %q, want %q", k, c.Values[k], v)
			}
		}
		primary, feat := c.Values["primary"], filepath.Join(c.Dirs[0], "a", "feat")
		if got := Git(t, primary, "config", "remote.origin.url"); got != c.Values["url"] {
			t.Errorf("primary remote = %q, want %q", got, c.Values["url"])
		}
		if got, want := Git(t, c.Values["origin"], "config", "remote.origin.url"), filepath.Join(c.Dirs[1], "work"); got != want {
			t.Errorf("origin remote = %q, want %q", got, want)
		}
		realPrimary, err := filepath.EvalSymlinks(primary)
		if err != nil {
			t.Fatal(err)
		}
		if got := Git(t, feat, "rev-parse", "--path-format=absolute", "--git-common-dir"); got != filepath.Join(realPrimary, ".git") {
			t.Errorf("worktree common dir = %q, want %q", got, filepath.Join(realPrimary, ".git"))
		}
		list := Git(t, primary, "worktree", "list", "--porcelain")
		if !strings.Contains(list, "worktree "+filepath.Join(filepath.Dir(realPrimary), "feat")+"\n") || strings.Contains(list, "prunable") {
			t.Errorf("worktree list:\n%s", list)
		}
		y, err := os.ReadFile(filepath.Join(c.Dirs[0], "sheaf.yaml"))
		if err != nil || !strings.Contains(string(y), "url: "+c.Values["url"]+"\n") {
			t.Errorf("sheaf.yaml = %q, %v", y, err)
		}
		noTreeReference(t, c.Dirs, append([]string{proc.root}, orig...))
	}
	noTreeReference(t, tmpl.Dirs, orig)

	// Changing a copy changes neither the template nor the other copy.
	before := map[string]string{"template": treeState(t, tmpl), "b": treeState(t, b)}
	primary, feat := a.Values["primary"], filepath.Join(a.Dirs[0], "a", "feat")
	CommitFile(t, feat, "copy.txt", "copy\n", "copy only")
	CommitFile(t, primary, "main.txt", "main\n", "pushed from the copy")
	Git(t, primary, "push", "-q", "origin", "main", "feat")
	Git(t, primary, "branch", "-q", "-D", "old")
	Git(t, primary, "worktree", "remove", feat)
	if _, err := os.Stat(feat); err == nil {
		t.Fatal("worktree not removed")
	}
	if Git(t, a.Values["origin"], "rev-parse", "main") != Git(t, primary, "rev-parse", "main") {
		t.Fatal("push did not reach the copy's origin")
	}
	for name, c := range map[string]TreeFixture{"template": tmpl, "b": b} {
		if got := treeState(t, c); got != before[name] {
			t.Errorf("%s changed:\n%s\nvs\n%s", name, before[name], got)
		}
	}
	// A copy made afterwards is like the others were.
	c := CachedTree(t, key, build)
	if got, want := normalizeDirs(treeState(t, c), c), normalizeDirs(before["b"], b); got != want {
		t.Errorf("new copy differs:\n%s\nvs\n%s", want, got)
	}
}

// In a test that changed the environment (here LANGUAGE), CachedTree builds
// the fixture itself, every time, and returns it as built. Not parallel:
// t.Setenv.
func TestCachedTreeBuildsUncachedWhenEnvironmentChanged(t *testing.T) {
	t.Setenv("LANGUAGE", "de")
	key := fmt.Sprintf("tree-probe-env-%d", treeRun.Add(1))
	var builds atomic.Int64
	var last TreeFixture
	build := func(t *testing.T) TreeFixture {
		builds.Add(1)
		last = buildTreeProbe(t)
		return last
	}
	for i := 1; i <= 2; i++ {
		got := CachedTree(t, key, build)
		if builds.Load() != int64(i) || got.Dirs[0] != last.Dirs[0] || got.Values["primary"] != last.Values["primary"] {
			t.Fatalf("call %d: builds %d, got %+v, want the build %+v", i, builds.Load(), got, last)
		}
	}
	if _, ok := cachedTreeTemplate(key); ok {
		t.Fatal("cached a fixture built with a changed environment")
	}
}

// A fixture whose directories are given with symbolic links resolved (as a
// sheaf workspace root is) gets copies whose values and recorded paths are
// resolved too, as git and sheaf would have written them, even though
// t.TempDir() is reached through a symbolic link (macOS: /var ->
// /private/var). Not parallel: it points TMPDIR at a symbolic link so that
// the build and the copies of this test are under one.
func TestCachedTreeKeepsResolvedPathsResolved(t *testing.T) {
	base, err := os.MkdirTemp("", "tree-link-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeAll(base) })
	if err := os.Mkdir(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(base, "link"))
	key := fmt.Sprintf("tree-probe-resolved-%d", treeRun.Add(1))
	resolve := func(p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	c := CachedTree(t, key, func(t *testing.T) TreeFixture {
		tf := buildTreeProbe(t)
		for i := range tf.Dirs {
			tf.Dirs[i] = resolve(tf.Dirs[i])
		}
		tf.Values["bundle"] = resolve(tf.Values["bundle"])
		tf.Values["primary"] = resolve(tf.Values["primary"])
		return tf
	})
	if c.Dirs[0] == resolve(c.Dirs[0]) {
		t.Fatalf("setup: the copy %s is not reached through a symbolic link", c.Dirs[0])
	}
	bundle := resolve(c.Dirs[0])
	if got := c.Values["bundle"]; got != bundle {
		t.Errorf("bundle = %q, want the resolved copy %q", got, bundle)
	}
	if got, want := c.Values["primary"], filepath.Join(bundle, "a", "main"); got != want {
		t.Errorf("primary = %q, want %q", got, want)
	}
	list := Git(t, c.Values["primary"], "worktree", "list", "--porcelain")
	if !strings.Contains(list, "worktree "+filepath.Join(bundle, "a", "feat")+"\n") {
		t.Errorf("worktree list does not name the resolved copy %s:\n%s", bundle, list)
	}
	for _, f := range []string{filepath.Join(c.Dirs[0], "a", "feat", ".git"), filepath.Join(c.Dirs[0], "a", "main", ".git", "worktrees", "feat", "gitdir")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), bundle+"/") {
			t.Errorf("%s = %q, want a path under the resolved copy %s", f, b, bundle)
		}
	}
}
