package ops

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// Tests of the two listings of a repository's checkouts, ListWorktrees (wt
// commands) and ListCheckouts (checkout commands), on the cases where they
// differed before they shared enumerateRepo.

func worktreeListing(infos []WorktreeInfo) []string {
	var out []string
	for _, in := range infos {
		s := in.Repo + "/" + in.Dir + " " + in.Path
		if in.Primary {
			s += " primary"
		}
		if in.Outside {
			s += " outside"
		}
		out = append(out, s)
	}
	return out
}

func checkoutListing(cs []Checkout) []string {
	var out []string
	for _, c := range cs {
		s := c.Name() + " " + c.Path
		if c.Primary {
			s += " primary"
		}
		if c.Outside {
			s += " outside"
		}
		out = append(out, s)
	}
	return out
}

// A worktree outside <repo>/ is listed last by both, with an empty Dir by
// ListWorktrees.
func TestListingsOutsideWorktreeOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat", "zz", "aaa")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	zz := addWorktree(t, alpha, "zz", "zz", true)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(tmp, "aaa")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "aaa", far, "origin/aaa")

	infos, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/feat " + feat, "alpha/zz " + zz, "alpha/ " + far + " outside"}
	if got := worktreeListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	want = []string{"alpha/main " + alpha + " primary", "alpha/feat " + feat, "alpha/zz " + zz, "alpha/aaa " + far + " outside"}
	if got := checkoutListing(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Worktrees are listed in the natural order of their directory names
// (feat-2 before feat-10, B62 L2), after the primary, by both.
func TestListingsNaturalWorktreeOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	for _, d := range []string{"feat-10", "feat-2", "feat-1", "feat"} {
		addWorktree(t, alpha, d, d, false)
	}
	want := []string{"alpha/main", "alpha/feat", "alpha/feat-1", "alpha/feat-2", "alpha/feat-10"}
	infos, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	var got []string
	for _, in := range infos {
		got = append(got, in.Repo+"/"+in.Dir)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListWorktrees = %q, want %q", got, want)
	}
	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	if got := checkoutNames(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts = %q, want %q", got, want)
	}
}

