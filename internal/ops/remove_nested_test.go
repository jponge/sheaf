package ops

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

// nestedSetup clones repo a and makes primary/vendor/ ignored.
func nestedSetup(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "vendor/\n")
	if err := os.MkdirAll(filepath.Join(primary, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f, primary
}

// staleNestedClone: a commit pushed to topic, topic then deleted on the
// nested repository's origin; the local refs still say "pushed".
func staleNestedClone(t *testing.T, parent string) string {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	n := filepath.Join(parent, "lib")
	plainClone(t, o, n)
	testutil.Git(t, n, "checkout", "-q", "-b", "topic")
	testutil.CommitFile(t, n, "t.txt", "t\n", "topic")
	testutil.Git(t, n, "push", "-q", "origin", "topic")
	testutil.Git(t, n, "checkout", "-q", "main")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "topic")
	return n
}

// Nested repositories are fetched before being assessed.
func TestRemoveFetchesNestedRepositories(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	n := staleNestedClone(t, filepath.Join(primary, "vendor"))
	_, err := f.remove(t, "a", false, yes)
	if err == nil || !strings.Contains(err.Error(), filepath.Join("vendor", "lib")) || !strings.Contains(err.Error(), "unpushed") {
		t.Fatalf("err = %v, want refusal: vendor/lib has an unpushed commit once fetched", err)
	}
	if !exists(filepath.Join(n, ".git")) {
		t.Fatal("deleted")
	}
}

func TestRemoveNestedFetchFailureBlocks(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	n := filepath.Join(primary, "vendor", "lib")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), n)
	testutil.Git(t, n, "remote", "set-url", "origin", "file:///nonexistent/lib.git")
	_, err := f.remove(t, "a", false, yes)
	if err == nil || !strings.Contains(err.Error(), "fetch failed") {
		t.Fatalf("err = %v, want refusal: the nested fetch failed", err)
	}
}

func TestRemoveOfflineDoesNotFetchNested(t *testing.T) {
	t.Parallel()
	f, primary := nestedSetup(t)
	staleNestedClone(t, filepath.Join(primary, "vendor"))
	evs, err := f.removeOpts(t, "a", RemoveOptions{Offline: true, Confirm: yes})
	if err != nil {
		t.Fatalf("offline remove (local refs only): %v", err)
	}
	if !slices.ContainsFunc(evs, func(e Event) bool {
		return strings.Contains(e.Message, "stale") && strings.Contains(e.Message, "nested")
	}) {
		t.Fatalf("offline warning does not mention nested repositories: %+v", evs)
	}
}

// A bare repository in the tree, and a nested repository whose only
// remote is inside the tree, block.
func TestRemoveNestedBareAndInsideRemoteBlock(t *testing.T) {
	t.Parallel()
	t.Run("bare", func(t *testing.T) {
		f, primary := nestedSetup(t)
		work := filepath.Join(t.TempDir(), "w")
		testutil.Git(t, filepath.Dir(work), "init", "-q", "-b", "main", work)
		testutil.CommitFile(t, work, "only.txt", "x\n", "only copy")
		testutil.Git(t, filepath.Join(primary, "vendor"), "clone", "-q", "--bare", work, "backup.git")
		if _, err := f.remove(t, "a", false, yes); err == nil || !strings.Contains(err.Error(), "bare repository") {
			t.Fatalf("err = %v, want refusal for the bare repository", err)
		}
	})
	t.Run("remote inside the deleted tree", func(t *testing.T) {
		f, primary := nestedSetup(t)
		vendor := filepath.Join(primary, "vendor")
		testutil.Git(t, vendor, "init", "-q", "--bare", "store.git")
		n := filepath.Join(vendor, "lib")
		testutil.Git(t, vendor, "init", "-q", "-b", "main", "lib")
		testutil.CommitFile(t, n, "x.txt", "x\n", "c1")
		testutil.Git(t, n, "remote", "add", "origin", filepath.Join(vendor, "store.git"))
		testutil.Git(t, n, "push", "-q", "-u", "origin", "main")
		if _, err := f.remove(t, "a", false, yes); err == nil || !strings.Contains(err.Error(), "inside the deleted tree") {
			t.Fatalf("err = %v, want refusal: the remote is deleted too", err)
		}
	})
}

// A nested repository whose state cannot be established (never fetched with
// an unreachable remote, or a dangling .git file) blocks removal, online or
// offline.
func TestRemoveNestedEdgeCasesBlock(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, n string){
		"never fetched, unreachable remote": func(t *testing.T, n string) {
			testutil.Git(t, filepath.Dir(n), "init", "-q", "-b", "main", n)
			testutil.CommitFile(t, n, "x.txt", "x\n", "local")
			testutil.Git(t, n, "remote", "add", "origin", "file:///nonexistent/x.git")
		},
		"dangling .git file": func(t *testing.T, n string) {
			if err := os.MkdirAll(n, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(n, ".git"), "gitdir: /nonexistent/gitdir\n")
			writeFile(t, filepath.Join(n, "work.txt"), "w\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, primary := nestedSetup(t)
			n := filepath.Join(primary, "vendor", "lib")
			setup(t, n)
			for _, offline := range []bool{false, true} {
				if _, err := f.removeOpts(t, "a", RemoveOptions{Offline: offline, Confirm: yes}); err == nil {
					t.Fatalf("offline=%v: removed", offline)
				}
			}
			if !exists(n) {
				t.Fatal("deleted")
			}
		})
	}
}

// A cancellation once the directory is being deleted does not leave
// its entry behind in sheaf.yaml (the unregistration runs under the bundle
// lock taken before the deletion, and cannot be cancelled).
func TestRemoveCancelledDuringDeletionUnregisters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := false
	_, err := Remove(ctx, f.w, "a", RemoveOptions{Confirm: yes}, func(e Event) {
		if strings.HasPrefix(e.Message, "deleting ") {
			deleted = true
			cancel()
		}
	})
	if !deleted {
		t.Fatalf("directory not deleted (err = %v)", err)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("directory still there")
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatalf("entry lingers in sheaf.yaml after a cancelled update (err = %v)", err)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
