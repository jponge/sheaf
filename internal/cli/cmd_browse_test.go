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
		{"feat", nil, home},
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
	e.mustRun(t, "browse", "--fork", "--branch", "x")
	wantBrowse(t, e, 0, hostingtest.BrowseCall{Slug: "up/x"}, hostingtest.BrowseCall{Slug: "me/x"},
		hostingtest.BrowseCall{Slug: "me/x", Page: hosting.Page{Branch: "main"}})
	if _, errs, code := e.run(t, "browse", "--fork", "plain"); code != 1 || !strings.Contains(errs, "plain has no fork") {
		t.Fatalf("--fork without a fork: exit %d, %s", code, errs)
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
