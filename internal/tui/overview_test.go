package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func ovRows() []Row {
	return []Row{
		{Key: "/b/a/main", Cells: []string{"a", "main", "main", "clean"}},
		{Key: "/b/a/feat-x", Cells: []string{"a", "feat-x", "feat/x", "dirty, behind 1"}},
		{Key: "/b/bravo/main", Cells: []string{"bravo", "main", "main", "clean"}},
	}
}

func ovActions() []OverviewAction {
	return []OverviewAction{
		{Key: "enter", Name: "cd", Help: "cd", Scope: ScopeCurrent},
		{Key: "u", Name: "update", Help: "update", Scope: ScopeRows},
		{Key: "w", Name: "add", Help: "add worktree", Scope: ScopeCurrent},
		{Key: "p", Name: "prune", Help: "prune", Scope: ScopeNone},
		{Key: "r", Name: "remove", Help: "remove worktree", Scope: ScopeRows, Check: func(targets []string) string {
			for _, k := range targets {
				if strings.HasSuffix(k, "/main") {
					return "cannot remove a primary checkout"
				}
			}
			return ""
		}},
	}
}

func newOv() Overview {
	return NewOverview("sheaf: /b", []string{"REPO", "DIR", "BRANCH", "STATE"}, ovRows(), ovActions())
}

func asOv(t *testing.T, m tea.Model) Overview {
	t.Helper()
	o, ok := m.(Overview)
	if !ok {
		t.Fatalf("model is %T, want Overview", m)
	}
	return o
}

func assertChosen(t *testing.T, m tea.Model, name string, targets ...string) {
	t.Helper()
	gotName, got := asOv(t, m).Chosen()
	if gotName != name || !reflect.DeepEqual(got, targets) && (len(got) != 0 || len(targets) != 0) {
		t.Fatalf("Chosen() = %q %q, want %q %q", gotName, got, name, targets)
	}
}

func TestOverviewRendersAlignedTable(t *testing.T) {
	t.Parallel()
	v := view(newOv())
	for _, want := range []string{
		"sheaf: /b",
		"      REPO   DIR     BRANCH  STATE",
		"> [ ] a      main    main    clean",
		"  [ ] a      feat-x  feat/x  dirty, behind 1",
		"  [ ] bravo  main    main    clean",
		"0/3 selected",
	} {
		if !hasLine(v, want) {
			t.Fatalf("view lacks line %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "enter cd") || !strings.Contains(v, "u update") || !strings.Contains(v, "esc/q quit") {
		t.Fatalf("help line lacks the actions:\n%s", v)
	}
}

// The help lists every action at the width of a usual terminal: actions
// cut off by the ellipsis of a single help line would be undiscoverable.
func TestOverviewHelpShowsEveryActionAt80Columns(t *testing.T) {
	t.Parallel()
	actions := append(ovActions(),
		OverviewAction{Key: "f", Name: "fetch", Help: "fetch"},
		OverviewAction{Key: "d", Name: "doctor", Help: "doctor", Scope: ScopeNone},
		OverviewAction{Key: "s", Name: "sync", Help: "sync", Scope: ScopeNone})
	var m tea.Model = NewOverview("t", []string{"REPO"}, ovRows(), actions)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v := view(m)
	for _, a := range actions {
		if !strings.Contains(v, a.Key+" "+a.Help) {
			t.Errorf("help lacks %q:\n%s", a.Key+" "+a.Help, v)
		}
	}
	if !strings.Contains(v, "esc/q quit") || strings.Contains(v, "…") {
		t.Errorf("help cut:\n%s", v)
	}
}

func TestOverviewCursorMovesAndActionTargetsCurrentRow(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "down", "j", "k")
	if got := asOv(t, m).CurrentKey(); got != "/b/a/feat-x" {
		t.Fatalf("CurrentKey() = %q", got)
	}
	m, cmd := press(m, "u")
	if !isQuit(cmd) {
		t.Fatal("u did not quit")
	}
	assertChosen(t, m, "update", "/b/a/feat-x")
	if asOv(t, m).Cancelled() || asOv(t, m).Interrupted() {
		t.Fatal("an action is neither a cancellation nor an interruption")
	}
	if v := view(m); v != "" {
		t.Fatalf("final view = %q, want empty (the overview leaves nothing behind)", v)
	}
	m, _ = press(m, "down", "q")
	assertChosen(t, m, "update", "/b/a/feat-x")
}

func TestOverviewSelectionTargetsSelectedRows(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "down", "down", "space", "up", "up", "space")
	if !hasLine(view(m), "2/3 selected") {
		t.Fatalf("status:\n%s", view(m))
	}
	if !strings.Contains(view(m), "> [x] a ") {
		t.Fatalf("current row not checked:\n%s", view(m))
	}
	m, _ = press(m, "u")
	assertChosen(t, m, "update", "/b/a/main", "/b/bravo/main")
}

