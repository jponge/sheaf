package cli

import (
	"strings"
	"testing"
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
