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
		{noForkCommand(bundle.Repository{Name: "x", Slug: "up/x"}, ""), "sheaf add --no-fork -- up/x"},
		{noForkCommand(bundle.Repository{Name: "y", Slug: "up/x"}, ""), "sheaf add --no-fork --as y -- up/x"},
		{noForkCommand(bundle.Repository{Name: "x", Slug: "up/x"}, "up"), "sheaf add --org up --no-fork --repo x"},
		{includeArchivedCommand(AddRequest{Org: "o", NoFork: true, As: "z"}, "old"), "sheaf add --org o --include-archived --no-fork --as z --repo old"},
	} {
		if tc.got != tc.want {
			t.Errorf("command = %q, want %q", tc.got, tc.want)
		}
	}
}
