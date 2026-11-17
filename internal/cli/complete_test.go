package cli

import (
	"context"
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

const noFileComp = ":4" // cobra.ShellCompDirectiveNoFileComp

// keepOrder is cobra.ShellCompDirectiveNoFileComp |
// cobra.ShellCompDirectiveKeepOrder: the shell shows the candidates in the
// order given (the issues of wt add --issue, most recently updated first).
const keepOrder = ":36"

// complete runs cobra's hidden __complete command and returns the candidates
// (sorted) and the directive line.
func (e *env) complete(t *testing.T, args ...string) ([]string, string) {
	t.Helper()
	out, errs, code := e.run(t, append([]string{"__complete"}, args...)...)
	if code != 0 {
		t.Fatalf("__complete %q: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, out, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	last := lines[len(lines)-1]
	lines = lines[:len(lines)-1]
	for i, l := range lines {
		lines[i], _, _ = strings.Cut(l, "\t") // drop descriptions
	}
	slices.Sort(lines)
	return lines, last
}

func (e *env) wantComplete(t *testing.T, want []string, args ...string) {
	t.Helper()
	got, dir := e.complete(t, args...)
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) || dir != noFileComp {
		t.Fatalf("__complete %q = %q %s, want %q %s", args, got, dir, want, noFileComp)
	}
}

// complBundle: repositories a (branches feat/x, dev, topic/y; worktree
// feat/x; extra local branch local-only in the primary) and b.
func complBundle(t *testing.T) (*env, string) {
	t.Helper()
	e := newBundleEnv(t)
	root := e.cwd
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/x", "dev", "topic/y"}})
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "a", CloneURL: o.URL, DefaultBranch: "main"})
	e.origin(t, "o", "b", false)
	e.mustRun(t, "add", "o/a", "o/b")
	e.mustRun(t, "wt", "add", "a", "feat/x", "--no-fetch")
	testutil.Git(t, filepath.Join(root, "a", "main"), "branch", "local-only")
	return e, root
}

func TestCompleteRepoNames(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	both := []string{"a", "b"}
	e.wantComplete(t, both, "remove", "")
	e.wantComplete(t, []string{"a"}, "remove", "a")
	e.wantComplete(t, both, "wt", "list", "")
	e.wantComplete(t, both, "worktree", "list", "")
	e.wantComplete(t, both, "path", "")
	e.wantComplete(t, both, "wt", "remove", "")
	e.wantComplete(t, both, "wt", "add", "")
	for _, c := range []string{"status", "update", "run", "fetch", "path"} {
		e.wantComplete(t, both, c, "--repo", "")
	}
	// remove takes a single argument
	e.wantComplete(t, nil, "remove", "a", "")
}

func TestCompleteWorktreeNames(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	e.wantComplete(t, []string{"feat-x", "feat/x", "main"}, "path", "a", "")
	e.wantComplete(t, []string{"feat-x", "feat/x"}, "wt", "remove", "a", "")
	e.wantComplete(t, []string{"feat-x", "feat/x"}, "wt", "rm", "a", "f")
	// b has only its primary
	e.wantComplete(t, []string{"main"}, "path", "b", "")
	e.wantComplete(t, nil, "wt", "remove", "b", "")
	// selectors
	e.wantComplete(t, []string{"feat/x", "main"}, "path", "--repo", "a", "--branch", "")
	e.wantComplete(t, []string{"feat-x", "main"}, "path", "--repo", "a", "--dir", "")
	e.wantComplete(t, []string{"feat-x"}, "wt", "remove", "a", "--dir", "")

	// Inside a checkout of a: its worktrees as well as the repositories.
	e.cwd = filepath.Join(root, "a", "main")
	e.wantComplete(t, []string{"a", "b", "feat-x", "feat/x", "main"}, "path", "")
	e.wantComplete(t, []string{"a", "b", "feat-x", "feat/x"}, "wt", "remove", "")
	e.wantComplete(t, []string{"feat/x"}, "wt", "remove", "--dir", "", "--branch", "")
}

func TestCompleteBranchesForWorktreeAdd(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	// local and origin/* branches, minus the checked-out ones (main, feat/x)
	e.wantComplete(t, []string{"dev", "local-only", "topic/y"}, "wt", "add", "a", "")
	e.wantComplete(t, []string{"topic/y"}, "wt", "add", "a", "to")
	e.wantComplete(t, nil, "wt", "add", "b", "")
	// inferred from the working directory: repositories and branches
	e.cwd = filepath.Join(root, "a", "feat-x")
	e.wantComplete(t, []string{"a", "b", "dev", "local-only", "topic/y"}, "wt", "add", "")
}

func TestCompleteOrgs(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	e.wantComplete(t, nil, "add", "--org", "")
	e.mustRun(t, "add", "--org", "o", "--all")
	e.wantComplete(t, []string{"o"}, "add", "--org", "")
	e.wantComplete(t, nil, "add", "--org", "x")
}