// Like MultiSelect.SelectedKeys on confirmation, the targets are every
// selected row, hidden by the filter or not; the status line counts them.
func TestOverviewSelectedRowsHiddenByFilterAreTargets(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "space", "/", "b", "r", "a", "v", "o", "enter", "space")
	if v := view(m); strings.Contains(v, "feat-x") || !strings.Contains(v, "2/3 selected · 1 shown") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(m, "u")
	assertChosen(t, m, "update", "/b/a/main", "/b/bravo/main")
}

func TestOverviewSpaceUntoggles(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "space", "space", "down", "u")
	assertChosen(t, m, "update", "/b/a/feat-x")
}

func TestOverviewScopeCurrentIgnoresSelection(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "space", "down", "enter")
	assertChosen(t, m, "cd", "/b/a/feat-x")
	m, _ = press(newOv(), "space", "down", "w")
	assertChosen(t, m, "add", "/b/a/feat-x")
}

func TestOverviewScopeNoneHasNoTargets(t *testing.T) {
	t.Parallel()
	m, cmd := press(newOv(), "space", "p")
	if !isQuit(cmd) {
		t.Fatal("p did not quit")
	}
	assertChosen(t, m, "prune")
}

func TestOverviewQuitKeys(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"q", "esc"} {
		m, cmd := press(newOv(), k)
		if !isQuit(cmd) || !asOv(t, m).Cancelled() || asOv(t, m).Interrupted() {
			t.Fatalf("%s: quit=%v cancelled=%v interrupted=%v", k, isQuit(cmd), asOv(t, m).Cancelled(), asOv(t, m).Interrupted())
		}
		assertChosen(t, m, "")
	}
	m, cmd := press(newOv(), "ctrl+c")
	if !isQuit(cmd) || !asOv(t, m).Interrupted() {
		t.Fatalf("ctrl+c: quit=%v interrupted=%v", isQuit(cmd), asOv(t, m).Interrupted())
	}
	assertChosen(t, m, "")
}

func TestOverviewFilterTwoStepEsc(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "/", "B", "r")
	v := view(m)
	if !hasLine(v, "/Br█") || strings.Contains(v, "feat-x") || !strings.Contains(v, "bravo") {
		t.Fatalf("filter mode, case-insensitive match on any cell:\n%s", v)
	}
	if _, cmd := press(m, "u"); isQuit(cmd) {
		t.Fatal("a letter typed in filter mode triggered an action")
	}
	// enter applies the filter; esc then clears it; esc again quits.
	m, cmd := press(m, "enter")
	if isQuit(cmd) || asOv(t, m).Filter() != "Br" || !hasLine(view(m), "/Br") {
		t.Fatalf("enter must apply the filter:\n%s", view(m))
	}
	m, cmd = press(m, "esc")
	if isQuit(cmd) || asOv(t, m).Filter() != "" || !strings.Contains(view(m), "feat-x") {
		t.Fatalf("first esc must clear the applied filter:\n%s", view(m))
	}
	_, cmd = press(m, "esc")
	if !isQuit(cmd) {
		t.Fatal("second esc did not quit")
	}
	// esc in filter mode clears the filter and returns to the list.
	m, _ = press(newOv(), "/", "z", "z", "esc")
	if asOv(t, m).Filter() != "" || strings.Contains(view(m), "zz") {
		t.Fatalf("esc in filter mode:\n%s", view(m))
	}
	m, _ = press(newOv(), "/", "z", "z", "z", "enter")
	if !strings.Contains(view(m), "No matches.") {
		t.Fatalf("no match:\n%s", view(m))
	}
	// Filter editing keys.
	m, _ = press(newOv(), "/", "f", "e", "a", "backspace")
	if asOv(t, m).Filter() != "fe" {
		t.Fatalf("backspace: filter %q", asOv(t, m).Filter())
	}
	m, _ = press(m, "ctrl+u")
	if asOv(t, m).Filter() != "" {
		t.Fatalf("ctrl+u: filter %q", asOv(t, m).Filter())
	}
}

