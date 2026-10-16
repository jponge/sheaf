package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func (f *fixture) addWT(t *testing.T, repo, branch string, opts WorktreeAddOptions) (WorktreeAddResult, []Event, error) {
	t.Helper()
	var evs []Event
	res, err := AddWorktree(context.Background(), f.w, repo, branch, opts, func(e Event) { evs = append(evs, e) })
	return res, evs, err
}

func (f *fixture) mustAddWT(t *testing.T, repo, branch string) WorktreeAddResult {
	t.Helper()
	res, _, err := f.addWT(t, repo, branch, WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatalf("add worktree %s %s: %v", repo, branch, err)
	}
	return res
}

func (f *fixture) rmWT(t *testing.T, repo, target string, opts WorktreeRemoveOptions) (string, error) {
	t.Helper()
	return f.rmWTT(t, repo, WorktreeTarget{Name: target}, opts)
}

func (f *fixture) rmWTT(t *testing.T, repo string, target WorktreeTarget, opts WorktreeRemoveOptions) (string, error) {
	t.Helper()
	if opts.Confirm == nil {
		opts.Confirm = func(WorktreeRemovePlan) bool { return true }
	}
	if opts.WorkingDir == "" {
		opts.WorkingDir = f.w.Root
	}
	return RemoveWorktree(context.Background(), f.w, repo, target, opts, noEmit)
}

func branchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	_, err := gitx.Run(context.Background(), dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func messages(evs []Event) string {
	var sb strings.Builder
	for _, e := range evs {
		sb.WriteString(e.Message + "\n")
	}
	return sb.String()
}

func TestResolveRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "alpha")
	f.clonedRepo(t, "beta")
	root := f.w.Root
	sub := filepath.Join(root, "alpha", "main", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		arg, cwd string
		want     RepoContext
	}{
		{"alpha", root, RepoContext{Name: "alpha"}},
		{"", sub, RepoContext{Name: "alpha", Worktree: "main"}},
		{"", filepath.Join(root, "beta", "main"), RepoContext{Name: "beta", Worktree: "main"}},
		{"", filepath.Join(root, "beta"), RepoContext{Name: "beta"}},
		{"beta", sub, RepoContext{Name: "beta"}},
	} {
		got, err := ResolveRepo(f.w, tc.arg, tc.cwd)
		if err != nil || got != tc.want {
			t.Errorf("ResolveRepo(%q, %s) = %+v, %v; want %+v", tc.arg, tc.cwd, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ arg, cwd string }{
		{"gamma", root},
		{"BETA", root}, // repository names are case-sensitive
		{"", root},
		{"", t.TempDir()},
	} {
		_, err := ResolveRepo(f.w, tc.arg, tc.cwd)
		if err == nil || !strings.Contains(err.Error(), "alpha, beta") {
			t.Errorf("ResolveRepo(%q, %s): err = %v, want an error listing the repositories", tc.arg, tc.cwd, err)
		}
	}
}

func TestAddWorktreeModes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	root := f.w.Root

	res, evs, err := f.addWT(t, "a", "feat", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != gitx.WorktreeRemoteBranch || res.Path != filepath.Join(root, "a", "feat") || res.Upstream != "origin/feat" || res.Dir != "feat" {
		t.Fatalf("remote branch: %+v", res)
	}
	if !strings.Contains(messages(evs), "tracking origin/feat") {
		t.Errorf("events do not report the mode:\n%s", messages(evs))
	}

	res, evs, err = f.addWT(t, "a", "fix/cli/rework", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != gitx.WorktreeNewBranch || res.Path != filepath.Join(root, "a", "fix-cli-rework") || res.Base != "origin/main" || res.Upstream != "" {
		t.Fatalf("new branch: %+v", res)
	}
	if !strings.Contains(messages(evs), "new branch fix/cli/rework from origin/main") {
		t.Errorf("events do not report the mode:\n%s", messages(evs))
	}

	testutil.Git(t, primary, "branch", "loc")
	res, evs, err = f.addWT(t, "a", "loc", WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != gitx.WorktreeLocalBranch || len(res.Warnings) != 0 {
		t.Fatalf("local branch: %+v", res)
	}
	if !strings.Contains(messages(evs), "existing local branch loc") {
		t.Errorf("events do not report the mode:\n%s", messages(evs))
	}

	res, _, err = f.addWT(t, "a", "on-feat", WorktreeAddOptions{Base: "origin/feat", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "origin/feat" || testutil.Git(t, res.Path, "rev-parse", "HEAD") != testutil.Git(t, primary, "rev-parse", "origin/feat") {
		t.Fatalf("--base not honoured: %+v", res)
	}
}

func TestAddWorktreeFetchesFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	// A branch pushed to origin after the clone.
	dir := filepath.Join(t.TempDir(), "w")
	testutil.Git(t, filepath.Dir(dir), "clone", "-q", o.Path, dir)
	testutil.Git(t, dir, "push", "-q", "origin", "HEAD:refs/heads/fresh", "HEAD:refs/heads/fresh2")

	res, _, err := f.addWT(t, "a", "fresh2", WorktreeAddOptions{NoFetch: true})
	if err != nil || res.Mode != gitx.WorktreeNewBranch {
		t.Fatalf("--no-fetch must not see origin/fresh2: %+v, %v", res, err)
	}
	res, _, err = f.addWT(t, "a", "fresh", WorktreeAddOptions{})
	if err != nil || res.Mode != gitx.WorktreeRemoteBranch {
		t.Fatalf("fetch did not happen: %+v, %v", res, err)
	}

	testutil.Git(t, primary, "remote", "set-url", "origin", "file:///nonexistent/origin.git")
	_, _, err = f.addWT(t, "a", "other", WorktreeAddOptions{})
	if err == nil || !strings.Contains(err.Error(), "--no-fetch") {
		t.Fatalf("fetch failure: err = %v, want a hint to use --no-fetch", err)
	}
	if exists(filepath.Join(f.w.Root, "a", "other")) {
		t.Fatal("worktree created despite the failed fetch")
	}
	if _, _, err := f.addWT(t, "a", "other", WorktreeAddOptions{NoFetch: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAddWorktreeRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "remote", "add", "upstream", "file:///nonexistent")
	f.mustAddWT(t, "a", "feat/a-b")
	if err := os.Mkdir(filepath.Join(f.w.Root, "a", "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ branch, want string }{
		{"a..b", "not a valid branch name"},
		{"-x", "not a valid branch name"},
		{"@{-1}", "not a valid branch name"},
		{"feat-a/b", "both map to directory"},
		{"feat/a-b", "already checked out"},
		{"main", "default branch"},
		{"origin/feat", "is not allowed"},
		{"origin", "is not allowed"},
		{"upstream", "is not allowed"},
		{"upstream/x", "is not allowed"},
		{"refs/x", "is not allowed"},
		{"heads/x", "is not allowed"},
		{"tags/v1", "is not allowed"},
		{"remotes/x", "is not allowed"},
		{"Main", "both map to directory"},
		{"JUNK", "not a worktree"},
	} {
		_, _, err := f.addWT(t, "a", tc.branch, WorktreeAddOptions{NoFetch: true})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("add %q: err = %v, want %q", tc.branch, err, tc.want)
		}
	}
	if _, _, err := f.addWT(t, "nope", "x", WorktreeAddOptions{NoFetch: true}); err == nil {
		t.Error("unknown repository accepted")
	}
}

func TestAddWorktreeWarnsAboutOtherUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "branch", "--track", "mine", "origin/feat")
	res, evs, err := f.addWT(t, "a", "mine", WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "origin/feat") || !strings.Contains(messages(evs), "warning:") {
		t.Fatalf("warnings = %q, events:\n%s", res.Warnings, messages(evs))
	}
	testutil.Git(t, primary, "branch", "--no-track", "feat-local", "origin/main")
	testutil.Git(t, primary, "branch", "-m", "feat-local", "feat")
	res, _, err = f.addWT(t, "a", "feat", WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no upstream") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}

func TestListWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primaryA, _ := f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	root := f.w.Root
	f.mustAddWT(t, "a", "zeta")
	f.mustAddWT(t, "a", "feat/x")
	testutil.Git(t, primaryA, "worktree", "add", "-q", "--detach", filepath.Join(root, "a", "det"))
	testutil.Git(t, primaryA, "worktree", "lock", "--reason", "usb", filepath.Join(root, "a", "zeta"))
	f.mustAddWT(t, "a", "gone")
	if err := os.RemoveAll(filepath.Join(root, "a", "gone")); err != nil {
		t.Fatal(err)
	}

	all, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	var got []string
	for _, wt := range all {
		got = append(got, wt.Repo+"/"+wt.Dir)
	}
	want := "a/main a/det a/feat-x a/gone a/zeta b/main"
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	byKey := map[string]WorktreeInfo{}
	for _, wt := range all {
		byKey[wt.Repo+"/"+wt.Dir] = wt
	}
	if m := byKey["a/main"]; !m.Primary || m.Branch != "main" || m.Path != primaryA || m.Head == "" {
		t.Errorf("primary: %+v", m)
	}
	if d := byKey["a/det"]; !d.Detached || d.Branch != "" || d.Primary {
		t.Errorf("detached: %+v", d)
	}
	if z := byKey["a/zeta"]; !z.Locked || z.LockReason != "usb" || z.Branch != "zeta" {
		t.Errorf("locked: %+v", z)
	}
	if g := byKey["a/gone"]; !g.Prunable {
		t.Errorf("prunable: %+v", g)
	}
	if x := byKey["a/feat-x"]; x.Branch != "feat/x" || x.Path != filepath.Join(root, "a", "feat-x") {
		t.Errorf("feat/x: %+v", x)
	}

	if all[0].OffDefault {
		t.Errorf("primary on its default branch flagged off-default: %+v", all[0])
	}
	only, _, err := ListWorktrees(context.Background(), f.w, "b")
	if err != nil || len(only) != 1 || only[0].Repo != "b" {
		t.Fatalf("filtered: %+v, %v", only, err)
	}
	if _, _, err := ListWorktrees(context.Background(), f.w, "nope"); err == nil {
		t.Fatal("unknown repository accepted")
	}
}

func TestListWorktreesReportsBrokenRepoButListsOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	if err := os.RemoveAll(filepath.Join(f.w.Root, "a")); err != nil {
		t.Fatal(err)
	}
	all, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 1 || fails[0].Repo != "a" || fails[0].Err == nil {
		t.Fatalf("err = %v, failures = %v, want one failure for a", err, fails)
	}
	if len(all) != 1 || all[0].Repo != "b" {
		t.Fatalf("listing = %+v", all)
	}
}

func TestRemoveWorktreeClean(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	root := f.w.Root
	f.mustAddWT(t, "a", "feat/cli/x")
	f.mustAddWT(t, "a", "other")

	if _, err := f.rmWT(t, "a", "feat-cli-x", WorktreeRemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "a", "feat-cli-x")) || !branchExists(t, primary, "feat/cli/x") {
		t.Fatal("removal by dir name: dir still there or branch deleted")
	}
	msg, err := f.rmWT(t, "a", "other", WorktreeRemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "a", "other")) || !strings.Contains(msg, filepath.Join(root, "a", "other")) {
		t.Fatalf("removal by branch: msg %q", msg)
	}
	if out := testutil.Git(t, primary, "worktree", "list", "--porcelain"); strings.Contains(out, "other") {
		t.Fatalf("worktree still registered:\n%s", out)
	}
}

func TestRemoveWorktreeRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	root := f.w.Root
	f.mustAddWT(t, "a", "locked")
	testutil.Git(t, primary, "worktree", "lock", filepath.Join(root, "a", "locked"))
	f.mustAddWT(t, "a", "here")
	outside := filepath.Join(t.TempDir(), "outside")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "outside", outside)

	for _, target := range []string{"main", "MAIN"} {
		_, err := f.rmWT(t, "a", target, WorktreeRemoveOptions{Force: true})
		if err == nil || !strings.Contains(err.Error(), "sheaf remove") {
			t.Errorf("primary %q: err = %v", target, err)
		}
	}
	if _, err := f.rmWT(t, "a", "nope", WorktreeRemoveOptions{}); err == nil || !strings.Contains(err.Error(), `no worktree "nope"`) ||
		!strings.Contains(err.Error(), "locked [locked]") || !strings.Contains(err.Error(), "here [here]") {
		t.Errorf("unknown target: err = %v, want the list of worktrees", err)
	}
	var re *WorktreeRefusedError
	_, err := f.rmWT(t, "a", "locked", WorktreeRemoveOptions{Force: true})
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "locked") {
		t.Errorf("locked: err = %v", err)
	}
	_, err = f.rmWT(t, "a", "here", WorktreeRemoveOptions{Force: true, WorkingDir: filepath.Join(root, "a", "here")})
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "working directory") {
		t.Errorf("cwd inside: err = %v", err)
	}
	_, err = f.rmWT(t, "a", "outside", WorktreeRemoveOptions{Force: true})
	if err == nil || !strings.Contains(err.Error(), "outside") || !exists(outside) {
		t.Errorf("outside worktree: err = %v", err)
	}
	_, err = f.rmWT(t, "a", "here", WorktreeRemoveOptions{Confirm: func(WorktreeRemovePlan) bool { return false }})
	if !errors.Is(err, ErrNotConfirmed) || !exists(filepath.Join(root, "a", "here")) {
		t.Errorf("not confirmed: err = %v", err)
	}
	for _, d := range []string{"locked", "here"} {
		if !exists(filepath.Join(root, "a", d)) {
			t.Errorf("%s was removed", d)
		}
	}
}

func TestRemoveWorktreeSymlinkedDirRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	root := f.w.Root
	target := filepath.Join(t.TempDir(), "real")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "linked", target)
	link := filepath.Join(root, "a", "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, primary, "worktree", "repair", link)
	_, err := f.rmWT(t, "a", "linked", WorktreeRemoveOptions{Force: true})
	if err == nil || !exists(target) {
		t.Fatalf("symlinked worktree: err = %v, real exists %v", err, exists(target))
	}
}

func TestRemoveWorktreeRisks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	dirty := f.mustAddWT(t, "a", "dirty").Path
	writeFile(t, filepath.Join(dirty, "new.txt"), "x\n")

	var re *WorktreeRefusedError
	_, err := f.rmWT(t, "a", "dirty", WorktreeRemoveOptions{})
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "untracked") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("dirty: err = %v", err)
	}
	if !exists(dirty) {
		t.Fatal("dirty worktree removed without --force")
	}
	if _, err := f.rmWT(t, "a", "dirty", WorktreeRemoveOptions{Force: true}); err != nil || exists(dirty) {
		t.Fatalf("dirty --force: err = %v, exists %v", err, exists(dirty))
	}

	// Unpushed commits on the branch are repository-wide: the branch
	// survives, so they do not block, but they are reported.
	ahead := f.mustAddWT(t, "a", "ahead").Path
	testutil.CommitFile(t, ahead, "a.txt", "a\n", "local work")
	var plan WorktreeRemovePlan
	if _, err := f.rmWT(t, "a", "ahead", WorktreeRemoveOptions{Confirm: func(p WorktreeRemovePlan) bool { plan = p; return true }}); err != nil {
		t.Fatal(err)
	}
	if exists(ahead) || !branchExists(t, primary, "ahead") {
		t.Fatal("ahead: worktree not removed or branch lost")
	}
	if !hasKind(plan.Risks, gitx.RiskUnpushedBranchCommits) {
		t.Fatalf("plan risks = %v, want the unpushed commits listed", plan.Risks)
	}
}

