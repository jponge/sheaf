package cli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/testutil"
)

// worktreeBundle creates a bundle with repositories o/a and o/b and returns its root.
func worktreeBundle(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.origin(t, "o", "a", false)
	e.origin(t, "o", "b", false)
	root := filepath.Join(e.cwd, "bundle")
	e.mustRun(t, "create", root)
	e.cwd = root
	e.mustRun(t, "add", "o/a", "o/b")
	// e.cwd keeps the path as given (it may go through a symbolic link);
	// sheaf reports resolved paths (bundle.Find), like git.
	root, _ = filepath.EvalSymlinks(root)
	return e, root
}

func TestWorktreeAddPrintsOnlyThePath(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	out, errs, code := e.run(t, "wt", "add", "a", "feat/x", "--no-fetch")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	want := filepath.Join(root, "a", "feat-x")
	if out != want+"\n" {
		t.Fatalf("stdout = %q, want %q", out, want+"\n")
	}
	if !strings.Contains(errs, "new branch feat/x from origin/main") {
		t.Fatalf("stderr lacks the mode:\n%s", errs)
	}
	// Repo inferred from the working directory; fetch by default.
	e.cwd = filepath.Join(root, "a", "main")
	out, errs, code = e.run(t, "worktree", "add", "other")
	if code != 0 || out != filepath.Join(root, "a", "other")+"\n" || !strings.Contains(errs, "fetching origin") {
		t.Fatalf("inferred: exit %d, stdout %q, stderr:\n%s", code, out, errs)
	}
	e.cwd = root
	if _, errs, code := e.run(t, "wt", "add", "x"); code == 0 || !strings.Contains(errs, "a, b") {
		t.Fatalf("no repo outside a checkout: exit %d, stderr:\n%s", code, errs)
	}
	// Outside a checkout a single argument is a repository name.
	if _, errs, code := e.run(t, "wt", "add", "a"); code == 0 || !strings.Contains(errs, "<branch>") {
		t.Fatalf("repo without branch: exit %d, stderr:\n%s", code, errs)
	}
}

