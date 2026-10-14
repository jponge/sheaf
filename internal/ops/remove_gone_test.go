package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteRepoGoneMatcher(t *testing.T) {
	for stderr, want := range map[string]bool{
		"remote: Repository not found.\nfatal: repository 'https://github.com/o/a.git/' not found":                         true,
		"ERROR: Repository not found.\nfatal: Could not read from remote repository.\n\nPlease make sure ...":              true,
		"fatal: repository 'https://example.com/a.git/' not found":                                                         true,
		"fatal: '/tmp/x/origin.git' does not appear to be a git repository\nfatal: Could not read from remote repository.": true,
		// Not proof that the repository is gone.
		"fatal: 'origin' does not appear to be a git repository\nfatal: Could not read from remote repository.": false,
		"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.":         false,
		"fatal: unable to access 'https://github.com/o/a.git/': Could not resolve host: github.com":             false,
		"": false,
	} {
		if got := remoteRepoGone(stderr); got != want {
			t.Errorf("remoteRepoGone(%q) = %v, want %v", stderr, got, want)
		}
	}
}

// A deleted remote must not be hidden behind a failed-fetch refusal whose
// hint (--offline) leads to deleting the last copy: it is a data risk that
// only --force overrides.
func TestRemoveDeletedRemoteIsADataRisk(t *testing.T) {
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	if err := os.RemoveAll(o.Path); err != nil {
		t.Fatal(err)
	}
	_, err := f.remove(t, "a", false, yes)
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *RisksError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "the remote repository appears to be gone: this clone may hold the only copy of its history") ||
		!strings.Contains(msg, "or you no longer have access (wrong SSH key/account)") ||
		strings.Contains(msg, "--offline") || !strings.Contains(msg, "--force") {
		t.Fatalf("refusal:\n%s", msg)
	}
	for _, it := range re.Plan.Items {
		if it.Hard {
			t.Fatalf("hard item %+v: --force must be able to override a gone remote", it)
		}
	}
	if !exists(primary) {
		t.Fatal("deleted without --force")
	}
	if _, err := f.remove(t, "a", true, yes); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("not deleted with --force")
	}
}
