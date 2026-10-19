package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// The hints that tell to run a command again with a flag print the complete
// command, with the flags and arguments of the run (global options aside)
// and the flag, shell-quoted, so that it can be pasted (B52 A3).
func TestConfirmationHintsPrintTheCommand(t *testing.T) {
	t.Parallel()
	e, _ := worktreeBundle(t)
	e.mustRun(t, "wt", "add", "a", "work", "--no-fetch")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--no-tui", "remove", "--offline", "b"}, "run: sheaf remove --offline --yes -- b\n"},
		{[]string{"wt", "rm", "a", "work", "--offline"}, "run: sheaf wt rm --offline --yes -- a work\n"},
		{[]string{"wt", "rm", "--dir=work", "--", "a"}, "run: sheaf wt rm --dir work --yes -- a\n"},
	} {
		_, errs, code := e.run(t, tc.args...)
		if code == 0 || !strings.Contains(errs, "needs confirmation") || !strings.HasSuffix(errs, tc.want) || strings.Contains(errs, "pass --yes") {
			t.Errorf("%v: exit %d, stderr %q; want it to end with %q", tc.args, code, errs, tc.want)
		}
	}
}

func TestPruneDryRunHintPrintsTheCommand(t *testing.T) {
	t.Parallel()
	e, _, _ := pruneBundle(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"prune"}, "dry run: to prune 1 candidate(s), run: sheaf prune --yes\n"},
		{[]string{"prune", "--dry-run", "--offline", "--repo", "a", "--delete-branches"}, "dry run: to prune 1 candidate(s), run: sheaf prune --delete-branches --offline --repo a --yes\n"},
	} {
		out, errs, code := e.run(t, tc.args...)
		if code != 0 || !strings.HasSuffix(out, tc.want) {
			t.Errorf("%v: exit %d, stdout %q (stderr %q); want it to end with %q", tc.args, code, out, errs, tc.want)
		}
	}
}

// hintArgs returns the arguments of the command of the last "run: sheaf "
// hint of s (to the end of its line), which must need no shell quoting.
func hintArgs(t *testing.T, s string) []string {
	t.Helper()
	i := strings.LastIndex(s, "run: sheaf ")
	if i < 0 {
		t.Fatalf("no hint in:\n%s", s)
	}
	line, _, _ := strings.Cut(s[i+len("run: sheaf "):], "\n")
	if strings.ContainsAny(line, `'"\`) {
		t.Fatalf("hint %q needs a shell to parse", line)
	}
	return strings.Fields(line)
}

// The commands that hints print do what they say when run: each step runs
// the hint of the previous one (B52 U1 review).
func TestHintCommandsRun(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	writeFile(t, filepath.Join(work, "untracked.txt"), "u\n")
	// wt rm: refused (untracked file), then --force needs --yes, then done.
	_, errs, code := e.run(t, "wt", "rm", "a", "work", "--yes")
	if code == 0 || !exists(work) {
		t.Fatalf("wt rm: exit %d\n%s", code, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") {
		t.Fatalf("hint of the refusal: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(work) {
		t.Fatal("work not removed by the hints")
	}
	// wt rm of a dirty worktree with a failed fetch: refused (hard
	// problem, no --force hint), then --offline is refused (untracked
	// file), then --force --offline needs --yes, then done.
	other := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "other", "--no-fetch"))
	writeFile(t, filepath.Join(other, "untracked.txt"), "u\n")
	testutil.Git(t, filepath.Join(root, "a", "main"), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, errs, code = e.run(t, "wt", "rm", "a", "other", "--yes")
	if code == 0 || !strings.Contains(errs, "fetch failed") || strings.Contains(errs, "run: sheaf wt rm --force") {
		t.Fatalf("wt rm, fetch failed: exit %d\n%s", code, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "untracked") || !strings.Contains(errs, "run: sheaf wt rm --force --offline --dir other -- a\n") {
		t.Fatalf("hint of the fetch failure: exit %d\n%s", code, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") {
		t.Fatalf("hint of the offline refusal: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(other) {
		t.Fatal("other not removed by the hints")
	}
	// remove: refused (dirty), then --force needs --yes, then done.
	writeFile(t, filepath.Join(root, "b", "main", "untracked.txt"), "u\n")
	_, errs, code = e.run(t, "remove", "b", "--yes")
	if code == 0 {
		t.Fatalf("remove: exit %d\n%s", code, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") {
		t.Fatalf("hint of the remove refusal: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(filepath.Join(root, "b")) {
		t.Fatal("b not removed by the hints")
	}
}

// With --bundle, the hints name the bundle, so that they act on it when
// pasted elsewhere, e.g. in another bundle (B52 U1 review).
func TestHintsNameAnExplicitBundle(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	e.cwd = t.TempDir()
	if err := os.WriteFile(filepath.Join(e.cwd, "sheaf.yaml"), []byte("repositories: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errs, code := e.run(t, "--bundle", root, "prune")
	if want := "run: sheaf --bundle " + root + " prune --yes\n"; code != 0 || !strings.HasSuffix(out, want) {
		t.Fatalf("exit %d, stdout %q (stderr %q); want it to end with %q", code, out, errs, want)
	}
	e.mustRun(t, hintArgs(t, out)...)
	if exists(filepath.Join(root, "a", "feat")) {
		t.Fatal("the hint did not prune the bundle")
	}
	// Without --bundle, no --bundle.
	e.cwd = root
	e.mustRun(t, "wt", "add", "a", "x", "--no-fetch")
	if _, errs, _ := e.run(t, "wt", "rm", "a", "x"); !strings.HasSuffix(errs, "run: sheaf wt rm --yes -- a x\n") {
		t.Errorf("stderr %q", errs)
	}
}
