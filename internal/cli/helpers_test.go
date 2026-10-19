package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// gitOnlyPath returns a PATH value with only a link to git, so that no real
// gh can be found.
func gitOnlyPath(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := filepath.Base(git)
	if err := os.Symlink(git, filepath.Join(dir, name)); err != nil {
		t.Skipf("cannot link git: %v", err)
	}
	return dir
}

func writeFileErr(p, content string) error { return os.WriteFile(p, []byte(content), 0o644) }

// repChar matches a printable ASCII character followed by a REP sequence
// (CSI n b: repeat it n times, once without n), which bubbletea's renderer
// writes for a run of that character (e.g. in a path).
var repChar = regexp.MustCompile("([\x20-\x7e])\x1b\\[([0-9]*)b")

// expandREP returns the output s of a TUI with its REP sequences replaced
// by the characters they repeat, so that text is found whatever its runs
// of a repeated character.
func expandREP(s string) string {
	return repChar.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.Atoi(repChar.FindStringSubmatch(m)[2])
		if err != nil || n < 1 {
			n = 1
		}
		return strings.Repeat(m[:1], n+1)
	})
}

func TestExpandREP(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/tmp/x\x1b[48b/b": "/tmp/" + strings.Repeat("x", 49) + "/b",
		"ab\x1b[bc":        "abbc",
		"plain":            "plain",
		"\x1b[33mok\x1b[m": "\x1b[33mok\x1b[m",
	} {
		if got := expandREP(in); got != want {
			t.Errorf("expandREP(%q) = %q, want %q", in, got, want)
		}
	}
}
