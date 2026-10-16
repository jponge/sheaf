package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

const noFileComp = ":4" // cobra.ShellCompDirectiveNoFileComp

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
