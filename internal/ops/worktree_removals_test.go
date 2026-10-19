package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// removalsEnv is the fixture of the RemoveWorktrees tests: repositories a
// and b, b with the clean worktree comp (the companion of every removal,
// see checkSameVerdict).
type removalsEnv struct {
	f          *fixture
	primary    string // of a
	origin     testutil.Origin
	comp, root string
}

func newRemovalsEnv(t *testing.T) removalsEnv {
	t.Helper()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	comp := f.mustAddWT(t, "b", "comp").Path
	return removalsEnv{f: f, primary: primary, origin: o, comp: comp, root: f.w.Root}
}

// checkSameVerdict checks that RemoveWorktrees decides on item exactly as
// RemoveWorktree does alone: the removal of item alone is run first
// (declined at its confirmation, so that nothing changes), then item is
// removed with RemoveWorktrees together with comp, a clean worktree,
// confirming. A removal planned alone must be planned identically (the
// same plan) and done; one refused or failing alone must fail with the
// same error (and the same plan for a refusal). comp must be removed in
// every case. It returns the result of item.
func checkSameVerdict(t *testing.T, w *Workspace, item, comp WorktreeRemoval, compPath string, opts WorktreeRemoveOptions) WorktreeRemovalResult {
	t.Helper()
	if opts.WorkingDir == "" {
		opts.WorkingDir = w.Root
	}
	ctx := context.Background()
	var alone *WorktreeRemovePlan
	o := opts
	o.Confirm = func(p WorktreeRemovePlan) bool { alone = &p; return false }
	_, aloneErr := RemoveWorktree(ctx, w, item.Repo, item.Target, o, noEmit)
	asked := 0
	res := RemoveWorktrees(ctx, w, []WorktreeRemoval{item, comp}, opts, func([]WorktreeRemovalResult) bool { asked++; return true }, ReporterFunc(noEmit))
	if len(res) != 2 {
		t.Fatalf("%d results, want 2", len(res))
	}
	got := res[0]
	if alone != nil {
		if !errors.Is(aloneErr, ErrNotConfirmed) {
			t.Fatalf("alone: planned, then err = %v", aloneErr)
		}
		if got.Err != nil {
			t.Fatalf("removable alone, but together: %v", got.Err)
		}
		if got.Plan == nil || !reflect.DeepEqual(*got.Plan, *alone) {
			t.Fatalf("plan together:\n%+v\nalone:\n%+v", got.Plan, *alone)
		}
	} else {
		if got.Err == nil {
			t.Fatalf("alone: %v; together: removed (%s)", aloneErr, got.Message)
		}
		if got.Err.Error() != aloneErr.Error() {
			t.Fatalf("together: %v\nalone: %v", got.Err, aloneErr)
		}
		var ra, rg *WorktreeRefusedError
		if errors.As(aloneErr, &ra) != errors.As(got.Err, &rg) || ra != nil && !reflect.DeepEqual(ra.Plan, rg.Plan) {
			t.Fatalf("refusal together: %#v\nalone: %#v", rg, ra)
		}
	}
	if res[1].Err != nil || exists(compPath) {
		t.Fatalf("companion: err = %v, exists %v", res[1].Err, exists(compPath))
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want 1", asked)
	}
	return got
}

func target(repo, dir string) WorktreeRemoval {
	return WorktreeRemoval{Repo: repo, Target: WorktreeTarget{Name: dir, Kind: TargetDir}}
}

