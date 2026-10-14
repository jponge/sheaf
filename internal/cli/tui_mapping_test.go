package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

func TestOrgRepoItems(t *testing.T) {
	b := &bundle.Bundle{Repositories: []bundle.Repository{{Name: "alpha", Slug: "o/Alpha"}}}
	avail := []hosting.Repo{
		{Owner: "o", Name: "alpha", Description: "already here"},
		{Owner: "o", Name: "beta", Description: "a fork", Fork: true},
		{Owner: "o", Name: "old", Archived: true},
	}
	got := orgRepoItems(avail, b)
	want := []tui.Item{
		{Key: "alpha", Label: "alpha", Detail: "already here", Tags: []string{"present"}, Selected: true, Disabled: true},
		{Key: "beta", Label: "beta", Detail: "a fork", Tags: []string{"fork"}},
		{Key: "old", Label: "old", Tags: []string{"archived"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orgRepoItems =\n%+v\nwant\n%+v", got, want)
	}
}

func TestProposalItems(t *testing.T) {
	ps := []ops.Proposal{
		{Key: "new:o/b", Kind: ops.ProposalNew, Org: "o", Name: "b", Slug: "o/b", Repo: hosting.Repo{Owner: "o", Name: "b", Description: "bee", Fork: true}},
		{Key: "new:o/c", Kind: ops.ProposalNew, Org: "o", Name: "c", Slug: "o/c", Repo: hosting.Repo{Owner: "o", Name: "c"}, Note: "directory c already used"},
		{Key: "gone:o/d", Kind: ops.ProposalGone, Org: "o", Name: "d", Slug: "o/d", MassRemoval: true},
		{Key: "moved:o/e", Kind: ops.ProposalMoved, Org: "o", Name: "e", Slug: "o/e", Repo: hosting.Repo{Owner: "p", Name: "e2"}},
		{Key: "archived:o/f", Kind: ops.ProposalArchived, Org: "o", Name: "f", Slug: "o/f", Repo: hosting.Repo{Owner: "o", Name: "f", Archived: true}},
	}
	got := proposalItems(ps)
	want := []tui.Item{
		{Key: "new:o/b", Label: "b", Detail: "o/b: bee", Tags: []string{"new", "fork"}},
		{Key: "new:o/c", Label: "c", Detail: "o/c: directory c already used", Tags: []string{"new"}},
		{Key: "gone:o/d", Label: "d", Detail: "o/d", Tags: []string{"gone", "mass removal: refused"}},
		{Key: "moved:o/e", Label: "e", Detail: "o/e, now p/e2", Tags: []string{"moved"}, Disabled: true},
		{Key: "archived:o/f", Label: "f", Detail: "o/f", Tags: []string{"archived"}, Disabled: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposalItems =\n%+v\nwant\n%+v", got, want)
	}
}

func TestExcludeItems(t *testing.T) {
	ps := []ops.Proposal{{Key: "new:o/c", Kind: ops.ProposalNew, Org: "o", Name: "c", Slug: "o/c"}}
	want := []tui.Item{{Key: "new:o/c", Label: "c", Detail: "o/c"}}
	if got := excludeItems(ps); !reflect.DeepEqual(got, want) {
		t.Fatalf("excludeItems = %+v, want %+v", got, want)
	}
}

func TestCandidateItems(t *testing.T) {
	cs := []ops.Candidate{
		{Name: "alpha", Path: "/b/alpha", Kind: ops.CandidatePlainClone, Primary: "/b/alpha"},
		{Name: "beta", Path: "/b/beta", Kind: ops.CandidateLaidOut, Primary: "/b/beta/main"},
	}
	want := []tui.Item{
		{Key: "/b/alpha", Label: "alpha", Detail: "/b/alpha", Tags: []string{"plain clone"}, Selected: true},
		{Key: "/b/beta", Label: "beta", Detail: "/b/beta/main", Tags: []string{"laid out"}, Selected: true},
	}
	if got := candidateItems(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidateItems = %+v, want %+v", got, want)
	}
}

func TestEventRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		e     ops.Event
		aside string
		want  tui.RowUpdate
	}{
		{"start", ops.Event{Repo: "a", Phase: ops.PhaseClone, Kind: ops.KindStart},
			"", tui.RowUpdate{Key: "a", Label: "a", State: tui.RowRunning, Message: "clone"}},
		{"progress keeps state", ops.Event{Repo: "a", Phase: ops.PhaseClone, Kind: ops.KindProgress, Message: "cloning x"},
			"", tui.RowUpdate{Key: "a", Label: "a", Message: "cloning x"}},
		{"worktree", ops.Event{Repo: "a", Worktree: "main", Phase: ops.PhaseUpdate, Kind: ops.KindDone, Message: "fast-forwarded"},
			"", tui.RowUpdate{Key: "a/main", Label: "a/main", State: tui.RowDone, Message: "fast-forwarded"}},
		{"skipped", ops.Event{Repo: "a", Phase: ops.PhaseUpdate, Kind: ops.KindSkipped, Message: "dirty"},
			"", tui.RowUpdate{Key: "a", Label: "a", State: tui.RowSkipped, Message: "dirty"}},
		{"failed first line", ops.Event{Repo: "a", Phase: ops.PhaseClone, Kind: ops.KindFailed, Err: errors.New("boom\ndetails")},
			"", tui.RowUpdate{Key: "a", Label: "a", State: tui.RowFailed, Message: "boom"}},
		{"not started", ops.Event{Repo: "a", Phase: ops.PhaseClone, Kind: ops.KindFailed, Message: "not started", Err: context.Canceled},
			"", tui.RowUpdate{Key: "a", Label: "a", State: tui.RowFailed, Message: "not started (interrupted)"}},
		{"wait", ops.Event{Repo: "a", Worktree: "main", Phase: ops.PhaseWaitFastForward, Kind: ops.KindProgress, Message: "waiting"},
			"", tui.RowUpdate{Key: "a/main", Label: "a/main", Message: "waiting for in-progress fast-forward to finish (Ctrl-C again to force)"}},
		{"aside phase", ops.Event{Repo: "a", Phase: ops.PhaseFetch, Kind: ops.KindDone, Message: "fetched origin"},
			ops.PhaseFetch, tui.RowUpdate{Key: "a (fetch)", Label: "a (fetch)", State: tui.RowDone, Message: "fetched origin", Aside: true}},
	} {
		if got := eventRow(tc.e, tc.aside); got != tc.want {
			t.Errorf("%s: eventRow = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Menus and dialogs keep their last line in the terminal: bubbletea erases
// the last line of the final frame on exit, so the session adds an empty
// one, and model updates keep the wrapper.
func TestKeepLastLineWrapper(t *testing.T) {
	m := keepLastLine{tui.NewConfirm("Remove alpha?", []string{"will delete /b/alpha"})}
	if v := m.View(); v != m.Model.View()+"\n" {
		t.Fatalf("View = %q", v)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	w, ok := next.(keepLastLine)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	if c, ok := unwrapModel(w).(tui.Confirm); !ok || !c.Confirmed() {
		t.Fatal("inner model not updated")
	}
}
