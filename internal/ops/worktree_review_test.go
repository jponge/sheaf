package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/testutil"
)

// B1: a name that is the branch of one worktree and the directory of another
// is ambiguous; --dir/--branch select explicitly.
func TestWorktreeTargetAmbiguity(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	dirA := filepath.Join(f.w.Root, "a")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "y", filepath.Join(dirA, "x"))
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "z", filepath.Join(dirA, "y"))

	_, err := f.rmWT(t, "a", "y", WorktreeRemoveOptions{DeleteBranch: true})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "x [y]") ||
		!strings.Contains(err.Error(), "y [z]") || !strings.Contains(err.Error(), "--dir") || !strings.Contains(err.Error(), "--branch") {
		t.Fatalf("err = %v, want an ambiguity error naming both and the selectors", err)
	}
	if !exists(filepath.Join(dirA, "x")) || !exists(filepath.Join(dirA, "y")) || !branchExists(t, primary, "y") {
		t.Fatal("something was removed")
	}
	if p, err := ResolvePath(context.Background(), f.w, PathQuery{Args: []string{"a", "y"}, Sel: nil, Cwd: f.w.Root}); err == nil {
		t.Fatalf("path a y = %q, want an ambiguity error", p)
	}
	if p, err := ResolvePath(context.Background(), f.w, PathQuery{Args: []string{"a"}, Sel: &WorktreeTarget{Name: "y", Kind: TargetDir}, Cwd: f.w.Root}); err != nil || p != filepath.Join(dirA, "y") {
		t.Fatalf("path --dir y = %q, %v", p, err)
	}
	if p, err := ResolvePath(context.Background(), f.w, PathQuery{Args: []string{"a"}, Sel: &WorktreeTarget{Name: "y", Kind: TargetBranch}, Cwd: f.w.Root}); err != nil || p != filepath.Join(dirA, "x") {
		t.Fatalf("path --branch y = %q, %v", p, err)
	}

	msg, err := f.rmWTT(t, "a", WorktreeTarget{Name: "y", Kind: TargetDir}, WorktreeRemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dirA, "y")) || !exists(filepath.Join(dirA, "x")) || !strings.Contains(msg, "y [z]") {
		t.Fatalf("--dir y removed the wrong worktree or msg %q lacks dir and branch", msg)
	}
	f.mustAddWT(t, "a", "z") // dir z, branch z
	testutil.Git(t, primary, "worktree", "add", "-q", filepath.Join(dirA, "y"), "-b", "w")
	if _, err := f.rmWTT(t, "a", WorktreeTarget{Name: "y", Kind: TargetBranch}, WorktreeRemoveOptions{DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dirA, "x")) || !exists(filepath.Join(dirA, "y")) || branchExists(t, primary, "y") {
		t.Fatal("--branch y removed the wrong worktree")
	}
}

// B2: a branch whose upstream is itself is not "merged into its upstream".
func TestDeleteBranchIgnoresSelfUpstream(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "work").Path
	testutil.CommitFile(t, p, "w.txt", "w\n", "unpushed")
	testutil.Git(t, p, "config", "branch.work.remote", ".")
	testutil.Git(t, p, "config", "branch.work.merge", "refs/heads/work")
	var re *WorktreeRefusedError
	_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("err = %v, want a refusal (unmerged)", err)
	}
	if !branchExists(t, primary, "work") || !exists(p) {
		t.Fatal("branch or worktree removed")
	}
}