func hasKind(rs []gitx.Risk, k gitx.RiskKind) bool {
	for _, r := range rs {
		if r.Kind == k {
			return true
		}
	}
	return false
}

func TestRemoveWorktreeDeleteBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")

	// Tracking origin/feat, nothing local: merged into its upstream.
	f.mustAddWT(t, "a", "feat")
	if _, err := f.rmWT(t, "a", "feat", WorktreeRemoveOptions{DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "feat") {
		t.Fatal("feat not deleted")
	}
	if out, _ := gitx.Run(context.Background(), primary, "config", "--get-regexp", `^branch\.feat\.`); strings.TrimSpace(out) != "" {
		t.Fatalf("branch config left behind:\n%s", out)
	}

	// New branch without commits: merged into origin/main.
	f.mustAddWT(t, "a", "empty")
	if _, err := f.rmWT(t, "a", "empty", WorktreeRemoveOptions{DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "empty") {
		t.Fatal("empty not deleted")
	}

	// Unmerged commits: refused before anything is removed; --force does
	// not override it, -D does.
	p := f.mustAddWT(t, "a", "work").Path
	testutil.CommitFile(t, p, "w.txt", "w\n", "work")
	var re *WorktreeRefusedError
	for _, force := range []bool{false, true} {
		_, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{DeleteBranch: true, Force: force})
		if !errors.As(err, &re) || !strings.Contains(err.Error(), "branch work is not merged") ||
			!strings.Contains(err.Error(), "1 commit") || !strings.Contains(err.Error(), "(use -D to delete anyway)") {
			t.Fatalf("unmerged (force %v): err = %v", force, err)
		}
		if !exists(p) || !branchExists(t, primary, "work") {
			t.Fatal("unmerged: something was removed")
		}
	}
	var plan WorktreeRemovePlan
	if _, err := f.rmWT(t, "a", "work", WorktreeRemoveOptions{ForceDeleteBranch: true,
		Confirm: func(pl WorktreeRemovePlan) bool { plan = pl; return true }}); err != nil {
		t.Fatal(err)
	}
	if plan.UnmergedCommits != 1 {
		t.Errorf("plan.UnmergedCommits = %d, want 1", plan.UnmergedCommits)
	}
	if exists(p) || branchExists(t, primary, "work") {
		t.Fatal("-D: worktree or branch still there")
	}

	// Detached: no branch to delete.
	det := filepath.Join(f.w.Root, "a", "det")
	testutil.Git(t, primary, "worktree", "add", "-q", "--detach", det)
	if _, err := f.rmWT(t, "a", "det", WorktreeRemoveOptions{DeleteBranch: true}); err == nil || !exists(det) {
		t.Fatalf("detached --delete-branch: err = %v", err)
	}
}

