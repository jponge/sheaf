package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// The hints of the refusals end with the complete command to run, its
// target filled in and shell-quoted, so that it can be pasted (B52 A3).

func TestWorktreeRefusedErrorRunHints(t *testing.T) {
	t.Parallel()
	dirty := []gitx.Risk{{Kind: gitx.RiskUnstaged, Count: 1}}
	plan := func(dir string, risks []gitx.Risk, unmerged string, hard ...string) WorktreeRemovePlan {
		return WorktreeRemovePlan{Repo: "api", Worktree: WorktreeInfo{Dir: dir, Branch: "abc"}, Risks: risks, Unmerged: unmerged, Hard: hard}
	}
	for _, tc := range []struct {
		name string
		err  WorktreeRefusedError
		want string
	}{
		{"blocking", WorktreeRefusedError{Plan: plan("abc", dirty, "")},
			"\nto remove anyway (what is listed above will be lost), run: sheaf wt rm --force --dir abc -- api"},
		{"unmerged", WorktreeRefusedError{Plan: plan("abc", nil, "branch abc is not merged"), DeleteBranch: true},
			"\nto remove anyway (what is listed above will be lost), run: sheaf wt rm -D --dir abc -- api"},
		{"forced but unmerged", WorktreeRefusedError{Plan: plan("abc", dirty, "branch abc is not merged"), Force: true, DeleteBranch: true},
			"\nto remove anyway (what is listed above will be lost), run: sheaf wt rm --force -D --dir abc -- api"},
		{"both offline", WorktreeRefusedError{Plan: plan("my dir", dirty, "branch abc is not merged"), DeleteBranch: true, Offline: true},
			"\nto remove anyway (what is listed above will be lost), run: sheaf wt rm --force -D --offline --dir 'my dir' -- api"},
		{"fetch failed", WorktreeRefusedError{Plan: plan("abc", nil, "", fetchProblem(errorString("boom")))},
			"\nto check local data only, run: sheaf wt rm --offline --dir abc -- api"},
		// A command with --force would be refused for the hard problem.
		{"fetch failed and dirty", WorktreeRefusedError{Plan: plan("abc", dirty, "branch abc is not merged", fetchProblem(errorString("boom"))), DeleteBranch: true},
			"\nto check local data only, run: sheaf wt rm -d --offline --dir abc -- api"},
		{"bundle", WorktreeRefusedError{Plan: plan("abc", dirty, ""), bundle: "/b x"},
			"\nto remove anyway (what is listed above will be lost), run: sheaf --bundle '/b x' wt rm --force --dir abc -- api"},
	} {
		got := tc.err.Error()
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s: error ends with\n%q\nwant\n%q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "pass --") || strings.Contains(got, "use -D") {
			t.Errorf("%s: flag hint without its command:\n%s", tc.name, got)
		}
	}
}

// A worktree outside its repository directory gets no command: --dir
// could select another worktree of the same directory name.
func TestWorktreeRefusedErrorNoRunHintOutside(t *testing.T) {
	t.Parallel()
	e := WorktreeRefusedError{Plan: WorktreeRemovePlan{Repo: "api", Worktree: WorktreeInfo{Dir: "abc", Path: "/elsewhere/abc", Outside: true},
		Risks: []gitx.Risk{{Kind: gitx.RiskUnstaged, Count: 1}}, Hard: []string{"outside"}}}
	if got := e.Error(); strings.Contains(got, "run:") {
		t.Errorf("error with a command for an outside worktree:\n%s", got)
	}
}