func TestOverviewActionOnFilteredRows(t *testing.T) {
	t.Parallel()
	m, _ := press(newOv(), "/", "f", "e", "a", "t", "enter", "u")
	assertChosen(t, m, "update", "/b/a/feat-x")
	// No visible row: an action needing one is refused.
	m, cmd := press(newOv(), "/", "z", "z", "enter", "u")
	if isQuit(cmd) {
		t.Fatal("u quit without any row")
	}
	assertChosen(t, m, "")
	if !strings.Contains(view(m), "no row") {
		t.Fatalf("refusal not displayed:\n%s", view(m))
	}
	// A bundle-wide action does not need a row.
	_, cmd = press(m, "p")
	if !isQuit(cmd) {
		t.Fatal("p refused without rows")
	}
}

func TestOverviewCheckRefusesWithMessage(t *testing.T) {
	t.Parallel()
	m, cmd := press(newOv(), "r")
	if isQuit(cmd) {
		t.Fatal("refused action quit")
	}
	assertChosen(t, m, "")
	if !strings.Contains(view(m), "cannot remove a primary checkout") {
		t.Fatalf("refusal not displayed:\n%s", view(m))
	}
	m, cmd = press(m, "down", "r")
	if !isQuit(cmd) {
		t.Fatal("accepted action did not quit")
	}
	assertChosen(t, m, "remove", "/b/a/feat-x")
}

func TestOverviewWithMessageCursorAndFilter(t *testing.T) {
	t.Parallel()
	m := newOv().WithMessage("update: 1 fast-forwarded").WithCursor("/b/bravo/main")
	v := view(m)
	if !hasLine(v, "update: 1 fast-forwarded") || !strings.Contains(v, "> [ ] bravo") {
		t.Fatalf("message or cursor:\n%s", v)
	}
	m = newOv().WithFilter("feat")
	if v := view(m); strings.Contains(v, "bravo") || !hasLine(v, "/feat") {
		t.Fatalf("filter:\n%s", v)
	}
	// A cursor key that is not visible keeps the first row.
	if got := newOv().WithCursor("/nope").CurrentKey(); got != "/b/a/main" {
		t.Fatalf("CurrentKey() = %q", got)
	}
}

func TestOverviewScrollsToFitHeight(t *testing.T) {
	t.Parallel()
	var rows []Row
	for i := range 30 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	if n := lineCount(view(m)); n > 10 {
		t.Fatalf("view has %d lines for a height of 10:\n%s", n, view(m))
	}
	m, _ = press(m, "end")
	v := view(m)
	if !strings.Contains(v, "> [ ] r29") || !strings.Contains(v, "of 30") {
		t.Fatalf("end:\n%s", v)
	}
	if n := lineCount(v); n > 10 {
		t.Fatalf("view has %d lines for a height of 10", n)
	}
}

func TestOverviewIgnoresMultiRuneKeysAndPastesInList(t *testing.T) {
	t.Parallel()
	m, cmd := newOv().Update(keyMsg("uq"))
	if isQuit(cmd) {
		t.Fatal("a multi-rune key event triggered an action")
	}
	m, cmd = m.Update(paste("r"))
	if isQuit(cmd) {
		t.Fatal("a paste triggered an action")
	}
	assertChosen(t, m, "")
	// In filter mode a paste edits the filter.
	m, _ = press(newOv(), "/")
	m, _ = m.Update(paste("fe\nat"))
	if asOv(t, m).Filter() != "feat" {
		t.Fatalf("pasted filter = %q", asOv(t, m).Filter())
	}
}

func TestOverviewSanitizesCells(t *testing.T) {
	t.Parallel()
	rows := []Row{{Key: "k", Cells: []string{"a" + injectedSGR + "b", "x\ny"}}}
	m := NewOverview("t\x1b[2J", []string{"REPO", "DIR"}, rows, nil).WithMessage("m\x07")
	assertNoInjectedSGR(t, m)
	assertNoControlChars(t, view(m))
}

// a selects and n deselects every visible row, like MultiSelect; the
// targets of a ScopeRows action are then those rows.
func TestOverviewSelectAllAndNone(t *testing.T) {
	t.Parallel()
	m, cmd := press(newOv(), "a")
	if isQuit(cmd) {
		t.Fatal("a quit the overview")
	}
	if v := view(m); !strings.Contains(v, "3/3 selected") || !strings.Contains(v, "[x] bravo") {
		t.Fatalf("a did not select every row:\n%s", v)
	}
	m2, _ := press(m, "u")
	assertChosen(t, m2, "update", "/b/a/main", "/b/a/feat-x", "/b/bravo/main")
	m, _ = press(m, "n")
	if v := view(m); !strings.Contains(v, "0/3 selected") || strings.Contains(v, "[x]") {
		t.Fatalf("n did not deselect every row:\n%s", v)
	}
	// Without a selection, the action targets the current row again.
	m, _ = press(m, "down", "u")
	assertChosen(t, m, "update", "/b/a/feat-x")
}

