package tui

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
		{Key: "enter", Name: "cd", Help: "cd", Scope: ScopeCurrent, Short: true},
		{Key: "u", Name: "update", Help: "update", Scope: ScopeRows, Short: true},
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
	if !strings.Contains(v, "enter cd") || !strings.Contains(v, "u update") || !strings.Contains(v, "q quit") {
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
	var m tea.Model = NewOverview("t", []string{"REPO"}, ovRows(), actions).WithFullHelp(true)
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
	if !hasLine(v, "0/3 selected · update: 1 fast-forwarded") || !strings.Contains(v, "> [ ] bravo") {
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
	var m tea.Model = newOv().WithFullHelp(true)
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

// WithAltScreen renders the overview on the alternate screen, its final
// (empty) view included, so that bubbletea leaves the alternate screen when
// the program ends; by default the overview is inline.
func TestOverviewWithAltScreen(t *testing.T) {
	t.Parallel()
	if newOv().View().AltScreen {
		t.Fatal("inline overview on the alternate screen")
	}
	m := newOv().WithAltScreen()
	if v := m.View(); !v.AltScreen || !strings.Contains(v.Content, "STATE") {
		t.Fatalf("view on the alternate screen %v:\n%s", v.AltScreen, v.Content)
	}
	for _, k := range []string{"q", "u"} {
		done, _ := press(m, k)
		if v := done.View(); !v.AltScreen || v.Content != "" {
			t.Errorf("%s: final view on the alternate screen %v: %q", k, v.AltScreen, v.Content)
		}
	}
}

// ovShape classifies the lines of an overview view: T title, F filter,
// H header, R row, S status line, K key help, ? other.
func ovShape(m tea.Model) string {
	row := regexp.MustCompile(`^(> |  )\[[ x]\] `)
	status := regexp.MustCompile(`^\d+/\d+ selected`)
	return shape(view(m), func(l string) string {
		switch {
		case l == "t" || l == "sheaf: /b":
			return "T"
		case strings.HasPrefix(l, "/"):
			return "F"
		case strings.Contains(l, "REPO"):
			return "H"
		case row.MatchString(l):
			return "R"
		case status.MatchString(l):
			return "S"
		case strings.Contains(l, " • "):
			return "K"
		}
		return "?"
	})
}

// A blank line separates the title from the rest (B41), another one the
// rows from the status line, which also shows the message, and another one
// it from the key help.
func TestOverviewSpacesTheFooter(t *testing.T) {
	t.Parallel()
	for _, size := range []tea.Msg{nil, tea.WindowSizeMsg{Width: 80, Height: 24}} {
		for _, msg := range []string{"", "prune: nothing to prune"} {
			var m tea.Model = newOv().WithFullHelp(true).WithMessage(msg)
			if size != nil {
				m, _ = m.Update(size)
			}
			if got := ovShape(m); got != "T _ H R R R _ S _ K K" {
				t.Errorf("size %v, message %q: shape %q:\n%s", size, msg, got, view(m))
			}
			want := "0/3 selected"
			if msg != "" {
				want += " · " + msg
			}
			if !hasLine(view(m), want) {
				t.Errorf("size %v: no status line %q:\n%s", size, want, view(m))
			}
			m, _ = press(m, "/")
			if got := ovShape(m); got != "T _ F H R R R _ S _ K" {
				t.Errorf("size %v, message %q, filter mode: shape %q:\n%s", size, msg, got, view(m))
			}
		}
	}
}

// On a short terminal the blank lines go first (the one above the help
// first, then the one below the title), then the help, title, status and
// header lines as before; the
// view never exceeds the height and keeps the cursor row.
func TestOverviewShortTerminalDropsSpacingFirst(t *testing.T) {
	t.Parallel()
	var rows []Row
	for i := range 30 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	want := map[int]string{
		10: "T _ H R R _ S _ K K",
		9:  "T _ H R _ S _ K K",
		8:  "T _ H R _ S K K",
		7:  "T H R _ S K K",
		6:  "T H R S K K",
		5:  "T H R R S",
		4:  "T H R S",
		3:  "H R S",
		2:  "H R",
		1:  "R",
	}
	for h := 1; h <= 10; h++ {
		var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions()).WithFullHelp(true).WithMessage("m")
		m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		m, _ = press(m, "end")
		v := view(m)
		if got := ovShape(m); got != want[h] || lineCount(v) > h || !strings.Contains(v, "> [ ] r29") {
			t.Errorf("height %d: shape %q, want %q:\n%s", h, got, want[h], v)
		}
	}
}

// An overview whose rows all fit without some of the blank lines drops
// them (the one above the help first, then the one below the title) rather
// than scroll; one that scrolls anyway keeps them, unless no row would fit.
func TestOverviewDropsSpacingToFitWithoutScrolling(t *testing.T) {
	t.Parallel()
	want := map[int]string{
		11: "T _ H R R R _ S _ K K",
		10: "T _ H R R R _ S K K",
		9:  "T H R R R _ S K K",
		8:  "T H R R R S K K",
		7:  "T H R _ S K K",
	}
	for h := 7; h <= 11; h++ {
		var m tea.Model = newOv().WithFullHelp(true).WithMessage("m")
		m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		v := view(m)
		if got := ovShape(m); got != want[h] || lineCount(v) > h {
			t.Errorf("height %d: shape %q, want %q:\n%s", h, got, want[h], v)
		}
		if scrolls := strings.Contains(v, "rows 1-"); scrolls != (h == 7) {
			t.Errorf("height %d: scroll status %v:\n%s", h, scrolls, v)
		}
	}
}

// A message cut off entirely by the truncation to the width leaves no
// dangling separator on the status line.
func TestOverviewNarrowStatusLineHasNoDanglingSeparator(t *testing.T) {
	t.Parallel()
	for w := 10; w <= 20; w++ {
		var m tea.Model = newOv().WithMessage("prune: nothing to prune")
		m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
		for _, l := range strings.Split(view(m), "\n") {
			if strings.HasSuffix(strings.TrimRight(l, " "), "·") || strings.HasPrefix(l, "0/3 selected ") && !strings.Contains(l, "· p") {
				t.Errorf("width %d: status line %q", w, l)
			}
		}
	}
	var m tea.Model = newOv().WithMessage("prune: nothing to prune")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 16, Height: 24})
	if !hasLine(view(m), "0/3 selected · p") {
		t.Errorf("width 16:\n%s", view(m))
	}
}

