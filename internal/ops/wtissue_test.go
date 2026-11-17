package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

func (f *fixture) addIssueWT(t *testing.T, repo string, n int, opts WorktreeAddOptions) (WorktreeAddResult, []Event, error) {
	t.Helper()
	return f.addIssueWTCtx(context.Background(), t, repo, n, opts)
}

func (f *fixture) addIssueWTCtx(ctx context.Context, t *testing.T, repo string, n int, opts WorktreeAddOptions) (WorktreeAddResult, []Event, error) {
	t.Helper()
	var evs []Event
	res, err := AddIssueWorktree(ctx, f.w, f.fake, repo, n, opts, func(e Event) { evs = append(evs, e) })
	return res, evs, err
}

// wantOnlyPrimary checks that nothing was created in repo: its directory
// holds only the primary checkout, and its local branches under issue/ are
// branches (sorted).
func (f *fixture) wantOnlyPrimary(t *testing.T, repo string, branches ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.w.Root, repo))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	if !slices.Equal(dirs, []string{"main"}) {
		t.Errorf("%s holds %v, want only main", repo, dirs)
	}
	out := testutil.Git(t, filepath.Join(f.w.Root, repo, "main"), "for-each-ref", "--format=%(refname:short)", "refs/heads/issue/")
	got := []string{}
	if out != "" {
		got = strings.Split(out, "\n")
	}
	if want := append([]string{}, branches...); !slices.Equal(got, want) {
		t.Errorf("local issue branches %v, want %v", got, want)
	}
}

func TestIssueBranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, c := range []struct {
		n     int
		title string
		want  string
	}{
		{123, "Fix crash on empty bundle", "issue/123-fix-crash-on-empty-bundle"},
		{10093, "Enhance `gh pr create` to support cross repo pull requests within the same organization", "issue/10093-enhance-gh-pr-create-to-support-cross-repo-pull"},
		{7, "日本語", "issue/7"},
		{8, "  --Déjà vu!! ", "issue/8-d-j-vu"},
		{9, "A_B/C..D", "issue/9-a-b-c-d"},
		{5, strings.Repeat("x", 80), "issue/5-" + strings.Repeat("x", 50)},
		// Only ASCII letters are lowercased: the Kelvin sign is not a k.
		{6, "K9s", "issue/6-9s"},
		// Cut at 50 right before a '-': the 50 characters are kept.
		{4, strings.Repeat("a", 50) + " b", "issue/4-" + strings.Repeat("a", 50)},
		{11, "ab " + strings.Repeat("c", 47) + " d", "issue/11-ab-" + strings.Repeat("c", 47)},
		// Cut inside a word that has a '-' before it: back to that '-'.
		{3, "ab " + strings.Repeat("c", 60), "issue/3-ab"},
		{2, "", "issue/2"},
	} {
		got := IssueBranch(c.n, c.title)
		if got != c.want {
			t.Errorf("IssueBranch(%d, %q) = %q, want %q", c.n, c.title, got, c.want)
			continue
		}
		if out := testutil.Git(t, dir, "check-ref-format", "--branch", got); out != got {
			t.Errorf("check-ref-format --branch %q = %q", got, out)
		}
	}
}

func TestAddIssueWorktreeCreatesBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 42, Title: "Add --issue flag", State: "OPEN"})
	res, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := WorktreeAddResult{Repo: "a", Branch: "issue/42-add-issue-flag", Dir: "issue-42-add-issue-flag",
		Path: filepath.Join(f.w.Root, "a", "issue-42-add-issue-flag"), Mode: gitx.WorktreeNewBranch, Base: "origin/main"}
	if res.Repo != want.Repo || res.Branch != want.Branch || res.Dir != want.Dir || res.Path != want.Path ||
		res.Mode != want.Mode || res.Base != want.Base || res.Upstream != "" || len(res.Warnings) != 0 {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != want.Branch {
		t.Fatalf("HEAD on %q", got)
	}
	if got := f.fake.IssueCalls(); !slices.Equal(got, []hostingtest.IssueCall{{Slug: "o/a", Number: 42}}) {
		t.Fatalf("Issue calls %v", got)
	}
}

func TestAddIssueWorktreeReusesRemoteBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, o := f.clonedRepo(t, "a")
	primaryB, oB := f.clonedRepo(t, "b")
	// Pushed from another machine after the clone; the issue was retitled
	// since: the branch is found by number, GitHub is not asked.
	tip := pushBranch(t, o, "issue/42-old-title")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 42, Title: "New title", State: "OPEN"})
	res, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "issue/42-old-title" || res.Mode != gitx.WorktreeRemoteBranch || res.Upstream != "origin/issue/42-old-title" ||
		res.Path != filepath.Join(f.w.Root, "a", "issue-42-old-title") {
		t.Fatalf("result %+v", res)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("HEAD %s, want %s", got, tip)
	}
	// --no-fetch, with the branch known from an earlier fetch: neither the
	// remote (made unreachable) nor GitHub is needed.
	tipB := pushBranch(t, oB, "issue/42-old-title")
	f.fake.AddIssue("o", "b", hosting.Issue{Number: 42, Title: "New title", State: "OPEN"})
	testutil.Git(t, primaryB, "fetch", "-q", "origin")
	testutil.Git(t, primaryB, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "gone.git"))
	res, evs, err := f.addIssueWT(t, "b", 42, WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "issue/42-old-title" || res.Mode != gitx.WorktreeRemoteBranch || res.Upstream != "origin/issue/42-old-title" {
		t.Fatalf("--no-fetch: result %+v", res)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != tipB {
		t.Fatalf("--no-fetch: HEAD %s, want %s", got, tipB)
	}
	for _, e := range evs {
		if e.Phase == PhaseFetch {
			t.Errorf("--no-fetch: fetch event %+v", e)
		}
	}
	if got := f.fake.IssueCalls(); len(got) != 0 {
		t.Fatalf("Issue calls %v, want none", got)
	}
}

func TestAddIssueWorktreePrefixIsExact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	pushBranch(t, o, "issue/12-x")
	testutil.Git(t, primary, "branch", "issue/1234-y", "main")
	for _, n := range []int{123, 1} {
		f.fake.AddIssue("o", "a", hosting.Issue{Number: n, Title: "New", State: "OPEN"})
		res, _, err := f.addIssueWT(t, "a", n, WorktreeAddOptions{})
		if err != nil {
			t.Fatalf("issue %d: %v", n, err)
		}
		if want := IssueBranch(n, "New"); res.Branch != want || res.Mode != gitx.WorktreeNewBranch {
			t.Fatalf("issue %d: result %+v, want new branch %s", n, res, want)
		}
	}
	// Issue 12 has its branch.
	res, _, err := f.addIssueWT(t, "a", 12, WorktreeAddOptions{})
	if err != nil || res.Branch != "issue/12-x" {
		t.Fatalf("issue 12: %+v, %v", res, err)
	}
}

func TestAddIssueWorktreeExactNumberBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	// issue/42 exists locally and on origin (same name: one branch).
	testutil.Git(t, primary, "branch", "issue/42", "main")
	testutil.Git(t, primary, "push", "-q", "origin", "issue/42")
	res, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "issue/42" || res.Mode != gitx.WorktreeLocalBranch || res.Dir != "issue-42" {
		t.Fatalf("result %+v", res)
	}
	if got := f.fake.IssueCalls(); len(got) != 0 {
		t.Fatalf("Issue calls %v, want none", got)
	}
}

func TestAddIssueWorktreeRefusesSeveralBranches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "branch", "issue/42-a", "main")
	pushBranch(t, o, "issue/42-b")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 42, Title: "C", State: "OPEN"})
	_, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{})
	want := "a has several branches for issue #42: issue/42-a, issue/42-b\nto add a worktree for one of them, run: sheaf wt add -- a issue/42-a"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	f.wantOnlyPrimary(t, "a", "issue/42-a")
	if got := f.fake.IssueCalls(); len(got) != 0 {
		t.Fatalf("Issue calls %v, want none", got)
	}
}

func TestAddIssueWorktreeRefusesPullRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 15065, Title: "A pull request", State: "OPEN", URL: "https://github.com/o/a/pull/15065", PullRequest: true})
	_, _, err := f.addIssueWT(t, "a", 15065, WorktreeAddOptions{})
	var pe *IssueIsPullRequestError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want an IssueIsPullRequestError", err)
	}
	if *pe != (IssueIsPullRequestError{Repo: "a", Slug: "o/a", Number: 15065, Command: "sheaf wt pr -- a 15065"}) {
		t.Fatalf("error %+v", *pe)
	}
	if want := "#15065 is a pull request of o/a, not an issue\nto check it out in a worktree, run: sheaf wt pr -- a 15065"; err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
	f.wantOnlyPrimary(t, "a")
	// With --bundle, the hint names the bundle.
	ctx := WithHintBundle(context.Background(), "/my bundle")
	if _, _, err := f.addIssueWTCtx(ctx, t, "a", 15065, WorktreeAddOptions{}); !errors.As(err, &pe) || pe.Command != "sheaf --bundle '/my bundle' wt pr -- a 15065" {
		t.Fatalf("with --bundle: %v", err)
	}
	f.wantOnlyPrimary(t, "a")
}