func TestWorktreeListAndPath(t *testing.T) {
	e, root := worktreeBundle(t)
	e.mustRun(t, "wt", "add", "a", "feat/x", "--no-fetch")
	a, ax, b := filepath.Join(root, "a", "main"), filepath.Join(root, "a", "feat-x"), filepath.Join(root, "b", "main")

	if got := e.mustRun(t, "wt", "list", "--porcelain"); got != a+"\n"+ax+"\n"+b+"\n" {
		t.Fatalf("porcelain:\n%s", got)
	}
	if got := e.mustRun(t, "wt", "ls", "b", "--porcelain"); got != b+"\n" {
		t.Fatalf("porcelain b:\n%s", got)
	}
	if got := e.mustRun(t, "wt", "list", "--porcelain", "-z"); got != a+"\x00"+ax+"\x00"+b+"\x00" {
		t.Fatalf("porcelain -z: %q", got)
	}
	var doc struct {
		Version   int              `json:"version"`
		Worktrees []map[string]any `json:"worktrees"`
		Errors    []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "wt", "list", "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	items := doc.Worktrees
	if v, ok := items[1]["off_default"]; !ok || v != false {
		t.Fatalf("off_default missing or wrong in %v", items[1])
	}
	if doc.Version != 1 || len(doc.Errors) != 0 || len(items) != 3 || items[0]["primary"] != true || items[1]["branch"] != "feat/x" || items[1]["dir"] != "feat-x" || items[2]["repo"] != "b" {
		t.Fatalf("json: %+v", doc)
	}
	plain := e.mustRun(t, "wt", "list")
	if !strings.Contains(plain, "feat/x") || !strings.Contains(plain, "primary") || !strings.Contains(plain, ax) {
		t.Fatalf("plain list:\n%s", plain)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"path"}, root},
		{[]string{"path", "a"}, a},
		{[]string{"path", "a", "feat/x"}, ax},
		{[]string{"path", "a", "feat-x"}, ax},
		{[]string{"path", "a", "--dir", "feat-x"}, ax},
		{[]string{"path", "a", "--branch", "feat/x"}, ax},
		{[]string{"path", "--repo", "b"}, b},
	} {
		if got := e.mustRun(t, tc.args...); got != tc.want+"\n" {
			t.Errorf("%v = %q, want %q", tc.args, got, tc.want)
		}
	}
	if out, errs, code := e.run(t, "path", "a", "nope"); code != 1 || out != "" || !strings.Contains(errs, "nope") {
		t.Errorf("unknown: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

func TestWorktreeRemoveNeedsYes(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	p := filepath.Join(root, "a", "feat-x")
	e.mustRun(t, "wt", "add", "a", "feat/x", "--no-fetch")

	_, errs, code := e.run(t, "wt", "remove", "a", "feat/x")
	if code == 0 || !strings.Contains(errs, "--yes") || !exists(p) {
		t.Fatalf("without --yes: exit %d, stderr:\n%s", code, errs)
	}
	_, errs, code = e.run(t, "wt", "rm", "a", "main", "--yes")
	if code == 0 || !strings.Contains(errs, "sheaf remove") {
		t.Fatalf("primary: exit %d, stderr:\n%s", code, errs)
	}
	out := e.mustRun(t, "wt", "rm", "a", "--dir", "feat-x", "--yes", "-d")
	if exists(p) || !strings.Contains(out, "removed worktree feat-x [feat/x] at "+p) || !strings.Contains(out, "deleted branch feat/x") {
		t.Fatalf("remove: stdout %q", out)
	}
	if got := testutil.Git(t, filepath.Join(root, "a", "main"), "branch", "--list", "feat/x"); got != "" {
		t.Fatalf("branch still exists: %q", got)
	}
}

// The unpushed commits of the repository's local branches (here on other)
// do not concern the removal of a worktree, whose branch is kept: wt rm
// mentions them only when it deletes a branch, naming the branches (B52
// C2).
func TestWorktreeRemoveNotesUnpushedCommitsOnlyWhenDeletingABranch(t *testing.T) {
	t.Parallel()
	e, _ := worktreeBundle(t)
	other := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "other", "--no-fetch"))
	testutil.CommitFile(t, other, "o.txt", "o\n", "local work")
	e.mustRun(t, "wt", "add", "a", "x", "--no-fetch")
	e.mustRun(t, "wt", "add", "a", "y", "--no-fetch")
	_, errs, code := e.run(t, "wt", "rm", "a", "x", "--yes")
	if code != 0 || strings.Contains(errs, "unpushed") || !strings.Contains(errs, "branch x is kept") {
		t.Fatalf("without -d: exit %d, stderr:\n%s", code, errs)
	}
	_, errs, code = e.run(t, "wt", "rm", "a", "y", "--yes", "-d")
	if code != 0 || !strings.Contains(errs, "1 unpushed commit on local branches: other (informational, not blocking)\n") || !strings.Contains(errs, "will delete branch y") {
		t.Fatalf("-d: exit %d, stderr:\n%s", code, errs)
	}
}

func TestWorktreeRemoveUnmergedBranchNeedsD(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	p := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	testutil.CommitFile(t, p, "w.txt", "w\n", "work")
	for extra, hint := range map[string]string{"-d": "run: sheaf --bundle " + root + " wt rm -D --dir work --yes -- a", "-d --force": "run: sheaf --bundle " + root + " wt rm --force -D --dir work --yes -- a"} {
		_, errs, code := e.run(t, append([]string{"wt", "rm", "a", "work", "--yes"}, strings.Fields(extra)...)...)
		if code == 0 || !strings.Contains(errs, "branch work is not merged") || !strings.Contains(errs, "its 1 commit would be lost\n") || !strings.Contains(errs, hint+"\n") || !exists(p) {
			t.Fatalf("%v: exit %d, stderr:\n%s", extra, code, errs)
		}
	}
	out, errs, code := e.run(t, "wt", "rm", "a", "work", "--yes", "-D")
	if code != 0 || exists(p) || !strings.Contains(errs, "1 commit will be lost") || !strings.Contains(out, "deleted unmerged branch work") {
		t.Fatalf("-D: exit %d, stdout %q, stderr:\n%s", code, out, errs)
	}
	if got := testutil.Git(t, filepath.Join(root, "a", "main"), "branch", "--list", "work"); got != "" {
		t.Fatalf("branch still exists: %q", got)
	}
}