func TestCompleteHonoursBundleFlagAndStaysSilentOnFailure(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	e.cwd = t.TempDir() // not inside a bundle
	e.wantComplete(t, nil, "remove", "")
	e.wantComplete(t, nil, "path", "a", "")
	e.wantComplete(t, nil, "wt", "add", "a", "")
	e.wantComplete(t, []string{"a", "b"}, "--bundle", root, "remove", "")
	e.wantComplete(t, []string{"feat-x", "feat/x", "main"}, "--bundle", root, "path", "a", "")
	e.wantComplete(t, []string{"feat-x", "feat/x"}, "wt", "remove", "--bundle", root, "a", "")
	e.wantComplete(t, nil, "--bundle", filepath.Join(root, "nope"), "remove", "")
	// unknown repository
	e.wantComplete(t, nil, "--bundle", root, "path", "zzz", "")
	out, _, _ := e.run(t, "__complete", "remove", "")
	if out != noFileComp+"\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestEveryCompletionTargetExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := NewRootCmdWithDeps(Streams{}, e.deps())
	if missing := installCompletionsReport(root, e.deps()); len(missing) != 0 {
		t.Fatalf("completion targets missing from the command tree: %q", missing)
	}
}

func TestCompleteNothingApplies(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	for _, args := range [][]string{
		{"status", ""}, {"update", ""}, {"fetch", ""}, {"version", ""},
		{"completion", "bash", ""}, {"completion", "zsh", ""}, {"completion", "fish", ""},
		{"add", "--repo", ""}, {"add", "--as", ""}, {"sync", ""},
	} {
		e.wantComplete(t, nil, args...)
	}
	e.wantComplete(t, []string{"bash", "fish", "zsh"}, "completion", "")
	e.wantComplete(t, []string{"bash", "fish", "zsh"}, "shell-init", "")
	e.wantComplete(t, nil, "shell-init", "bash", "")
}

func TestCompleteBundleFlagOffersDirectories(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	got, dir := e.complete(t, "--bundle", "")
	if len(got) != 0 || dir != ":16" { // cobra.ShellCompDirectiveFilterDirs
		t.Fatalf("--bundle value: %q %s", got, dir)
	}
	got, dir = e.complete(t, "path", "--bundle", "")
	if len(got) != 0 || dir != ":16" {
		t.Fatalf("path --bundle value: %q %s", got, dir)
	}
}

func TestCompleteSyncOrg(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	e.mustRun(t, "add", "--org", "o", "--all")
	e.wantComplete(t, []string{"o"}, "sync", "--org", "")
}

func TestCompleteMissingCheckoutsAreNotOfferedByPath(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	if err := os.RemoveAll(filepath.Join(root, "a", "feat-x")); err != nil {
		t.Fatal(err)
	}
	e.wantComplete(t, []string{"main"}, "path", "a", "")
	// a missing worktree can still be removed (pruned)
	e.wantComplete(t, []string{"feat-x", "feat/x"}, "wt", "remove", "a", "")
}

func TestCompleteNeverUsesTheNetwork(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	for _, r := range []string{"a", "b"} {
		testutil.Git(t, filepath.Join(root, r, "main"), "remote", "set-url", "origin", "http://127.0.0.1:1/unreachable.git")
	}
	e.wantComplete(t, []string{"dev", "local-only", "topic/y"}, "wt", "add", "a", "")
	e.wantComplete(t, []string{"a", "b"}, "remove", "")
}

