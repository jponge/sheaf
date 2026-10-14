package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func abc() []Item {
	return []Item{
		{Key: "a", Label: "alpha", Detail: "first letter"},
		{Key: "b", Label: "bravo", Detail: "second"},
		{Key: "c", Label: "charlie", Detail: "third"},
	}
}

func manyItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Key: fmt.Sprintf("k%02d", i), Label: fmt.Sprintf("item%02d", i)}
	}
	return items
}

func asMS(t *testing.T, m tea.Model) MultiSelect {
	t.Helper()
	ms, ok := m.(MultiSelect)
	if !ok {
		t.Fatalf("model is %T, want MultiSelect", m)
	}
	return ms
}

func assertKeys(t *testing.T, m tea.Model, want ...string) {
	t.Helper()
	got := asMS(t, m).SelectedKeys()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectedKeys() = %q, want %q", got, want)
	}
}

// currentLine returns the View line carrying the cursor marker.
func currentLine(t *testing.T, m tea.Model) string {
	t.Helper()
	for _, l := range strings.Split(view(m), "\n") {
		if strings.HasPrefix(l, "> ") {
			return l
		}
	}
	t.Fatalf("no cursor line in view:\n%s", view(m))
	return ""
}

func TestMultiSelectToggleAndConfirm(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "space", "down", "down", "space")
	m, cmd := press(m, "enter")
	if !asMS(t, m).Confirmed() || asMS(t, m).Cancelled() {
		t.Fatalf("Confirmed=%v Cancelled=%v, want true/false", asMS(t, m).Confirmed(), asMS(t, m).Cancelled())
	}
	if !isQuit(cmd) {
		t.Fatal("enter did not return tea.Quit")
	}
	assertKeys(t, m, "a", "c")
}

func TestMultiSelectSpaceUntoggles(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "space", "space")
	assertKeys(t, m)
}

func TestMultiSelectKeysInOriginalOrder(t *testing.T) {
	items := abc()
	items[1].Selected = true
	m, _ := press(NewMultiSelect("Pick", items), "end", "space", "home", "space")
	assertKeys(t, m, "a", "b", "c")
}

func TestMultiSelectCursorMovement(t *testing.T) {
	m := tea.Model(NewMultiSelect("Pick", abc()))
	steps := []struct {
		key  string
		want string
	}{
		{"up", "alpha"}, {"k", "alpha"}, {"down", "bravo"}, {"j", "charlie"}, {"j", "charlie"},
		{"down", "charlie"}, {"k", "bravo"}, {"home", "alpha"}, {"end", "charlie"},
		{"pgup", "alpha"}, {"pgdown", "charlie"},
	}
	for _, s := range steps {
		m, _ = press(m, s.key)
		if l := currentLine(t, m); !strings.Contains(l, s.want) {
			t.Fatalf("after %q cursor line = %q, want %q", s.key, l, s.want)
		}
	}
}

func TestMultiSelectPageKeysMoveByViewportHeight(t *testing.T) {
	m, _ := NewMultiSelect("Pick", manyItems(20)).Update(tea.WindowSizeMsg{Width: 80, Height: 7}) // 4 list rows
	m, _ = press(m, "pgdown")
	if l := currentLine(t, m); !strings.Contains(l, "item04") {
		t.Fatalf("after pgdown cursor line = %q, want item04", l)
	}
	m, _ = press(m, "pgdown", "pgdown", "pgdown", "pgdown", "pgdown")
	if l := currentLine(t, m); !strings.Contains(l, "item19") {
		t.Fatalf("pgdown past end: cursor line = %q, want item19", l)
	}
	m, _ = press(m, "pgup")
	if l := currentLine(t, m); !strings.Contains(l, "item15") {
		t.Fatalf("after pgup cursor line = %q, want item15", l)
	}
}

func TestMultiSelectDisabledItems(t *testing.T) {
	items := abc()
	items[0].Disabled = true
	items[2].Disabled, items[2].Selected = true, true
	m, _ := press(NewMultiSelect("Pick", items), "space")
	assertKeys(t, m, "c")
	m, _ = press(m, "a")
	assertKeys(t, m, "b", "c")
	m, _ = press(m, "n")
	assertKeys(t, m, "c")
}