// The verdicts of RemoveWorktrees are those of RemoveWorktree, worktree by
// worktree: every refusal and data risk of a removal alone, with and
// without --force, -d and -D.
func TestRemoveWorktreesSameVerdictsAsAlone(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name string
		// setup prepares the worktree of a to remove and returns its
		// directory name and path.
		setup   func(t *testing.T, e removalsEnv) (dir, path string)
		opts    WorktreeRemoveOptions
		refused bool   // expected verdict
		want    string // in the error (refused) or the message
		// branch, if set, is the branch of a that must exist afterwards
		// (keep) or not (!keep).
		branch string
		keep   bool
	}
	add := func(dir string) func(t *testing.T, e removalsEnv) (string, string) {
		return func(t *testing.T, e removalsEnv) (string, string) { return dir, e.f.mustAddWT(t, "a", dir).Path }
	}
	with := func(dir string, change func(t *testing.T, e removalsEnv, p string)) func(t *testing.T, e removalsEnv) (string, string) {
		return func(t *testing.T, e removalsEnv) (string, string) {
			p := e.f.mustAddWT(t, "a", dir).Path
			change(t, e, p)
			return dir, p
		}
	}
	untracked := func(t *testing.T, _ removalsEnv, p string) { writeFile(t, filepath.Join(p, "u.txt"), "u\n") }
	ahead := func(t *testing.T, _ removalsEnv, p string) { testutil.CommitFile(t, p, "w.txt", "w\n", "local work") }
	for _, sc := range []scenario{
		{name: "clean", setup: add("x"), want: "removed worktree", branch: "x", keep: true},
		{name: "offline", setup: add("x"), opts: WorktreeRemoveOptions{Offline: true}, want: "removed worktree"},
		{name: "untracked", setup: with("x", untracked), refused: true, want: "1 untracked file"},
		{name: "untracked forced", setup: with("x", untracked), opts: WorktreeRemoveOptions{Force: true}, want: "removed worktree"},
		{name: "staged", setup: with("x", func(t *testing.T, _ removalsEnv, p string) {
			writeFile(t, filepath.Join(p, "s.txt"), "s\n")
			testutil.Git(t, p, "add", "s.txt")
		}), refused: true, want: "staged change"},
		{name: "unstaged", setup: with("x", func(t *testing.T, e removalsEnv, p string) {
			ahead(t, e, p)
			writeFile(t, filepath.Join(p, "w.txt"), "changed\n")
		}), refused: true, want: "unstaged change"},
		{name: "detached commits", setup: func(t *testing.T, e removalsEnv) (string, string) {
			p := filepath.Join(e.root, "a", "det")
			testutil.Git(t, e.primary, "worktree", "add", "-q", "--detach", p)
			testutil.CommitFile(t, p, "d.txt", "d\n", "detached work")
			return "det", p
		}, refused: true, want: "commit only on detached HEAD"},
		{name: "stash", setup: with("x", func(t *testing.T, e removalsEnv, p string) {
			ahead(t, e, p)
			writeFile(t, filepath.Join(p, "w.txt"), "stashed\n")
			testutil.Git(t, p, "stash", "-q")
		}), want: "removed worktree", branch: "x", keep: true},
		{name: "unpushed branch", setup: with("x", ahead), want: "removed worktree", branch: "x", keep: true},
		{name: "locked", setup: with("x", func(t *testing.T, e removalsEnv, p string) {
			testutil.Git(t, e.primary, "worktree", "lock", p)
		}), opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "locked"},
		{name: "nested repository at risk", setup: with("x", func(t *testing.T, e removalsEnv, p string) {
			n := filepath.Join(p, "lib")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), n)
			writeFile(t, filepath.Join(n, "wip.txt"), "wip\n")
		}), refused: true, want: "nested repository"},
		{name: "stale nested repository", setup: with("x", func(t *testing.T, e removalsEnv, p string) { staleNestedClone(t, p) }),
			refused: true, want: "nested repository"},
		{name: "worktree of another repository", setup: with("x", func(t *testing.T, e removalsEnv, p string) {
			other := filepath.Join(t.TempDir(), "other")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}}), other)
			wt := filepath.Join(p, "scratch", "wt")
			testutil.Git(t, other, "worktree", "add", "-q", wt, "feat")
			writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
		}), opts: WorktreeRemoveOptions{Force: false}, refused: true, want: "nested repository"},
		{name: "fetch failure", setup: with("x", func(t *testing.T, e removalsEnv, _ string) {
			testutil.Git(t, e.primary, "remote", "set-url", "origin", "file:///nonexistent/origin.git")
		}), opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "--offline"},
		{name: "unreachable origin offline", setup: with("x", func(t *testing.T, e removalsEnv, _ string) {
			testutil.Git(t, e.primary, "remote", "set-url", "origin", "file:///nonexistent/origin.git")
		}), opts: WorktreeRemoveOptions{Offline: true}, want: "removed worktree"},
		{name: "primary", setup: func(t *testing.T, e removalsEnv) (string, string) { return "main", e.primary },
			opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "sheaf remove"},
		{name: "outside", setup: func(t *testing.T, e removalsEnv) (string, string) {
			p := filepath.Join(t.TempDir(), "outside")
			testutil.Git(t, e.primary, "worktree", "add", "-q", "-b", "outside", p)
			return "outside", p
		}, opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "outside"},
		{name: "symbolic link", setup: func(t *testing.T, e removalsEnv) (string, string) {
			real := filepath.Join(t.TempDir(), "real")
			testutil.Git(t, e.primary, "worktree", "add", "-q", "-b", "linked", real)
			link := filepath.Join(e.root, "a", "linked")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			testutil.Git(t, e.primary, "worktree", "repair", link)
			return "linked", real
		}, opts: WorktreeRemoveOptions{Force: true}, refused: true},
		{name: "missing directory", setup: with("x", func(t *testing.T, _ removalsEnv, p string) {
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}), opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "git worktree prune"},
		{name: "working directory inside", setup: add("x"), opts: WorktreeRemoveOptions{Force: true}, refused: true, want: "working directory"},
		{name: "tag", setup: func(t *testing.T, e removalsEnv) (string, string) {
			testutil.Git(t, e.primary, "tag", "v1")
			return "v1", e.f.mustAddTag(t, "a", "v1").Path
		}, want: "tag v1"},
		{name: "tag with detached commits", setup: func(t *testing.T, e removalsEnv) (string, string) {
			testutil.Git(t, e.primary, "tag", "v1")
			p := e.f.mustAddTag(t, "a", "v1").Path
			testutil.CommitFile(t, p, "x.txt", "x\n", "detached work")
			return "v1", p
		}, refused: true, want: "detached HEAD"},
		{name: "tag with -d", setup: func(t *testing.T, e removalsEnv) (string, string) {
			testutil.Git(t, e.primary, "tag", "v1")
			return "v1", e.f.mustAddTag(t, "a", "v1").Path
		}, opts: WorktreeRemoveOptions{DeleteBranch: true}, refused: true, want: "pinned at tag v1"},
		{name: "pull request", setup: func(t *testing.T, e removalsEnv) (string, string) {
			pushPullRef(t, e.origin, 7, "main", "fork.txt")
			e.f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "topic", HeadOwner: "someone", HeadRepo: "a", IsCrossRepository: true})
			res, err := e.f.addPRWorktree(t, "a", 7)
			if err != nil {
				t.Fatal(err)
			}
			return filepath.Base(res.Path), res.Path
		}, want: "removed worktree", branch: "pr/7", keep: true},
		{name: "-d merged", setup: add("feat"), opts: WorktreeRemoveOptions{DeleteBranch: true}, want: "deleted branch feat (merged into origin/feat)", branch: "feat"},
		{name: "-d unmerged", setup: with("x", ahead), opts: WorktreeRemoveOptions{DeleteBranch: true, Force: true}, refused: true,
			want: "branch x is not merged", branch: "x", keep: true},
		{name: "-D unmerged", setup: with("x", ahead), opts: WorktreeRemoveOptions{ForceDeleteBranch: true}, want: "deleted unmerged branch x (1 commit lost)", branch: "x"},
		{name: "-d detached", setup: func(t *testing.T, e removalsEnv) (string, string) {
			p := filepath.Join(e.root, "a", "det")
			testutil.Git(t, e.primary, "worktree", "add", "-q", "--detach", p)
			return "det", p
		}, opts: WorktreeRemoveOptions{DeleteBranch: true}, refused: true, want: "no branch to delete"},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			e := newRemovalsEnv(t)
			dir, path := sc.setup(t, e)
			opts := sc.opts
			if sc.name == "working directory inside" {
				opts.WorkingDir = path
			}
			got := checkSameVerdict(t, e.f.w, target("a", dir), target("b", "comp"), e.comp, opts)
			if (got.Err != nil) != sc.refused {
				t.Fatalf("err = %v, want refused %v", got.Err, sc.refused)
			}
			text := got.Message
			if got.Err != nil {
				text = got.Err.Error()
			}
			if !strings.Contains(text, sc.want) {
				t.Errorf("%q lacks %q", text, sc.want)
			}
			if sc.name != "missing directory" && exists(path) != sc.refused {
				t.Errorf("%s exists: %v, want %v", path, exists(path), sc.refused)
			}
			if sc.branch != "" && branchExists(t, e.primary, sc.branch) != sc.keep {
				t.Errorf("branch %s exists: %v, want %v", sc.branch, !sc.keep, sc.keep)
			}
		})
	}
}