// The status line joined with the message is truncated to the width like
// the other lines.
func TestOverviewTruncatesTheStatusLine(t *testing.T) {
	t.Parallel()
	var m tea.Model = newOv().WithMessage(strings.Repeat("x", 100))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	for _, l := range strings.Split(view(m), "\n") {
		if w := lipgloss.Width(l); w > 30 {
			t.Errorf("line of width %d: %q", w, l)
		}
	}
	if !strings.Contains(view(m), "0/3 selected · xxx") {
		t.Errorf("view:\n%s", view(m))
	}
}

// Truncating the status line to the width never leaves a separator (or
// its start) at its end, whichever part is cut off: the number
// shown, the scroll position or the message.
func TestOverviewNarrowStatusPartsLeaveNoDanglingSeparator(t *testing.T) {
	t.Parallel()
	var rows []Row
	for i := range 25 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	for _, msg := range []string{"", "prune: nothing to prune"} {
		for _, f := range []string{"", "r", "zzz"} {
			var ov tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions()).WithMessage(msg).WithFilter(f)
			wide, _ := ov.Update(tea.WindowSizeMsg{Width: 200, Height: 10})
			full := statusLine(t, wide)
			for w := 10; w <= 40; w++ {
				m, _ := ov.Update(tea.WindowSizeMsg{Width: w, Height: 10})
				if l := statusLine(t, m); danglingSeparator(full, l) {
					t.Errorf("message %q, filter %q, width %d: status line %q", msg, f, w, l)
				}
			}
		}
	}
}