func TestAddIssueWorktreeNotGitHub(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	// Even with a branch for the issue: the rule is the one of wt pr.
	testutil.Git(t, primary, "branch", "issue/3-x", "main")
	if err := f.w.update(func(b *bundle.Bundle) error {
		b.Repositories[0].Host = "gitlab.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 3, Title: "X", State: "OPEN"})
	_, _, err := f.addIssueWT(t, "a", 3, WorktreeAddOptions{})
	if want := "a is hosted on gitlab.com: worktrees for issues can only be added for GitHub repositories"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if got := f.fake.IssueCalls(); len(got) != 0 {
		t.Fatalf("Issue calls %v, want none", got)
	}
	f.wantOnlyPrimary(t, "a", "issue/3-x")
}

func TestAddIssueWorktreeForkUsesReference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addForked(t, "x", testutil.OriginOpts{})
	f.fake.AddIssue("up", "x", hosting.Issue{Number: 42, Title: "Fork issue", State: "OPEN"})
	f.fake.AddIssue(forkUser, "x", hosting.Issue{Number: 42, Title: "Wrong one", State: "OPEN"})
	res, _, err := f.addIssueWT(t, "x", 42, WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "issue/42-fork-issue" || res.Mode != gitx.WorktreeNewBranch || res.Base != "upstream/main" {
		t.Fatalf("result %+v", res)
	}
	if got := f.fake.IssueCalls(); !slices.Equal(got, []hostingtest.IssueCall{{Slug: "up/x", Number: 42}}) {
		t.Fatalf("Issue calls %v", got)
	}
}

func TestAddIssueWorktreeHostingError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	_, _, err := f.addIssueWT(t, "a", 99, WorktreeAddOptions{})
	if !errors.Is(err, hosting.ErrNotFound) || !strings.HasPrefix(err.Error(), "issue #99 of o/a: ") {
		t.Fatalf("err = %v", err)
	}
	f.wantOnlyPrimary(t, "a")
	f.fake.IssueErr["o/a"] = hosting.ErrIssuesDisabled
	if _, _, err := f.addIssueWT(t, "a", 1, WorktreeAddOptions{}); !errors.Is(err, hosting.ErrIssuesDisabled) {
		t.Fatalf("issues disabled: err = %v", err)
	}
	f.wantOnlyPrimary(t, "a")
	if _, _, err := f.addIssueWT(t, "a", 0, WorktreeAddOptions{}); err == nil || err.Error() != "invalid issue number 0" {
		t.Fatalf("number 0: err = %v", err)
	}
	f.wantOnlyPrimary(t, "a")
}

func TestAddIssueWorktreeClosedIssue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 5, Title: "Old", State: "CLOSED"})
	res, evs, err := f.addIssueWT(t, "a", 5, WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "issue/5-old" || res.Mode != gitx.WorktreeNewBranch {
		t.Fatalf("result %+v", res)
	}
	const warn = "issue #5 of o/a is closed"
	if !slices.Equal(res.Warnings, []string{warn}) {
		t.Fatalf("warnings %q", res.Warnings)
	}
	if !slices.ContainsFunc(evs, func(e Event) bool { return e.Phase == PhaseIssue && e.Message == "warning: "+warn }) {
		t.Fatalf("no warning event in %+v", evs)
	}
}

func TestAddIssueWorktreeFetchFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 42, Title: "X", State: "OPEN"})
	testutil.Git(t, primary, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "gone.git"))
	_, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{Base: "feat"})
	if err == nil || !strings.HasPrefix(err.Error(), "fetching a failed: ") ||
		!strings.HasSuffix(err.Error(), "\nto create the worktree without fetching, run: sheaf wt add --no-fetch --base feat --issue=42 -- a") {
		t.Fatalf("err = %v", err)
	}
	if got := f.fake.IssueCalls(); len(got) != 0 {
		t.Fatalf("Issue calls %v, want none", got)
	}
	f.wantOnlyPrimary(t, "a")
}

// A start point that does not exist is reported as missing after the
// fetch that AddIssueWorktree made, not as "--no-fetch".
func TestAddIssueWorktreeBaseNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.fake.AddIssue("o", "a", hosting.Issue{Number: 42, Title: "X", State: "OPEN"})
	_, _, err := f.addIssueWT(t, "a", 42, WorktreeAddOptions{Base: "nope"})
	if err == nil || !strings.HasPrefix(err.Error(), "base nope not found in a (after fetching origin)") {
		t.Fatalf("err = %v", err)
	}
	f.wantOnlyPrimary(t, "a")
	_, _, err = f.addIssueWT(t, "a", 42, WorktreeAddOptions{Base: "nope", NoFetch: true})
	if err == nil || !strings.HasPrefix(err.Error(), "base nope not found in a (not fetched: --no-fetch)") {
		t.Fatalf("--no-fetch: err = %v", err)
	}
	f.wantOnlyPrimary(t, "a")
}