// naturalCompare orders digit runs by their numeric value, the rest byte
// by byte; names equal but for leading zeros are ordered by bytes, so
// that the order is total.
func TestNaturalCompare(t *testing.T) {
	t.Parallel()
	sorted := []string{"", "01", "1", "01x", "2", "10", "a", "a01", "a1", "a2", "a10", "a10b", "a10c", "a99999999999999999999", "a100000000000000000000", "b", "feat", "feat-1", "feat-2", "feat-10", "feat-10-1", "feat-10-2", "feat-10a", "feat2"}
	for i, a := range sorted {
		for j, b := range sorted {
			if got, want := naturalCompare(a, b), cmp.Compare(i, j); got != want {
				t.Errorf("naturalCompare(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// Problems are sorted by repository, then in the natural order of their
// paths.
func TestSortProblemsNaturalOrder(t *testing.T) {
	t.Parallel()
	ps := []Problem{{Repo: "b", Path: "/x/b/gone-1"}, {Repo: "a", Path: "/x/a/gone-10"}, {Repo: "a", Path: "/x/a/gone-2"}}
	sortProblems(ps)
	var got []string
	for _, p := range ps {
		got = append(got, p.Path)
	}
	if want := []string{"/x/a/gone-2", "/x/a/gone-10", "/x/b/gone-1"}; !slices.Equal(got, want) {
		t.Errorf("sortProblems: %q, want %q", got, want)
	}
}

// A registered worktree directory now holding another repository is
// reported as a problem by both.
func TestListingsWorktreeOfAnotherRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	other := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, filepath.Dir(feat), "clone", "-q", other.URL, feat)

	infos, fails, err := ListWorktrees(context.Background(), f.w, "alpha")
	if err != nil || len(fails) != 1 || !strings.HasPrefix(fails[0].Error(), "alpha: "+feat+": worktree of another repository (") {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary"}
	if got := worktreeListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutListing(cs); !slices.Equal(got, []string{"alpha/main " + alpha + " primary"}) {
		t.Errorf("ListCheckouts = %q", got)
	}
	if len(probs) != 1 || probs[0].Path != feat || !strings.HasPrefix(probs[0].Err.Error(), "worktree of another repository (") {
		t.Errorf("problems = %v", probs)
	}
}

// A primary checkout whose .git is a file (separate git directory) is
// refused by both, with the same message.
func TestListingsPrimaryWithGitFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(tmp, "alpha.git")
	if err := os.Rename(filepath.Join(alpha, ".git"), gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(fails) != 1 || fails[0].Error() != "alpha: "+alpha+": primary checkout's .git is not a directory" {
		t.Errorf("ListWorktrees: err %v, failures %v", err, fails)
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(cs) != 0 || len(probs) != 1 || probs[0].Error() != "alpha: "+alpha+": primary checkout's .git is not a directory" {
		t.Errorf("ListCheckouts: err %v, checkouts %v, problems %v", err, cs, probs)
	}
}

// A worktree registered at a path whose parent directory is now a symbolic
// link to <repo>/ is outside for both (the lexical parent is not <repo>/).
func TestListingsWorktreeThroughSymlinkedParent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "x")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(tmp, "far")
	x := filepath.Join(far, "x")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "x", x, "origin/x")
	repoDir := filepath.Dir(alpha)
	if err := os.Rename(x, filepath.Join(repoDir, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(far); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, far); err != nil {
		t.Fatal(err)
	}

	infos, fails, err := ListWorktrees(context.Background(), f.w, "alpha")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/ " + x + " outside"}
	if got := worktreeListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	want = []string{"alpha/main " + alpha + " primary", "alpha/x " + x + " outside"}
	if got := checkoutListing(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// listingFailures returns the failure texts of both listings for the
// repository alpha, the only one of the bundle.
func listingFailures(t *testing.T, f *fixture) (wt, co []string) {
	t.Helper()
	infos, fails, err := ListWorktrees(context.Background(), f.w, "")
	if err != nil || len(infos) != 0 {
		t.Fatalf("ListWorktrees: %v, %v", err, worktreeListing(infos))
	}
	for _, e := range fails {
		wt = append(wt, e.Error())
	}
	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("ListCheckouts listed %q", checkoutListing(cs))
	}
	for _, p := range probs {
		co = append(co, p.Error())
	}
	return wt, co
}

// The repository directory and the primary checkout must not be symbolic
// links, for every command; both listings and fetch report the same
// problem.
func TestListingsRefuseSymlinks(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"repository directory", "primary checkout"} {
		t.Run(which, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, alpha := addRepoWith(t, f, "alpha")
			link := alpha
			if which == "repository directory" {
				link = filepath.Dir(alpha)
			}
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(tmp, "real")
			if err := os.Rename(link, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			want := "alpha: " + link + ": " + which + " is a symbolic link"
			wt, co := listingFailures(t, f)
			if !slices.Equal(wt, []string{want}) || !slices.Equal(co, []string{want}) {
				t.Errorf("failures:\nwt list %q\nstatus  %q\nwant %q", wt, co, want)
			}
			sum, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 1}, Discard)
			if err != nil || len(sum.Results) != 1 || sum.Results[0].Status != StatusFailed || !strings.Contains(sum.Results[0].Err.Error(), which+" is a symbolic link") {
				t.Errorf("fetch: %v %+v", err, sum.Results)
			}
		})
	}
}

// A missing repository directory is reported the same way by both.
func TestListingsMissingRepoDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	if err := os.RemoveAll(filepath.Dir(alpha)); err != nil {
		t.Fatal(err)
	}
	want := "alpha: " + filepath.Dir(alpha) + ": repository directory missing"
	wt, co := listingFailures(t, f)
	if !slices.Equal(wt, []string{want}) || !slices.Equal(co, []string{want}) {
		t.Errorf("failures:\nwt list %q\nstatus  %q\nwant %q", wt, co, want)
	}
}

// foreignWorktree replaces the worktree feat of alpha (tracking origin/feat)
// by a clone of another repository and returns the primary and its path.
func foreignWorktree(t *testing.T, f *fixture) (testutil.Origin, string, string) {
	t.Helper()
	o, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	other := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, filepath.Dir(feat), "clone", "-q", other.URL, feat)
	return o, alpha, feat
}

// The wt commands refuse a worktree of another repository as a target.
func TestWorktreeCommandsRefuseWorktreeOfAnotherRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, _, feat := foreignWorktree(t, f)
	const why = "worktree of another repository ("
	_, err := ResolvePath(context.Background(), f.w, PathQuery{Args: []string{"alpha", "feat"}, Cwd: f.w.Root})
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("path: %v", err)
	}
	_, err = RemoveWorktree(context.Background(), f.w, "alpha", WorktreeTarget{Name: "feat"},
		WorktreeRemoveOptions{WorkingDir: f.w.Root, Offline: true, Force: true, Confirm: func(WorktreeRemovePlan) bool { return true }}, noEmit)
	if err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("wt rm: %v", err)
	}
	if !exists(filepath.Join(feat, ".git")) {
		t.Fatal("the other repository was touched")
	}
}