// A forked repository whose fork is gone: same verdicts together as alone
// (the fork's stale refs are not trusted).
func TestRemoveWorktreesForkGoneSameVerdictsAsAlone(t *testing.T) {
	t.Parallel()
	for _, opts := range []WorktreeRemoveOptions{{DeleteBranch: true}, {}, {ForceDeleteBranch: true}} {
		f, primary := forkGoneTopic(t)
		comp, _, err := f.addWT(t, "x", "comp", WorktreeAddOptions{NoFetch: true})
		if err != nil {
			t.Fatal(err)
		}
		got := checkSameVerdict(t, f.w, target("x", "topic"), target("x", "comp"), comp.Path, opts)
		if opts.DeleteBranch && (got.Err == nil || !strings.Contains(got.Err.Error(), "your fork (origin) appears to be gone")) {
			t.Fatalf("-d: err = %v, want the unmerged refusal", got.Err)
		}
		if !opts.DeleteBranch && got.Err != nil {
			t.Fatalf("%+v: %v", opts, got.Err)
		}
		if branchExists(t, primary, "topic") == opts.ForceDeleteBranch {
			t.Fatalf("%+v: branch topic exists %v", opts, !opts.ForceDeleteBranch)
		}
	}
}

// fetchesByDir counts the fetches among calls by directory.
func fetchesByDir(calls []gitx.Call) map[string]int {
	n := map[string]int{}
	for _, c := range calls {
		for _, a := range c.Args {
			if a == "fetch" {
				n[c.Dir]++
				break
			}
			if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
				break
			}
		}
	}
	return n
}

