//go:build linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// B6 S5: the single-repository commands (remove, wt add, wt add --tag,
// wt rm, wt pr) run git interactively in CLI mode with stdin on a terminal,
// whatever -j: they never run git invocations in parallel.
func TestSingleRepoCommandsInteractiveGitModeIgnoreJobs(t *testing.T) {
	for _, args := range [][]string{
		{"remove", "-j", "4", "--yes", "a"},
		{"wt", "add", "-j", "4", "a", "feat/new"},
		{"wt", "add", "-j", "4", "a", "--tag", "v1"},
		{"wt", "rm", "-j", "4", "--yes", "a", "feat"},
		{"wt", "pr", "-j", "4", "a", "7"},
	} {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			e, root := prBundle(t)
			primary := filepath.Join(root, "a", "main")
			testutil.Git(t, primary, "worktree", "add", "-q", "--track", "-b", "feat", filepath.Join(root, "a", "feat"), "origin/feat")
			testutil.Git(t, primary, "config", "remote.origin.url", "ssh://sheaf-test.invalid/o/a.git")
			testutil.Git(t, primary, "config", "ssh.variant", "ssh")
			rec := coFakeSSH(t)
			coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return true })
			e.run(t, args...) // the fake ssh fails the fetch
			coWantSelfSession(t, name, rec)
			// Still not interactive when stdin is not a terminal.
			os.Remove(rec)
			coSeam(t, &stdinIsTerminal, func(io.Reader) bool { return false })
			e.run(t, args...)
			b, err := os.ReadFile(rec)
			if err != nil {
				t.Fatal("stdin not a terminal: fake ssh never ran")
			}
			if strings.TrimSpace(string(b)) == coSessionOf(t, "self") {
				t.Fatal("stdin not a terminal: ssh ran in sheaf's session")
			}
		})
	}
}
