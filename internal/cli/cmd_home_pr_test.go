package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
)

// setHost records host as the host of repository name of the bundle root.
func setHost(t *testing.T, root, name, host string) {
	t.Helper()
	b := load(t, root)
	r, ok := b.Repository(name)
	if !ok {
		t.Fatalf("no repository %s", name)
	}
	r.Host = host
	if err := bundle.Save(root, b); err != nil {
		t.Fatal(err)
	}
}

// # asks for a pull request number of the current row's repository and
// checks it out with wt pr; a value that is not a number is refused in the
// prompt, which stays open.
func TestHomeChecksOutPullRequest(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	var out syncBuffer
	in := newScript(t, false,
		ovKeys(t, &out, "STATE", 1, "#"),
		[]key{{keys: "x", wait: waitScreen(t, &out, "Pull request of a")}, {keys: "\r"},
			{keys: "\x15", wait: waitScreen(t, &out, `invalid pull request number "x"`)}, {keys: "7"}, {keys: "\r"}},
		reportKey(t, &out, "wt pr", "q"), ovKeys(t, &out, "wt pr: done", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	pr := filepath.Join(root, "a", "pr-7")
	if !exists(filepath.Join(pr, ".git")) {
		t.Fatalf("pull request not checked out\nstdout:\n%s\nstderr:\n%s", out.String(), errs)
	}
	// The overview after the action lists the new worktree, with its pull
	// request in STATE rather than "no upstream" (B52 E4).
	o := out.String()
	if done := strings.Index(o, "wt pr: done"); done < 0 || !strings.Contains(o[:done], "pr-7") || !strings.Contains(o[:done], "clean, PR #7") {
		t.Fatalf("overview without the new worktree:\n%s", o)
	}
	// esc cancels.
	out = syncBuffer{}
	errs, code = runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "#"), keys("3", "\x1b"), ovKeys(t, &out, "wt pr: cancelled", 1, "q")), &out)
	if code != 0 || exists(filepath.Join(root, "a", "feat")) {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// # is refused on a problem row and on a repository not hosted on GitHub.
func TestHomeRefusesPullRequestOnProblemAndNonGitHubRows(t *testing.T) {
	t.Parallel()
	e, _, feat, _, _ := homeBundle(t)
	if err := os.RemoveAll(feat); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	in := newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", "#"),
		key{keys: "q", wait: waitScreen(t, &out, problemRefusal)}))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || strings.Contains(out.String(), "Pull request of") {
		t.Fatalf("problem row: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	setHost(t, e.cwd, "beta", "gitlab.com")
	out = syncBuffer{}
	in = newScript(t, false, append(ovKeys(t, &out, "STATE", 1, "j", "j", "#"),
		key{keys: "q", wait: waitOutput(t, &out, "beta is not a GitHub repository")}))
	errs, code = runHomeWith(t, e.deps(), in, &out)
	if code != 0 || strings.Contains(out.String(), "Pull request of") {
		t.Fatalf("non-GitHub row: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}