// Several worktrees of several repositories: each repository is fetched
// once, one confirmation shows every plan and refusal, and the confirmed
// worktrees are removed, the refused one kept.
func TestRemoveWorktreesConfirmsOnceAndFetchesEachRepositoryOnce(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	bPrimary := filepath.Join(e.root, "b", "main")
	x := e.f.mustAddWT(t, "a", "x").Path
	y := e.f.mustAddWT(t, "a", "y").Path
	dirty := e.f.mustAddWT(t, "a", "dirty").Path
	writeFile(t, filepath.Join(dirty, "u.txt"), "u\n")
	z := e.f.mustAddWT(t, "b", "z").Path
	ctx, counter := gitx.WithCallCounter(context.Background())
	var asked [][]WorktreeRemovalResult
	items := []WorktreeRemoval{target("a", "x"), target("a", "dirty"), target("b", "comp"), target("a", "y"), target("b", "z")}
	res := RemoveWorktrees(ctx, e.f.w, items, WorktreeRemoveOptions{WorkingDir: e.root}, func(rs []WorktreeRemovalResult) bool {
		asked = append(asked, rs)
		return true
	}, ReporterFunc(noEmit))
	if len(asked) != 1 {
		t.Fatalf("asked %d times, want once", len(asked))
	}
	if len(asked[0]) != len(items) {
		t.Fatalf("confirmation shows %d worktrees, want %d", len(asked[0]), len(items))
	}
	for i, r := range asked[0] {
		if r.WorktreeRemoval != items[i] || r.Plan == nil {
			t.Fatalf("confirmation item %d: %+v, want %+v planned", i, r, items[i])
		}
		var refused *WorktreeRefusedError
		if (r.Err != nil) != (items[i].Target.Name == "dirty") || r.Err != nil && !errors.As(r.Err, &refused) {
			t.Fatalf("confirmation item %d: err = %v", i, r.Err)
		}
	}
	if n := fetchesByDir(counter.Calls()); n[e.primary] != 1 || n[bPrimary] != 1 || len(n) != 2 {
		t.Fatalf("fetches by directory: %v, want one in each primary", n)
	}
	for i, r := range res {
		if (r.Err != nil) != (items[i].Target.Name == "dirty") {
			t.Errorf("%v: err = %v", items[i], r.Err)
		}
	}
	for _, p := range []string{x, y, e.comp, z} {
		if exists(p) {
			t.Errorf("%s not removed", p)
		}
	}
	if !exists(filepath.Join(dirty, "u.txt")) {
		t.Fatal("refused worktree removed")
	}
}

