package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

// wantBrowse fails unless the fake recorded exactly the Browse calls want
// since its calls numbered from.
func wantBrowse(t *testing.T, e *env, from int, want ...hostingtest.BrowseCall) {
	t.Helper()
	if got := e.fake.BrowseCalls()[from:]; !slices.Equal(got, want) {
		t.Fatalf("browse calls = %+v\nwant           %+v", got, want)
	}
}

// browse opens the repository of the working directory, or the named one;
// the page of the pull request in a worktree of `wt pr` (pull request from
// a fork); the tree of the current branch with --branch.
func TestBrowse(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	e.mustRun(t, "wt", "pr", "a", "7")
	e.mustRun(t, "wt", "pr", "a", "3") // head branch feat of o/a: checked out as feat
	home := hostingtest.BrowseCall{Slug: "o/a"}
	// From the bundle root, the repository must be named.
	if _, errs, code := e.run(t, "browse"); code != 1 || !strings.Contains(errs, "not inside a repository of the bundle") {
		t.Fatalf("browse at the root: exit %d, %s", code, errs)
	}
	_, errs, code := e.run(t, "browse", "a")
	if code != 0 || !strings.Contains(errs, "opened o/a") {
		t.Fatalf("browse a: exit %d, %s", code, errs)
	}
	wantBrowse(t, e, 0, home)
	for _, c := range []struct {
		dir  string
		args []string
		want hostingtest.BrowseCall
	}{
		{"main", nil, home},
		{"main", []string{"--branch"}, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{Branch: "main"}}},
		{"pr-7", nil, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{PR: 7}}},
		{filepath.Join("pr-7", "sub"), nil, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{PR: 7}}},
		{"feat", nil, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{Head: "feat"}}},
		{"feat", []string{"--branch"}, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{Branch: "feat"}}},
		// A repository argument names its primary checkout.
		{"pr-7", []string{"a"}, home},
	} {
		e.cwd = filepath.Join(root, "a", c.dir)
		if err := os.MkdirAll(e.cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		from := len(e.fake.BrowseCalls())
		_, errs, code := e.run(t, append([]string{"browse"}, c.args...)...)
		if code != 0 {
			t.Fatalf("%s %q: exit %d, %s", c.dir, c.args, code, errs)
		}
		wantBrowse(t, e, from, c.want)
	}
	// A detached checkout has no branch to open.
	e.cwd = filepath.Join(root, "a", "feat")
	testutil.Git(t, e.cwd, "checkout", "-q", "--detach")
	from := len(e.fake.BrowseCalls())
	if _, errs, code := e.run(t, "browse", "--branch"); code != 1 || !strings.Contains(errs, "not on a branch") {
		t.Fatalf("detached --branch: exit %d, %s", code, errs)
	}
	wantBrowse(t, e, from)
}

// The argument of browse completes to repository names.
func TestCompleteBrowse(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	e.wantComplete(t, []string{"a", "b"}, "browse", "")
	e.wantComplete(t, nil, "browse", "a", "")
}

