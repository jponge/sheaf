package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// forkOnlyBranch adds a worktree of the new branch topic whose commit is
// pushed to the fork only (tracking origin/topic).
func (f *fixture) forkOnlyBranch(t *testing.T) {
	t.Helper()
	res, _, err := f.addWT(t, "x", "topic", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testutil.CommitFile(t, res.Path, "topic.txt", "topic\n", "only on the fork")
	testutil.Git(t, res.Path, "push", "-q", "-u", "origin", "topic")
}

func TestRemoveForkCommitsOnForkCountAsPushed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	if _, err := f.remove(t, "x", false, yes); err != nil {
		t.Fatalf("commits pushed to the fork must count as pushed: %v", err)
	}
	if exists(filepath.Join(f.w.Root, "x")) {
		t.Fatal("not removed")
	}
}

func TestRemoveForkGoneWithForkOnlyCommitsIsADataRisk(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	_, err := f.remove(t, "x", false, yes)
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *RisksError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "your fork (origin) appears to be gone") || !strings.Contains(msg, "1 commit exists only on it") ||
		!strings.Contains(msg, "--force") || strings.Contains(msg, "--offline") {
		t.Fatalf("refusal:\n%s", msg)
	}
	if _, err := f.remove(t, "x", true, yes); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
	f.wantRemoved(t, "x")
}

func TestRemoveForkGoneWithoutForkOnlyCommitsProceeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkTopic(t, fo) // pushed to the fork, then merged into the upstream
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	evs, err := f.remove(t, "x", false, yes)
	if err != nil {
		t.Fatalf("nothing exists only on the gone fork: %v", err)
	}
	if !strings.Contains(messages(evs), "your fork (origin) appears to be gone") {
		t.Fatalf("no note about the gone fork: %s", messages(evs))
	}
	f.wantRemoved(t, "x")
}

func TestRemoveForkUpstreamGoneIsADataRisk(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	if err := os.RemoveAll(fo.Upstream.Path); err != nil {
		t.Fatal(err)
	}
	_, err := f.remove(t, "x", false, yes)
	var re *RisksError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "the remote repository appears to be gone") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.remove(t, "x", true, yes); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
	f.wantRemoved(t, "x")
}

func TestRemoveForkExpectedGoneAppliesToTheReference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addForked(t, "x", testutil.OriginOpts{})
	_, err := Remove(context.Background(), f.w, "x", RemoveOptions{Confirm: yes, ExpectedGone: "up/x"}, noEmit)
	if err == nil || !strings.Contains(err.Error(), "the GitHub API cannot see up/x but its remote still answers") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoteRepoGoneForNamedRemote(t *testing.T) {
	t.Parallel()
	if remoteRepoGoneFor("fatal: 'upstream' does not appear to be a git repository", "upstream") {
		t.Fatal("a missing remote named upstream is not a gone repository")
	}
	if !remoteRepoGoneFor("fatal: '/tmp/x/fork.git' does not appear to be a git repository", "origin") {
		t.Fatal("a missing repository path is gone")
	}
}

// Commits never pushed anywhere are reported as unpushed by the risk
// checks, not as existing only on the gone fork.
func TestRemoveForkGoneUnpushedCommitsAreNotForkOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	res, _, err := f.addWT(t, "x", "local", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testutil.CommitFile(t, res.Path, "l.txt", "l\n", "never pushed")
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	_, err = f.remove(t, "x", false, yes)
	var re *RisksError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "only on it") || !strings.Contains(msg, "unpushed") {
		t.Fatalf("refusal:\n%s", msg)
	}
}

// A commit that exists only on a stale remote-tracking ref of the gone
// fork (its local branch was deleted) would be lost with the clone.
func TestRemoveForkGoneCommitOnlyOnStaleForkRefIsADataRisk(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	if _, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{ForceDeleteBranch: true, WorkingDir: f.w.Root, Confirm: func(WorktreeRemovePlan) bool { return true }}); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "topic") || !branchExistsRef(t, primary, "refs/remotes/origin/topic") {
		t.Fatal("setup: want no local topic and a remote-tracking origin/topic")
	}
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	_, err := f.remove(t, "x", false, yes)
	var re *RisksError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "your fork (origin) appears to be gone and 1 commit exists only on it") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.remove(t, "x", true, yes); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
	f.wantRemoved(t, "x")
}

func branchExistsRef(t *testing.T, dir, ref string) bool {
	t.Helper()
	ok, err := refExists(context.Background(), dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
