package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// runDeps is env.run with explicit dependencies.
func runDeps(t *testing.T, d Deps, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &errb}, d)
	code = execute(cmd, args)
	return out.String(), errb.String(), code
}

// S3: doctor uses the probe of Deps (no real gh: PATH holds no gh).
func TestR10DoctorUsesDepsProbe(t *testing.T) {
	e, _ := wtBundle(t)
	t.Setenv("PATH", gitOnlyPath(t))
	d := e.deps()
	called := false
	d.GHProbe = func(context.Context) error { called = true; return nil }
	out, _, code := runDeps(t, d, "doctor")
	if code != 0 || !called || !strings.Contains(out, "OK    gh is installed and authenticated") {
		t.Fatalf("exit %d, called %v:\n%s", code, called, out)
	}
}

// NIT: the help names the minimum git version from gitx.
func TestR10DoctorHelpNamesMinGitVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	out := e.mustRun(t, "doctor", "--help")
	if !strings.Contains(out, gitx.MinGitVersion.String()) {
		t.Fatalf("help:\n%s", out)
	}
}

// NIT: pull request numbers are plain positive decimals.
func TestR10WorktreePRNumberParsing(t *testing.T) {
	t.Parallel()
	e, _ := prBundle(t)
	for _, n := range []string{"+7", "007", "#07", "0", "7x", "#"} {
		if _, errs, code := e.run(t, "wt", "pr", "a", n); code == 0 || !strings.Contains(errs, "invalid pull request number") {
			t.Errorf("%s: exit %d, %s", n, code, errs)
		}
	}
}

// S5: the dry run and JSON list non-blocking risks per candidate.
func TestR10PruneListsNotes(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	primary := filepath.Join(root, "a", "main")
	common := testutil.Git(t, primary, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := writeFileErr(filepath.Join(common, "info", "exclude"), "*.bin\n"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.bin", "b.bin"} {
		if err := writeFileErr(filepath.Join(root, "a", "feat", n), "x"); err != nil {
			t.Fatal(err)
		}
	}
	out := e.mustRun(t, "prune")
	if !strings.Contains(out, "  note: 2 ignored files (deleted with the worktree)") {
		t.Fatalf("dry run:\n%s", out)
	}
	var doc struct {
		Candidates []struct {
			Notes []string `json:"notes"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "prune", "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	// feat's commit is on no remote ref any more: a repository-wide note too.
	if len(doc.Candidates) != 1 || !strings.Contains(strings.Join(doc.Candidates[0].Notes, "\n"), "2 ignored files (deleted with the worktree)") {
		t.Fatalf("doc %+v", doc)
	}
	out = e.mustRun(t, "prune", "--yes")
	if !strings.Contains(out, "  note: 2 ignored files (deleted with the worktree)") {
		t.Fatalf("--yes:\n%s", out)
	}
}