func TestRisksErrorRunHints(t *testing.T) {
	t.Parallel()
	dirty := RemoveItem{Path: "/b/old/main", Risks: []gitx.Risk{{Kind: gitx.RiskUnstaged, Count: 1}}}
	fetch := RemoveItem{Path: "/b/old/main", Problem: fetchProblem(errorString("boom")), Hard: true}
	for _, tc := range []struct {
		name string
		err  RisksError
		want string
	}{
		{"blocking", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{dirty}}},
			"\nto remove anyway (the data listed above will be lost), run: sheaf remove --force -- old"},
		{"offline", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{dirty}}, Offline: true},
			"\nto remove anyway (the data listed above will be lost), run: sheaf remove --force --offline -- old"},
		{"fetch failed", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{fetch}}},
			"\nto check local data only, run: sheaf remove --offline -- old"},
		{"bundle", RisksError{Plan: RemovePlan{Repo: "old", Items: []RemoveItem{dirty}}, bundle: "/b"},
			"\nto remove anyway (the data listed above will be lost), run: sheaf --bundle /b remove --force -- old"},
	} {
		got := tc.err.Error()
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s: error ends with\n%q\nwant\n%q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "pass --") {
			t.Errorf("%s: flag hint without its command:\n%s", tc.name, got)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestPruneSkipRunHints(t *testing.T) {
	t.Parallel()
	f, primary, _ := pruneScenario(t)
	testutil.Git(t, primary, "tag", "v1", "origin/main")
	f.mustAddTag(t, "a", "v1")
	for _, tc := range []struct {
		opts PruneOptions
		want string
	}{
		{PruneOptions{}, "run: sheaf prune --include-branches"},
		{PruneOptions{Offline: true, Repos: []string{"a"}}, "run: sheaf prune --include-branches --offline --repo a"},
	} {
		plan := f.prunePlan(t, tc.opts)
		s, ok := skipFor(plan, "old")
		if !ok || !strings.HasSuffix(s.Reason, "; to delete such branches, "+tc.want) {
			t.Errorf("%+v: skip old = %+v, %v; want it to end with %q", tc.opts, s, ok, tc.want)
		}
		var tag *PruneSkip
		for i := range plan.Skipped {
			if plan.Skipped[i].Tag == "v1" {
				tag = &plan.Skipped[i]
			}
		}
		if want := "; to remove it, run: sheaf wt rm --dir v1 -- a"; tag == nil || !strings.HasSuffix(tag.Reason, want) {
			t.Errorf("%+v: tag skip = %+v, want it to end with %q", tc.opts, tag, want)
		}
	}
}

func TestWorktreeAddFetchFailureRunHints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1", "origin/main")
	testutil.Git(t, primary, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	_, _, err := f.addWT(t, "a", "feat&x", WorktreeAddOptions{Base: "origin/main"})
	if want := "\nto create the worktree from the local state of the repository, run: sheaf wt add --no-fetch --base origin/main -- a 'feat&x'"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("wt add: err = %v, want it to end with %q", err, want)
	}
	_, _, err = f.addTag(t, "a", "v1", WorktreeAddOptions{})
	if want := "\nto create the worktree from the local tags of the repository, run: sheaf wt add --no-fetch --tag v1 -- a"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("wt add --tag: err = %v, want it to end with %q", err, want)
	}
}

func TestRemoveWorktreeOfPrimaryRunHint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "a")
	_, err := f.rmWT(t, "a", "main", WorktreeRemoveOptions{Offline: true})
	if want := "; to remove the repository, run: sheaf remove -- a"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("err = %v, want it to end with %q", err, want)
	}
}

// SyncRemoveGone's refusal of a mass removal names the sync command that
// allows it.
func TestSyncMassRemovalRunHint(t *testing.T) {
	t.Parallel()
	p := Proposal{Key: "gone:o/a", Kind: ProposalGone, Org: "o", Name: "a", Slug: "o/a", MassRemoval: true}
	for _, tc := range []struct {
		force bool
		want  string
	}{
		{false, "; check, then run: sheaf sync --org o --remove-gone --yes --allow-mass-removal"},
		{true, "; check, then run: sheaf sync --org o --remove-gone --yes --force --allow-mass-removal"},
	} {
		opts := SyncRemoveOptions{Remove: RemoveOptions{Force: tc.force, WorkingDir: t.TempDir()}}
		s := SyncRemoveGone(context.Background(), nil, []Proposal{p}, opts, 1, Discard)
		if len(s.Results) != 1 || s.Results[0].Err == nil || !strings.HasSuffix(s.Results[0].Err.Error(), tc.want) {
			t.Errorf("force %v: summary %+v, want an error ending with %q", tc.force, s, tc.want)
		}
	}
}

func TestAddRunHintCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ got, want string }{
		{noForkCommand(context.Background(), bundle.Repository{Name: "x", Slug: "up/x"}, "", false), "sheaf add --no-fork -- up/x"},
		{noForkCommand(context.Background(), bundle.Repository{Name: "y", Slug: "up/x"}, "", false), "sheaf add --no-fork --as y -- up/x"},
		{noForkCommand(context.Background(), bundle.Repository{Name: "x", Slug: "up/x"}, "up", false), "sheaf add --org up --no-fork --repo x"},
		{noForkCommand(context.Background(), bundle.Repository{Name: "x", Slug: "up/x"}, "up", true), "sheaf add --org up --include-archived --no-fork --repo x"},
		{noForkCommand(WithHintBundle(context.Background(), "/b"), bundle.Repository{Name: "x", Slug: "up/x"}, "", false), "sheaf --bundle /b add --no-fork -- up/x"},
		{includeArchivedCommand(context.Background(), AddRequest{Org: "o", NoFork: true, As: "z"}, "old"), "sheaf add --org o --include-archived --no-fork --as z --repo old"},
	} {
		if tc.got != tc.want {
			t.Errorf("command = %q, want %q", tc.got, tc.want)
		}
	}
}

// A tag worktree outside its repository directory gets no command: it has
// no directory name there that --dir could select (B52 U1 review).
func TestPruneTagSkipOutsideHasNoCommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1", "origin/main")
	tw := f.mustAddTag(t, "a", "v1")
	outside := filepath.Join(t.TempDir(), "v1out")
	testutil.Git(t, primary, "worktree", "move", tw.Path, outside)
	plan := f.prunePlan(t, PruneOptions{Offline: true})
	var reason string
	for _, s := range plan.Skipped {
		if s.Tag == "v1" {
			reason = s.Reason
		}
	}
	if reason == "" || strings.Contains(reason, "run:") || strings.Contains(reason, "--dir") {
		t.Errorf("tag skip reason = %q, want one without a command (skipped %+v)", reason, plan.Skipped)
	}
}

// The hints of prune keep its options: --delete-branches in the "no
// worktree" hint, -d in the hint of a kept worktree (B52 U1 review).
func TestPruneRunHintsKeepDeleteBranches(t *testing.T) {
	t.Parallel()
	f, _, _ := pruneScenario(t)
	plan := f.prunePlan(t, PruneOptions{DeleteBranches: true})
	if s, ok := skipFor(plan, "old"); !ok || !strings.HasSuffix(s.Reason, "run: sheaf prune --delete-branches --include-branches") {
		t.Errorf("skip old = %+v, %v", s, ok)
	}
	opts := PruneOptions{DeleteBranches: true, WorkingDir: f.w.Root}
	plan = f.prunePlan(t, opts)
	writeFile(t, filepath.Join(f.w.Root, "a", "feat", "file-1.txt"), "changed\n")
	rs := ApplyPrune(context.Background(), f.w, plan, opts, Discard)
	if r, _ := resultFor(rs, "feat"); r.Outcome != PruneKept || !strings.HasSuffix(r.Message, "; review it, then run: sheaf wt rm -d --dir feat -- a") {
		t.Errorf("dirty feat: %+v", r)
	}
}

// The hints of a command name the bundle of hints of its context (see
// WithHintBundle).
func TestCommandLineNamesTheBundle(t *testing.T) {
	t.Parallel()
	ctx := WithHintBundle(context.Background(), "/my bundle")
	if got, want := CommandLine(ctx, "prune", "--yes"), "sheaf --bundle '/my bundle' prune --yes"; got != want {
		t.Errorf("CommandLine = %q, want %q", got, want)
	}
	p := Proposal{Key: "gone:o/a", Kind: ProposalGone, Org: "o", Name: "a", Slug: "o/a", MassRemoval: true}
	s := SyncRemoveGone(ctx, nil, []Proposal{p}, SyncRemoveOptions{Remove: RemoveOptions{WorkingDir: t.TempDir()}}, 1, Discard)
	if want := "run: sheaf --bundle '/my bundle' sync --org o --remove-gone --yes --allow-mass-removal"; len(s.Results) != 1 || !strings.HasSuffix(s.Results[0].Err.Error(), want) {
		t.Errorf("summary %+v, want an error ending with %q", s, want)
	}
}
