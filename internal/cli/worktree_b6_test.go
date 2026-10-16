package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// B6: `wt add --offline` is an alias of --no-fetch, like --offline of the
// other commands (--no-fetch keeps working).
func TestWorktreeAddOffline(t *testing.T) {
	t.Parallel()
	e, root := wtBundle(t)
	primary := filepath.Join(root, "a", "main")
	testutil.Git(t, primary, "tag", "v1")
	testutil.Git(t, primary, "config", "remote.origin.url", "file://"+filepath.Join(t.TempDir(), "gone.git"))
	if _, errs, code := e.run(t, "wt", "add", "a", "feat/y"); code == 0 {
		t.Fatalf("a fetch from the missing origin succeeded:\n%s", errs)
	}
	for _, args := range [][]string{
		{"wt", "add", "--offline", "a", "feat/y"},
		{"wt", "add", "--no-fetch", "a", "feat/z"},
		{"wt", "add", "--offline", "a", "--tag", "v1"},
	} {
		if out, errs, code := e.run(t, args...); code != 0 || strings.Contains(errs, "fetching") || out == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr:\n%s", args, code, out, errs)
		}
	}
}

// B6: `wt rm` without argument inside a checkout says what is missing, not
// the hint about single arguments outside a checkout.
func TestWorktreeRemoveWithoutArgumentInsideACheckout(t *testing.T) {
	t.Parallel()
	e, root := wtBundle(t)
	e.cwd = filepath.Join(root, "a", "main")
	_, errs, code := e.run(t, "wt", "rm", "--yes")
	if want := "missing <branch-or-dir> for repository a (inferred from the working directory)"; code == 0 || !strings.Contains(errs, want) || strings.Contains(errs, "outside a checkout") {
		t.Fatalf("exit %d, stderr lacks %q (or has the outside hint):\n%s", code, want, errs)
	}
}

// B6: when only worktrees fail, the error header does not speak of
// repositories.
func TestWorktreeListErrorHeader(t *testing.T) {
	t.Parallel()
	e, root := wtBundle(t)
	p := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "feat", "--no-fetch"))
	if err := os.Remove(filepath.Join(p, ".git")); err != nil {
		t.Fatal(err)
	}
	_, errs, code := e.run(t, "wt", "list")
	if code != 1 || !strings.Contains(errs, "1 worktree could not be listed:\n  a: "+p+": ") {
		t.Fatalf("exit %d, stderr:\n%s", code, errs)
	}
	if err := os.Rename(filepath.Join(root, "b", "main"), filepath.Join(root, "b", "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if _, errs, _ := e.run(t, "wt", "list"); !strings.Contains(errs, "1 repository and 1 worktree could not be listed:") {
		t.Fatalf("stderr:\n%s", errs)
	}
}