// Every key of the movement and filter help lines is visible at any width
// from 18 columns (B31 follow-up: the movement line was cut below ~73
// columns and, at 18, the filter help lost "esc clear filter"), and the
// wrapped lines count in the layout: the view never exceeds the height.
func TestOverviewKeyHelpWrapsToWidth(t *testing.T) {
	t.Parallel()
	list := []string{"↑/k up", "↓/j down", "space toggle", "a all", "n none", "/ filter", "esc/q quit"}
	filter := []string{"enter apply filter", "esc clear filter"}
	for w := 18; w <= 80; w++ {
		var m tea.Model = newOv().WithFullHelp(true)
		m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		for _, mode := range []struct {
			name string
			keys []string
		}{{"list", list}, {"filter", filter}} {
			if mode.name == "filter" {
				m, _ = press(m, "/")
			}
			v := view(m)
			for _, k := range mode.keys {
				if !strings.Contains(v, k) {
					t.Errorf("width %d, %s mode: key help %q missing:\n%s", w, mode.name, k, v)
				}
			}
			for _, l := range strings.Split(v, "\n") {
				if lipgloss.Width(l) > w || strings.Contains(l, "…") {
					t.Errorf("width %d, %s mode: line %q", w, mode.name, l)
				}
			}
		}
	}
	for _, filtering := range []bool{false, true} {
		for h := 1; h <= 16; h++ {
			var m tea.Model = newOv().WithFullHelp(true)
			if filtering {
				m, _ = press(m, "/")
			}
			m, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: h})
			m, _ = press(m, "end")
			if v := view(m); lineCount(v) > h || !strings.Contains(v, "> [ ] bravo") {
				t.Errorf("width 20, filtering %v, height %d: %d lines:\n%s", filtering, h, lineCount(v), v)
			}
		}
	}
}

// A status part is shown only when it fits whole with its separator: a
// part cut partway could read as another number ("12 shown" as "12",
// "rows 18-20 of 20" as "… of 2"). The message, free text, is still
// truncated like any line (see TestOverviewTruncatesTheStatusLine).
func TestOverviewStatusPartsAreShownWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	var rows []Row
	for i := range 25 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	for f, n := range map[string]int{"": 25, "r": 25, "1": 12, "zzz": 0} {
		whole := regexp.MustCompile(fmt.Sprintf(`^(%d shown|rows \d+-\d+ of %d)$`, n, n))
		for w := 14; w <= 60; w++ {
			var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions()).WithFilter(f)
			m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 10})
			m, _ = press(m, "end")
			l := statusLine(t, m)
			for _, p := range strings.Split(l, " · ")[1:] {
				if !whole.MatchString(p) {
					t.Errorf("filter %q, width %d: part %q cut (status line %q)", f, w, p, l)
				}
			}
		}
	}
	var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions()).WithFilter("1")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 18, Height: 30})
	if l := statusLine(t, m); l != "0/25 selected" {
		t.Errorf("width 18: status line %q, want %q", l, "0/25 selected")
	}
}

// With grouping, the first cell of a row repeating the one of the visible
// row above it is blank; the first visible row of a group, and the first
// row rendered after scrolling, show it. The filter still matches the
// blanked cells.
func TestOverviewGroupsRowsByFirstCell(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{Key: "1", Cells: []string{"alpha", "main", "clean"}},
		{Key: "2", Cells: []string{"alpha", "feat-x", "dirty"}},
		{Key: "3", Cells: []string{"alpha", "feat-z", "clean"}},
		{Key: "4", Cells: []string{"bravo", "main", "clean"}},
	}
	newGrouped := func() Overview {
		return NewOverview("t", []string{"REPO", "DIR", "STATE"}, rows, nil).WithGrouping()
	}
	wantLines := func(m tea.Model, want ...string) {
		t.Helper()
		v := view(m)
		for _, w := range want {
			if !hasLine(v, w) {
				t.Errorf("view lacks line %q:\n%s", w, v)
			}
		}
	}
	wantLines(newGrouped(),
		"> [ ] alpha  main    clean",
		"  [ ]        feat-x  dirty",
		"  [ ]        feat-z  clean",
		"  [ ] bravo  main    clean")
	// The first visible row of a group shows its name.
	wantLines(newGrouped().WithFilter("feat-z"), "> [ ] alpha  feat-z  clean")
	wantLines(newGrouped().WithFilter("dirty"), "> [ ] alpha  feat-x  dirty")
	// The filter matches the blanked names.
	m := newGrouped().WithFilter("alph")
	wantLines(m, "> [ ] alpha  main    clean", "  [ ]        feat-x  dirty", "  [ ]        feat-z  clean")
	if v := view(m); !strings.Contains(v, "3 shown") || strings.Contains(v, "bravo") {
		t.Errorf("filter alph:\n%s", v)
	}
	// The row above is the visible one, not the one hidden by the filter.
	split := []Row{rows[0], rows[3], rows[1]}
	wantLines(NewOverview("t", []string{"REPO", "DIR", "STATE"}, split, nil).WithGrouping().WithFilter("alpha"),
		"> [ ] alpha  main    clean", "  [ ]        feat-x  dirty")
	// After scrolling, the first row rendered shows its name although the
	// row above it is scrolled off.
	more := append(slices.Clone(rows), Row{Key: "5", Cells: []string{"bravo", "b1", "clean"}}, Row{Key: "6", Cells: []string{"bravo", "b2", "clean"}})
	var sm tea.Model = NewOverview("t", []string{"REPO", "DIR", "STATE"}, more, nil).WithGrouping()
	sm, _ = sm.Update(tea.WindowSizeMsg{Width: 80, Height: 9}) // 2 rows
	sm, _ = press(sm, "down", "down")
	if v := view(sm); !hasLine(v, "  [ ] alpha  feat-x  dirty") || !hasLine(v, "> [ ]        feat-z  clean") {
		t.Errorf("scrolled:\n%s", v)
	}
	// Without grouping every row shows its first cell.
	wantLines(NewOverview("t", []string{"REPO", "DIR", "STATE"}, rows, nil), "  [ ] alpha  feat-x  dirty")
}