// Declining removes nothing: the planned worktrees are not confirmed.
func TestRemoveWorktreesDeclinedRemovesNothing(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	x := e.f.mustAddWT(t, "a", "x").Path
	for _, confirm := range []func([]WorktreeRemovalResult) bool{nil, func([]WorktreeRemovalResult) bool { return false }} {
		res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "x"), target("b", "comp")}, WorktreeRemoveOptions{WorkingDir: e.root}, confirm, ReporterFunc(noEmit))
		for _, r := range res {
			if !errors.Is(r.Err, ErrNotConfirmed) || r.Plan == nil {
				t.Fatalf("%v: plan %v, err = %v; want planned, not confirmed", r.WorktreeRemoval, r.Plan, r.Err)
			}
		}
		if !exists(x) || !exists(e.comp) {
			t.Fatal("removed without confirmation")
		}
	}
}

// Nothing to confirm when every worktree is refused.
func TestRemoveWorktreesAllRefusedDoesNotAsk(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	writeFile(t, filepath.Join(e.comp, "u.txt"), "u\n")
	res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "main"), target("b", "comp"), target("a", "nope")}, WorktreeRemoveOptions{WorkingDir: e.root},
		func([]WorktreeRemovalResult) bool { t.Fatal("asked"); return true }, ReporterFunc(noEmit))
	for _, r := range res {
		if r.Err == nil {
			t.Fatalf("%v: removed", r.WorktreeRemoval)
		}
	}
	if !exists(e.comp) {
		t.Fatal("comp removed")
	}
}

// Each confirmed worktree is checked again under the lock, with fresh
// reads, before its removal: one changed while the confirmation was shown
// is kept, the others are removed.
func TestRemoveWorktreesRechecksEachUnderTheLock(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	x := e.f.mustAddWT(t, "a", "x").Path
	y := e.f.mustAddWT(t, "a", "y").Path
	res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "x"), target("a", "y"), target("b", "comp")}, WorktreeRemoveOptions{WorkingDir: e.root},
		func([]WorktreeRemovalResult) bool {
			writeFile(t, filepath.Join(y, "late.txt"), "late\n")
			return true
		}, ReporterFunc(noEmit))
	if res[1].Err == nil || !strings.Contains(res[1].Err.Error(), "changed while waiting") || !exists(filepath.Join(y, "late.txt")) {
		t.Fatalf("y: err = %v", res[1].Err)
	}
	if res[0].Err != nil || res[2].Err != nil || exists(x) || exists(e.comp) {
		t.Fatalf("x: %v, comp: %v; both must be removed", res[0].Err, res[2].Err)
	}
}