func TestMultiSelectAllAndNone(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "a")
	assertKeys(t, m, "a", "b", "c")
	m, _ = press(m, "n")
	assertKeys(t, m)
}

func TestMultiSelectFilterMatchesLabelAndDetailCaseInsensitively(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "R")
	v := view(m)
	// "bravo" and "charlie" match on the label, "alpha" on its detail ("first letter").
	for _, s := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(v, s) {
			t.Fatalf("filter R: view lacks %q:\n%s", s, v)
		}
	}
	m, _ = press(m, "D")
	v = view(m)
	if !strings.Contains(v, "third") || strings.Contains(v, "alpha") || strings.Contains(v, "bravo") {
		t.Fatalf("filter RD: want only charlie (detail \"third\"):\n%s", v)
	}
}

func TestMultiSelectFilterTypingIsNotACommand(t *testing.T) {
	items := append(abc(), Item{Key: "q", Label: "q a n j k"})
	m, cmd := press(NewMultiSelect("Pick", items), "/", "q", "space", "a", "space", "n", "space", "j")
	if asMS(t, m).Cancelled() || asMS(t, m).Confirmed() || cmd != nil && isQuit(cmd) {
		t.Fatal("typing in filter mode ended the selection")
	}
	assertKeys(t, m)
	if !strings.Contains(view(m), "q a n j") || strings.Contains(view(m), "alpha") {
		t.Fatalf("filter %q not applied:\n%s", "q a n j", view(m))
	}
	m, _ = press(m, "backspace", "backspace")
	if v := view(m); !strings.Contains(v, "/q a n") || strings.Contains(v, "/q a n j") {
		t.Fatalf("backspace: filter line should read /q a n:\n%s", view(m))
	}
}

func TestMultiSelectFilterEnterKeepsFilterAndLeavesFilterMode(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "a", "enter")
	if asMS(t, m).Confirmed() {
		t.Fatal("enter in filter mode confirmed the selection")
	}
	// "alpha", "bravo", "charlie" all contain "a"; narrow further with a real filter.
	m, _ = press(NewMultiSelect("Pick", abc()), "/", "br", "enter")
	if strings.Contains(view(m), "alpha") || !strings.Contains(view(m), "bravo") {
		t.Fatalf("filter not kept after enter:\n%s", view(m))
	}
	// Back in normal mode: space toggles, enter confirms.
	m, _ = press(m, "space")
	m, cmd := press(m, "enter")
	if !asMS(t, m).Confirmed() || !isQuit(cmd) {
		t.Fatal("enter after leaving filter mode did not confirm")
	}
	assertKeys(t, m, "b")
}

func TestMultiSelectFilterEscClears(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "br", "esc")
	if asMS(t, m).Cancelled() {
		t.Fatal("esc in filter mode cancelled")
	}
	v := view(m)
	if !strings.Contains(v, "alpha") || !strings.Contains(v, "charlie") || strings.Contains(v, "/br") {
		t.Fatalf("esc did not clear the filter:\n%s", v)
	}
	// Normal mode again: space toggles the item under the cursor.
	m, _ = press(m, "home", "space")
	assertKeys(t, m, "a")
}

func TestMultiSelectEscClearsAppliedFilterBeforeCancelling(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "br", "enter", "esc")
	if asMS(t, m).Cancelled() {
		t.Fatal("first esc should clear the applied filter, not cancel")
	}
	if !strings.Contains(view(m), "alpha") {
		t.Fatalf("filter not cleared:\n%s", view(m))
	}
	m, cmd := press(m, "esc")
	if !asMS(t, m).Cancelled() || !isQuit(cmd) {
		t.Fatal("second esc did not cancel")
	}
}

func TestMultiSelectSelectionSurvivesFiltering(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "down", "space", "/", "char")
	m, _ = press(m, "enter", "space", "esc")
	assertKeys(t, m, "b", "c")
}

func TestMultiSelectAllAndNoneOnlyAffectVisible(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "space", "/", "brav", "enter", "a", "esc")
	assertKeys(t, m, "a", "b")
	m, _ = press(m, "/", "brav", "enter", "n", "esc")
	assertKeys(t, m, "a")
}

