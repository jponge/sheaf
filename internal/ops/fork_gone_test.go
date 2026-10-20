package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func confirmAll(WorktreeRemovePlan) bool { return true }

// forkGoneTopic: a forked repository whose worktree topic tracks
// origin/topic (one commit, pushed to the fork only), then the fork is
// deleted: origin/topic is a stale ref holding the only remote copy.
//
// The fixture is built once per test process (see testutil.CachedTree) and
// each test gets a copy of the bundle and of the forked origin.
func forkGoneTopic(t *testing.T) (*fixture, string) {
	t.Helper()
	tf := testutil.CachedTree(t, "ops.forkGoneTopic", func(t *testing.T) testutil.TreeFixture {
		f, primary, fo := buildForkGoneTopic(t)
		return testutil.TreeFixture{
			Dirs: []string{f.w.Root, filepath.Dir(filepath.Dir(fo.Upstream.Path))},
			Values: map[string]string{"root": f.w.Root, "primary": primary,
				"upstreamURL": fo.Upstream.URL, "forkURL": fo.Fork.URL},
		}
	})
	f := openFixture(t, tf.Values["root"])
	// As addForked registered them.
	f.fake.User = forkUser
	f.fake.AddRepo(hostingRepoBranch("up", "x", tf.Values["upstreamURL"], "main"))
	f.fake.AddFork(hostingFork("x", tf.Values["forkURL"]))
	return f, tf.Values["primary"]
}

// buildForkGoneTopic builds what forkGoneTopic returns.
func buildForkGoneTopic(t *testing.T) (*fixture, string, testutil.ForkedOrigin) {
	t.Helper()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	return f, primary, fo
}

func TestRemoveWorktreeForkGoneKeepsBranchUnlessForced(t *testing.T) {
	t.Parallel()
	f, primary := forkGoneTopic(t)
	_, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{DeleteBranch: true, WorkingDir: f.w.Root, Confirm: confirmAll})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(refused.Plan.Hard) != 0 {
		t.Fatalf("a gone fork must not be a hard problem: %v", refused.Plan.Hard)
	}
	if u := refused.Plan.Unmerged; !strings.Contains(u, "not merged into upstream/main") || strings.Contains(u, "origin/topic") ||
		!strings.Contains(u, "your fork (origin) appears to be gone") || refused.Plan.UnmergedCommits != 1 {
		t.Fatalf("unmerged = %q (%d commits)", u, refused.Plan.UnmergedCommits)
	}
	if _, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{ForceDeleteBranch: true, WorkingDir: f.w.Root, Confirm: confirmAll}); err != nil {
		t.Fatalf("-D must override: %v", err)
	}
	if branchExists(t, primary, "topic") {
		t.Fatal("topic not deleted with -D")
	}
}

func TestRemoveWorktreeForkGoneWithoutDeleteProceeds(t *testing.T) {
	t.Parallel()
	f, primary := forkGoneTopic(t)
	if _, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{WorkingDir: f.w.Root, Confirm: confirmAll}); err != nil {
		t.Fatalf("the branch survives: removing the worktree must proceed: %v", err)
	}
	if !branchExists(t, primary, "topic") {
		t.Fatal("branch deleted")
	}
}

func TestRemoveWorktreeForkGoneDetachedCommitOnlyOnStaleRefBlocks(t *testing.T) {
	t.Parallel()
	f, primary := forkGoneTopic(t)
	res, _, err := f.addWT(t, "x", "other", WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, res.Path, "switch", "-q", "--detach", "origin/topic")
	// Only the stale origin/topic and the detached HEAD hold the commit.
	if _, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{ForceDeleteBranch: true, WorkingDir: f.w.Root, Confirm: confirmAll}); err != nil {
		t.Fatal(err)
	}
	if branchExists(t, primary, "topic") {
		t.Fatal("setup: topic still exists")
	}
	_, err = f.rmWT(t, "x", "other", WorktreeRemoveOptions{WorkingDir: f.w.Root, Confirm: confirmAll})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) || len(refused.Plan.Hard) != 0 || !hasKind(refused.Plan.Blocking(), gitx.RiskDetachedCommits) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.rmWT(t, "x", "other", WorktreeRemoveOptions{Force: true, WorkingDir: f.w.Root, Confirm: confirmAll}); err != nil {
		t.Fatalf("--force must override: %v", err)
	}
	if exists(res.Path) {
		t.Fatalf("%s kept", res.Path)
	}
}

