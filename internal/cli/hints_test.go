package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

// The hints that tell to run a command again with a flag print the complete
// command, with the flags and arguments of the run (global options aside)
// and the flag, shell-quoted, so that it can be pasted (B52 A3). With
// --yes, they name the bundle (B59).
func TestConfirmationHintsPrintTheCommand(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	e.mustRun(t, "wt", "add", "a", "work", "--no-fetch")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--no-tui", "remove", "--offline", "b"}, "run: sheaf --bundle " + root + " remove --offline --yes -- b\n"},
		{[]string{"wt", "rm", "a", "work", "--offline"}, "run: sheaf --bundle " + root + " wt rm --offline --yes -- a work\n"},
		{[]string{"wt", "rm", "--dir=work", "--", "a"}, "run: sheaf --bundle " + root + " wt rm --dir work --yes -- a\n"},
	} {
		_, errs, code := e.run(t, tc.args...)
		if code == 0 || !strings.Contains(errs, "needs confirmation") || !strings.HasSuffix(errs, tc.want) || strings.Contains(errs, "pass --yes") {
			t.Errorf("%v: exit %d, stderr %q; want it to end with %q", tc.args, code, errs, tc.want)
		}
	}
}

func TestPruneDryRunHintPrintsTheCommand(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"prune"}, "dry run: to prune 1 candidate(s), run: sheaf --bundle " + root + " prune --yes\n"},
		{[]string{"prune", "--dry-run", "--offline", "--repo", "a", "--delete-branches"}, "dry run: to prune 1 candidate(s), run: sheaf --bundle " + root + " prune --delete-branches --offline --repo a --yes\n"},
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
// the hint of the previous one (B52 U1 review). A refusal of a command
// confirmed with --yes prints --force hints with --yes and the bundle:
// pasted, they run as stated and remove exactly their target; its --offline
// hint has neither, so the offline plan is confirmed again (B59).
func TestHintCommandsRun(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	other := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "other", "--no-fetch"))
	writeFile(t, filepath.Join(work, "untracked.txt"), "u\n")
	// wt rm: refused (untracked file), then --force --yes, done.
	_, errs, code := e.run(t, "wt", "rm", "a", "work", "--yes")
	if code == 0 || !exists(work) || !strings.Contains(errs, "run: sheaf --bundle "+root+" wt rm --force --dir work --yes -- a\n") {
		t.Fatalf("wt rm: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(work) || !exists(other) || !exists(filepath.Join(root, "a", "main")) {
		t.Fatalf("after the hint: work exists %v, other exists %v (want only work removed)", exists(work), exists(other))
	}
	// wt rm of a dirty worktree with a failed fetch: refused (hard
	// problem, no --force hint), then --offline is refused (untracked
	// file), then --force --offline needs --yes, then done.
	writeFile(t, filepath.Join(other, "untracked.txt"), "u\n")
	third := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "third", "--no-fetch"))
	testutil.Git(t, filepath.Join(root, "a", "main"), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, errs, code = e.run(t, "wt", "rm", "a", "other", "--yes")
	if code == 0 || !strings.Contains(errs, "fetch failed") || strings.Contains(errs, "run: sheaf wt rm --force") ||
		!strings.Contains(errs, "run: sheaf wt rm --offline --dir other -- a\n") {
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
	if exists(other) || !exists(third) {
		t.Fatalf("after the hints: other exists %v, third exists %v (want only other removed)", exists(other), exists(third))
	}
	// remove: refused (dirty), then --force --yes, done.
	writeFile(t, filepath.Join(root, "b", "main", "untracked.txt"), "u\n")
	_, errs, code = e.run(t, "remove", "b", "--yes")
	if code == 0 || !strings.Contains(errs, "run: sheaf --bundle "+root+" remove --force --yes -- b\n") {
		t.Fatalf("remove: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(filepath.Join(root, "b")) || !exists(filepath.Join(root, "a", "main")) || !exists(third) {
		t.Fatal("the hint did not remove exactly b")
	}
}

// The --offline hint of a failed fetch never carries --yes, even when the
// refused command had it: offline, the stale remote-tracking refs that the
// fetch could not refresh count, so a branch deleted on the remote looks
// pushed (or merged into its own stale upstream). Pasted, the hint must stop
// at the confirmation rather than delete it (B59 review: data loss).
func TestOfflineHintOfAFailedFetchNeedsConfirmation(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	// topic: one commit, pushed, then deleted on the remote; the clones
	// keep a stale origin/topic. origin is then unreachable (state unknown,
	// not gone: no transport for the scheme, no network).
	stale := func(repo string) string {
		primary := filepath.Join(root, repo, "main")
		bare := strings.TrimPrefix(strings.TrimSpace(testutil.Git(t, primary, "remote", "get-url", "origin")), "file://")
		testutil.Git(t, primary, "branch", "topic")
		testutil.CommitFile(t, primary, "f.txt", "f\n", "topic work") // on main: moved to topic below
		testutil.Git(t, primary, "branch", "-f", "topic", "HEAD")
		testutil.Git(t, primary, "reset", "-q", "--hard", "HEAD~1")
		testutil.Git(t, primary, "push", "-q", "origin", "topic")
		testutil.Git(t, primary, "fetch", "-q", "origin")
		testutil.Git(t, bare, "branch", "-q", "-D", "topic")
		testutil.Git(t, primary, "branch", "--set-upstream-to=origin/topic", "topic")
		testutil.Git(t, primary, "remote", "set-url", "origin", "sheaf-no-such-transport://example.invalid/"+repo+".git")
		return primary
	}
	// remove: the unpushed topic would count as pushed offline.
	stale("a")
	_, errs, code := e.run(t, "remove", "a", "--yes")
	if code == 0 || !strings.Contains(errs, "fetch failed") {
		t.Fatalf("remove: exit %d\n%s", code, errs)
	}
	hint := errs
	_, errs, code = e.run(t, hintArgs(t, hint)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") || !exists(filepath.Join(root, "a", "main")) {
		t.Fatalf("pasted --offline hint: exit %d, a exists %v\n%s", code, exists(filepath.Join(root, "a", "main")), errs)
	}
	if !strings.HasSuffix(hint, "run: sheaf remove --offline -- a\n") {
		t.Fatalf("remove: hint\n%s", hint)
	}
	// wt rm -d: topic would count as merged into its stale upstream offline.
	primary := stale("b")
	e.mustRun(t, "wt", "add", "b", "topic", "--no-fetch")
	_, errs, code = e.run(t, "wt", "rm", "-d", "b", "topic", "--yes")
	if code == 0 || !strings.Contains(errs, "fetch failed") {
		t.Fatalf("wt rm -d: exit %d\n%s", code, errs)
	}
	hint = errs
	_, errs, code = e.run(t, hintArgs(t, hint)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") || testutil.Git(t, primary, "branch", "--list", "topic") == "" {
		t.Fatalf("pasted --offline hint: exit %d, branch topic deleted\n%s", code, errs)
	}
	if !strings.HasSuffix(hint, "run: sheaf wt rm -d --offline --dir topic -- b\n") {
		t.Fatalf("wt rm -d: hint\n%s", hint)
	}
}

// A hint with --yes names the bundle even without --bundle: pasted in
// another bundle that has a repository of the same name, it acts on the
// bundle that printed it, never on that other one (B59).
func TestYesHintsActOnTheirBundle(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	writeFile(t, filepath.Join(work, "untracked.txt"), "u\n")
	writeFile(t, filepath.Join(root, "b", "main", "untracked.txt"), "u\n")
	root2 := filepath.Join(t.TempDir(), "other")
	e.mustRun(t, "create", root2)
	root2, _ = filepath.EvalSymlinks(root2)
	e.cwd = root2
	e.mustRun(t, "add", "o/a", "o/b")
	work2 := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	writeFile(t, filepath.Join(work2, "untracked.txt"), "u\n")
	for _, tc := range []struct {
		args       []string
		gone, kept string
	}{
		{[]string{"wt", "rm", "a", "work", "--yes"}, work, work2},
		{[]string{"remove", "b", "--yes"}, filepath.Join(root, "b"), filepath.Join(root2, "b", "main")},
	} {
		e.cwd = root
		_, errs, code := e.run(t, tc.args...)
		if code == 0 || !strings.Contains(errs, "run: sheaf --bundle "+root+" ") {
			t.Fatalf("%v: exit %d\n%s", tc.args, code, errs)
		}
		e.cwd = root2
		e.mustRun(t, hintArgs(t, errs)...)
		if exists(tc.gone) || !exists(tc.kept) {
			t.Fatalf("%v pasted in the other bundle: %s exists %v, %s exists %v", tc.args, tc.gone, exists(tc.gone), tc.kept, exists(tc.kept))
		}
	}
}

// Without --yes, the hints of a refusal omit it (a pasted command shows the
// TUI confirmation dialog, B52); in plain mode the pasted command asks for
// --yes with the complete command, which then runs.
func TestRefusalHintsWithoutYes(t *testing.T) {
	t.Parallel()
	e, root := worktreeBundle(t)
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	writeFile(t, filepath.Join(work, "untracked.txt"), "u\n")
	_, errs, code := e.run(t, "wt", "rm", "a", "work")
	if code == 0 || !strings.HasSuffix(errs, "run: sheaf wt rm --force --dir work -- a\n") {
		t.Fatalf("wt rm: exit %d\n%s", code, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") || !strings.HasSuffix(errs, "run: sheaf --bundle "+root+" wt rm --force --dir work --yes -- a\n") {
		t.Fatalf("hint of the refusal: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(work) {
		t.Fatal("work not removed by the hints")
	}
}

// In TUI mode, the hints omit --yes even when the refused command had it:
// the pasted command shows its confirmation dialog (B52, B59).
func TestTUIRefusalHintsOmitYes(t *testing.T) {
	e, _ := worktreeBundle(t)
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	writeFile(t, filepath.Join(work, "untracked.txt"), "u\n")
	var out syncBuffer
	errs, code := e.runTUI(t, newScript(t, false), &out, "wt", "rm", "--yes", "a", "work")
	if code == 0 || !strings.Contains(errs, "run: sheaf wt rm --force --dir work -- a\n") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
}

// prune --yes keeps a dirty worktree with a hint that repeats --yes (and
// names the bundle); the pasted hint refuses with its own --force --yes
// hint, which removes exactly that worktree (B59).
func TestPruneHintsRunWithYes(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	feat := filepath.Join(root, "a", "feat")
	keep := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "keep", "--no-fetch"))
	writeFile(t, filepath.Join(feat, "untracked.txt"), "u\n")
	out, errs, _ := e.run(t, "prune", "--yes")
	if !strings.Contains(out+errs, "review it, then run: sheaf --bundle "+root+" wt rm --dir feat --yes -- a\n") || !exists(feat) {
		t.Fatalf("prune --yes: stdout\n%s\nstderr\n%s", out, errs)
	}
	_, errs, code := e.run(t, hintArgs(t, out+errs)...)
	if code == 0 || !strings.HasSuffix(errs, "run: sheaf --bundle "+root+" wt rm --force --dir feat --yes -- a\n") {
		t.Fatalf("hint of prune: exit %d\n%s", code, errs)
	}
	e.mustRun(t, hintArgs(t, errs)...)
	if exists(feat) || !exists(keep) || !exists(filepath.Join(root, "a", "main")) {
		t.Fatalf("after the hints: feat exists %v, keep exists %v (want only feat removed)", exists(feat), exists(keep))
	}
}

// sync --remove-gone refuses a gone repository whose remote is gone with a
// remove hint without --yes: sync's --yes confirmed removing gone
// repositories, not forcing past their risks; pasted, the hint asks for
// confirmation (B59).
func TestSyncRemoveGoneHintOmitsYes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.origin(t, "o", "a", false)
	e.origin(t, "o", "c", false)
	root := filepath.Join(t.TempDir(), "bundle")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	e.mustRun(t, "add", "--org", "o", "--repo", "a", "--repo", "c")
	if err := os.RemoveAll(o.Path); err != nil {
		t.Fatal(err)
	}
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "c", false)
	out, errs, code := e.run(t, "sync", "--remove-gone", "--yes")
	if code != 1 || !strings.Contains(errs, "run: sheaf remove --force -- a\n") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	_, errs, code = e.run(t, hintArgs(t, errs)...)
	if code == 0 || !strings.Contains(errs, "needs confirmation") || !exists(filepath.Join(root, "a", "main")) {
		t.Fatalf("pasted hint: exit %d\n%s", code, errs)
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
	// Without --bundle, no --bundle in a hint without --yes (one with
	// --yes always names the bundle, B59).
	e.cwd = root
	x := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "x", "--no-fetch"))
	writeFile(t, filepath.Join(x, "untracked.txt"), "u\n")
	if _, errs, _ := e.run(t, "wt", "rm", "a", "x"); !strings.HasSuffix(errs, "run: sheaf wt rm --force --dir x -- a\n") {
		t.Errorf("stderr %q", errs)
	}
}

