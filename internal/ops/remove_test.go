package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// clonedRepo adds o/<name> through Add and returns its primary checkout.
func (f *fixture) clonedRepo(t *testing.T, name string) (string, testutil.Origin) {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}})
	f.fake.AddRepo(hostingRepo("o", name, o.URL))
	if s, err := f.add(t, AddRequest{Slugs: []string{"o/" + name}}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	return filepath.Join(f.w.Root, name, "main"), o
}

func yes(RemovePlan) bool { return true }

func (f *fixture) remove(t *testing.T, name string, force bool, confirm func(RemovePlan) bool) ([]Event, error) {
	t.Helper()
	var evs []Event
	_, err := Remove(context.Background(), f.w, name, RemoveOptions{Force: force, Confirm: confirm}, func(e Event) { evs = append(evs, e) })
	return evs, err
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func riskKinds(c RemoveItem) []gitx.RiskKind {
	var ks []gitx.RiskKind
	for _, r := range c.Risks {
		ks = append(ks, r.Kind)
	}
	return ks
}

func TestRemoveCleanRepoWithWorktree(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "worktree", "add", "-q", "../feat", "feat")
	var plan RemovePlan
	evs, err := f.remove(t, "a", false, func(p RemovePlan) bool { plan = p; return true })
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("repository directory still exists")
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("entry still recorded")
	}
	var paths []string
	for _, it := range plan.Items {
		paths = append(paths, it.Path)
	}
	if want := []string{primary, filepath.Join(f.w.Root, "a", "feat")}; !slices.Equal(paths, want) {
		t.Fatalf("plan paths = %v, want %v", paths, want)
	}
	if !slices.ContainsFunc(evs, func(e Event) bool { return e.Phase == PhaseFetch }) {
		t.Fatalf("no fetch event: %+v", evs)
	}
	// The bundle root and sheaf.yaml survive.
	if !exists(filepath.Join(f.w.Root, bundle.FileName)) {
		t.Fatal("sheaf.yaml removed")
	}
}

func TestRemoveFetchesBeforeCheckingRisks(t *testing.T) {
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	// topic is pushed, so the local remote-tracking ref makes it look safe...
	testutil.Git(t, primary, "checkout", "-q", "-b", "topic")
	testutil.CommitFile(t, primary, "t.txt", "t\n", "topic work")
	testutil.Git(t, primary, "push", "-q", "origin", "topic")
	testutil.Git(t, primary, "checkout", "-q", "main")
	// ...but it was deleted on the remote: only a fetch reveals the risk.
	testutil.Git(t, o.Path, "branch", "-q", "-D", "topic")

	_, err := f.remove(t, "a", false, yes)
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want RisksError", err)
	}
	if !slices.Contains(riskKinds(re.Plan.Items[0]), gitx.RiskUnpushedBranchCommits) {
		t.Fatalf("primary risks = %+v, want unpushed commits (fetch --prune before the check)", re.Plan.Items[0])
	}
	if !exists(primary) {
		t.Fatal("refused removal deleted files")
	}
}

func TestRemoveRefusesRisksUnlessForced(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := filepath.Join(f.w.Root, "a", "feat")
	testutil.Git(t, primary, "worktree", "add", "-q", wt, "feat")
	writeFile(t, filepath.Join(wt, "scratch.txt"), "wip\n")
	writeFile(t, filepath.Join(primary, "file-1.txt"), "changed\n")
	testutil.Git(t, primary, "stash", "-q")

	confirmed := false
	_, err := f.remove(t, "a", false, func(RemovePlan) bool { confirmed = true; return true })
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want RisksError", err)
	}
	if confirmed {
		t.Fatal("Confirm called although removal is refused")
	}
	msg := err.Error()
	for _, want := range []string{wt, "untracked", "stash", "--force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message lacks %q:\n%s", want, msg)
		}
	}
	// Repository-wide risks are reported once, on the primary checkout.
	if ks := riskKinds(re.Plan.Items[0]); !slices.Contains(ks, gitx.RiskStash) {
		t.Fatalf("primary risks = %v, want stash", ks)
	}
	if ks := riskKinds(re.Plan.Items[1]); slices.Contains(ks, gitx.RiskStash) || !slices.Contains(ks, gitx.RiskUntracked) {
		t.Fatalf("worktree risks = %v, want untracked and no stash", ks)
	}
	if !exists(wt) || len(f.onDisk(t).Repositories) != 1 {
		t.Fatal("refused removal changed something")
	}

	if _, err := f.remove(t, "a", true, yes); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if exists(filepath.Join(f.w.Root, "a")) || len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("forced removal incomplete")
	}
}

func TestRemoveNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	for name, c := range map[string]func(RemovePlan) bool{"nil": nil, "no": func(RemovePlan) bool { return false }} {
		if _, err := f.remove(t, "a", false, c); !errors.Is(err, ErrNotConfirmed) {
			t.Fatalf("%s confirm: err = %v, want ErrNotConfirmed", name, err)
		}
	}
	if !exists(primary) || len(f.onDisk(t).Repositories) != 1 {
		t.Fatal("unconfirmed removal changed something")
	}
}

func TestRemoveRefusesUnknownEntriesUnlessForced(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	notes := filepath.Join(f.w.Root, "a", "notes")
	if err := os.Mkdir(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := f.remove(t, "a", false, yes)
	var re *RisksError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), notes) {
		t.Fatalf("err = %v, want refusal naming %s", err, notes)
	}
	if _, err := f.remove(t, "a", true, yes); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveNeverDeletesOutsideTheRepositoryDirectory(t *testing.T) {
	t.Run("worktree outside", func(t *testing.T) {
		f := newFixture(t)
		primary, _ := f.clonedRepo(t, "a")
		outside := filepath.Join(resolved(t, t.TempDir()), "wt")
		testutil.Git(t, primary, "worktree", "add", "-q", outside, "feat")
		_, err := f.remove(t, "a", true, yes)
		var re *RisksError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), outside) {
			t.Fatalf("err = %v, want hard refusal naming %s", err, outside)
		}
		if !exists(filepath.Join(outside, ".git")) || !exists(primary) {
			t.Fatal("something was deleted")
		}
	})
	t.Run("repository directory is a symlink", func(t *testing.T) {
		f := newFixture(t)
		o := testutil.NewOrigin(t, testutil.OriginOpts{})
		target := filepath.Join(t.TempDir(), "victim")
		plainClone(t, o, filepath.Join(target, "main"))
		if err := os.Symlink(target, filepath.Join(f.w.Root, "a")); err != nil {
			t.Fatal(err)
		}
		if err := f.w.update(func(b *bundle.Bundle) error {
			return b.AddRepository(bundle.Repository{Name: "a", Slug: "o/a", URL: o.URL, DefaultBranch: "main", Origin: "explicit"})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.remove(t, "a", true, yes); err == nil {
			t.Fatal("removal through a symlink succeeded")
		}
		if !exists(filepath.Join(target, "main", ".git")) {
			t.Fatal("symlink target was deleted")
		}
	})
	t.Run("primary checkout is a symlink", func(t *testing.T) {
		f := newFixture(t)
		o := testutil.NewOrigin(t, testutil.OriginOpts{})
		target := filepath.Join(t.TempDir(), "victim")
		plainClone(t, o, target)
		if err := os.Mkdir(filepath.Join(f.w.Root, "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(f.w.Root, "a", "main")); err != nil {
			t.Fatal(err)
		}
		if err := f.w.update(func(b *bundle.Bundle) error {
			return b.AddRepository(bundle.Repository{Name: "a", Slug: "o/a", URL: o.URL, DefaultBranch: "main", Origin: "explicit"})
		}); err != nil {
			t.Fatal(err)
		}
		// Forced: the link itself may go, never its target.
		_, _ = f.remove(t, "a", true, yes)
		if !exists(filepath.Join(target, ".git")) || !exists(filepath.Join(target, "file-1.txt")) {
			t.Fatal("symlink target was deleted")
		}
	})
}

func TestRemoveUnknownAndMissing(t *testing.T) {
	f := newFixture(t)
	if _, err := f.remove(t, "nope", true, yes); err == nil {
		t.Fatal("removing an unknown repository succeeded")
	}
	if err := f.w.update(func(b *bundle.Bundle) error {
		return b.AddRepository(bundle.Repository{Name: "gone", Slug: "o/gone", URL: "file:///x", DefaultBranch: "main", Origin: "explicit"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.remove(t, "gone", false, yes); err != nil {
		t.Fatalf("removing an entry without directory: %v", err)
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("entry kept")
	}
}

func TestRemoveRefusesHiddenModifications(t *testing.T) {
	for _, where := range []string{"primary", "worktree"} {
		t.Run(where, func(t *testing.T) {
			f := newFixture(t)
			primary, _ := f.clonedRepo(t, "a")
			wt := filepath.Join(f.w.Root, "a", "feat")
			testutil.Git(t, primary, "worktree", "add", "-q", wt, "feat")
			dir, idx := primary, 0
			if where == "worktree" {
				dir, idx = wt, 1
			}
			testutil.Git(t, dir, "update-index", "--skip-worktree", "file-1.txt")
			writeFile(t, filepath.Join(dir, "file-1.txt"), "hidden change\n")

			_, err := f.remove(t, "a", false, yes)
			var re *RisksError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want RisksError", err)
			}
			if ks := riskKinds(re.Plan.Items[idx]); !slices.Contains(ks, gitx.RiskHiddenModifications) {
				t.Fatalf("%s risks = %v, want hidden modifications", where, ks)
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "hidden modifications") {
				t.Fatalf("refusal message lacks %s / hidden modifications:\n%v", dir, err)
			}
			if !exists(filepath.Join(dir, "file-1.txt")) {
				t.Fatal("refused removal deleted data")
			}
		})
	}
}

func TestRemoveTagOnlyCommitsAreInformational(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	// A release tag on a commit that is on no branch of origin.
	work := filepath.Join(t.TempDir(), "work")
	testutil.Git(t, filepath.Dir(work), "clone", "-q", o.URL, work)
	testutil.Git(t, work, "checkout", "-q", "--detach")
	testutil.CommitFile(t, work, "release.txt", "1.0\n", "release 1.0")
	testutil.Git(t, work, "tag", "v1.0")
	testutil.Git(t, work, "push", "-q", "origin", "v1.0")
	f.fake.AddRepo(hostingRepo("o", "a", o.URL))
	if s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}

	var plan RemovePlan
	if _, err := f.remove(t, "a", false, func(p RemovePlan) bool { plan = p; return true }); err != nil {
		t.Fatalf("remove without --force: %v", err)
	}
	if len(plan.Items) == 0 || !slices.Contains(riskKinds(plan.Items[0]), gitx.RiskTagOnlyCommits) {
		t.Fatalf("plan = %+v, want the tag-only commit listed (informational)", plan)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("repository not removed")
	}
}

func TestRemoveStaleLockOnFetch(t *testing.T) {
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	testutil.AdvanceOrigin(t, o, "main", "new.txt", "new\n", "advance")
	lock := filepath.Join(primary, ".git", "refs", "remotes", "origin", "main.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, lock, "")

	_, err := f.remove(t, "a", false, yes)
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want RisksError (fetch failed)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "stale lock file "+lock) || !strings.Contains(msg, "delete it") {
		t.Fatalf("refusal message lacks the lock path %s and a hint to delete it:\n%s", lock, msg)
	}
}