// A clone of a fork opens the repository (upstream), --fork the fork.
func TestBrowseFork(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	e.forked(t, "up", "x")
	e.mustRun(t, "add", "up/x")
	e.addRepo(t, "plain")
	e.mustRun(t, "browse", "x")
	e.mustRun(t, "browse", "--fork", "x")
	e.mustRun(t, "browse", "--branch", "x") // main tracks upstream/main
	wantBrowse(t, e, 0, hostingtest.BrowseCall{Slug: "up/x"}, hostingtest.BrowseCall{Slug: "me/x"},
		hostingtest.BrowseCall{Slug: "up/x", Page: hosting.Page{Branch: "main"}})
	if _, errs, code := e.run(t, "browse", "--fork", "plain"); code != 1 || !strings.Contains(errs, "plain has no fork") {
		t.Fatalf("--fork without a fork: exit %d, %s", code, errs)
	}
	// --branch opens the branch where it is pushed: --fork has no meaning.
	if _, errs, code := e.run(t, "browse", "--fork", "--branch", "x"); code != 1 || !strings.Contains(errs, "--fork and --branch cannot be combined") {
		t.Fatalf("--fork --branch: exit %d, %s", code, errs)
	}
	wantBrowse(t, e, 3)
	// A pull request (on the repository) is not a page of the fork.
	primary := filepath.Join(e.cwd, "x", "main")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "pr/5", filepath.Join(e.cwd, "x", "pr-5"), "main")
	testutil.Git(t, primary, "config", "branch.pr/5.remote", "upstream")
	testutil.Git(t, primary, "config", "branch.pr/5.merge", "refs/pull/5/head")
	e.cwd = filepath.Join(e.cwd, "x", "pr-5")
	e.mustRun(t, "browse")
	e.mustRun(t, "browse", "--fork")
	wantBrowse(t, e, 3, hostingtest.BrowseCall{Slug: "up/x", Page: hosting.Page{PR: 5}}, hostingtest.BrowseCall{Slug: "me/x"})
	// A pull request ref tracked from the fork is not one wt pr sets up:
	// no pull request, and no head to look up.
	testutil.Git(t, primary, "config", "branch.pr/5.remote", "origin")
	e.mustRun(t, "browse")
	wantBrowse(t, e, 5, hostingtest.BrowseCall{Slug: "up/x"})
	// A branch pushed to the fork: its pull request on the repository is
	// looked up as <fork owner>:<branch>; --branch opens it on the fork,
	// under its upstream name.
	testutil.Git(t, primary, "update-ref", "refs/remotes/origin/topic", "HEAD")
	testutil.Git(t, primary, "worktree", "add", "-q", "--track", "-b", "local-topic", filepath.Join(filepath.Dir(primary), "topic"), "origin/topic")
	e.fake.AddPR("up", "x", hosting.PR{Number: 9, HeadBranch: "topic", HeadOwner: "me", HeadRepo: "x", IsCrossRepository: true})
	e.cwd = filepath.Join(filepath.Dir(primary), "topic")
	e.mustRun(t, "browse")
	e.mustRun(t, "browse", "--branch")
	wantBrowse(t, e, 6, hostingtest.BrowseCall{Slug: "up/x", Page: hosting.Page{Head: "me:topic"}},
		hostingtest.BrowseCall{Slug: "me/x", Page: hosting.Page{Branch: "topic"}})
}