// Shrinkable columns are truncated, in their order and down to a minimum
// width, before the last column is cut at the window width; the width of
// the last column ignores alert rows.
func TestOverviewShrinksColumnsBeforeTheLast(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{Key: "1", Cells: []string{"alpha", "feature-dir-long", "feature/very-long-branch", "2d", "clean, ↑2 ↓1"}},
		{Key: "2", Cells: []string{"alpha", "main", "main", "5m", "dirty"}},
		{Key: "3", Cells: []string{"beta", "x", "-", "", "problem: " + strings.Repeat("long text ", 8)}, Alert: true},
	}
	header := []string{"REPO", "DIR", "BRANCH", "AGE", "STATE"}
	at := func(m Overview, width int) string {
		t.Helper()
		var tm tea.Model = m
		tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		v := view(tm)
		for _, l := range strings.Split(v, "\n") {
			if w := lipgloss.Width(l); w > width {
				t.Errorf("width %d: line of width %d: %q", width, w, l)
			}
		}
		return v
	}
	shrinkable := func() Overview { return NewOverview("t", header, rows, nil).WithShrinkable(2, 1) }
	for _, tc := range []struct {
		width int
		want  []string
	}{
		{100, []string{"      REPO   DIR               BRANCH                    AGE  STATE", "> [ ] alpha  feature-dir-long  feature/very-long-branch  2d   clean, ↑2 ↓1"}},
		{60, []string{"      REPO   DIR               BRANCH      AGE  STATE", "> [ ] alpha  feature-dir-long  feature/v…  2d   clean, ↑2 ↓1", "  [ ] alpha  main              main        5m   dirty"}},
		{50, []string{"      REPO   DIR       BRANCH    AGE  STATE", "> [ ] alpha  feature…  feature…  2d   clean, ↑2 ↓1"}},
		{40, []string{"> [ ] alpha  feature…  feature…  2d   cl"}},
	} {
		if v := at(shrinkable(), tc.width); !hasAllLines(v, tc.want) {
			t.Errorf("width %d: view lacks one of %q:\n%s", tc.width, tc.want, v)
		}
	}
	// The last column is never shrunk.
	if v := at(NewOverview("t", header, rows, nil).WithShrinkable(4, 2, 1), 60); !hasLine(v, "> [ ] alpha  feature-dir-long  feature/v…  2d   clean, ↑2 ↓1") {
		t.Errorf("last column listed as shrinkable:\n%s", v)
	}
	// The filter matches the whole cells.
	if v := at(shrinkable().WithFilter("very-long"), 60); !strings.Contains(v, "1 shown") {
		t.Errorf("filter on a truncated cell:\n%s", v)
	}
	// Without shrinkable columns the line is cut.
	if v := at(NewOverview("t", header, rows, nil), 60); !hasLine(v, "> [ ] alpha  feature-dir-long  feature/very-long-branch  2d ") {
		t.Errorf("without shrinkable columns:\n%s", v)
	}
}

func hasAllLines(v string, want []string) bool {
	for _, w := range want {
		if !hasLine(v, w) {
			return false
		}
	}
	return true
}

