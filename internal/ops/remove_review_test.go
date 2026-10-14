package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func (f *fixture) removeOpts(t *testing.T, name string, opts RemoveOptions) ([]Event, error) {
	t.Helper()
	var evs []Event
	_, err := Remove(t.Context(), f.w, name, opts, func(e Event) { evs = append(evs, e) })
	return evs, err
}

// B2: a repository nested in an ignored directory is only "ignored files"
// for git status; remove must refuse to delete it without --force.
func TestRemoveRefusesNestedCloneInIgnoredDir(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "vendor/\n")
	nested := filepath.Join(primary, "vendor", "lib")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), nested)
	testutil.CommitFile(t, nested, "precious.txt", "x\n", "unpushed commit in nested repo")
	_, err := f.remove(t, "a", false, yes)
	var re *RisksError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "nested repositories") {
		t.Fatalf("err = %v, want refusal for the nested repository", err)
	}
	if !exists(filepath.Join(nested, "precious.txt")) {
		t.Fatal("nested repository deleted")
	}
}

// A clean, fully pushed nested repository is shown but does not block.
func TestRemoveCleanNestedRepoIsInformational(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "vendor/\n")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), filepath.Join(primary, "vendor", "lib"))
	var plan RemovePlan
	if _, err := f.remove(t, "a", false, func(p RemovePlan) bool { plan = p; return true }); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !slices.Contains(riskKinds(plan.Items[0]), gitx.RiskNestedRepositoryClean) {
		t.Fatalf("plan = %+v, want the clean nested repository listed", plan.Items)
	}
}

func TestRemoveRefusesNestedWorktreeOfAnotherRepo(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	other := filepath.Join(t.TempDir(), "other")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}}), other)
	wt := filepath.Join(primary, "scratch", "wt")
	testutil.Git(t, other, "worktree", "add", "-q", wt, "feat")
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
	_, err := f.remove(t, "a", false, yes)
	if err == nil || !strings.Contains(err.Error(), "nested repositories") {
		t.Fatalf("err = %v, want refusal for the nested worktree", err)
	}
	if !exists(filepath.Join(wt, ".git")) {
		t.Fatal("nested worktree deleted")
	}
}

// S2: no or several primary checkouts are hard problems, even with --force.
func TestRemoveAmbiguousOrMissingPrimaryIsHard(t *testing.T) {
	t.Run("several primaries", func(t *testing.T) {
		f := newFixture(t)
		primary, o := f.clonedRepo(t, "a")
		other := filepath.Join(f.w.Root, "a", "old")
		plainClone(t, o, other)
		testutil.CommitFile(t, other, "unpushed.txt", "x\n", "unpushed work")
		_, err := f.remove(t, "a", true, yes)
		var re *RisksError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), primary) || !strings.Contains(err.Error(), other) {
			t.Fatalf("err = %v, want hard refusal listing %s and %s", err, primary, other)
		}
		if !exists(filepath.Join(other, "unpushed.txt")) {
			t.Fatal("data deleted")
		}
	})
	t.Run("no primary", func(t *testing.T) {
		f := newFixture(t)
		f.clonedRepo(t, "a")
		if err := os.RemoveAll(filepath.Join(f.w.Root, "a", "main")); err != nil {
			t.Fatal(err)
		}
		notes := filepath.Join(f.w.Root, "a", "notes.txt")
		writeFile(t, notes, "mine\n")
		_, err := f.remove(t, "a", true, yes)
		var re *RisksError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), "no primary checkout") {
			t.Fatalf("err = %v, want hard refusal (no primary)", err)
		}
		if !exists(notes) {
			t.Fatal("data deleted")
		}
	})
	t.Run("empty directory is removed", func(t *testing.T) {
		f := newFixture(t)
		f.clonedRepo(t, "a")
		if err := os.RemoveAll(filepath.Join(f.w.Root, "a", "main")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.w.Root, "a", ".DS_Store"), "")
		if _, err := f.remove(t, "a", false, yes); err != nil {
			t.Fatalf("removing an empty repository directory: %v", err)
		}
		if exists(filepath.Join(f.w.Root, "a")) {
			t.Fatal("directory kept")
		}
	})
}

// S3: anything done while Confirm is pending is checked again.
func TestRemoveRechecksAfterConfirm(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	_, err := f.remove(t, "a", false, func(RemovePlan) bool {
		writeFile(t, filepath.Join(primary, "written-during-dialog.txt"), "wip\n")
		testutil.CommitFile(t, primary, "c.txt", "c\n", "commit made during dialog")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, want abort because the repository changed", err)
	}
	if !exists(filepath.Join(primary, "written-during-dialog.txt")) {
		t.Fatal("work done during confirmation was deleted")
	}
}

// S6: OS junk is not an unknown entry.
func TestRemoveIgnoresOSJunk(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	for _, n := range []string{".DS_Store", "Thumbs.db", "desktop.ini"} {
		writeFile(t, filepath.Join(f.w.Root, "a", n), "junk")
	}
	if _, err := f.remove(t, "a", false, yes); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

// S6: a failed fetch is refused with a hint to use --offline, which --force
// does not replace; --offline skips the fetch with a warning, but still
// checks local data.
func TestRemoveOffline(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	// An unreachable remote whose state is unknown (no transport for the
	// scheme; never touches the network). A missing local path would be
	// classified as a gone remote instead (TestRemoveDeletedRemoteIsADataRisk).
	testutil.Git(t, primary, "remote", "set-url", "origin", "sheaf-no-such-transport://example.invalid/a.git")
	for _, force := range []bool{false, true} {
		_, err := f.removeOpts(t, "a", RemoveOptions{Force: force, Confirm: yes})
		if err == nil || !strings.Contains(err.Error(), "--offline") {
			t.Fatalf("force=%v: err = %v, want refusal hinting at --offline", force, err)
		}
	}
	writeFile(t, filepath.Join(primary, "wip.txt"), "wip\n")
	if _, err := f.removeOpts(t, "a", RemoveOptions{Offline: true, Confirm: yes}); err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatalf("offline with untracked file: err = %v, want refusal", err)
	}
	if err := os.Remove(filepath.Join(primary, "wip.txt")); err != nil {
		t.Fatal(err)
	}
	evs, err := f.removeOpts(t, "a", RemoveOptions{Offline: true, Confirm: yes})
	if err != nil {
		t.Fatalf("offline remove: %v", err)
	}
	if slices.ContainsFunc(evs, func(e Event) bool { return e.Phase == PhaseFetch && e.Message == "fetching origin" }) {
		t.Fatal("offline remove fetched")
	}
	if !slices.ContainsFunc(evs, func(e Event) bool { return strings.Contains(e.Message, "stale") }) {
		t.Fatalf("no warning about stale remote state: %+v", evs)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("not removed")
	}
}

// NIT: never delete the process's working directory.
func TestRemoveRefusesWhenWorkingDirectoryInside(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	t.Chdir(primary)
	_, err := f.remove(t, "a", true, yes)
	var re *RisksError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("err = %v, want hard refusal about the working directory", err)
	}
	if !exists(primary) {
		t.Fatal("removed")
	}
}