func TestRemoveWorktreeForkOtherFetchFailureIsHard(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t)
	testutil.Git(t, primary, "remote", "set-url", "origin", "ext::false")
	_, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{WorkingDir: f.w.Root, Confirm: confirmAll})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) || len(refused.Plan.Hard) == 0 {
		t.Fatalf("err = %v, want a hard refusal", err)
	}
}

func TestRemoveWorktreeOfflineDeleteBranchDoesNotTrustForkRefs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkOnlyBranch(t) // the fork exists and has topic
	_, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{DeleteBranch: true, Offline: true, WorkingDir: f.w.Root, Confirm: confirmAll})
	var refused *WorktreeRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Plan.Unmerged, "--offline") {
		t.Fatalf("err = %v", err)
	}
	if !branchExists(t, primary, "topic") {
		t.Fatal("branch deleted")
	}
	// Online, origin/topic is trusted: merged into its upstream.
	if _, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{DeleteBranch: true, WorkingDir: f.w.Root, Confirm: confirmAll}); err != nil {
		t.Fatal(err)
	}
	if p := filepath.Join(f.w.Root, "x", "topic"); exists(p) || branchExists(t, primary, "topic") {
		t.Fatalf("%s kept (%v) or branch topic kept", p, exists(p))
	}
}

func TestPruneForkGoneProposesAndKeepsForkOnlyBranch(t *testing.T) {
	t.Parallel()
	f, primary := forkGoneTopic(t)
	plan := f.prunePlan(t, PruneOptions{DeleteBranches: true})
	if len(plan.Problems) != 0 {
		t.Fatalf("problems: %v", plan.Problems)
	}
	if got := candidateNames(plan); len(got) != 1 || got[0] != "x/topic:wt:gone" || !strings.Contains(plan.Candidates[0].Reason, "your fork (origin) appears to be gone") {
		t.Fatalf("candidates = %v %+v", got, plan.Candidates)
	}
	rs := ApplyPrune(context.Background(), f.w, plan, PruneOptions{DeleteBranches: true, WorkingDir: f.w.Root}, Discard)
	r, ok := resultFor(rs, "topic")
	if !ok || r.Outcome != PruneRemoved || !strings.Contains(r.Message, "kept branch topic") {
		t.Fatalf("result = %+v", r)
	}
	if !branchExists(t, primary, "topic") {
		t.Fatal("the branch whose commit exists only on the gone fork was deleted")
	}
}

func TestPruneForkGoneStillPrunesWhatIsMergedUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkTopic(t, fo)
	if err := os.RemoveAll(fo.Fork.Path); err != nil {
		t.Fatal(err)
	}
	plan := f.prunePlan(t, PruneOptions{DeleteBranches: true})
	rs := ApplyPrune(context.Background(), f.w, plan, PruneOptions{DeleteBranches: true, WorkingDir: f.w.Root}, Discard)
	r, ok := resultFor(rs, "topic")
	if !ok || r.Outcome != PruneRemoved || !strings.Contains(r.Message, "merged into upstream/main") {
		t.Fatalf("result = %+v (plan %v)", r, candidateNames(plan))
	}
	if branchExists(t, primary, "topic") {
		t.Fatal("merged branch kept")
	}
}

// A gone upstream is not named as a merge target in the kept-branch message.
func TestPruneKeptBranchMessageOmitsGoneTargets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	res := f.mustAddWT(t, "a", "squashed")
	testutil.CommitFile(t, res.Path, "s.txt", "s\n", "squash me")
	testutil.Git(t, res.Path, "push", "-q", "-u", "origin", "squashed")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "squashed")
	plan := f.prunePlan(t, PruneOptions{DeleteBranches: true})
	rs := ApplyPrune(context.Background(), f.w, plan, PruneOptions{DeleteBranches: true, WorkingDir: f.w.Root}, Discard)
	r, ok := resultFor(rs, "squashed")
	if !ok || !strings.Contains(r.Message, "kept branch squashed: branch squashed is not merged into origin/main;") || strings.Contains(r.Message, "origin/squashed") {
		t.Fatalf("result = %+v", r)
	}
	if !branchExists(t, primary, "squashed") {
		t.Fatal("branch deleted")
	}
}
