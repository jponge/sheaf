package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// pruneBundle creates a bundle with repository o/a whose origin has branch
// feat, checked out in a worktree, then deleted on origin.
func pruneBundle(t *testing.T) (*env, string, testutil.Origin) {
	t.Helper()
	e := newEnv(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "a", CloneURL: o.URL, DefaultBranch: "main"})
	root := filepath.Join(e.cwd, "bundle")
	e.mustRun(t, "create", root)
	e.cwd = root
	e.mustRun(t, "add", "o/a")
	e.mustRun(t, "wt", "add", "a", "feat")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "feat")
	// e.cwd keeps the path as given (it may go through a symbolic link);
	// sheaf reports resolved paths (bundle.Find), like git.
	root, _ = filepath.EvalSymlinks(root)
	return e, root, o
}

func TestPruneIsADryRunWithoutYes(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	wt := filepath.Join(root, "a", "feat")
	out, errs, code := e.run(t, "prune")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "would remove worktree a/feat") || !strings.Contains(out, "origin/feat is gone") || !strings.Contains(out, "--yes") {
		t.Fatalf("stdout:\n%s", out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree removed by a dry run: %v", err)
	}
	if _, errs, code := e.run(t, "prune", "--dry-run", "--yes"); code == 0 || !strings.Contains(errs, "mutually exclusive") {
		t.Fatalf("--dry-run --yes: exit %d, %s", code, errs)
	}
	out, errs, code = e.run(t, "prune", "--yes", "--delete-branches")
	if code != 0 {
		t.Fatalf("--yes: exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "removed worktree") || !strings.Contains(out, "kept branch feat") {
		t.Fatalf("--yes stdout:\n%s", out)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := e.mustRun(t, "prune"); !strings.Contains(out, "nothing to prune") {
		t.Fatalf("second run:\n%s", out)
	}
}

func TestPruneJSON(t *testing.T) {
	e, root, _ := pruneBundle(t)
	out := e.mustRun(t, "prune", "--json", "--include-branches", "--repo", "a")
	var doc struct {
		Version    int  `json:"version"`
		DryRun     bool `json:"dry_run"`
		Candidates []struct {
			Repo, Branch, Path string
			Worktree, Gone     bool
		} `json:"candidates"`
		Skipped []any `json:"skipped"`
		Results []any `json:"results"`
		Errors  []any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Version != 1 || !doc.DryRun || len(doc.Candidates) != 1 || doc.Results == nil || doc.Errors == nil || doc.Skipped == nil {
		t.Fatalf("doc %+v", doc)
	}
	c := doc.Candidates[0]
	if c.Repo != "a" || c.Branch != "feat" || !c.Worktree || !c.Gone || c.Path != filepath.Join(root, "a", "feat") {
		t.Fatalf("candidate %+v", c)
	}
	out = e.mustRun(t, "prune", "--json", "--yes")
	var applied struct {
		DryRun  bool `json:"dry_run"`
		Results []struct {
			Branch  string `json:"branch"`
			Outcome string `json:"outcome"`
			Message string `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if applied.DryRun || len(applied.Results) != 1 || applied.Results[0].Outcome != "removed" {
		t.Fatalf("applied %+v", applied)
	}
}

func TestPruneReportsFetchFailures(t *testing.T) {
	t.Parallel()
	e, root, o := pruneBundle(t)
	if err := os.RemoveAll(o.Path); err != nil {
		t.Fatal(err)
	}
	_, errs, code := e.run(t, "prune")
	if code != 1 || !strings.Contains(errs, "fetch failed") {
		t.Fatalf("exit %d, stderr:\n%s", code, errs)
	}
	// Offline, the stale remote-tracking state is used (feat looks alive).
	if out := e.mustRun(t, "prune", "--offline"); !strings.Contains(out, "nothing to prune") {
		t.Fatalf("offline:\n%s", out)
	}
	_ = root
}

// probeDeps returns e's dependencies with a gh probe answering err.
func probeDeps(e *env, err error) Deps {
	d := e.deps()
	d.GHProbe = func(context.Context) error { return err }
	return d
}

func TestDoctor(t *testing.T) {
	e, root := wtBundle(t)
	out, errs, code := runDeps(t, probeDeps(e, nil), "doctor")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"OK    git ", "OK    gh is installed", "OK    bundle " + root, "OK    a: primary checkout"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	out, _, code = runDeps(t, probeDeps(e, hosting.ErrGHNotFound), "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  b: directory "+filepath.Join(root, "b")+" is missing") ||
		!strings.Contains(out, "WARN  gh not found") || !strings.Contains(out, "      hint: install the GitHub CLI") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	out, _, code = runDeps(t, probeDeps(e, hosting.ErrGHNotFound), "doctor", "--json")
	var doc struct {
		Version int  `json:"version"`
		Failed  bool `json:"failed"`
		Checks  []struct {
			Level, Check, Repo, Message, Hint string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if code != 1 || doc.Version != 1 || !doc.Failed || len(doc.Checks) == 0 || doc.Checks[0].Level != "ok" || doc.Checks[0].Check != "git" {
		t.Fatalf("exit %d, doc %+v", code, doc)
	}
}

func TestDoctorOutsideABundle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	out, _, code := runDeps(t, probeDeps(e, nil), "doctor")
	if code != 1 || !strings.Contains(out, "OK    git ") || !strings.Contains(out, "FAIL  ") || !strings.Contains(out, "sheaf.yaml") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// prBundle creates a bundle with repository o/a, pull request #7 from a fork
// (refs/pull/7/head on origin) and #3 from branch feat.
func prBundle(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	testutil.Git(t, o.Path, "update-ref", "refs/pull/7/head", "refs/heads/feat")
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "a", CloneURL: o.URL, DefaultBranch: "main"})
	e.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "patch", HeadOwner: "fork", HeadRepo: "a", IsCrossRepository: true})
	e.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
	root := filepath.Join(e.cwd, "bundle")
	e.mustRun(t, "create", root)
	e.cwd = root
	e.mustRun(t, "add", "o/a")
	// e.cwd keeps the path as given (it may go through a symbolic link);
	// sheaf reports resolved paths (bundle.Find), like git.
	root, _ = filepath.EvalSymlinks(root)
	return e, root
}

func TestWorktreePR(t *testing.T) {
	t.Parallel()
	e, root := prBundle(t)
	out, errs, code := e.run(t, "wt", "pr", "a", "7")
	if code != 0 || out != filepath.Join(root, "a", "pr-7")+"\n" || !strings.Contains(errs, "refs/pull/7/head") {
		t.Fatalf("exit %d, stdout %q, stderr:\n%s", code, out, errs)
	}
	// The repository is inferred inside a checkout; "#3" is accepted.
	e.cwd = filepath.Join(root, "a", "main")
	if out := e.mustRun(t, "worktree", "pr", "#3"); out != filepath.Join(root, "a", "feat")+"\n" {
		t.Fatalf("inferred: %q", out)
	}
	if _, errs, code := e.run(t, "wt", "pr", "a", "x"); code == 0 || !strings.Contains(errs, "pull request number") {
		t.Fatalf("bad number: exit %d, %s", code, errs)
	}
	if _, errs, code := e.run(t, "wt", "pr", "a", "99"); code == 0 || !strings.Contains(errs, "not found") {
		t.Fatalf("unknown PR: exit %d, %s", code, errs)
	}
}
