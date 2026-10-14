package ops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// stopRebaseIn starts in dir a rebase of its current branch onto HEAD~1 that
// stops at once (failing --exec), leaving HEAD detached and the branch "being
// rebased".
func stopRebaseIn(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "rebase", "--exec", "false", "HEAD~1")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("rebase did not stop: %s", out)
	}
}

// goneMergedTopic creates branch topic in the primary checkout of a, whose
// upstream origin/topic was merged into main then deleted, and checks it out
// in the primary.
func goneMergedTopic(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	pushBranch(t, o, "topic")
	testutil.Git(t, primary, "fetch", "-q", "origin")
	testutil.Git(t, primary, "branch", "--track", "topic", "origin/topic")
	mergeOnOrigin(t, o, "topic")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "topic")
	return f, primary
}

// B1: a branch being rebased in the primary checkout (HEAD detached) is in
// use: not proposed, not deleted.
func TestR10PrimaryMidRebaseBranchDeleted(t *testing.T) {
	t.Parallel()
	f, primary := goneMergedTopic(t)
	testutil.Git(t, primary, "switch", "-q", "topic")
	stopRebaseIn(t, primary)
	testutil.CommitFile(t, primary, "fix.txt", "fix\n", "fixup during rebase")
	opts := PruneOptions{IncludeBranches: true, DeleteBranches: true, WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates %v", candidateNames(plan))
	}
	if s, ok := skipFor(plan, "topic"); !ok || !strings.Contains(s.Reason, "rebased") {
		t.Fatalf("skip = %+v, %v", s, ok)
	}
	ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	if !branchExists(t, primary, "topic") {
		t.Fatal("topic deleted during its rebase")
	}
}

// B1: same in a linked worktree; and bisect.
func TestR10LinkedWorktreeMidRebaseAndBisect(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	stopRebaseIn(t, filepath.Join(f.w.Root, "a", "merged"))
	testutil.Git(t, filepath.Join(f.w.Root, "a", "done"), "bisect", "start", "HEAD", "HEAD~2")
	opts := PruneOptions{IncludeBranches: true, DeleteBranches: true, WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	for _, c := range plan.Candidates {
		if c.Branch == "merged" || c.Branch == "done" {
			t.Fatalf("candidate %+v", c)
		}
	}
	if s, ok := skipFor(plan, "merged"); !ok || !strings.Contains(s.Reason, "rebased") {
		t.Fatalf("merged skip = %+v, %v", s, ok)
	}
	if s, ok := skipFor(plan, "done"); !ok || !strings.Contains(s.Reason, "bisected") {
		t.Fatalf("done skip = %+v, %v", s, ok)
	}
	ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	for _, b := range []string{"merged", "done"} {
		if !branchExists(t, primary, b) {
			t.Errorf("%s deleted", b)
		}
	}
}

// B1 at deletion time (wt rm -d/-D, prune): a branch being rebased is kept.
func TestR10DeleteBranchIfUnchangedKeepsBranchInUse(t *testing.T) {
	t.Parallel()
	f, primary := goneMergedTopic(t)
	testutil.Git(t, primary, "switch", "-q", "topic")
	oid := testutil.Git(t, primary, "rev-parse", "topic")
	stopRebaseIn(t, primary)
	reason := deleteBranchIfUnchanged(context.Background(), primary, "topic", oid)
	if reason == "" || !strings.Contains(reason, "rebased") || !branchExists(t, primary, "topic") {
		t.Fatalf("reason %q, exists %v", reason, branchExists(t, primary, "topic"))
	}
	_ = f
}

// S4: a commit in the worktree after the plan keeps it, with a clear message.
func TestR10HeadMovedAfterPlan(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root, Offline: true}
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root})
	wt := filepath.Join(f.w.Root, "a", "merged")
	testutil.CommitFile(t, wt, "more.txt", "m\n", "more work after plan")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || !exists(wt) || !strings.Contains(r.Message, "new commits since it was proposed") {
		t.Fatalf("%+v", r)
	}
}

// S4: the worktree directory now holds another branch on the same commit.
func TestR10WorktreeSwitchedBranchAfterPlan(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root, Offline: true}
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root})
	wt := filepath.Join(f.w.Root, "a", "merged")
	testutil.Git(t, wt, "switch", "-q", "-c", "other")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || !exists(wt) || !branchExists(t, primary, "other") {
		t.Fatalf("%+v", r)
	}
}

// S4: a branch without worktree that moved after the plan is kept.
func TestR10BranchMovedAfterPlan(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root, IncludeBranches: true, Offline: true}
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root, IncludeBranches: true})
	testutil.Git(t, primary, "update-ref", "refs/heads/old", "refs/remotes/origin/main")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "old")
	if r.Outcome != PruneKept || !branchExists(t, primary, "old") || !strings.Contains(r.Message, "moved") {
		t.Fatalf("%+v", r)
	}
}

// NIT: the candidate's branch is now checked out in the primary: kept.
func TestR10CandidateNowInPrimaryIsKept(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root, Offline: true}
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root})
	testutil.Git(t, primary, "worktree", "remove", filepath.Join(f.w.Root, "a", "merged"))
	testutil.Git(t, primary, "switch", "-q", "merged")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || r.Err != nil {
		t.Fatalf("%+v", r)
	}
}

