package testutil

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TreeFixture is a fixture made of whole directories (bundles, checkouts,
// bare repositories), see CachedTree.
type TreeFixture struct {
	// Dirs are the absolute directories holding the fixture.
	Dirs []string
	// Values are named strings the caller needs afterwards (paths, file://
	// URLs); those naming something under Dirs name the same thing in a copy.
	Values map[string]string
}

// treeTemplates holds the cached template of each CachedTree key (stored by
// the build of the key's cache entry, loaded after cached returns).
var treeTemplates sync.Map // cache key -> TreeFixture

// CachedTree returns a fixture built by build, which creates it in
// directories of its own (from t.TempDir() and the other fixtures of this
// package) and returns them with the values the caller needs.
//
// In a test that did not change the environment, build runs once per test
// process and key, in the first test that asks: it must build the same
// fixture whichever test runs it. Its directories are copied into the
// process cache, and every caller, the first one included, gets a copy of
// its own under t.TempDir(): file modes and symbolic links are kept, and
// every absolute path into the fixture's directories, in its given and
// symlink-resolved forms (worktree links, remote URLs, sheaf.yaml,
// FETCH_HEAD, reflogs...), is moved into the copy, as are the values. A copy
// never names the cache or the directories of the build, and no repository
// borrows objects (alternates): CachedTree fails the test otherwise. The
// stat data in a git index are those of the build's files, so git re-reads
// the files of a copy to see that they are unchanged.
//
// In a test that changed the environment (see IsolateGit), build runs every
// time and its fixture is returned as built.
func CachedTree(t *testing.T, key string, build func(t *testing.T) TreeFixture) TreeFixture {
	t.Helper()
	requireMain(t, "CachedTree")
	if !isolatedNow() {
		return build(t)
	}
	key = treeCacheKey(key)
	_, err := cached(key, func(root string) error {
		tf := build(t)
		dsts := make([]string, len(tf.Dirs))
		for i := range dsts {
			dsts[i] = filepath.Join(root, fmt.Sprint(i))
		}
		tmpl, err := copyTreeFixture(tf, dsts, nil)
		if err != nil {
			return fmt.Errorf("caching the tree fixture built by %s: %w", t.Name(), err)
		}
		treeTemplates.Store(key, tmpl)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := treeTemplates.Load(key)
	tmpl := v.(TreeFixture)
	dsts := make([]string, len(tmpl.Dirs))
	for i := range dsts {
		dsts[i] = t.TempDir()
	}
	out, err := copyTreeFixture(tmpl, dsts, []string{proc.root})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func treeCacheKey(key string) string { return "tree\x00" + key }

// cachedTreeTemplate returns the cached template of key, if built (tests).
func cachedTreeTemplate(key string) (TreeFixture, bool) {
	v, ok := treeTemplates.Load(treeCacheKey(key))
	if !ok {
		return TreeFixture{}, false
	}
	return v.(TreeFixture), true
}

// copyTreeFixture copies each directory of tf to the directory of dsts at
// the same index (an existing empty directory is replaced), moves every path
// into the directories of tf into the copies, and checks that no file of the
// copies names the directories of tf or one of forbidden afterwards.
func copyTreeFixture(tf TreeFixture, dsts []string, forbidden []string) (TreeFixture, error) {
	if len(dsts) != len(tf.Dirs) {
		return TreeFixture{}, fmt.Errorf("%d destinations for %d directories", len(dsts), len(tf.Dirs))
	}
	var pairs []pathPair
	for i, src := range tf.Dirs {
		if !filepath.IsAbs(src) {
			return TreeFixture{}, fmt.Errorf("fixture directory %q is not absolute", src)
		}
		for _, other := range tf.Dirs[:i] {
			if rel, err := filepath.Rel(other, src); err == nil && filepath.IsLocal(rel) || rel == "." {
				return TreeFixture{}, fmt.Errorf("fixture directories %q and %q overlap", other, src)
			}
			if rel, err := filepath.Rel(src, other); err == nil && filepath.IsLocal(rel) {
				return TreeFixture{}, fmt.Errorf("fixture directories %q and %q overlap", other, src)
			}
		}
		if err := os.Remove(dsts[i]); err != nil && !os.IsNotExist(err) {
			return TreeFixture{}, err
		}
		if err := copyTree(src, dsts[i]); err != nil {
			return TreeFixture{}, err
		}
		p, err := pathPairs(src, dsts[i])
		if err != nil {
			return TreeFixture{}, err
		}
		pairs = append(pairs, p...)
	}
	// Longer paths first: where two match at the same place, the longer one
	// is the more specific.
	slices.SortStableFunc(pairs, func(a, b pathPair) int { return len(b.old) - len(a.old) })
	var bad [][]byte
	for _, f := range append(slices.Clone(forbidden), tf.Dirs...) {
		for _, form := range pathForms(f) {
			bad = append(bad, []byte(form))
		}
	}
	for _, dst := range dsts {
		if err := rewriteTree(dst, pairs, bad); err != nil {
			return TreeFixture{}, err
		}
	}
	out := TreeFixture{Dirs: slices.Clone(dsts), Values: map[string]string{}}
	for k, v := range tf.Values {
		out.Values[k] = string(replacePaths([]byte(v), pairs))
	}
	return out, nil
}

// pathPair maps a path (as written in a file) of a fixture directory to the
// same path in its copy.
type pathPair struct{ old, new []byte }

// pathForms returns p and, if it differs, p with symbolic links resolved.
func pathForms(p string) []string {
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

// pathPairs maps the resolved form of src (git writes some paths resolved)
// to the resolved form of dst and, if it differs, src to dst. Paths with characters that
// git or YAML may escape cannot be moved by replacing bytes.
func pathPairs(src, dst string) ([]pathPair, error) {
	rs, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil, err
	}
	rd, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{src, dst, rs, rd} {
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || strings.ContainsRune("\"'\\", r) }) {
			return nil, fmt.Errorf("cannot move the path %q: it has characters that git or YAML may escape", p)
		}
	}
	// A resolved path maps to a resolved path, whichever form src has.
	pairs := []pathPair{{[]byte(rs), []byte(rd)}}
	if src != rs {
		pairs = append(pairs, pathPair{[]byte(src), []byte(dst)})
	}
	return pairs, nil
}

// replacePaths replaces in b every occurrence of an old path of pairs that
// ends a path component (followed by '/', the end, or any byte that cannot
// continue a file name here) with its new path.
func replacePaths(b []byte, pairs []pathPair) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(b); {
		matched := false
		for _, p := range pairs {
			if bytes.HasPrefix(b[i:], p.old) && endsComponent(b, i+len(p.old)) {
				out = append(append(out, b[last:i]...), p.new...)
				i += len(p.old)
				last = i
				matched = true
				break
			}
		}
		if !matched {
			i++
		}
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

func endsComponent(b []byte, i int) bool {
	if i >= len(b) {
		return true
	}
	c := b[i]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c >= 0x80)
}

// rewriteTree moves the paths of pairs in every regular file under dir,
// except in the object directories of git repositories (no paths there;
// alternates, which would borrow objects, are refused). A file that names
// one of bad afterwards, or a binary file naming an old path, is an error.
func rewriteTree(dir string, pairs []pathPair, bad [][]byte) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "objects" && isGitDir(filepath.Dir(p)) {
				if _, err := os.Lstat(filepath.Join(p, "info", "alternates")); err == nil {
					return fmt.Errorf("%s borrows objects (objects/info/alternates)", filepath.Dir(p))
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
		nb := replacePaths(b, pairs)
		if !bytes.Equal(nb, b) {
			if bytes.IndexByte(b, 0) >= 0 {
				return fmt.Errorf("binary file %s names a fixture directory", p)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			perm := info.Mode().Perm()
			if err := os.Chmod(p, perm|0o200); err != nil { // read-only files
				return err
			}
			if err := os.WriteFile(p, nb, perm); err != nil {
				return err
			}
			if err := os.Chmod(p, perm); err != nil {
				return err
			}
		}
		for _, f := range bad {
			if bytes.Contains(nb, f) {
				return fmt.Errorf("%s still names %s after the copy:\n%s", p, f, nb)
			}
		}
		return nil
	})
}

// isGitDir reports whether dir looks like a git directory (HEAD and refs).
func isGitDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "refs"))
	return err == nil
}