func TestMultiSelectCursorStaysOnItemWhenFilterCleared(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "char", "esc")
	if l := currentLine(t, m); !strings.Contains(l, "charlie") {
		t.Fatalf("cursor line = %q, want charlie", l)
	}
}

func TestMultiSelectCancelKeys(t *testing.T) {
	for _, k := range []string{"esc", "q", "ctrl+c"} {
		m, cmd := press(NewMultiSelect("Pick", abc()), "space", k)
		ms := asMS(t, m)
		if !ms.Cancelled() || ms.Confirmed() || !isQuit(cmd) {
			t.Fatalf("%s: Cancelled=%v Confirmed=%v quit=%v", k, ms.Cancelled(), ms.Confirmed(), isQuit(cmd))
		}
	}
	m, cmd := press(NewMultiSelect("Pick", abc()), "/", "x", "ctrl+c")
	if !asMS(t, m).Cancelled() || !isQuit(cmd) {
		t.Fatal("ctrl+c in filter mode did not cancel")
	}
}

func TestMultiSelectIgnoresKeysOnceDone(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "enter", "space", "esc")
	ms := asMS(t, m)
	if !ms.Confirmed() || ms.Cancelled() {
		t.Fatalf("Confirmed=%v Cancelled=%v after keys following enter", ms.Confirmed(), ms.Cancelled())
	}
	assertKeys(t, m)
}

func TestMultiSelectViewRendersRows(t *testing.T) {
	items := abc()
	items[0].Tags = []string{"fork", "archived"}
	items[1].Selected = true
	v := view(NewMultiSelect("Pick repositories", items))
	for _, s := range []string{"Pick repositories", "> [ ] alpha", "first letter", "[fork]", "[archived]", "[x] bravo", "1/3 selected", "space", "toggle", "enter", "filter"} {
		if !strings.Contains(v, s) {
			t.Fatalf("view lacks %q:\n%s", s, v)
		}
	}
}

func TestMultiSelectFilterModeView(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "br")
	v := view(m)
	for _, s := range []string{"/br", "0/3 selected", "1 shown", "esc"} {
		if !strings.Contains(v, s) {
			t.Fatalf("filter-mode view lacks %q:\n%s", s, v)
		}
	}
}

func TestMultiSelectViewportKeepsCursorVisible(t *testing.T) {
	height := 7
	m, _ := NewMultiSelect("Pick", manyItems(20)).Update(tea.WindowSizeMsg{Width: 80, Height: height})
	check := func(when string, in, out []string) {
		t.Helper()
		v := view(m)
		if n := strings.Count(v, "\n") + 1; n > height {
			t.Fatalf("%s: view has %d lines, window height is %d:\n%s", when, n, height, v)
		}
		for _, s := range in {
			if !strings.Contains(v, s) {
				t.Fatalf("%s: view lacks %q:\n%s", when, s, v)
			}
		}
		for _, s := range out {
			if strings.Contains(v, s) {
				t.Fatalf("%s: view should not contain %q:\n%s", when, s, v)
			}
		}
	}
	check("initial", []string{"item00", "item03"}, []string{"item04"})
	m, _ = press(m, "down", "down", "down", "down", "down")
	check("cursor at 5", []string{"item02", "item05"}, []string{"item01", "item06"})
	m, _ = press(m, "end")
	check("end", []string{"item16", "item19"}, []string{"item15"})
	m, _ = press(m, "up", "up", "up", "up")
	check("cursor at 15", []string{"item15", "item18"}, []string{"item14", "item19"})
	// Growing the window shows more rows.
	height = 30
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
	check("tall window", []string{"item00", "item19"}, nil)
}

func TestMultiSelectViewportShrinkKeepsCursorVisible(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", manyItems(20)), "end")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	if l := currentLine(t, m); !strings.Contains(l, "item19") {
		t.Fatalf("cursor line = %q, want item19", l)
	}
}

func TestMultiSelectTruncatesToWidth(t *testing.T) {
	items := []Item{{Key: "x", Label: strings.Repeat("long-label-", 10), Detail: "detail", Tags: []string{"fork"}}}
	m, _ := NewMultiSelect("Pick", items).Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	for _, l := range strings.Split(view(m), "\n") {
		if w := lipgloss.Width(l); w > 30 {
			t.Fatalf("line %q is %d cells wide, window is 30", l, w)
		}
	}
}