// S1: git requires --force for a worktree with an initialized submodule;
// a clean, pushed submodule is vetted by sheaf, so no --force is needed.
func TestRemoveWorktreeWithCleanSubmodule(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	sub := testutil.NewOrigin(t, testutil.OriginOpts{})
	p := f.mustAddWT(t, "a", "withsub").Path
	testutil.Git(t, p, "submodule", "add", "-q", sub.URL, "sub")
	testutil.Git(t, p, "commit", "-q", "-m", "add submodule")
	if _, err := f.rmWT(t, "a", "withsub", WorktreeRemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(p) {
		t.Fatal("worktree still exists")
	}
}

// S3: two concurrent adds whose directory names collide case-insensitively
// cannot both succeed.
func TestAddWorktreeConcurrentCollision(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	var arrived sync.WaitGroup
	arrived.Add(2)
	testHookAfterAddCheck = func() {
		arrived.Done()
		// Without serialization both adds get here; wait (bounded) for the
		// other one to make the race deterministic.
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Cleanup(func() { testHookAfterAddCheck = nil })
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, b := range []string{"Br", "br"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = AddWorktree(context.Background(), f.w, "a", b, WorktreeAddOptions{NoFetch: true}, func(Event) {})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("errors = %v, want exactly one success", errs)
	}
}

func TestRemoveWorktreeChangedDuringConfirm(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	_, err := f.rmWT(t, "a", "x", WorktreeRemoveOptions{Confirm: func(WorktreeRemovePlan) bool {
		writeFile(t, filepath.Join(p, "late.txt"), "late\n")
		return true
	}})
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") || !exists(filepath.Join(p, "late.txt")) {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveWorktreeKeepsBranchThatMoved(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	testHookAfterWorktreeRemove = func() {
		testutil.Git(t, primary, "update-ref", "refs/heads/x", "origin/feat")
	}
	t.Cleanup(func() { testHookAfterWorktreeRemove = nil })
	var evs []Event
	msg, err := RemoveWorktree(context.Background(), f.w, "a", WorktreeTarget{Name: "x"}, WorktreeRemoveOptions{
		DeleteBranch: true, WorkingDir: f.w.Root, Confirm: func(WorktreeRemovePlan) bool { return true },
	}, func(e Event) { evs = append(evs, e) })
	if err != nil {
		t.Fatal(err)
	}
	if exists(p) || !branchExists(t, primary, "x") {
		t.Fatal("worktree not removed or moved branch deleted")
	}
	if !strings.Contains(msg, "kept branch x") || !strings.Contains(messages(evs), "warning:") {
		t.Fatalf("msg %q, events:\n%s", msg, messages(evs))
	}
}

func TestRemoveWorktreeKeepsBranchCheckedOutElsewhere(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	other := filepath.Join(t.TempDir(), "other")
	testHookAfterWorktreeRemove = func() {
		testutil.Git(t, primary, "worktree", "add", "-q", other, "x")
	}
	t.Cleanup(func() { testHookAfterWorktreeRemove = nil })
	msg, err := f.rmWT(t, "a", "x", WorktreeRemoveOptions{DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if exists(p) || !branchExists(t, primary, "x") || !strings.Contains(msg, "kept branch x") {
		t.Fatalf("msg %q; branch exists %v", msg, branchExists(t, primary, "x"))
	}
}

func TestRemoveWorktreePrunableRefused(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "gone").Path
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	var re *WorktreeRefusedError
	_, err := f.rmWT(t, "a", "gone", WorktreeRemoveOptions{Force: true})
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "git worktree prune") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(testutil.Git(t, primary, "worktree", "list", "--porcelain"), "gone") {
		t.Fatal("prunable worktree was pruned")
	}
}

func TestResolveRepoSymlinkedCwd(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	links := t.TempDir()
	inRepo := filepath.Join(links, "in-a")
	toRoot := filepath.Join(links, "root")
	if err := os.Symlink(primary, inRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.w.Root, toRoot); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{inRepo, filepath.Join(toRoot, "a", "main")} {
		got, err := ResolveRepo(f.w, "", cwd)
		if err != nil || got != (RepoContext{Name: "a", Worktree: "main"}) {
			t.Errorf("ResolveRepo(%s) = %+v, %v", cwd, got, err)
		}
	}
}

func TestParseRepoArgs(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	root := f.w.Root
	dir := &WorktreeTarget{Name: "d", Kind: TargetDir}
	for _, tc := range []struct {
		args       []string
		sel        *WorktreeTarget
		cwd        string
		repo       string
		target     *WorktreeTarget
		wantErrSub string
	}{
		{[]string{"b", "x"}, nil, primary, "b", &WorktreeTarget{Name: "x"}, ""},
		{[]string{"x"}, nil, primary, "a", &WorktreeTarget{Name: "x"}, ""},
		{[]string{"b"}, nil, root, "b", nil, ""},
		{[]string{"x"}, nil, root, "", nil, "a, b"},
		{nil, nil, primary, "a", nil, ""},
		{nil, nil, root, "", nil, "a, b"},
		{nil, dir, primary, "a", dir, ""},
		{[]string{"b"}, dir, primary, "b", dir, ""},
		{[]string{"b", "x"}, dir, primary, "", nil, "at most"},
		{[]string{"a", "b", "c"}, nil, root, "", nil, "at most"},
	} {
		repo, target, err := ParseRepoArgs(f.w, tc.args, tc.sel, tc.cwd)
		if tc.wantErrSub != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("ParseRepoArgs(%q, %+v, %s): err = %v, want %q", tc.args, tc.sel, tc.cwd, err, tc.wantErrSub)
			}
			continue
		}
		if err != nil || repo != tc.repo || (target == nil) != (tc.target == nil) || target != nil && *target != *tc.target {
			t.Errorf("ParseRepoArgs(%q, %+v, %s) = %q, %+v, %v; want %q, %+v", tc.args, tc.sel, tc.cwd, repo, target, err, tc.repo, tc.target)
		}
	}
}

func TestPrimaryOffDefaultBranch(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "switch", "-q", "-c", "dev")
	_, _, err := f.addWT(t, "a", "main", WorktreeAddOptions{NoFetch: true})
	if err == nil || !strings.Contains(err.Error(), "default branch") || !strings.Contains(err.Error(), "on dev") {
		t.Fatalf("err = %v", err)
	}
	all, _, err := ListWorktrees(context.Background(), f.w, "a")
	if err != nil || len(all) != 1 || !all[0].OffDefault {
		t.Fatalf("list = %+v, %v; want the primary flagged off-default", all, err)
	}
}