func TestRemoveWorktreeFetchFailureRefusesUnlessOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	testutil.Git(t, primary, "remote", "set-url", "origin", "file:///nonexistent/origin.git")
	_, err := f.rmWT(t, "a", "x", WorktreeRemoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "--offline") || !exists(p) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.rmWT(t, "a", "x", WorktreeRemoveOptions{Offline: true}); err != nil || exists(p) {
		t.Fatalf("offline: err = %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	root := f.w.Root
	x := f.mustAddWT(t, "a", "feat/x").Path
	ctx := context.Background()
	for _, tc := range []struct {
		args []string
		sel  *WorktreeTarget
		cwd  string
		want string
	}{
		{nil, nil, root, root},
		{nil, nil, x, root},
		{[]string{"a"}, nil, root, primary},
		{[]string{"a", "feat/x"}, nil, root, x},
		{[]string{"a", "feat-x"}, nil, root, x},
		{[]string{"a", "main"}, nil, root, primary},
		{[]string{"feat-x"}, nil, primary, x}, // inside a checkout: a target of its repository
		{[]string{"main"}, nil, x, primary},
		{[]string{"b", "main"}, nil, x, filepath.Join(root, "b", "main")},
		{[]string{"b"}, nil, x, filepath.Join(root, "b", "main")}, // no worktree "b" in a: the repository
		{nil, &WorktreeTarget{Name: "feat-x", Kind: TargetDir}, primary, x},
		{[]string{"a"}, &WorktreeTarget{Name: "feat/x", Kind: TargetBranch}, root, x},
	} {
		got, err := ResolvePath(ctx, f.w, PathQuery{Args: tc.args, Sel: tc.sel, Cwd: tc.cwd})
		if err != nil || got != tc.want {
			t.Errorf("ResolvePath(%q, %+v, %s) = %q, %v; want %q", tc.args, tc.sel, tc.cwd, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		args []string
		sel  *WorktreeTarget
		cwd  string
	}{
		{[]string{"nope"}, nil, root},
		{[]string{"A"}, nil, root}, // repository names are case-sensitive
		{[]string{"a", "nope"}, nil, root},
		{[]string{"a", "b", "c"}, nil, root},
		{[]string{"a", "x"}, &WorktreeTarget{Name: "feat-x", Kind: TargetDir}, root},
		{nil, &WorktreeTarget{Name: "feat/x", Kind: TargetDir}, primary},
		{nil, &WorktreeTarget{Name: "feat-x", Kind: TargetBranch}, primary},
	} {
		if got, err := ResolvePath(ctx, f.w, PathQuery{Args: tc.args, Sel: tc.sel, Cwd: tc.cwd}); err == nil {
			t.Errorf("ResolvePath(%q, %+v, %s) = %q, want an error", tc.args, tc.sel, tc.cwd, got)
		}
	}
}