func TestMultiSelectEmptyList(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", nil), "space", "a", "n", "up", "down", "pgdown", "end", "home")
	v := view(m)
	if !strings.Contains(v, "No items") || !strings.Contains(v, "0/0 selected") {
		t.Fatalf("empty view:\n%s", v)
	}
	m, cmd := press(m, "enter")
	if !asMS(t, m).Confirmed() || !isQuit(cmd) {
		t.Fatal("enter on an empty list did not confirm")
	}
	assertKeys(t, m)
}

func TestMultiSelectFilterWithNoMatch(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "zzz")
	if !strings.Contains(view(m), "No matches") {
		t.Fatalf("no-match view:\n%s", view(m))
	}
	m, _ = press(m, "enter", "space", "a", "down", "end")
	assertKeys(t, m)
	m, _ = press(m, "esc", "space")
	assertKeys(t, m, "a")
}

func TestMultiSelectDoesNotAliasCallerItems(t *testing.T) {
	items := abc()
	m, _ := press(NewMultiSelect("Pick", items), "space")
	if items[0].Selected {
		t.Fatal("toggling mutated the caller's slice")
	}
	assertKeys(t, m, "a")
}

func TestMultiSelectEarlierModelValuesKeepTheirState(t *testing.T) {
	m0 := NewMultiSelect("Pick", abc())
	m1, _ := press(m0, "space")
	m2, _ := press(m1, "a")
	assertKeys(t, m0)
	assertKeys(t, m1, "a")
	assertKeys(t, m2, "a", "b", "c")
}

func TestMultiSelectArrowsNavigateInFilterMode(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "r", "down", "down")
	if l := currentLine(t, m); !strings.Contains(l, "charlie") {
		t.Fatalf("cursor line = %q, want charlie", l)
	}
	m, _ = press(m, "home")
	if l := currentLine(t, m); !strings.Contains(l, "alpha") {
		t.Fatalf("cursor line = %q, want alpha", l)
	}
}

func TestMultiSelectStatusShowsScrollPosition(t *testing.T) {
	m, _ := NewMultiSelect("Pick", manyItems(20)).Update(tea.WindowSizeMsg{Width: 80, Height: 7})
	m, _ = press(m, "end")
	if !strings.Contains(view(m), "rows 17-20 of 20") {
		t.Fatalf("status lacks scroll position:\n%s", view(m))
	}
}

// Without text attributes (e.g. TERM=dumb) faint is dropped, so disabled rows
// need a textual marker.
func TestMultiSelectDisabledRowsHaveTextualMarker(t *testing.T) {
	items := abc()
	items[0].Disabled = true
	items[1].Disabled, items[1].Selected = true, true
	v := view(NewMultiSelect("Pick", items))
	for _, s := range []string{"[-] alpha", "[+] bravo", "[ ] charlie"} {
		if !strings.Contains(v, s) {
			t.Fatalf("view lacks %q:\n%s", s, v)
		}
	}
}

func TestMultiSelectViewStripsControlCharacters(t *testing.T) {
	items := []Item{
		{Key: "a", Label: "evil\x1b]0;pwned\x07name" + injectedSGR, Detail: "\x1b[31mred\u009b2J" + injectedSGR, Tags: []string{"t\x1bag" + injectedSGR}},
		{Key: "b", Label: "two\nlines", Detail: "cr\rhere\ttab"},
	}
	m := tea.Model(NewMultiSelect("Ti\x1b[2Jtle\n"+injectedSGR, items))
	for _, size := range []tea.Msg{nil, tea.WindowSizeMsg{Width: 80, Height: 20}} {
		if size != nil {
			m, _ = m.Update(size)
		}
		v := view(m)
		assertNoControlChars(t, v)
		assertNoInjectedSGR(t, m)
		if n := lineCount(v); n != 5 { // title, 2 rows, status, help
			t.Fatalf("view has %d lines, want 5:\n%s", n, v)
		}
	}
}