// The key help is one short line by default (B45): toggle, the Short
// actions, filter, "? more" and quit; ? toggles the full list (every key,
// "? less"), and back. In filter mode ? is typed into the filter.
func TestOverviewCompactHelpTogglesWithQuestionMark(t *testing.T) {
	t.Parallel()
	const short = "space toggle • enter cd • u update • / filter • ? more • q quit"
	var m tea.Model = newOv()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	helpOf := func(m tea.Model) []string {
		var help []string
		for _, l := range strings.Split(view(m), "\n") {
			if strings.Contains(l, " • ") {
				help = append(help, l)
			}
		}
		return help
	}
	if got := helpOf(m); !slices.Equal(got, []string{short}) || asOv(t, m).FullHelp() {
		t.Fatalf("compact help %q, want %q", got, short)
	}
	m, cmd := press(m, "?")
	full := strings.Join(helpOf(m), "\n")
	if isQuit(cmd) || !asOv(t, m).FullHelp() || len(helpOf(m)) != 2 {
		t.Fatalf("full help (quit %v):\n%s", isQuit(cmd), full)
	}
	for _, want := range []string{"↑/k up", "↓/j down", "a all", "n none", "? less", "esc/q quit", "w add worktree", "p prune", "r remove worktree"} {
		if !strings.Contains(full, want) {
			t.Errorf("full help lacks %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "? more") {
		t.Errorf("full help offers more:\n%s", full)
	}
	m, _ = press(m, "?")
	if got := helpOf(m); !slices.Equal(got, []string{short}) {
		t.Errorf("help after ? ?: %q", got)
	}
	m, _ = press(m, "/", "?")
	if o := asOv(t, m); o.Filter() != "?" || o.FullHelp() {
		t.Errorf("filter mode: filter %q, full help %v", o.Filter(), o.FullHelp())
	}
	// WithFullHelp sets the state, e.g. that of a previous overview.
	if o := newOv().WithFullHelp(true); !o.FullHelp() || !strings.Contains(view(o), "? less") {
		t.Errorf("WithFullHelp(true):\n%s", view(o))
	}
}

// The compact help is one line of the layout (help lines are chrome, see
// fitGaps): the blank lines are kept or dropped as with the full help, and
// toggling the full help on a short terminal keeps the cursor row shown.
func TestOverviewCompactHelpLayout(t *testing.T) {
	t.Parallel()
	for _, size := range []tea.Msg{nil, tea.WindowSizeMsg{Width: 80, Height: 24}} {
		var m tea.Model = newOv().WithMessage("m")
		if size != nil {
			m, _ = m.Update(size)
		}
		if got := ovShape(m); got != "T _ H R R R _ S _ K" {
			t.Errorf("size %v: shape %q:\n%s", size, got, view(m))
		}
	}
	want := map[int]string{
		10: "T _ H R R R _ S _ K",
		9:  "T _ H R R R _ S K",
		8:  "T H R R R _ S K",
		7:  "T H R R R S K",
		6:  "T H R _ S K",
	}
	for h := 6; h <= 10; h++ {
		var m tea.Model = newOv().WithMessage("m")
		m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: h})
		if got := ovShape(m); got != want[h] || lineCount(view(m)) > h {
			t.Errorf("height %d: shape %q, want %q:\n%s", h, got, want[h], view(m))
		}
	}
	// Narrower than the line, it wraps rather than cut a key off.
	for w := 18; w < 80; w++ {
		var m tea.Model = newOv()
		m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		v := view(m)
		for _, k := range []string{"space toggle", "enter cd", "u update", "/ filter", "? more", "q quit"} {
			if !strings.Contains(v, k) {
				t.Errorf("width %d: compact help lacks %q:\n%s", w, k, v)
			}
		}
		for _, l := range strings.Split(v, "\n") {
			if lipgloss.Width(l) > w || strings.Contains(l, "…") {
				t.Errorf("width %d: line %q", w, l)
			}
		}
	}
	var rows []Row
	for i := range 30 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m, _ = press(m, "end", "?")
	if v := view(m); lineCount(v) > 10 || !strings.Contains(v, "> [ ] r29") || !strings.Contains(v, "? less") {
		t.Errorf("full help on a short terminal:\n%s", v)
	}
}