// A linked worktree directory without .git is reported as a problem by wt
// list (it used to be listed as prunable).
func TestListWorktreesReportsWorktreeWithoutGit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	if err := os.Remove(filepath.Join(feat, ".git")); err != nil {
		t.Fatal(err)
	}
	infos, fails, err := ListWorktrees(context.Background(), f.w, "")
	want := "alpha: " + feat + ": not a git work tree: its .git file is missing (`git -C " + alpha + " worktree repair " + feat + "` restores it)"
	if err != nil || len(fails) != 1 || fails[0].Error() != want {
		t.Fatalf("err %v, failures %q, want %q", err, fails, want)
	}
	if got := worktreeListing(infos); !slices.Equal(got, []string{"alpha/main " + alpha + " primary"}) {
		t.Errorf("ListWorktrees = %q", got)
	}
}

// prune never proposes a worktree of another repository.
func TestPruneSkipsWorktreeOfAnotherRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, _, feat := foreignWorktree(t, f)
	testutil.Git(t, o.Path, "branch", "-q", "-D", "feat")
	p, err := PlanPrune(context.Background(), f.w, PruneOptions{Jobs: 1, WorkingDir: f.w.Root}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 0 {
		t.Errorf("candidates = %+v", p.Candidates)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Path != feat || !strings.Contains(p.Skipped[0].Reason, "worktree of another repository (") {
		t.Errorf("skipped = %+v", p.Skipped)
	}
}

// wt pr never refreshes nor returns the worktree of a pull request when its
// directory now holds another repository (here another clone of the same
// origin, whose HEAD a refresh would fast-forward).
func TestPRWorktreeRefusesWorktreeOfAnotherRepository(t *testing.T) {
	t.Parallel()
	t.Run("pull ref", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		_, o := f.clonedRepo(t, "a")
		head := pushPullRef(t, o, 7, "main", "fork.txt")
		f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "x", HeadOwner: "someone", HeadRepo: "a", IsCrossRepository: true})
		res, err := f.addPRWorktree(t, "a", 7)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(res.Path); err != nil {
			t.Fatal(err)
		}
		plainClone(t, o, res.Path)
		before := testutil.Git(t, res.Path, "rev-parse", "HEAD")
		pushPullRef(t, o, 7, head, "fork2.txt")
		_, err = f.addPRWorktree(t, "a", 7)
		if err == nil || !strings.Contains(err.Error(), "worktree of another repository (") {
			t.Errorf("err = %v", err)
		}
		if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != before {
			t.Errorf("HEAD of the other repository moved from %s to %s", before, got)
		}
	})
	t.Run("same repository", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		_, o := f.clonedRepo(t, "a")
		f.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
		res, err := f.addPRWorktree(t, "a", 3)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(res.Path); err != nil {
			t.Fatal(err)
		}
		plainClone(t, o, res.Path)
		if _, err = f.addPRWorktree(t, "a", 3); err == nil || !strings.Contains(err.Error(), "worktree of another repository (") {
			t.Errorf("err = %v", err)
		}
	})
}

// The other commands share the check: a symbolic link as primary checkout is
// refused by adopt --recheck, doctor, wt add and wt add --tag.
func TestCommandsRefuseSymlinkedPrimary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "real")
	if err := os.Rename(alpha, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alpha); err != nil {
		t.Fatal(err)
	}
	const why = "primary checkout is a symbolic link"
	if _, err := Adopt(context.Background(), f.w, filepath.Dir(alpha), AdoptOptions{Recheck: true}, noEmit); err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("adopt --recheck: %v", err)
	}
	cs := Doctor(context.Background(), DoctorOptions{Root: f.w.Root, GH: goodGH})
	if !slices.ContainsFunc(cs, func(c Check) bool { return c.Level == CheckFail && strings.Contains(c.Message, why) }) {
		t.Errorf("doctor: %s", dump(cs))
	}
	if _, err := AddWorktree(context.Background(), f.w, "alpha", "feat", WorktreeAddOptions{NoFetch: true}, noEmit); err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("wt add: %v", err)
	}
	if _, err := AddTagWorktree(context.Background(), f.w, "alpha", "v1", WorktreeAddOptions{NoFetch: true}, noEmit); err == nil || !strings.Contains(err.Error(), why) {
		t.Errorf("wt add --tag: %v", err)
	}
}

