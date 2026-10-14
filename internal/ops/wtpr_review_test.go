package ops

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// forkPR7 sets up pull request #7 from a fork, checked out once, with a local
// commit on pr/7, then its worktree removed (pr/7 stays).
func forkPR7(t *testing.T, localCommit bool) (*fixture, string, testutil.Origin, string) {
	t.Helper()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	head := pushPullRef(t, o, 7, "main", "fork.txt")
	f.fake.AddPR("o", "a", hosting.PR{Number: 7, HeadBranch: "x", IsCrossRepository: true})
	res, err := f.wtPR(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if localCommit {
		testutil.CommitFile(t, res.Path, "mine.txt", "m\n", "my review fixup")
	}
	if _, err := f.rmWT(t, "a", "pr-7", WorktreeRemoveOptions{Offline: true}); err != nil {
		t.Fatal(err)
	}
	return f, primary, o, head
}

// S1: pr/7 ahead of the PR head: the worktree is added on the local branch.
func TestR10PRLocalAhead(t *testing.T) {
	t.Parallel()
	f, primary, _, _ := forkPR7(t, true)
	local := testutil.Git(t, primary, "rev-parse", "pr/7")
	res, err := f.wtPR(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != local || !strings.Contains(res.Message, "ahead of the pull request") {
		t.Fatalf("HEAD %s (want %s), %+v", got, local, res)
	}
}

// S1: pr/7 behind the PR head: fast-forwarded, then added.
func TestR10PRLocalBehind(t *testing.T) {
	t.Parallel()
	f, _, o, head := forkPR7(t, false)
	next := pushPullRef(t, o, 7, head, "fork2.txt")
	res, err := f.wtPR(t, "a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.Git(t, res.Path, "rev-parse", "HEAD"); got != next {
		t.Fatalf("HEAD %s, want %s", got, next)
	}
}

// S1: pr/7 diverged (local commit, PR force-pushed): refused, nothing moves,
// and the message says that local commits would be lost (2: the previous
// PR head commit and the local fixup are both only on pr/7 now).
func TestR10PRLocalDiverged(t *testing.T) {
	t.Parallel()
	f, primary, o, _ := forkPR7(t, true)
	local := testutil.Git(t, primary, "rev-parse", "pr/7")
	pushPullRef(t, o, 7, "main", "rewritten.txt")
	_, err := f.wtPR(t, "a", 7)
	if err == nil || !strings.Contains(err.Error(), "diverged") || !strings.Contains(err.Error(), "pr/7 has 2 commits that are not in the pull request") || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("err = %v", err)
	}
	if got := testutil.Git(t, primary, "rev-parse", "pr/7"); got != local {
		t.Fatalf("pr/7 moved to %s", got)
	}
	if exists(filepath.Join(f.w.Root, "a", "pr-7")) {
		t.Fatal("worktree created")
	}
}

func samePR3(t *testing.T) (*fixture, string, testutil.Origin) {
	t.Helper()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	f.fake.AddPR("o", "a", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "o", HeadRepo: "a"})
	return f, primary, o
}

// S2: a local head branch strictly behind origin is fast-forwarded first.
func TestR10SameRepoStaleLocal(t *testing.T) {
	t.Parallel()
	f, primary, o := samePR3(t)
	testutil.Git(t, primary, "branch", "--track", "feat", "origin/feat")
	testutil.AdvanceOrigin(t, o, "feat", "new.txt", "new\n", "pr update")
	res, err := f.wtPR(t, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if h, oh := testutil.Git(t, res.Path, "rev-parse", "HEAD"), testutil.Git(t, primary, "rev-parse", "origin/feat"); h != oh {
		t.Fatalf("worktree HEAD %s, origin/feat %s", h, oh)
	}
}

// S2: a diverged local head branch is checked out as is, with a warning.
func TestR10SameRepoDivergedLocal(t *testing.T) {
	t.Parallel()
	f, primary, o := samePR3(t)
	testutil.Git(t, primary, "branch", "--track", "feat", "origin/feat")
	testutil.Git(t, primary, "switch", "-q", "feat")
	testutil.CommitFile(t, primary, "local.txt", "l\n", "local work")
	local := testutil.Git(t, primary, "rev-parse", "HEAD")
	testutil.Git(t, primary, "switch", "-q", "main")
	testutil.AdvanceOrigin(t, o, "feat", "new.txt", "new\n", "pr update")
	res, err := f.wtPR(t, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if h := testutil.Git(t, res.Path, "rev-parse", "HEAD"); h != local || !strings.Contains(strings.Join(res.Warnings, "\n"), "diverged") {
		t.Fatalf("HEAD %s (want %s), %+v", h, local, res)
	}
}

// S2: already checked out and behind: warned, not modified.
func TestR10SameRepoCheckedOutBehind(t *testing.T) {
	t.Parallel()
	f, _, o := samePR3(t)
	wt := f.mustAddWT(t, "a", "feat")
	before := testutil.Git(t, wt.Path, "rev-parse", "HEAD")
	testutil.AdvanceOrigin(t, o, "feat", "new.txt", "new\n", "pr update")
	res, err := f.wtPR(t, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if h := testutil.Git(t, wt.Path, "rev-parse", "HEAD"); h != before || !strings.Contains(strings.Join(res.Warnings, "\n"), "behind origin/feat") {
		t.Fatalf("HEAD %s (want %s), %+v", h, before, res)
	}
}

// NIT: SSH aliases of github.com (as add treats them) are accepted.
func TestR10PRAliasHost(t *testing.T) {
	t.Parallel()
	f, _, _ := samePR3(t)
	if err := f.w.update(func(b *bundle.Bundle) error {
		b.Repositories[0].Host = "github_work"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.wtPR(t, "a", 3); err != nil {
		t.Fatal(err)
	}
}

// NIT: no trailing space for a lock without reason.
func TestR10DoctorLockedWithoutReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	wt := f.mustAddWT(t, "a", "feat")
	testutil.Git(t, primary, "worktree", "lock", wt.Path)
	c := wantCheck(t, f.doctor(t, DoctorOptions{}), CheckInfo, "worktree", "a", "locked")
	if strings.HasSuffix(c.Message, " ") {
		t.Fatalf("trailing space: %q", c.Message)
	}
}