// NIT: the plan fetched already; removals do not fetch origin again.
func TestR10ApplyDoesNotRefetch(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	opts := PruneOptions{WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	var evs []Event
	ApplyPrune(context.Background(), f.w, plan, opts, ReporterFunc(func(e Event) { evs = append(evs, e) }))
	for _, e := range evs {
		if e.Phase == PhaseFetch && strings.Contains(e.Message, "fetching origin") {
			t.Fatalf("refetched: %+v", e)
		}
	}
}

// S5: non-blocking risks are listed in the plan and the results.
func TestR10NonBlockingRisksListed(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	common := testutil.Git(t, primary, "rev-parse", "--path-format=absolute", "--git-common-dir")
	writeFile(t, filepath.Join(common, "info", "exclude"), "*.bin\n")
	wt := filepath.Join(f.w.Root, "a", "merged")
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		writeFile(t, filepath.Join(wt, n), "x")
	}
	opts := PruneOptions{WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	var notes []string
	for _, c := range plan.Candidates {
		if c.Branch == "merged" {
			notes = c.Notes
		}
	}
	if !strings.Contains(strings.Join(notes, "\n"), "3 ignored files (deleted with the worktree)") {
		t.Fatalf("plan notes %q", notes)
	}
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneRemoved || !strings.Contains(strings.Join(r.Notes, "\n"), "3 ignored files") {
		t.Fatalf("result %+v", r)
	}
}

// E2: a gone branch with commits on no remote is never deleted, even with
// --delete-branches and --include-branches.
func TestR10GoneUnpushedDeleteBranches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	f.mustAddWT(t, "a", "feat")
	wt := filepath.Join(f.w.Root, "a", "feat")
	testutil.CommitFile(t, wt, "unpushed.txt", "u\n", "unpushed")
	tip := testutil.Git(t, wt, "rev-parse", "HEAD")
	testutil.Git(t, o.Path, "branch", "-q", "-D", "feat")
	opts := PruneOptions{DeleteBranches: true, IncludeBranches: true, WorkingDir: f.w.Root}
	plan := f.prunePlan(t, opts)
	ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	if got := testutil.Git(t, primary, "rev-parse", "refs/heads/feat"); got != tip {
		t.Fatalf("feat = %s, want %s", got, tip)
	}
}

// E8: doctor changes nothing in the bundle.
func TestR10DoctorReadOnly(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	writeFile(t, filepath.Join(f.w.Root, "a", "feat", "file-1.txt"), "dirty\n")
	writeFile(t, filepath.Join(f.w.Root, "a", "wip", "untracked.txt"), "u\n")
	testutil.Git(t, primary, "worktree", "add", "-q", filepath.Join(t.TempDir(), "outside"), "-b", "outside")
	snap := func() map[string]string {
		m := map[string]string{}
		_ = filepath.Walk(f.w.Root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			b, _ := os.ReadFile(p)
			m[p] = fi.ModTime().String() + "|" + fi.Mode().String() + "|" + string(b)
			return nil
		})
		return m
	}
	before := snap()
	f.doctor(t, DoctorOptions{})
	after := snap()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("changed: %s", k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("created: %s", k)
		}
	}
}

// N1: in a reftable repository a checked-out branch is not deleted.
func TestR10DeleteBranchIfUnchangedReftable(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := filepath.Join(t.TempDir(), "r")
	if out, err := exec.Command("git", "init", "-q", "--ref-format=reftable", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git init --ref-format=reftable unsupported: %v: %s", err, out)
	}
	testutil.CommitFile(t, dir, "a.txt", "a\n", "a")
	wt := filepath.Join(filepath.Dir(dir), "feat")
	testutil.Git(t, dir, "worktree", "add", "-q", "-b", "feat", wt)
	oid := testutil.Git(t, dir, "rev-parse", "feat")
	if reason := deleteBranchIfUnchanged(context.Background(), dir, "feat", oid); reason == "" || !branchExists(t, dir, "feat") {
		t.Fatalf("reason %q, exists %v", reason, branchExists(t, dir, "feat"))
	}
}

// NIT: a worktree moved after the plan (git worktree move) is kept.
func TestR10WorktreeMovedAfterPlan(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	plan := f.prunePlan(t, PruneOptions{WorkingDir: f.w.Root})
	moved := filepath.Join(f.w.Root, "a", "merged-moved")
	testutil.Git(t, primary, "worktree", "move", filepath.Join(f.w.Root, "a", "merged"), moved)
	rs := ApplyPrune(context.Background(), f.w, plan, PruneOptions{WorkingDir: f.w.Root, Offline: true}, Discard)
	r, _ := resultFor(rs, "merged")
	if r.Outcome != PruneKept || !exists(moved) || !branchExists(t, primary, "merged") {
		t.Fatalf("%+v", r)
	}
}

// NIT: a gh probe timeout is not reported as an authentication failure.
func TestR10DoctorGHTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cs := f.doctor(t, DoctorOptions{GH: func(context.Context) error {
		return fmt.Errorf("%w within 20s", ErrGHProbeTimeout)
	}})
	c := wantCheck(t, cs, CheckWarn, "gh", "", "did not answer within 20s")
	if strings.Contains(c.Message, "not authenticated") {
		t.Fatalf("message %q", c.Message)
	}
}