// Without a recorded pull request, gh is asked for the pull request of the
// branch's upstream, and the repository opens when there is none; --branch
// opens the upstream branch and needs one.
func TestBrowseBranchUpstreams(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	e.mustRun(t, "wt", "pr", "a", "7")
	repo := filepath.Join(root, "a")
	primary := filepath.Join(repo, "main")
	git := func(args ...string) { t.Helper(); testutil.Git(t, primary, args...) }
	git("update-ref", "refs/remotes/origin/lonely", "HEAD")
	git("worktree", "add", "-q", "--track", "-b", "lonely", filepath.Join(repo, "lonely"), "origin/lonely")
	git("worktree", "add", "-q", "--track", "-b", "mine", filepath.Join(repo, "mine"), "origin/lonely")
	git("worktree", "add", "-q", "-b", "local", filepath.Join(repo, "local"))
	git("update-ref", "refs/remotes/origin/123", "HEAD")
	git("worktree", "add", "-q", "--track", "-b", "123", filepath.Join(repo, "123"), "origin/123")
	git("worktree", "add", "-q", "-b", "merge-ref", filepath.Join(repo, "merge-ref"))
	git("config", "branch.merge-ref.remote", "origin")
	git("config", "branch.merge-ref.merge", "refs/pull/7/merge")
	git("remote", "add", "other", "https://github.com/someone/a.git")
	git("worktree", "add", "-q", "-b", "elsewhere", filepath.Join(repo, "elsewhere"))
	git("config", "branch.elsewhere.remote", "other")
	git("config", "branch.elsewhere.merge", "refs/heads/elsewhere")
	if err := os.Mkdir(filepath.Join(repo, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := hostingtest.BrowseCall{Slug: "o/a"}
	for _, c := range []struct {
		dir     string
		args    []string
		want    []hostingtest.BrowseCall
		message string // in stderr: what was opened, or the error (exit 1)
	}{
		{"lonely", nil, []hostingtest.BrowseCall{{Slug: "o/a", Page: hosting.Page{Head: "lonely"}}, home}, "opened o/a (no pull request for branch lonely)"},
		{"lonely", []string{"--branch"}, []hostingtest.BrowseCall{{Slug: "o/a", Page: hosting.Page{Branch: "lonely"}}}, "opened branch lonely of o/a"},
		{"mine", []string{"--branch"}, []hostingtest.BrowseCall{{Slug: "o/a", Page: hosting.Page{Branch: "lonely"}}}, "opened branch lonely of o/a"},
		{"local", nil, []hostingtest.BrowseCall{home}, "opened o/a"},
		{"local", []string{"--branch"}, nil, "local has no upstream branch: push it first (git push -u origin local)"},
		{"pr-7", []string{"--branch"}, nil, "pr/7 tracks pull request #7, not a branch: run sheaf browse without --branch to open it"},
		{"123", nil, []hostingtest.BrowseCall{home}, "opened o/a"}, // gh would read 123 as a pull request number
		{"merge-ref", nil, []hostingtest.BrowseCall{home}, "opened o/a"},
		{"elsewhere", nil, []hostingtest.BrowseCall{home}, "opened o/a"},
		{"elsewhere", []string{"--branch"}, nil, "elsewhere tracks other/elsewhere: other is not a remote of a known to sheaf (origin)"},
		{"notes", nil, nil, "is not inside a checkout of a"},
	} {
		e.cwd = filepath.Join(repo, c.dir)
		from := len(e.fake.BrowseCalls())
		_, errs, code := e.run(t, append([]string{"browse"}, c.args...)...)
		if wantCode := map[bool]int{true: 0, false: 1}[strings.HasPrefix(c.message, "opened")]; code != wantCode || !strings.Contains(errs, c.message) {
			t.Errorf("%s %q: exit %d, stderr %q; want exit %d and %q", c.dir, c.args, code, errs, wantCode, c.message)
		}
		if got := e.fake.BrowseCalls()[from:]; !slices.Equal(got, c.want) {
			t.Errorf("%s %q: browse calls = %+v, want %+v", c.dir, c.args, got, c.want)
		}
	}
}

// b on the home screen opens the current row like browse in its directory;
// the outcome is the overview's message, without a report view.
func TestHomeBrowse(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	e.mustRun(t, "wt", "pr", "a", "7")
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "b"), ovKeys(t, &out, "browse: opened o/a", 1, "j", "b"),
		ovKeys(t, &out, "browse: opened pull request #7 of o/a", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || strings.Contains(out.String(), "sheaf browse: ") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	wantBrowse(t, e, 0, hostingtest.BrowseCall{Slug: "o/a"}, hostingtest.BrowseCall{Slug: "o/a", Page: hosting.Page{PR: 7}})
	// A failure is the message too.
	e.fake.BrowseErr = fmt.Errorf("gh repo view o/a --web: %w: exit status 4", hosting.ErrGHNotAuthenticated)
	out = syncBuffer{}
	in = newScript(t, false, ovKeys(t, &out, "STATE", 1, "b"), ovKeys(t, &out, "browse failed: gh repo view o/a --web: gh is not authenticated", 1, "q"))
	errs, code = runHomeWith(t, e.deps(), in, &out)
	if code != 0 || strings.Contains(out.String(), "sheaf browse: ") || !strings.Contains(errs, "gh is not authenticated") {
		t.Fatalf("failure: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	// Problem rows and worktrees outside their repository directory are
	// refused.
	if err := os.RemoveAll(filepath.Join(root, "a", "pr-7")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	testutil.Git(t, filepath.Join(root, "a", "main"), "worktree", "add", "-q", "-b", "elsewhere", outside)
	from := len(e.fake.BrowseCalls())
	for _, c := range []struct{ filter, refusal string }{
		{"pr-7", "not available on a row with a problem"},
		{"elsewhere", "outside its repository directory"},
	} {
		out = syncBuffer{}
		ks := append([]string{"/"}, append(strings.Split(c.filter, ""), "\r", "b")...)
		in = newScript(t, false, append(ovKeys(t, &out, "STATE", 1, ks...), key{keys: "q", wait: waitOutput(t, &out, c.refusal)}))
		errs, code = runHomeWith(t, e.deps(), in, &out)
		if code != 0 || strings.Contains(out.String(), "browse: ") || strings.Contains(out.String(), "browse failed") {
			t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", c.filter, code, out.String(), errs)
		}
	}
	wantBrowse(t, e, from)
}

// Repositories of other hosts are refused; gh failures are reported.
func TestBrowseErrors(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	setHost(t, root, "a", "gitlab.com")
	if _, errs, code := e.run(t, "browse", "a"); code != 1 || !strings.Contains(errs, "a is not a GitHub repository") {
		t.Fatalf("non-GitHub: exit %d, %s", code, errs)
	}
	wantBrowse(t, e, 0)
	setHost(t, root, "a", "github.com")
	for _, err := range []error{
		fmt.Errorf("gh repo view o/a --web: %w: exit status 4: To get started with GitHub CLI, please run:  gh auth login", hosting.ErrGHNotAuthenticated),
		fmt.Errorf("%w (gh): install the GitHub CLI from https://cli.github.com/ and run `gh auth login`", hosting.ErrGHNotFound),
	} {
		e.fake.BrowseErr = err
		if _, errs, code := e.run(t, "browse", "a"); code != 1 || !strings.Contains(errs, errors.Unwrap(err).Error()) || strings.Contains(errs, "opened") {
			t.Errorf("%v: exit %d, %s", err, code, errs)
		}
	}
}