// WithMouse turns mouse reporting on (cell motion: clicks and wheel, not
// bare motion) while the overview is displayed, and off in its final view;
// the wheel moves the cursor by wheelStep rows within bounds, scrolling
// the rows, in filter mode too; clicks are ignored (B49).
func TestOverviewMouseWheelMovesTheCursor(t *testing.T) {
	t.Parallel()
	if newOv().View().MouseMode != tea.MouseModeNone {
		t.Error("mouse mode on without WithMouse")
	}
	var rows []Row
	for i := range 30 {
		rows = append(rows, Row{Key: fmt.Sprint(i), Cells: []string{fmt.Sprintf("r%02d", i), "main"}})
	}
	var m tea.Model = NewOverview("t", []string{"REPO", "DIR"}, rows, ovActions()).WithMouse()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	if mode := m.View().MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode %v, want cell motion", mode)
	}
	down := tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 3, Y: 4}
	up := tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 3, Y: 4}
	for _, step := range []struct {
		msg  tea.Msg
		want string
	}{
		{down, "3"},
		{down, "6"},
		{up, "3"},
		{up, "0"},
		{up, "0"},
		{tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: 6}, "0"},
		{tea.MouseWheelMsg{Button: tea.MouseWheelRight}, "0"},
	} {
		var cmd tea.Cmd
		m, cmd = m.Update(step.msg)
		if got := asOv(t, m).CurrentKey(); cmd != nil || got != step.want {
			t.Fatalf("after %v: current row %q (command %v), want %q", step.msg, got, cmd != nil, step.want)
		}
	}
	for range 12 {
		m, _ = m.Update(down)
	}
	if v := view(m); asOv(t, m).CurrentKey() != "29" || !strings.Contains(v, "> [ ] r29") {
		t.Fatalf("after wheeling to the end, current row %q:\n%s", asOv(t, m).CurrentKey(), v)
	}
	m, _ = press(m, "/", "r", "1")
	m, _ = m.Update(down)
	if got := asOv(t, m).CurrentKey(); got != "13" { // r10..r19 shown, from the first
		t.Fatalf("filter mode: current row %q, want 13", got)
	}
	m, _ = press(m, "enter", "q")
	if v := m.View(); !asOv(t, m).Cancelled() || v.MouseMode != tea.MouseModeNone {
		t.Errorf("final view: mouse mode %v", v.MouseMode)
	}
}

// An action with Do runs in the overview (B44): its message is displayed,
// its command returned, and the overview stays open, with no action chosen;
// a refusal of Check comes first, and Do receives the targets of the scope.
func TestOverviewActionWithDoRunsInPlace(t *testing.T) {
	t.Parallel()
	type copied struct{ path string }
	var got []string
	actions := append(ovActions(), OverviewAction{Key: "y", Name: "copy", Help: "copy path", Scope: ScopeCurrent,
		Check: func(targets []string) string {
			if targets[0] == "/b/bravo/main" {
				return "not this one"
			}
			return ""
		},
		Do: func(targets []string) (string, tea.Cmd) {
			got = targets
			return "copied " + targets[0], func() tea.Msg { return copied{targets[0]} }
		}})
	var m tea.Model = NewOverview("t", []string{"REPO"}, ovRows(), actions)
	m, cmd := press(m, "down", "y")
	if cmd == nil || isQuit(cmd) {
		t.Fatalf("command %v: want Do's command", cmd)
	}
	if msg, ok := cmd().(copied); !ok || msg.path != "/b/a/feat-x" {
		t.Fatalf("command message %#v", msg)
	}
	assertChosen(t, m, "")
	if asOv(t, m).Cancelled() || !slices.Equal(got, []string{"/b/a/feat-x"}) || !strings.Contains(view(m), "copied /b/a/feat-x") {
		t.Fatalf("targets %q, view:\n%s", got, view(m))
	}
	// Still open: keys keep working.
	m, _ = press(m, "space")
	if !slices.Equal(asOv(t, m).SelectedKeys(), []string{"/b/a/feat-x"}) {
		t.Fatalf("selection %q after Do", asOv(t, m).SelectedKeys())
	}
	got = nil
	m, cmd = press(m, "down", "y")
	if cmd != nil || got != nil || !strings.Contains(view(m), "not this one") {
		t.Fatalf("refused Do ran (targets %q, command %v):\n%s", got, cmd != nil, view(m))
	}
}