func TestMultiSelectFilterDropsPastedControlCharacters(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/")
	m, _ = m.Update(paste("b\nr\x1b"))
	v := view(m)
	assertNoControlChars(t, v)
	assertNoInjectedSGR(t, m)
	if !hasLine(v, "/br█") || !strings.Contains(v, "bravo") {
		t.Fatalf("filter should read br and match bravo:\n%s", v)
	}
	if n := lineCount(v); n != 5 { // title, filter, 1 row, status, help
		t.Fatalf("view has %d lines, want 5:\n%s", n, v)
	}
}

// A key event can carry several runes (bubbletea v2: a grapheme of several
// runes; v1 also merged runes typed fast or read from a non-TTY); list mode
// handles them rune by rune.
func TestMultiSelectMultiRuneKeysAreProcessedPerRune(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "jj")
	if l := currentLine(t, m); !strings.Contains(l, "charlie") {
		t.Fatalf("after \"jj\" cursor line = %q, want charlie", l)
	}
	m, _ = press(m, "na")
	assertKeys(t, m, "a", "b", "c")
	m, cmd := press(m, "kq")
	if !asMS(t, m).Cancelled() || !isQuit(cmd) {
		t.Fatal("\"kq\" did not cancel")
	}
	if l := currentLine(t, m); !strings.Contains(l, "bravo") {
		t.Fatalf("after \"kq\" cursor line = %q, want bravo", l)
	}
	m, cmd = press(NewMultiSelect("Pick", abc()), "qj")
	if !asMS(t, m).Cancelled() || !isQuit(cmd) {
		t.Fatal("\"qj\" did not cancel")
	}
	m, _ = press(NewMultiSelect("Pick", abc()), "j j")
	assertKeys(t, m, "b")
}

func TestMultiSelectPasteInListModeIsIgnored(t *testing.T) {
	m, cmd := NewMultiSelect("Pick", abc()).Update(paste("aq"))
	if ms := asMS(t, m); ms.Cancelled() || ms.Confirmed() || cmd != nil {
		t.Fatal("paste in list mode acted as keys")
	}
	assertKeys(t, m)
}

func TestMultiSelectTinyHeightsKeepCursorRowVisible(t *testing.T) {
	for h := 1; h <= 4; h++ {
		for _, filtering := range []bool{false, true} {
			m, _ := NewMultiSelect("Pick", manyItems(20)).Update(tea.WindowSizeMsg{Width: 80, Height: h})
			m, _ = press(m, "end")
			if filtering {
				m, _ = press(m, "/")
			}
			v := view(m)
			if n := lineCount(v); n > h {
				t.Fatalf("h=%d filtering=%v: view has %d lines:\n%s", h, filtering, n, v)
			}
			if l := currentLine(t, m); !strings.Contains(l, "item19") {
				t.Fatalf("h=%d filtering=%v: cursor line = %q, want item19", h, filtering, l)
			}
			// Help goes first, then the title; the status line stays longest.
			if !filtering && h <= 3 {
				hasTitle, hasStatus := strings.Contains(v, "Pick"), strings.Contains(v, "selected")
				if hasTitle != (h == 3) || hasStatus != (h >= 2) || strings.Contains(v, "toggle") {
					t.Fatalf("h=%d: title=%v status=%v, view:\n%s", h, hasTitle, hasStatus, v)
				}
			}
		}
	}
}

func TestMultiSelectFilterLineEditing(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "foo bar", "ctrl+w")
	if v := view(m); !hasLine(v, "/foo █") {
		t.Fatalf("ctrl+w: want filter line \"/foo █\":\n%s", v)
	}
	m, _ = press(m, "ctrl+w")
	if v := view(m); !hasLine(v, "/█") {
		t.Fatalf("second ctrl+w: want empty filter line:\n%s", v)
	}
	m, _ = press(m, "baz qux", "ctrl+u")
	if v := view(m); !hasLine(v, "/█") {
		t.Fatalf("ctrl+u: want empty filter line:\n%s", v)
	}
}

func TestMultiSelectCommandKeysAreTypedInFilterMode(t *testing.T) {
	m, _ := press(NewMultiSelect("Pick", abc()), "/", "k", "/", "j")
	if v := view(m); !hasLine(v, "/k/j█") {
		t.Fatalf("want filter line \"/k/j█\":\n%s", v)
	}
}