func TestWorktreeListPartialFailure(t *testing.T) {
	e, root := worktreeBundle(t)
	if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	out, errs, code := e.run(t, "wt", "list")
	if code != 1 || !strings.Contains(out, filepath.Join(root, "a", "main")) || !strings.Contains(errs, "b: "+filepath.Join(root, "b")+": repository directory missing") {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	out, _, code = e.run(t, "wt", "list", "--json")
	var doc struct {
		Version   int              `json:"version"`
		Worktrees []map[string]any `json:"worktrees"`
		Errors    []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err, out)
	}
	if code != 1 || len(doc.Worktrees) != 1 || len(doc.Errors) != 1 || doc.Errors[0]["repo"] != "b" || doc.Errors[0]["error"] == "" {
		t.Fatalf("json: exit %d, %+v", code, doc)
	}
}

// A worktree that cannot be used is reported in errors[] with its path, and
// makes the exit status 1.
func TestWorktreeListJSONUnusableWorktree(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	p := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "feat", "--no-fetch"))
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, root, "clone", "-q", filepath.Join(root, "b", "main"), p)
	out, _, code := e.run(t, "wt", "list", "--json")
	var doc struct {
		Worktrees []map[string]any `json:"worktrees"`
		Errors    []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err, out)
	}
	want := map[string]any{"repo": "a", "path": p, "error": "worktree of another repository (" + filepath.Join(p, ".git") + ")"}
	if code != 1 || len(doc.Worktrees) != 2 || len(doc.Errors) != 1 || !maps.Equal(doc.Errors[0], want) {
		t.Fatalf("exit %d, errors %v, want [%v], worktrees %v", code, doc.Errors, want, doc.Worktrees)
	}
}

// `wt add --offline` is an alias of --no-fetch, like --offline of the
// other commands (--no-fetch keeps working).
func TestWorktreeAddOffline(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
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

// `wt rm` without argument inside a checkout says what is missing, not
// the hint about single arguments outside a checkout.
func TestWorktreeRemoveWithoutArgumentInsideACheckout(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	e.cwd = filepath.Join(root, "a", "main")
	_, errs, code := e.run(t, "wt", "rm", "--yes")
	if want := "missing <branch-or-dir> for repository a (inferred from the working directory)"; code == 0 || !strings.Contains(errs, want) || strings.Contains(errs, "outside a checkout") {
		t.Fatalf("exit %d, stderr lacks %q (or has the outside hint):\n%s", code, want, errs)
	}
}

// When only worktrees fail, the error header does not speak of
// repositories.
func TestWorktreeListErrorHeader(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
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

// wt rm names the worktree repo/dir, in its plan as prune and the removal
// of several worktrees do, and on the line of its outcome (B62 L4).
func TestWorktreeRemoveNamesTheRepository(t *testing.T) {
	t.Parallel()
	p := ops.WorktreeRemovePlan{Repo: "alpha", Worktree: ops.WorktreeInfo{Repo: "alpha", Dir: "x", Branch: "feat/x", Path: "/b/alpha/x"}}
	if got, want := worktreeRemoveLine(p), "will remove worktree alpha/x [feat/x] at /b/alpha/x"; got != want {
		t.Errorf("worktreeRemoveLine = %q, want %q", got, want)
	}
	e, root := worktreeBundle(t)
	p1 := filepath.Join(root, "a", "feat-x")
	e.mustRun(t, "wt", "add", "a", "feat/x", "--no-fetch")
	out := e.mustRun(t, "wt", "rm", "a", "feat/x", "--yes")
	if want := "a/feat-x: removed worktree feat-x [feat/x] at " + p1 + "\n"; out != want || exists(p1) {
		t.Errorf("wt rm: stdout %q, want %q", out, want)
	}
}
