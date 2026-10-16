package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// B6 S2: a locked worktree is a hard refusal, like in wt rm: --force does
// not delete it.
func TestRemoveRefusesLockedWorktreeEvenWithForce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := filepath.Join(f.w.Root, "a", "feat")
	testutil.Git(t, primary, "worktree", "add", "-q", wt, "feat")
	testutil.Git(t, primary, "worktree", "lock", "--reason", "on a USB stick", wt)
	evs, err := f.remove(t, "a", true, yes)
	var re *RisksError
	if !errors.As(err, &re) || !re.Plan.Refused(true) {
		t.Fatalf("err = %v, want a hard refusal", err)
	}
	msg := err.Error()
	for _, want := range []string{wt, "locked (on a USB stick)", "git worktree unlock " + wt, "cannot be overridden with --force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
	if !exists(filepath.Join(wt, ".git")) || len(f.onDisk(t).Repositories) != 1 {
		t.Fatalf("refused removal changed something; events:\n%s", messages(evs))
	}
}

// B6 S3: a worktree of another repository directly inside the repository
// directory is a hard refusal (deleting it would lose its changes and leave
// the other repository's administrative files dangling), like a worktree of
// this repository outside it.
func TestRemoveRefusesOtherRepositoryWorktreeInsideEvenWithForce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	other := filepath.Join(t.TempDir(), "other")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}}), other)
	for _, rel := range []string{"intruder", filepath.Join("box", "deeper")} {
		wt := filepath.Join(f.w.Root, "a", rel)
		testutil.Git(t, other, "worktree", "add", "-q", "-b", "w-"+filepath.Base(rel), wt, "origin/feat")
		writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
	}
	_, err := f.remove(t, "a", true, yes)
	var re *RisksError
	if !errors.As(err, &re) || !re.Plan.Refused(true) {
		t.Fatalf("err = %v, want a hard refusal", err)
	}
	msg := err.Error()
	for _, rel := range []string{"intruder", filepath.Join("box", "deeper")} {
		wt := filepath.Join(f.w.Root, "a", rel)
		if !strings.Contains(msg, wt+": worktree of another repository") || !exists(filepath.Join(wt, "wip.txt")) {
			t.Errorf("%s not refused as another repository's worktree, or deleted:\n%s", wt, msg)
		}
	}
	if !strings.Contains(msg, "remove or move it first") {
		t.Errorf("refusal lacks the hint:\n%s", msg)
	}
}

// A leftover worktree of the repository itself (its administrative files
// gone, so git no longer lists it) stays an unknown entry that --force
// deletes.
func TestRemoveOwnUnlistedWorktreeIsForceable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := filepath.Join(f.w.Root, "a", "stale")
	testutil.Git(t, primary, "worktree", "add", "-q", wt, "feat")
	if err := os.RemoveAll(filepath.Join(primary, ".git", "worktrees", "stale")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.remove(t, "a", false, yes); err == nil || !strings.Contains(err.Error(), wt+": not a checkout of this repository") {
		t.Fatalf("err = %v, want the unknown entry refused without --force", err)
	}
	if _, err := f.remove(t, "a", true, yes); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
}

// B6: remove and wt rm word their hard refusals alike.
func TestWorktreeRemoveHardRefusalWording(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	p := f.mustAddWT(t, "a", "x").Path
	testutil.Git(t, primary, "worktree", "lock", p)
	_, err := f.rmWT(t, "a", "x", WorktreeRemoveOptions{Force: true})
	if err == nil || !strings.Contains(err.Error(), "\nsome problems cannot be overridden with --force; fix them first") {
		t.Fatalf("err = %v", err)
	}
}

// otherRepoWorktree adds, at wt, a worktree of a fresh clone of another
// repository (outside the bundle) with an uncommitted file.
func otherRepoWorktree(t *testing.T, wt string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}}), other)
	testutil.Git(t, other, "worktree", "add", "-q", "-b", "w-"+filepath.Base(wt), wt, "origin/feat")
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
}

// Review of B6 S3: worktrees of other repositories are hard refusals
// wherever they are in the repository directory: below a repository
// embedded in an unknown entry, and inside the primary checkout or a linked
// worktree (e.g. in an ignored directory).
func TestRemoveRefusesForeignWorktreesEverywhereEvenWithForce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	repoDir := filepath.Join(f.w.Root, "a")
	linked := filepath.Join(repoDir, "feat")
	testutil.Git(t, primary, "worktree", "add", "-q", linked, "feat")
	emb := filepath.Join(repoDir, "unk2", "emb")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), emb)
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "tmp/\n")
	foreign := []string{
		filepath.Join(emb, "sub", "wB"),
		filepath.Join(primary, "tmp", "wC"),
		filepath.Join(linked, "tmp", "wD"),
	}
	for _, wt := range foreign {
		otherRepoWorktree(t, wt)
	}
	_, err := f.remove(t, "a", true, yes)
	var re *RisksError
	if !errors.As(err, &re) || !re.Plan.Refused(true) {
		t.Fatalf("err = %v, want a hard refusal", err)
	}
	for _, wt := range foreign {
		rel, _ := filepath.Rel(repoDir, wt)
		if !strings.Contains(err.Error(), filepath.Base(rel)) || !exists(filepath.Join(wt, "wip.txt")) {
			t.Errorf("%s not refused, or deleted:\n%v", wt, err)
		}
	}
}

// A worktree whose git directory is inside the repository directory (here:
// of a repository embedded in it) is not another repository's: like the
// embedded repository itself, --force deletes it.
func TestRemoveWorktreeOfEmbeddedRepositoryIsForceable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	repoDir := filepath.Join(f.w.Root, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "tmp/\n")
	for _, base := range []string{filepath.Join(primary, "tmp"), filepath.Join(repoDir, "unk3")} {
		emb := filepath.Join(base, "emb")
		plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat"}}), emb)
		testutil.Git(t, emb, "worktree", "add", "-q", "-b", "w", filepath.Join(base, "wE"), "origin/feat")
	}
	if _, err := f.remove(t, "a", false, yes); err == nil {
		t.Fatal("removed without --force")
	}
	if _, err := f.remove(t, "a", true, yes); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
}

// Review of B6 S2: when `git worktree remove` fails (here: the worktree was
// locked after the re-check), the deletion stops with an error; the
// repository directory is never deleted anyway.
func TestRemoveDeletionStopsWhenAWorktreeCannotBeRemoved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := filepath.Join(f.w.Root, "a", "feat")
	testutil.Git(t, primary, "worktree", "add", "-q", wt, "feat")
	var plan RemovePlan
	if _, err := f.remove(t, "a", false, func(p RemovePlan) bool { plan = p; return false }); !errors.Is(err, ErrNotConfirmed) {
		t.Fatal(err)
	}
	testutil.Git(t, primary, "worktree", "lock", wt)
	err := deleteRepoDir(t.Context(), f.w.Root, plan, noEmit)
	if err == nil || !strings.Contains(err.Error(), "removing worktree "+wt) {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(wt, ".git")) || !exists(primary) {
		t.Fatal("the repository directory was deleted anyway")
	}
}