// A symbolic link in <repo>/ to a worktree elsewhere is outside for both
// (its resolved parent is not <repo>/).
func TestListingsSymlinkInRepoDirToWorktreeElsewhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "x")
	sym := filepath.Join(filepath.Dir(alpha), "symwt")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "x", sym, "origin/x")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(tmp, "wt")
	if err := os.Rename(sym, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, sym); err != nil {
		t.Fatal(err)
	}

	infos, fails, err := ListWorktrees(context.Background(), f.w, "alpha")
	if err != nil || len(fails) != 0 {
		t.Fatal(err, fails)
	}
	want := []string{"alpha/main " + alpha + " primary", "alpha/ " + sym + " outside"}
	if got := worktreeListing(infos); !slices.Equal(got, want) {
		t.Errorf("ListWorktrees =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	want = []string{"alpha/main " + alpha + " primary", "alpha/symwt " + sym + " outside"}
	if got := checkoutListing(cs); !slices.Equal(got, want) {
		t.Errorf("ListCheckouts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// doctor warns about linked worktrees that cannot be used, and does not call
// a worktree directory that lost its .git missing.
func TestDoctorWarnsAboutUnusableWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha, foreign := foreignWorktree(t, f)
	nogit := addWorktree(t, alpha, "nogit", "nogit", false)
	if err := os.Remove(filepath.Join(nogit, ".git")); err != nil {
		t.Fatal(err)
	}
	cs := Doctor(context.Background(), DoctorOptions{Root: f.w.Root, GH: goodGH})
	var got []string
	for _, c := range cs {
		if c.Check == "worktree" {
			got = append(got, c.Level.String()+" "+c.Message)
		}
	}
	want := []string{
		"warn worktree " + foreign + " [feat] cannot be used: worktree of another repository (" + filepath.Join(foreign, ".git") + ")",
		"warn worktree " + nogit + " [nogit] cannot be used: not a git work tree: its .git file is missing (`git -C " + alpha + " worktree repair " + nogit + "` restores it)",
	}
	if !slices.ContainsFunc(cs, func(c Check) bool {
		return strings.Contains(c.Message, foreign) && c.Hint == "git keeps the entry while the directory holds another repository: move "+foreign+" away, run `git -C "+alpha+" worktree prune`, then move it back"
	}) {
		t.Errorf("doctor: no hint for %s: %s", foreign, dump(cs))
	}
	if !slices.Equal(got, want) {
		t.Errorf("doctor worktree checks:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// RemoveWorktree re-checks under the lock that the worktree can still be
// used: a directory replaced by another repository after the plan was
// confirmed is refused, and left alone.
func TestRemoveWorktreeRechecksUsabilityUnderLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o, alpha := addRepoWith(t, f, "alpha", "feat")
	feat := addWorktree(t, alpha, "feat", "feat", true)
	confirm := func(WorktreeRemovePlan) bool {
		if err := os.RemoveAll(feat); err != nil {
			t.Error(err)
		}
		plainClone(t, o, feat)
		return true
	}
	_, err := RemoveWorktree(context.Background(), f.w, "alpha", WorktreeTarget{Name: "feat"},
		WorktreeRemoveOptions{WorkingDir: f.w.Root, Offline: true, Confirm: confirm}, noEmit)
	if err == nil || !strings.Contains(err.Error(), "cannot be used: "+feat+": worktree of another repository (") {
		t.Errorf("err = %v", err)
	}
	if !exists(filepath.Join(feat, ".git")) {
		t.Fatal("the other repository was touched")
	}
}

// enumerateRepo refuses a listing whose first entry is not the primary
// checkout. checkPrimary makes this unreachable for the commands (a .git
// directory that is its own common directory makes git list the checkout
// first); here a primary with a separate git directory bypasses it.
func TestEnumerateRepoRequiresPrimaryFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(tmp, "alpha.git")
	if err := os.Rename(filepath.Join(alpha, ".git"), gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := enumerateRepo(context.Background(), alpha, gitDir, enumPolicy{}); !errors.Is(err, errNotMainWorkTree) {
		t.Fatalf("err = %v, want %v", err, errNotMainWorkTree)
	}
}

// Linked worktrees outside <bundle>/<repo>/ are listed, marked Outside.
func TestListCheckoutsMarksOutsideWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, alpha := addRepoWith(t, f, "alpha", "feat", "far")
	addWorktree(t, alpha, "feat", "feat", true)
	far := filepath.Join(t.TempDir(), "far")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--track", "-b", "far", far, "origin/far")
	cs, probs, err := ListCheckouts(context.Background(), f.w, CheckoutFilter{}, 1)
	if err != nil || len(probs) != 0 || len(cs) != 3 {
		t.Fatalf("%v %v %+v", err, probs, cs)
	}
	for _, c := range cs {
		if want := c.Dir == "far"; c.Outside != want {
			t.Errorf("%s: Outside = %v, want %v", c.Name(), c.Outside, want)
		}
	}
}