// A worktree directory replaced by another repository while the
// confirmation was shown is not removed, and left alone.
func TestRemoveWorktreesRechecksTheDirectory(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	x := e.f.mustAddWT(t, "a", "x").Path
	res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "x"), target("b", "comp")}, WorktreeRemoveOptions{WorkingDir: e.root},
		func([]WorktreeRemovalResult) bool {
			if err := os.RemoveAll(x); err != nil {
				t.Fatal(err)
			}
			plainClone(t, e.origin, x)
			return true
		}, ReporterFunc(noEmit))
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "cannot be used: "+x+": worktree of another repository (") || !exists(filepath.Join(x, ".git")) {
		t.Fatalf("x: err = %v, the other repository exists %v", res[0].Err, exists(filepath.Join(x, ".git")))
	}
	if res[1].Err != nil || exists(e.comp) {
		t.Fatalf("comp: %v", res[1].Err)
	}
}

// The re-check under the lock does not answer from the queries cached
// while planning: a branch checked out elsewhere while the confirmation
// was shown keeps its worktree (-d).
func TestRemoveWorktreesRecheckSeesBranchCheckedOutDuringConfirm(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	feat := e.f.mustAddWT(t, "a", "feat").Path
	other := filepath.Join(e.root, "a", "other")
	res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "feat"), target("b", "comp")}, WorktreeRemoveOptions{WorkingDir: e.root, DeleteBranch: true},
		func([]WorktreeRemovalResult) bool {
			testutil.Git(t, e.primary, "worktree", "add", "-q", "-f", other, "feat")
			return true
		}, ReporterFunc(noEmit))
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "changed while waiting") || !exists(feat) {
		t.Fatalf("feat: err = %v, exists %v", res[0].Err, exists(feat))
	}
	if res[1].Err != nil || exists(e.comp) {
		t.Fatalf("comp: %v", res[1].Err)
	}
}

// Nested repositories are fetched before being assessed: a commit whose
// branch was deleted on the nested repository's origin is unpushed.
func TestRemoveWorktreesFetchesNestedRepositories(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	x := e.f.mustAddWT(t, "a", "x").Path
	staleNestedClone(t, x)
	res := RemoveWorktrees(context.Background(), e.f.w, []WorktreeRemoval{target("a", "x"), target("b", "comp")}, WorktreeRemoveOptions{WorkingDir: e.root},
		func([]WorktreeRemovalResult) bool { return true }, ReporterFunc(noEmit))
	var refused *WorktreeRefusedError
	if !errors.As(res[0].Err, &refused) || !hasKind(refused.Plan.Blocking(), gitx.RiskNestedRepository) || !exists(x) {
		t.Fatalf("x: err = %v, exists %v", res[0].Err, exists(x))
	}
	if res[1].Err != nil || exists(e.comp) {
		t.Fatalf("comp: %v", res[1].Err)
	}
}

// With a branch deletion the plan names the local branches carrying the
// unpushed commits of the repository (B52 C2); without one it does not
// look for them.
func TestRemoveWorktreePlanNamesUnpushedBranchesWhenDeletingABranch(t *testing.T) {
	t.Parallel()
	e := newRemovalsEnv(t)
	other := e.f.mustAddWT(t, "a", "other").Path
	testutil.CommitFile(t, other, "o.txt", "o\n", "local work")
	e.f.mustAddWT(t, "a", "x")
	for _, deleteBr := range []bool{false, true} {
		var plan WorktreeRemovePlan
		if _, err := e.f.rmWT(t, "a", "x", WorktreeRemoveOptions{DeleteBranch: deleteBr, Confirm: func(p WorktreeRemovePlan) bool { plan = p; return false }}); !errors.Is(err, ErrNotConfirmed) {
			t.Fatal(err)
		}
		want := gitx.Risk{Kind: gitx.RiskUnpushedBranchCommits, Count: 1}
		if deleteBr {
			want.Detail = "other"
		}
		if !slices.Contains(plan.Risks, want) {
			t.Errorf("-d %v: risks %v, want %v", deleteBr, plan.Risks, want)
		}
	}
}
