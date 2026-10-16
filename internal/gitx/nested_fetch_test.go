package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func risksWith(t *testing.T, dir string, opts DeletionRisksOptions) []Risk {
	t.Helper()
	r, err := DeletionRisksWithOptions(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// staleNested makes a nested clone at rel (inside an ignored dir) whose
// commit was pushed to topic, then topic deleted on its origin: only a fetch
// of the nested repository reveals the risk.
func staleNested(t *testing.T, parent, rel string) string {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	n := filepath.Join(parent, rel)
	testutil.Git(t, parent, "clone", "-q", o.URL, rel)
	testutil.Git(t, n, "checkout", "-q", "-b", "topic")
	testutil.CommitFile(t, n, "t.txt", "t\n", "topic")
	testutil.Git(t, n, "push", "-q", "origin", "topic")
	testutil.Git(t, n, "checkout", "-q", "main")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "topic")
	return n
}

func TestDeletionRisksBeforeNestedHookSeesFreshRemoteState(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	dir = resolved(t, dir) // git, hence the hook, sees resolved paths
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	n := staleNested(t, dir, "vendor/lib")
	inner := staleNested(t, n, "deps/inner")
	writeFile(t, n, ".git/info/exclude", "deps/\n")
	// Without fetching, the stale refs make both look pushed.
	if _, ok := nestedRisk(mustRisks(t, dir)); ok {
		t.Fatal("setup: stale nested repository already blocking without fetch")
	}
	var seen []string
	hook := func(ctx context.Context, d string) error {
		seen = append(seen, d)
		return FetchAll(ctx, d)
	}
	r := risksWith(t, dir, DeletionRisksOptions{BeforeNested: hook})
	wantBlockingNested(t, r, filepath.Join("vendor", "lib"))
	if !slices.Contains(seen, n) || !slices.Contains(seen, inner) {
		t.Fatalf("hook called for %v, want %s and %s", seen, n, inner)
	}
}

func TestDeletionRisksBeforeNestedFailureBlocks(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	testutil.Git(t, dir, "clone", "-q", testutil.NewOrigin(t, testutil.OriginOpts{}).URL, "vendor/lib")
	r := risksWith(t, dir, DeletionRisksOptions{BeforeNested: func(context.Context, string) error { return errors.New("unreachable") }})
	wantBlockingNested(t, r, filepath.Join("vendor", "lib"))
	if n, _ := nestedRisk(r); !strings.Contains(n.Detail, "fetch failed") {
		t.Fatalf("detail = %q, want fetch failure", n.Detail)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := DeletionRisksWithOptions(ctx, dir, DeletionRisksOptions{BeforeNested: func(context.Context, string) error {
		cancel()
		return context.Canceled
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation to propagate", err)
	}
}

func TestFetchAllPrunesEveryRemote(t *testing.T) {
	t.Parallel()
	o, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"gone"}})
	o2 := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"other"}})
	testutil.Git(t, dir, "remote", "add", "second", o2.URL)
	testutil.Git(t, o.Path, "branch", "-q", "-D", "gone")
	if err := FetchAll(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	refs := testutil.Git(t, dir, "for-each-ref", "--format=%(refname)", "refs/remotes")
	if strings.Contains(refs, "origin/gone") || !strings.Contains(refs, "second/other") {
		t.Fatalf("remote refs after FetchAll:\n%s", refs)
	}
}

// Bare repositories inside the checkout are nested repositories.
func TestDeletionRisksNestedBareRepository(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	work := filepath.Join(t.TempDir(), "w")
	testutil.Git(t, filepath.Dir(work), "init", "-q", "-b", "main", work)
	testutil.CommitFile(t, work, "only.txt", "x\n", "only copy")
	if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, filepath.Join(dir, "vendor"), "clone", "-q", "--bare", work, "backup.git")
	r := mustRisks(t, dir)
	wantBlockingNested(t, r, filepath.Join("vendor", "backup.git"))
	if n, _ := nestedRisk(r); !strings.Contains(n.Detail, "bare") {
		t.Fatalf("detail = %q, want bare repository", n.Detail)
	}
}

// A remote inside the tree being deleted is no remote.
func TestDeletionRisksRemoteInsideDeletedTree(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, ".git/info/exclude", "vendor/\n")
	vendor := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, vendor, "init", "-q", "--bare", "store.git")
	n := filepath.Join(vendor, "lib")
	testutil.Git(t, vendor, "init", "-q", "-b", "main", "lib")
	testutil.CommitFile(t, n, "x.txt", "x\n", "c1")
	testutil.Git(t, n, "remote", "add", "origin", filepath.Join(vendor, "store.git"))
	testutil.Git(t, n, "push", "-q", "-u", "origin", "main")
	r := mustRisks(t, dir)
	n2, ok := nestedRisk(r)
	if !ok || !strings.Contains(n2.Detail, filepath.Join("vendor", "lib")) || !strings.Contains(n2.Detail, "inside") {
		t.Fatalf("risks = %v, want vendor/lib blocking: its only remote is inside the deleted tree", r)
	}
	// With a deleted root that does not contain the remote, it is fine.
	outside := filepath.Join(t.TempDir(), "other-root")
	r = risksWith(t, dir, DeletionRisksOptions{DeletedRoot: outside})
	if n3, ok := nestedRisk(r); ok && strings.Contains(n3.Detail, filepath.Join("vendor", "lib")+" (") {
		t.Fatalf("vendor/lib blocking although its remote is outside the deleted root: %v", n3)
	}
}
