package gitx

import (
	"context"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func mustRisks(t *testing.T, dir string) []Risk {
	t.Helper()
	r, err := DeletionRisks(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantRisks checks that risks contains exactly the given kinds with counts.
func wantRisks(t *testing.T, risks []Risk, want map[RiskKind]int) {
	t.Helper()
	got := map[RiskKind]int{}
	for _, r := range risks {
		if r.Count <= 0 {
			t.Errorf("risk %v with count %d reported", r.Kind, r.Count)
		}
		got[r.Kind] = r.Count
	}
	if len(got) != len(want) {
		t.Errorf("risks = %v, want %v", risks, want)
		return
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("risks = %v, want %v", risks, want)
			return
		}
	}
}

func TestDeletionRisksClean(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"pushed"}})
	testutil.Git(t, dir, "branch", "--track", "pushed", "origin/pushed")
	r := mustRisks(t, dir)
	wantRisks(t, r, nil)
	if !SafeToDelete(r) {
		t.Fatal("SafeToDelete = false for a clean, fully pushed clone")
	}
}

func TestDeletionRisksWorkTreeChanges(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{Commits: 2})
	writeFile(t, dir, "file-1.txt", "staged\n")
	testutil.Git(t, dir, "add", "file-1.txt")
	writeFile(t, dir, "file-2.txt", "unstaged\n")
	writeFile(t, dir, "new.txt", "untracked\n")
	r := mustRisks(t, dir)
	wantRisks(t, r, map[RiskKind]int{RiskStaged: 1, RiskUnstaged: 1, RiskUntracked: 1})
	if SafeToDelete(r) {
		t.Fatal("SafeToDelete = true")
	}
}

func TestDeletionRisksUntrackedOnlyIsUnsafe(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "config", "status.showUntrackedFiles", "no")
	writeFile(t, dir, "notes.txt", "precious\n")
	r := mustRisks(t, dir)
	wantRisks(t, r, map[RiskKind]int{RiskUntracked: 1})
	if SafeToDelete(r) {
		t.Fatal("SafeToDelete = true with an untracked file")
	}
}

func TestDeletionRisksConflict(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "-b", "other")
	testutil.CommitFile(t, dir, "file-1.txt", "other side\n", "other")
	testutil.Git(t, dir, "checkout", "-q", "main")
	testutil.CommitFile(t, dir, "file-1.txt", "main side\n", "main")
	if _, err := Run(context.Background(), dir, "merge", "-q", "other"); err == nil {
		t.Fatal("merge unexpectedly succeeded")
	}
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskConflicted: 1, RiskUnpushedBranchCommits: 2})
}

func TestDeletionRisksStash(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	writeFile(t, dir, "file-1.txt", "wip\n")
	testutil.Git(t, dir, "stash", "push", "-q")
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskStash: 1})
}

func TestDeletionRisksUnpushedBranchCommits(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "-b", "side")
	testutil.CommitFile(t, dir, "s1.txt", "1\n", "side 1")
	testutil.CommitFile(t, dir, "s2.txt", "2\n", "side 2")
	testutil.Git(t, dir, "checkout", "-q", "main")
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskUnpushedBranchCommits: 2})
}

func TestDeletionRisksDetachedCommits(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	testutil.CommitFile(t, dir, "d.txt", "d\n", "detached")
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskDetachedCommits: 1})
}

func TestDeletionRisksTagOnlyCommits(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "checkout", "-q", "--detach")
	testutil.CommitFile(t, dir, "t1.txt", "1\n", "tagged 1")
	testutil.CommitFile(t, dir, "t2.txt", "2\n", "tagged 2")
	testutil.Git(t, dir, "tag", "-a", "-m", "annotated", "v-local")
	testutil.Git(t, dir, "checkout", "-q", "main")
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskTagOnlyCommits: 2})
}

func TestDeletionRisksIgnoredFilesAreInformational(t *testing.T) {
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.CommitFile(t, dir, ".gitignore", "*.log\n", "ignore logs")
	testutil.Git(t, dir, "push", "-q", "origin", "main")
	writeFile(t, dir, "build.log", "x\n")
	r := mustRisks(t, dir)
	wantRisks(t, r, map[RiskKind]int{RiskIgnoredFiles: 1})
	if !r[0].Informational() || !SafeToDelete(r) {
		t.Fatalf("ignored files must be informational only: %v", r)
	}
}

func TestDeletionRisksWithoutRemoteAndUnborn(t *testing.T) {
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	wantRisks(t, mustRisks(t, dir), nil)
	testutil.CommitFile(t, dir, "a.txt", "a\n", "1")
	testutil.CommitFile(t, dir, "b.txt", "b\n", "2")
	wantRisks(t, mustRisks(t, dir), map[RiskKind]int{RiskUnpushedBranchCommits: 2})
}

func TestRiskKindString(t *testing.T) {
	for k, want := range map[RiskKind]string{
		RiskStaged:                "staged changes",
		RiskUnstaged:              "unstaged changes",
		RiskConflicted:            "conflicts",
		RiskUntracked:             "untracked files",
		RiskStash:                 "stashes",
		RiskUnpushedBranchCommits: "unpushed commits on local branches",
		RiskDetachedCommits:       "commits only on detached HEAD",
		RiskTagOnlyCommits:        "commits only reachable from local tags",
		RiskIgnoredFiles:          "ignored files",
		RiskKind(0):               "RiskKind(0)",
	} {
		if got := k.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", int(k), got, want)
		}
	}
}