// The "needs confirmation" hints of plain mode (remove, wt rm) and the
// dry-run hint of prune carry --yes, so they name the bundle even without
// --bundle: pasted in another bundle with repositories of the same names,
// they act on the bundle that printed them (B59).
func TestConfirmationHintsActOnTheirBundle(t *testing.T) {
	t.Parallel()
	e, root, _ := pruneBundle(t)
	e.origin(t, "o", "b", false)
	e.mustRun(t, "add", "o/b")
	feat := filepath.Join(root, "a", "feat")
	work := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	root2 := filepath.Join(t.TempDir(), "other")
	e.mustRun(t, "create", root2)
	root2, _ = filepath.EvalSymlinks(root2)
	e.cwd = root2
	e.mustRun(t, "add", "o/a", "o/b")
	// feat no longer exists on the remote: a new branch, not a candidate.
	feat2 := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "feat", "--no-fetch"))
	work2 := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "work", "--no-fetch"))
	for _, tc := range []struct {
		args       []string
		gone, kept string
	}{
		{[]string{"prune"}, feat, feat2},
		{[]string{"wt", "rm", "a", "work"}, work, work2},
		{[]string{"remove", "b"}, filepath.Join(root, "b"), filepath.Join(root2, "b", "main")},
	} {
		e.cwd = root
		out, errs, _ := e.run(t, tc.args...)
		if !strings.Contains(out+errs, "run: sheaf --bundle "+root+" ") {
			t.Fatalf("%v: stdout\n%s\nstderr\n%s", tc.args, out, errs)
		}
		e.cwd = root2
		e.mustRun(t, hintArgs(t, out+errs)...)
		if exists(tc.gone) || !exists(tc.kept) {
			t.Fatalf("%v pasted in the other bundle: %s exists %v, %s exists %v", tc.args, tc.gone, exists(tc.gone), tc.kept, exists(tc.kept))
		}
	}
}
