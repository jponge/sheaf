package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func riskOf(risks []Risk, k RiskKind) (Risk, bool) {
	for _, r := range risks {
		if r.Kind == k {
			return r, true
		}
	}
	return Risk{}, false
}

func nestedRisk(risks []Risk) (Risk, bool) { return riskOf(risks, RiskNestedRepository) }

// wantBlockingNested checks for exactly one blocking nested repository at rel.
func wantBlockingNested(t *testing.T, risks []Risk, rel string) {
	t.Helper()
	n, ok := nestedRisk(risks)
	if !ok || n.Count != 1 || !strings.Contains(n.Detail, rel) {
		t.Fatalf("risks = %v, want 1 blocking nested repository %s", risks, rel)
	}
	if SafeToDelete(risks) || SafeToRemoveWorktree(risks) || n.Kind.RepositoryWide() || n.Informational() {
		t.Fatal("a nested repository with data at risk must be a blocking, work-tree-local risk")
	}
}

func TestDeletionRisksNestedCloneInIgnoredDir(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "clone", "-q", o2.URL, "vendor/lib")
	testutil.CommitFile(t, filepath.Join(dir, "vendor", "lib"), "x.txt", "x\n", "unpushed")
	r := mustRisks(t, dir)
	wantBlockingNested(t, r, filepath.Join("vendor", "lib"))
	if n, _ := nestedRisk(r); !strings.Contains(n.Detail, "unpushed") {
		t.Fatalf("detail %q does not summarize the nested risks", n.Detail)
	}
}

func TestDeletionRisksCleanPushedNestedIsInformational(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "clone", "-q", o2.URL, "vendor/lib")
	r := mustRisks(t, dir)
	if _, ok := nestedRisk(r); ok {
		t.Fatalf("risks = %v: a clean, fully pushed nested repository must not block", r)
	}
	c, ok := riskOf(r, RiskNestedRepositoryClean)
	if !ok || c.Count != 1 || !c.Informational() || !strings.Contains(c.Detail, filepath.Join("vendor", "lib")) {
		t.Fatalf("risks = %v, want an informational clean nested repository", r)
	}
	if !SafeToDelete(r) {
		t.Fatal("SafeToDelete = false")
	}
}

func TestDeletionRisksNestedWithoutRemoteBlocks(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "scratch/\n")
	testutil.Git(t, dir, "init", "-q", "scratch")
	wantBlockingNested(t, mustRisks(t, dir), "scratch")
}

func TestDeletionRisksNestedWorktreeOfAnotherRepo(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	_, other := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	wt := filepath.Join(dir, "tmp", "wt")
	testutil.Git(t, other, "worktree", "add", "-q", wt, "feat")
	writeFile(t, wt, "wip.txt", "wip\n")
	wantBlockingNested(t, mustRisks(t, dir), filepath.Join("tmp", "wt"))
}

// R1 (a): a repository embedded as a gitlink without .gitmodules.
func TestDeletionRisksEmbeddedGitlink(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "clone", "-q", o2.URL, "lib")
	testutil.CommitFile(t, filepath.Join(dir, "lib"), "only-here.txt", "x\n", "commit only in lib/.git")
	testutil.Git(t, dir, "add", "lib")
	testutil.Git(t, dir, "commit", "-q", "-m", "embed lib")
	testutil.Git(t, dir, "push", "-q", "origin", "main")
	wantBlockingNested(t, mustRisks(t, dir), "lib")
}

// R1 (b): an old-style submodule (.git directory inside its work tree).
func TestDeletionRisksOldStyleSubmoduleUnpushed(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{})
	lib := filepath.Join(dir, "lib")
	testutil.Git(t, dir, "clone", "-q", o2.URL, "lib")
	writeFile(t, dir, ".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = "+o2.URL+"\n")
	testutil.Git(t, dir, "add", ".gitmodules", "lib")
	testutil.Git(t, dir, "commit", "-q", "-m", "submodule")
	testutil.CommitFile(t, lib, "only-here.txt", "x\n", "unpushed in submodule")
	testutil.Git(t, dir, "add", "lib")
	testutil.Git(t, dir, "commit", "-q", "-m", "bump")
	testutil.Git(t, dir, "push", "-q", "origin", "main")
	wantBlockingNested(t, mustRisks(t, dir), "lib")
}

// R1 (b'): a modern submodule (gitdir under .git/modules) with an unpushed
// commit.
func TestDeletionRisksModernSubmoduleUnpushed(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	sub := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "submodule", "add", "-q", sub.URL, "sub")
	testutil.Git(t, dir, "commit", "-q", "-m", "add submodule")
	testutil.CommitFile(t, filepath.Join(dir, "sub"), "s.txt", "s\n", "unpushed in submodule")
	testutil.Git(t, dir, "add", "sub")
	testutil.Git(t, dir, "commit", "-q", "-m", "bump")
	testutil.Git(t, dir, "push", "-q", "origin", "main")
	wantBlockingNested(t, mustRisks(t, dir), "sub")
}

// R1 (c): a repository nested in a tracked directory.
func TestDeletionRisksNestedRepoInTrackedDir(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	d := filepath.Join(dir, "vendor", "lib")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, d, "a.txt", "a\n")
	testutil.Git(t, dir, "add", "vendor")
	testutil.Git(t, dir, "commit", "-q", "-m", "vendor files")
	testutil.Git(t, dir, "push", "-q", "origin", "main")
	testutil.Git(t, d, "init", "-q")
	testutil.Git(t, d, "add", "a.txt")
	testutil.Git(t, d, "commit", "-q", "-m", "local history only")
	wantBlockingNested(t, mustRisks(t, dir), filepath.Join("vendor", "lib"))
}

// Recursion: data at risk two levels down blocks the outer repository.
func TestDeletionRisksNestedRecursively(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "deps/\n")
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{})
	mid := filepath.Join(dir, "deps", "mid")
	testutil.Git(t, dir, "clone", "-q", o2.URL, "deps/mid")
	writeFile(t, mid, ".git/info/exclude", "inner/\n")
	testutil.Git(t, mid, "init", "-q", "inner")
	writeFile(t, filepath.Join(mid, "inner"), "w.txt", "w\n")
	n, ok := nestedRisk(mustRisks(t, dir))
	if !ok || !strings.Contains(n.Detail, filepath.Join("deps", "mid")) {
		t.Fatalf("nested = %+v, want deps/mid blocking because of its own nested repository", n)
	}
}

func TestDeletionRisksIgnoresOwnWorktreesAndSymlinks(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	writeFile(t, dir, ".git/info/exclude", ".wt/\nlink\n")
	testutil.Git(t, dir, "worktree", "add", "-q", ".wt/feat", "feat")
	sub := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "submodule", "add", "-q", sub.URL, "sub")
	testutil.Git(t, dir, "commit", "-q", "-m", "add submodule")
	_, elsewhere := cloneOrigin(t, testutil.OriginOpts{})
	testutil.CommitFile(t, elsewhere, "x.txt", "x\n", "unpushed elsewhere")
	if err := os.Symlink(elsewhere, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r := mustRisks(t, dir)
	if n, ok := nestedRisk(r); ok {
		t.Fatalf("risks = %v: own worktree, clean submodule and symlink must not block", n)
	}
	// The clean submodule is shown (informational), the rest not at all.
	if c, ok := riskOf(r, RiskNestedRepositoryClean); !ok || c.Detail != "sub" {
		t.Fatalf("risks = %v, want only sub as a clean nested repository", r)
	}
}