// a and n apply to the visible rows only: rows hidden by the filter keep
// their selection state.
func TestOverviewSelectAllAndNoneApplyToVisibleRows(t *testing.T) {
	t.Parallel()
	// a/main selected, then a on the rows matching "feat" (feat-x only).
	m, _ := press(newOv(), "space", "/", "f", "e", "a", "t", "enter", "a")
	if v := view(m); !strings.Contains(v, "2/3 selected · 1 shown") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(m, "esc", "u")
	assertChosen(t, m, "update", "/b/a/main", "/b/a/feat-x")

	// Everything selected, then n on the rows matching "bravo".
	m, _ = press(newOv(), "a", "/", "b", "r", "a", "v", "o", "enter", "n")
	if v := view(m); !strings.Contains(v, "2/3 selected · 1 shown") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(m, "esc", "u")
	assertChosen(t, m, "update", "/b/a/main", "/b/a/feat-x")

	// No match: a and n change nothing.
	m, _ = press(newOv(), "space", "/", "z", "z", "enter", "n", "esc", "u")
	assertChosen(t, m, "update", "/b/a/main")
	m, _ = press(newOv(), "/", "z", "z", "enter", "a", "esc")
	if v := view(m); !strings.Contains(v, "0/3 selected") {
		t.Fatalf("a without visible rows selected some:\n%s", v)
	}
}

// Alert rows are selectable with space, so a selects them too.
func TestOverviewSelectAllIncludesAlertRows(t *testing.T) {
	t.Parallel()
	rows := append(ovRows(), Row{Key: "/b/c/main", Cells: []string{"c", "main", "", "problem"}, Alert: true})
	m, _ := press(NewOverview("t", []string{"REPO", "DIR", "BRANCH", "STATE"}, rows, ovActions()), "a", "u")
	assertChosen(t, m, "update", "/b/a/main", "/b/a/feat-x", "/b/bravo/main", "/b/c/main")
}

func TestOverviewSelectAllKeepsEarlierCopies(t *testing.T) {
	t.Parallel()
	m1 := newOv()
	m2, _ := press(m1, "a")
	if v := view(m1); !strings.Contains(v, "0/3 selected") {
		t.Fatalf("earlier copy changed:\n%s", v)
	}
	if v := view(m2); !strings.Contains(v, "3/3 selected") {
		t.Fatalf("view:\n%s", v)
	}
	m3, _ := press(m2, "n")
	if v := view(m2); !strings.Contains(v, "3/3 selected") || !strings.Contains(view(m3), "0/3 selected") {
		t.Fatalf("earlier copy changed:\n%s", v)
	}
}

func TestOverviewSelectAllAndNoneWithoutRows(t *testing.T) {
	t.Parallel()
	m, cmd := press(NewOverview("t", []string{"REPO"}, nil, ovActions()), "a", "n")
	if isQuit(cmd) || !strings.Contains(view(m), "0/0 selected") {
		t.Fatalf("view:\n%s", view(m))
	}
}

func TestOverviewHelpShowsSelectAllAndNone(t *testing.T) {
	t.Parallel()
	var m tea.Model = newOv()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v := view(m)
	for _, want := range []string{"space toggle", "a all", "n none", "w add worktree"} {
		if !strings.Contains(v, want) {
			t.Errorf("help lacks %q:\n%s", want, v)
		}
	}
}

// A new overview can start with the selection of a previous one (keys of
// rows that no longer exist are ignored), and returns its selection.
func TestOverviewWithSelectedRestoresTheSelection(t *testing.T) {
	t.Parallel()
	m := newOv().WithSelected([]string{"/b/bravo/main", "/b/gone/main"})
	if v := view(m); !strings.Contains(v, "1/3 selected") || !hasLine(v, "  [x] bravo  main    main    clean") {
		t.Fatalf("view:\n%s", v)
	}
	next, _ := press(m, "space")
	if got := asOv(t, next).SelectedKeys(); !reflect.DeepEqual(got, []string{"/b/a/main", "/b/bravo/main"}) {
		t.Fatalf("SelectedKeys() = %q", got)
	}
	next, _ = press(next, "u")
	assertChosen(t, next, "update", "/b/a/main", "/b/bravo/main")
}
