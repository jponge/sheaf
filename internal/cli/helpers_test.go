package cli

import (
	"os"
	"os/exec"
	"path/filepath"
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