// path with one argument inside a checkout of a: a worktree of a first,
// else a repository; both is ambiguous. --repo selects explicitly.
func TestResolvePathSingleArgumentInsideCheckout(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	f.clonedRepo(t, "c")
	root := f.w.Root
	wb := f.mustAddWT(t, "a", "b").Path // worktree "b" of a, and repository b
	wx := f.mustAddWT(t, "a", "x").Path
	ctx := context.Background()
	for _, tc := range []struct {
		q    PathQuery
		want string
	}{
		{PathQuery{Args: []string{"x"}, Cwd: primary}, wx},
		{PathQuery{Args: []string{"c"}, Cwd: primary}, filepath.Join(root, "c", "main")},
		{PathQuery{Repo: "b", Cwd: primary}, filepath.Join(root, "b", "main")},
		{PathQuery{Repo: "a", Args: []string{"b"}, Cwd: root}, wb},
		{PathQuery{Repo: "a", Sel: &WorktreeTarget{Name: "x", Kind: TargetDir}, Cwd: root}, wx},
		{PathQuery{Args: []string{"a"}, Sel: &WorktreeTarget{Name: "x", Kind: TargetDir}, Cwd: root}, wx},
		{PathQuery{Sel: &WorktreeTarget{Name: "b", Kind: TargetDir}, Cwd: primary}, wb},
	} {
		got, err := ResolvePath(ctx, f.w, tc.q)
		if err != nil || got != tc.want {
			t.Errorf("ResolvePath(%+v) = %q, %v; want %q", tc.q, got, err, tc.want)
		}
	}
	_, err := ResolvePath(ctx, f.w, PathQuery{Args: []string{"b"}, Cwd: primary})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "b [b]") ||
		!strings.Contains(err.Error(), "repository b") || !strings.Contains(err.Error(), "--repo b") || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("err = %v, want an ambiguity error naming the worktree and the repository", err)
	}
	for _, q := range []PathQuery{
		{Args: []string{"nope"}, Cwd: primary},
		{Repo: "a", Args: []string{"x", "y"}, Cwd: root},
		{Repo: "a", Args: []string{"x"}, Sel: &WorktreeTarget{Name: "x", Kind: TargetDir}, Cwd: root},
		{Repo: "nope", Cwd: root},
	} {
		if got, err := ResolvePath(ctx, f.w, q); err == nil {
			t.Errorf("ResolvePath(%+v) = %q, want an error", q, got)
		}
	}
}

// -d deletes a branch none of whose commits would be lost, even if it is
// not merged into its upstream or the default branch.
func TestDeleteBranchContainedElsewhere(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "work").Path
	testutil.CommitFile(t, p, "w.txt", "w\n", "work")
	testutil.Git(t, primary, "branch", "keep", "work")
	msg, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "work") || !strings.Contains(msg, "(contained in keep)") {
		t.Fatalf("msg %q; branch exists %v", msg, branchExists(t, primary, "work"))
	}
}

// Commits reachable from tags are not lost; they are reported separately.
func TestDeleteBranchLostCommitsExcludeTags(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "work").Path
	testutil.CommitFile(t, p, "t.txt", "t\n", "tagged")
	testutil.Git(t, p, "tag", "v1")
	testutil.CommitFile(t, p, "u.txt", "u\n", "untagged")
	var re *WorktreeRefusedError
	_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true})
	if !errors.As(err, &re) || re.Plan.UnmergedCommits != 1 || re.Plan.TagOnlyCommits != 1 ||
		!strings.Contains(err.Error(), "its 1 commit would be lost") || !strings.Contains(err.Error(), "1 more reachable only from tags") {
		t.Fatalf("err = %v", err)
	}
	// Only tag-reachable commits: nothing is lost. (Delete work first: it
	// contains the tagged commit too.)
	if _, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{ForceDeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	q := f.mustAddWT(t, "a", "tagonly").Path
	testutil.Git(t, q, "reset", "-q", "--hard", "v1")
	msg, err := f.rmWT(t, "a", "tagonly", WorktreeRemoveOptions{DeleteBranch: true})
	if err != nil || branchExists(t, primary, "tagonly") || !strings.Contains(msg, "(contained in v1)") {
		t.Fatalf("tag only: msg %q, err %v", msg, err)
	}
}

// A branch checked out in another worktree (git worktree add -f) is
// reported in the plan and kept.
func TestDeleteBranchCheckedOutElsewhereReportedInPlan(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	other := filepath.Join(resolved(t, t.TempDir()), "other")
	testutil.Git(t, primary, "worktree", "add", "-q", "-f", other, "x")
	var plan WorktreeRemovePlan
	msg, err := f.rmWTT(t, "a", WorktreeTarget{Name: "x", Kind: TargetDir}, WorktreeRemoveOptions{DeleteBranch: true,
		Confirm: func(pl WorktreeRemovePlan) bool { plan = pl; return true }})
	if err != nil {
		t.Fatal(err)
	}
	if want := "branch x is checked out at " + other + " and will be kept"; plan.BranchKept != want {
		t.Fatalf("plan.BranchKept = %q, want %q", plan.BranchKept, want)
	}
	if exists(p) || !branchExists(t, primary, "x") || !strings.Contains(msg, "kept branch x") {
		t.Fatalf("msg %q", msg)
	}
}
