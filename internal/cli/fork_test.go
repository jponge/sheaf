package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// forked registers owner/name in the fake with the user's fork me/name.
func (e *env) forked(t *testing.T, owner, name string) testutil.ForkedOrigin {
	t.Helper()
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	e.fake.User = "me"
	e.fake.AddRepo(hosting.Repo{Owner: owner, Name: name, CloneURL: fo.Upstream.URL, DefaultBranch: "main"})
	e.fake.AddFork(hosting.Fork{Repo: hosting.Repo{Owner: "me", Name: name, CloneURL: fo.Fork.URL, Fork: true}, ParentOwner: owner, ParentName: name})
	return fo
}

func remoteOf(t *testing.T, dir, remote string) string {
	t.Helper()
	u, _ := gitx.RemoteURL(t.Context(), dir, remote)
	return u
}

func TestAddReportsForkAndNoForkOptsOut(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	fx := e.forked(t, "up", "x")
	fy := e.forked(t, "up", "y")
	out, errs, code := e.run(t, "add", "up/x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "using your fork me/x as origin") || !strings.Contains(out, "origin: your fork me/x") {
		t.Fatalf("fork not reported:\nstdout:\n%s\nstderr:\n%s", out, errs)
	}
	if got := remoteOf(t, filepath.Join(e.cwd, "x", "main"), "origin"); got != fx.Fork.URL {
		t.Fatalf("origin = %q", got)
	}
	if y := readYAML(t, e.cwd); !strings.Contains(y, "fork:\n      slug: me/x\n") {
		t.Fatalf("sheaf.yaml:\n%s", y)
	}
	calls := e.fake.ForkCalls()
	e.mustRun(t, "add", "--no-fork", "up/y")
	if e.fake.ForkCalls() != calls {
		t.Fatal("--no-fork listed the forks")
	}
	if got := remoteOf(t, filepath.Join(e.cwd, "y", "main"), "origin"); got != fy.Upstream.URL {
		t.Fatalf("origin = %q, want the repository itself", got)
	}
}

func TestSyncAddNewDetectsForksUnlessNoFork(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	fo := e.forked(t, "o", "d")
	e.mustRun(t, "sync", "--add-new", "--exclude", "o/b", "--exclude", "o/c")
	if got := remoteOf(t, filepath.Join(root, "d", "main"), "upstream"); got != fo.Upstream.URL {
		t.Fatalf("upstream = %q", got)
	}

	e2, root2 := syncBundle(t)
	fo2 := e2.forked(t, "o", "d")
	e2.mustRun(t, "sync", "--add-new", "--no-fork", "--exclude", "o/b", "--exclude", "o/c")
	if got := remoteOf(t, filepath.Join(root2, "d", "main"), "origin"); got != fo2.Upstream.URL {
		t.Fatalf("origin = %q", got)
	}
}

func TestCompleteBranchesOfForkedRepository(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	fo := e.forked(t, "up", "x")
	testutil.Git(t, fo.Fork.Path, "branch", "on-fork", "main")
	testutil.Git(t, fo.Upstream.Path, "branch", "on-upstream", "main")
	e.mustRun(t, "add", "up/x")
	e.wantComplete(t, []string{"on-fork", "on-upstream"}, "wt", "add", "x", "")
}