// completeRaw runs __complete and returns the candidate lines as printed
// (descriptions kept, not sorted) and the directive line.
func completeRaw(t *testing.T, run func(args ...string) (string, string, int), args ...string) ([]string, string) {
	t.Helper()
	out, errs, code := run(append([]string{"__complete"}, args...)...)
	if code != 0 {
		t.Fatalf("__complete %q: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, out, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	return lines[:len(lines)-1], lines[len(lines)-1]
}

// listedIssues answers Issues with issues and err, and records the
// repositories ("owner/name") and limits it was asked for.
type listedIssues struct {
	*hostingtest.Fake
	issues []hosting.Issue
	err    error
	calls  *[]string
}

func (h listedIssues) Issues(_ context.Context, owner, name string, limit int) ([]hosting.Issue, error) {
	*h.calls = append(*h.calls, fmt.Sprintf("%s/%s %d", owner, name, limit))
	return h.issues, h.err
}

// --issue completes the open issues of the repository from the hosting
// service, in its order (kept by the shell), "<number>\t<title>" with the
// title on one line; when the hosting service fails, nothing.
func TestCompleteIssues(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	e.fake.AddIssue("o", "a", hosting.Issue{Number: 12, Title: "Other", State: "OPEN"})
	e.fake.AddIssue("o", "a", hosting.Issue{Number: 7, Title: "Fix\tthing\nnow", State: "OPEN"})
	e.fake.AddIssue("o", "a", hosting.Issue{Number: 9, Title: "A pull request", State: "OPEN", PullRequest: true})
	run := func(args ...string) (string, string, int) { return e.run(t, args...) }
	want := func(wantLines []string, wantDir string, args ...string) {
		t.Helper()
		got, dir := completeRaw(t, run, args...)
		if !slices.Equal(got, wantLines) || dir != wantDir {
			t.Errorf("__complete %q = %q %s, want %q %s", args, got, dir, wantLines, wantDir)
		}
	}
	both := []string{"12\tOther", "7\tFix thing now"}
	want(both, keepOrder, "wt", "add", "a", "--issue", "")
	want([]string{"12\tOther"}, keepOrder, "wt", "add", "a", "--issue", "1")
	want([]string{}, keepOrder, "wt", "add", "b", "--issue", "")
	e.cwd = filepath.Join(root, "a", "feat-x") // repository inferred
	want(both, keepOrder, "wt", "add", "--issue", "")
	e.cwd = root
	// With --issue, the only positional argument is the repository.
	e.wantComplete(t, []string{"a", "b"}, "wt", "add", "--issue", "7", "")
	e.wantComplete(t, nil, "wt", "add", "--issue", "7", "a", "")
	// A hosting error: no candidates.
	e.fake.IssueErr["o/a"] = fmt.Errorf("the 'o/a' repository has disabled issues: %w", hosting.ErrIssuesDisabled)
	want([]string{}, noFileComp, "wt", "add", "a", "--issue", "")
}

// Control characters of titles never reach the completion lines: each run
// of them (and of white space) becomes one space. Invisible format
// characters (bidi controls, zero-width spaces, byte order marks) are
// dropped.
func TestCompleteIssuesTitlesOnOneLine(t *testing.T) {
	t.Parallel()
	e, _ := complBundle(t)
	e.fake.AddIssue("o", "a", hosting.Issue{Number: 1, Title: "\ta\r\n\x1b[31mb\x00 \x7f c\u0085d\u2028e  ", State: "OPEN"})
	e.fake.AddIssue("o", "a", hosting.Issue{Number: 2, Title: "\ufeff\u202edrawkcab\u202c ze\u200bro \u200b width\u2066", State: "OPEN"})
	got, dir := completeRaw(t, func(args ...string) (string, string, int) { return e.run(t, args...) }, "wt", "add", "a", "--issue", "")
	if want := []string{"1\ta [31mb c d e", "2\tdrawkcab zero width"}; !slices.Equal(got, want) || dir != keepOrder {
		t.Fatalf("got %q %s, want %q %s", got, dir, want, keepOrder)
	}
}

// A truncated list is still offered; any other error offers nothing, even
// with issues. The issues are those of the reference repository of a fork,
// at most 200; a repository that is not on GitHub is not asked for.
func TestCompleteIssuesErrorsHostsAndForks(t *testing.T) {
	t.Parallel()
	e, root := complBundle(t)
	e.forked(t, "up", "x")
	e.mustRun(t, "add", "up/x")
	var calls []string
	issues := []hosting.Issue{{Number: 3, Title: "Three"}, {Number: 1, Title: "One"}}
	h := listedIssues{Fake: e.fake, issues: issues, calls: &calls}
	with := func(h hosting.Hosting) func(args ...string) (string, string, int) {
		return func(args ...string) (string, string, int) { return e.runWith(t, h, args...) }
	}
	check := func(h hosting.Hosting, wantLines []string, wantDir string, args ...string) {
		t.Helper()
		got, dir := completeRaw(t, with(h), args...)
		if !slices.Equal(got, wantLines) || dir != wantDir {
			t.Errorf("__complete %q = %q %s, want %q %s", args, got, dir, wantLines, wantDir)
		}
	}
	check(h, []string{"3\tThree", "1\tOne"}, keepOrder, "wt", "add", "x", "--issue", "")
	h.err = fmt.Errorf("%w: more than 200", hosting.ErrPossiblyTruncated)
	check(h, []string{"3\tThree", "1\tOne"}, keepOrder, "wt", "add", "a", "--issue", "")
	h.err = errors.New("gh: not logged in")
	check(h, []string{}, noFileComp, "wt", "add", "a", "--issue", "")
	h.err = nil
	setHost(t, root, "b", "gitlab.com")
	check(h, []string{}, noFileComp, "wt", "add", "b", "--issue", "")
	setHost(t, root, "b", "github-work") // an SSH alias of github.com, as ops sees it
	check(h, []string{"3\tThree", "1\tOne"}, keepOrder, "wt", "add", "b", "--issue", "")
	if want := []string{"up/x 200", "o/a 200", "o/a 200", "o/b 200"}; !slices.Equal(calls, want) {
		t.Errorf("Issues calls %q, want %q", calls, want)
	}
}
