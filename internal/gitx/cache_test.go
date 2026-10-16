package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// cachedCtx returns a context with a query cache and a call counter.
func cachedCtx() (context.Context, *CallCounter) {
	ctx, c := WithCallCounter(context.Background())
	return WithQueryCache(ctx), c
}

func mustTopLevel(t *testing.T, ctx context.Context, dir string) string {
	t.Helper()
	top, err := TopLevel(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return top
}

func mustWorktrees(t *testing.T, ctx context.Context, dir string) []string {
	t.Helper()
	wts, err := WorktreeList(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, w := range wts {
		out = append(out, filepath.Base(w.Path)+"="+w.Branch)
	}
	return out
}

func mustRef(t *testing.T, ctx context.Context, dir, ref string) bool {
	t.Helper()
	ok, err := refExists(ctx, dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestQueryCacheAnswersRepeatedQueriesOnce(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	ctx, calls := cachedCtx()
	for range 3 {
		if got, want := mustTopLevel(t, ctx, dir), mustTopLevel(t, context.Background(), dir); got != want {
			t.Fatalf("TopLevel = %q, want %q", got, want)
		}
		cd, err := CommonDir(ctx, dir)
		if err != nil || cd != filepath.Join(mustTopLevel(t, context.Background(), dir), ".git") {
			t.Fatalf("CommonDir = %q, %v", cd, err)
		}
		if got := mustWorktrees(t, ctx, dir); !slices.Equal(got, []string{"clone=main"}) {
			t.Fatalf("worktrees = %v", got)
		}
		if !mustRef(t, ctx, dir, "refs/heads/main") || mustRef(t, ctx, dir, "refs/heads/nope") {
			t.Fatal("wrong ref existence")
		}
	}
	if n := calls.Count(); n != 5 {
		t.Fatalf("git calls = %d, want 5 (one per distinct question): %v", n, calls.Calls())
	}
}

func TestQueryCacheIsScopedToItsContext(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	base, calls := WithCallCounter(context.Background())
	mustTopLevel(t, base, dir)
	mustTopLevel(t, base, dir)
	if n := calls.Count(); n != 2 {
		t.Fatalf("without a cache: %d calls, want 2", n)
	}
	calls.Reset()
	a, b := WithQueryCache(base), WithQueryCache(base)
	mustTopLevel(t, a, dir)
	mustTopLevel(t, b, dir)
	if n := calls.Count(); n != 2 {
		t.Fatalf("two caches: %d calls, want 2 (nothing shared)", n)
	}
	calls.Reset()
	mustTopLevel(t, WithQueryCache(a), dir) // nested: reuses a's cache
	if n := calls.Count(); n != 0 {
		t.Fatalf("nested WithQueryCache: %d calls, want 0", n)
	}
}

func TestQueryCacheMutationsInvalidate(t *testing.T) {
	t.Parallel()
	t.Run("worktree add then list", func(t *testing.T) {
		t.Parallel()
		_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		ctx, _ := cachedCtx()
		mustWorktrees(t, ctx, dir)
		if _, err := WorktreeAdd(ctx, dir, filepath.Join(filepath.Dir(dir), "feat"), "feat", WorktreeAddOpts{}); err != nil {
			t.Fatal(err)
		}
		if got := mustWorktrees(t, ctx, dir); !slices.Equal(got, []string{"clone=main", "feat=feat"}) {
			t.Fatalf("worktrees after add = %v", got)
		}
		if err := WorktreeRemove(ctx, dir, filepath.Join(filepath.Dir(dir), "feat"), false); err != nil {
			t.Fatal(err)
		}
		if got := mustWorktrees(t, ctx, dir); !slices.Equal(got, []string{"clone=main"}) {
			t.Fatalf("worktrees after remove = %v", got)
		}
	})
	t.Run("fetch then ref check", func(t *testing.T) {
		t.Parallel()
		o, dir := cloneOrigin(t, testutil.OriginOpts{})
		ctx, _ := cachedCtx()
		if mustRef(t, ctx, dir, "refs/remotes/origin/late") {
			t.Fatal("origin/late exists before the fetch")
		}
		testutil.Git(t, o.Path, "branch", "late", "main")
		if err := (Net{}).FetchRemote(ctx, dir, "origin"); err != nil {
			t.Fatal(err)
		}
		if !mustRef(t, ctx, dir, "refs/remotes/origin/late") {
			t.Fatal("origin/late not seen after the fetch")
		}
	})
	t.Run("branch create and delete then show-ref", func(t *testing.T) {
		t.Parallel()
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		ctx, _ := cachedCtx()
		if mustRef(t, ctx, dir, "refs/heads/x") {
			t.Fatal("x exists")
		}
		if _, err := Run(ctx, dir, "branch", "x"); err != nil {
			t.Fatal(err)
		}
		if !mustRef(t, ctx, dir, "refs/heads/x") {
			t.Fatal("x not seen after branch")
		}
		if _, err := Run(ctx, dir, "branch", "-q", "-D", "x"); err != nil {
			t.Fatal(err)
		}
		if mustRef(t, ctx, dir, "refs/heads/x") {
			t.Fatal("x still seen after branch -D")
		}
	})
	t.Run("mutation in a linked worktree invalidates the repository", func(t *testing.T) {
		t.Parallel()
		_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
		wt := filepath.Join(filepath.Dir(dir), "feat")
		testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat")
		ctx, _ := cachedCtx()
		mustWorktrees(t, ctx, dir)
		if _, err := Run(ctx, wt, "switch", "-q", "-c", "other"); err != nil {
			t.Fatal(err)
		}
		if got := mustWorktrees(t, ctx, dir); !slices.Equal(got, []string{"clone=main", "feat=other"}) {
			t.Fatalf("worktrees = %v", got)
		}
	})
	t.Run("mutation under fresh reads still invalidates", func(t *testing.T) {
		t.Parallel()
		_, dir := cloneOrigin(t, testutil.OriginOpts{})
		ctx, _ := cachedCtx()
		mustRef(t, ctx, dir, "refs/heads/y")
		if _, err := Run(WithFreshReads(ctx), dir, "branch", "y"); err != nil {
			t.Fatal(err)
		}
		if !mustRef(t, ctx, dir, "refs/heads/y") {
			t.Fatal("y not seen after a branch made under fresh reads")
		}
	})
	t.Run("mutation outside any repository invalidates everything", func(t *testing.T) {
		t.Parallel()
		o, dir := cloneOrigin(t, testutil.OriginOpts{})
		ctx, calls := cachedCtx()
		mustTopLevel(t, ctx, dir)
		if err := (Net{}).Clone(ctx, o.URL, filepath.Join(t.TempDir(), "second"), ""); err != nil {
			t.Fatal(err)
		}
		calls.Reset()
		mustTopLevel(t, ctx, dir)
		if n := calls.Count(); n != 1 {
			t.Fatalf("calls after a clone = %d, want 1 (cache flushed)", n)
		}
	})
}

func TestFreshReadsBypassTheCache(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	ctx, calls := cachedCtx()
	mustWorktrees(t, ctx, dir)
	// A change the cache cannot know about (another process).
	testutil.Git(t, dir, "worktree", "add", "-q", filepath.Join(filepath.Dir(dir), "feat"), "feat")
	if got := mustWorktrees(t, ctx, dir); len(got) != 1 {
		t.Fatalf("cached listing = %v, want the cached single entry", got)
	}
	fresh := WithFreshReads(ctx)
	calls.Reset()
	for range 2 {
		if got := mustWorktrees(t, fresh, dir); len(got) != 2 {
			t.Fatalf("fresh listing = %v, want 2 entries", got)
		}
	}
	// A command started under fresh reads (e.g. under the bundle lock)
	// stays fresh.
	if got := mustWorktrees(t, WithQueryCache(fresh), dir); len(got) != 2 {
		t.Fatalf("listing under WithQueryCache(fresh) = %v", got)
	}
	if n := calls.Count(); n != 3 {
		t.Fatalf("fresh calls = %d, want 3 (none answered from the cache)", n)
	}
}

func TestQueryCacheIgnoresDirectoriesWithoutGitEntry(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, calls := cachedCtx()
	mustTopLevel(t, ctx, sub)
	mustTopLevel(t, ctx, sub)
	if n := calls.Count(); n != 2 {
		t.Fatalf("calls = %d, want 2 (a directory without .git is never cached)", n)
	}
	// A worktree deleted behind the cache's back is asked again.
	wt := filepath.Join(filepath.Dir(dir), "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat")
	mustTopLevel(t, ctx, wt)
	testutil.Git(t, dir, "worktree", "remove", wt)
	if top, err := TopLevel(ctx, wt); err == nil {
		t.Fatalf("TopLevel of a deleted worktree = %q, want an error", top)
	}
}

func TestQueryCacheDoesNotKeepCancelledResults(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	ctx, _ := cachedCtx()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := TopLevel(cctx, dir); err == nil {
		t.Fatal("TopLevel under a cancelled context succeeded")
	}
	mustTopLevel(t, ctx, dir)
}

// Read-your-writes per goroutine while other goroutines query and mutate the
// same repositories through the same cache (run with -race).
func TestQueryCacheConcurrentUse(t *testing.T) {
	t.Parallel()
	var dirs []string
	for range 2 {
		_, d := cloneOrigin(t, testutil.OriginOpts{})
		dirs = append(dirs, d)
	}
	ctx, _ := cachedCtx()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := dirs[g%len(dirs)]
			ref := fmt.Sprintf("refs/heads/g%d", g)
			for i := range 3 {
				if _, err := TopLevel(ctx, dir); err != nil {
					errs <- err
					return
				}
				if _, err := WorktreeList(ctx, dir); err != nil {
					errs <- err
					return
				}
				if _, err := Run(ctx, dir, "update-ref", ref, "HEAD"); err != nil {
					errs <- err
					return
				}
				if ok, err := refExists(ctx, dir, ref); err != nil || !ok {
					errs <- fmt.Errorf("goroutine %d round %d: %s after update-ref: %v %v", g, i, ref, ok, err)
					return
				}
				if _, err := Run(ctx, dir, "update-ref", "-d", ref); err != nil {
					errs <- err
					return
				}
				if ok, err := refExists(ctx, dir, ref); err != nil || ok {
					errs <- fmt.Errorf("goroutine %d round %d: %s after update-ref -d: %v %v", g, i, ref, ok, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClassifyGitCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want callKind
	}{
		{[]string{"rev-parse", "--show-toplevel"}, callCacheable},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, callCacheable},
		{[]string{"rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir"}, callCacheable},
		{[]string{"worktree", "list", "--porcelain", "-z"}, callCacheable},
		{[]string{"show-ref", "--verify", "--quiet", "refs/heads/x"}, callCacheable},
		{[]string{"rev-parse", "--verify", "HEAD"}, callReadOnly},
		{[]string{"rev-list", "--count", "HEAD"}, callReadOnly},
		{[]string{"for-each-ref", "--format=%(refname)", "refs/heads"}, callReadOnly},
		{[]string{"ls-files", "-s", "-z"}, callReadOnly},
		{[]string{"merge-base", "--is-ancestor", "a", "b"}, callReadOnly},
		{[]string{"check-ref-format", "--branch", "x"}, callReadOnly},
		{[]string{"hash-object", "--stdin-paths"}, callReadOnly},
		{[]string{"--no-optional-locks", "status", "--porcelain=v2"}, callReadOnly},
		{[]string{"symbolic-ref", "--quiet", "HEAD"}, callReadOnly},
		{[]string{"config", "--get", "remote.origin.url"}, callReadOnly},
		{[]string{"config", "-z", "--get-regexp", "^branch"}, callReadOnly},
		{[]string{"remote"}, callReadOnly},
		{[]string{"remote", "get-url", "--", "origin"}, callReadOnly},
		{[]string{"ls-remote", "--symref", "--", "origin", "HEAD"}, callReadOnly},
		{[]string{"version"}, callReadOnly},
		{[]string{"worktree", "list"}, callReadOnly},
		// Everything else changes something, or is not known not to.
		{[]string{"status", "--porcelain=v2"}, callMutating}, // may refresh the index
		{[]string{"hash-object", "-w", "f"}, callMutating},
		{[]string{"symbolic-ref", "HEAD", "refs/heads/x"}, callMutating},
		{[]string{"symbolic-ref", "--delete", "HEAD"}, callMutating},
		{[]string{"config", "branch.x.merge", "refs/heads/x"}, callMutating},
		{[]string{"config", "--remove-section", "branch.x"}, callMutating},
		{[]string{"config", "--get", "x", "--unset", "y"}, callMutating},
		{[]string{"remote", "add", "--", "origin", "u"}, callMutating},
		{[]string{"-c", "fetch.output=full", "fetch", "--tags", "origin"}, callMutating},
		{[]string{"fetch", "--quiet", "--prune", "origin"}, callMutating},
		{[]string{"worktree", "add", "--quiet", "--", "p", "b"}, callMutating},
		{[]string{"worktree", "remove", "--", "p"}, callMutating},
		{[]string{"worktree", "repair"}, callMutating},
		{[]string{"update-ref", "-d", "refs/heads/x"}, callMutating},
		{[]string{"merge", "--ff-only", "@{upstream}"}, callMutating},
		{[]string{"clone", "--", "u", "d"}, callMutating},
		{[]string{"branch", "-D", "x"}, callMutating},
		{[]string{"show-ref", "--verify", "--quiet", "a", "b"}, callReadOnly},
		{nil, callMutating},
	} {
		if got := classify(tc.args); got != tc.want {
			t.Errorf("classify(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// The answer to the combined rev-parse (top level, git dir, common dir)
// also answers TopLevel and CommonDir, byte for byte as git would.
func TestQueryCacheDerivesTopLevelAndCommonDirFromInspection(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	wt := filepath.Join(filepath.Dir(dir), "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat")
	for _, d := range []string{dir, wt} {
		ctx, calls := cachedCtx()
		if _, err := Run(ctx, d, inspectArgs...); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{topLevelArgs, commonDirArgs} {
			got, err := Run(ctx, d, args...)
			want, werr := Run(context.Background(), d, args...)
			if err != nil || werr != nil || got != want {
				t.Errorf("%s: %q: got %q (%v), git says %q (%v)", d, args, got, err, want, werr)
			}
		}
		if n := calls.Count(); n != 1 {
			t.Errorf("%s: %d calls, want 1 (the inspection only)", d, n)
		}
	}
}

// A cached answer does not hide a cancellation: the query fails as it would
// without a cache.
func TestQueryCacheHonoursCancellation(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	ctx, _ := cachedCtx()
	mustTopLevel(t, ctx, dir)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if top, err := TopLevel(cctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("TopLevel under a cancelled context = %q, %v; want context.Canceled", top, err)
	}
}

// A relative gitdir in a linked worktree's .git file is resolved like git
// does, against the physical directory: reached through a symbolic link
// whose lexical parent holds a decoy repository with an admin directory of
// the same name, the key is still the worktree's own repository, and a
// mutation there invalidates its entries.
func TestQueryCacheResolvesRelativeGitdirPhysically(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	wt := filepath.Join(filepath.Dir(dir), "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", wt, "feat")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../clone/.git/worktrees/feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Decoy: another repository at <L>/clone with an admin directory
	// worktrees/feat, and <L>/feat a symbolic link to the worktree.
	_, decoy := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	l := filepath.Dir(decoy)
	testutil.Git(t, decoy, "worktree", "add", "-q", filepath.Join(l, "feat-real"), "feat")
	if err := os.Rename(filepath.Join(decoy, ".git", "worktrees", "feat-real"), filepath.Join(decoy, ".git", "worktrees", "feat")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l, "feat")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(testutil.Git(t, link, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := repoKey(link); !ok || k != want {
		t.Errorf("repoKey(%s) = %q, %v; git's common dir is %q", link, k, ok, want)
	}
	ctx, _ := cachedCtx()
	mustWorktrees(t, ctx, dir)
	if _, err := Run(ctx, link, "switch", "-q", "-c", "other"); err != nil {
		t.Fatal(err)
	}
	if got, fresh := mustWorktrees(t, ctx, dir), mustWorktrees(t, WithFreshReads(ctx), dir); !slices.Equal(got, fresh) {
		t.Errorf("stale: cached %v, fresh %v", got, fresh)
	}
}

// Only successes and expected negative answers (show-ref --verify --quiet
// exiting 1) are kept; a fatal git failure (exit 128) is asked again.
func TestQueryCacheDoesNotKeepFatalFailures(t *testing.T) {
	t.Parallel()
	// An empty .git directory: not a repository (git fails with 128), but
	// the directory has a cache key.
	broken := t.TempDir()
	if err := os.Mkdir(filepath.Join(broken, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, calls := cachedCtx()
	for range 2 {
		_, err := TopLevel(ctx, broken)
		var ge *GitError
		if !errors.As(err, &ge) || ge.ExitCode != 128 {
			t.Fatalf("TopLevel of a broken repository: %v, want exit status 128", err)
		}
	}
	if n := calls.Count(); n != 2 {
		t.Fatalf("calls = %d, want 2 (a fatal failure is not cached)", n)
	}
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	calls.Reset()
	mustRef(t, ctx, dir, "refs/heads/nope")
	mustRef(t, ctx, dir, "refs/heads/nope")
	if n := calls.Count(); n != 1 {
		t.Fatalf("calls = %d, want 1 (a missing ref is an answer)", n)
	}
}
