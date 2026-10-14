package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// A prune candidate line as cli.pruneCandidateLines produces it.
const longPruneLine = "would remove worktree alpha/feat-cli-rework-ergonomics [feat/cli/rework-ergonomics] at /home/someone/bundles/work/alpha/feat-cli-rework-ergonomics: origin/feat/cli/rework-ergonomics is gone"

// words returns the whitespace-separated words of s, with the wrapped
// pieces of a word split by hard wrapping glued back by the caller.
func squash(s string) string { return strings.Join(strings.Fields(s), "") }

func checkWrapped(t *testing.T, v string, width int, detail string) {
	t.Helper()
	for _, l := range strings.Split(v, "\n") {
		if w := lipgloss.Width(l); w > width {
			t.Fatalf("line %q is %d cells wide, window is %d:\n%s", l, w, width, v)
		}
	}
	if !strings.Contains(squash(v), squash(detail)) {
		t.Fatalf("detail not displayed in full (wrapped) at width %d:\n%s", width, v)
	}
}

func TestConfirmWrapsDetailsToWidth(t *testing.T) {
	for _, width := range []int{30, 80} {
		m, _ := NewConfirm("Prune 1 candidate?", []string{longPruneLine, "  note: 3 ignored files (deleted with the worktree)"}).
			Update(tea.WindowSizeMsg{Width: width, Height: 40})
		checkWrapped(t, view(m), width, longPruneLine)
	}
}

func TestConfirmElisionCountsWrappedLines(t *testing.T) {
	m, _ := NewConfirm("Prune?", []string{longPruneLine}).Update(tea.WindowSizeMsg{Width: 30, Height: 4})
	v := view(m)
	lines := strings.Split(v, "\n")
	if len(lines) > 4 {
		t.Fatalf("%d lines for a height of 4:\n%s", len(lines), v)
	}
	full, _ := NewConfirm("Prune?", []string{longPruneLine}).Update(tea.WindowSizeMsg{Width: 30, Height: 100})
	wrapped := strings.Count(view(full), "\n") // detail lines below the question
	shown := 4 - 2                             // question and "… and K more"
	want := fmt.Sprintf("… and %d more", wrapped-shown)
	if !strings.Contains(v, want) {
		t.Fatalf("want %q (wrapped line count %d):\n%s", want, wrapped, v)
	}
}

func TestProgressAskWrapsDetails(t *testing.T) {
	for _, width := range []int{30, 80} {
		p, _ := newTestProgress("Pruning")
		r := make(chan bool, 1)
		m, _ := feed(p, tea.WindowSizeMsg{Width: width, Height: 40},
			RowUpdate{Key: "alpha", Label: "alpha (fetch)", State: RowDone, Aside: true},
			AskMsg{Question: "Prune 1 candidate?", Details: []string{longPruneLine}, Reply: r})
		checkWrapped(t, view(m), width, longPruneLine)
	}
}

// When the dialog takes most of the height, hidden running rows are not
// reported as finished, and the view fits.
func TestProgressAskFillingHeightKeepsRowsHonest(t *testing.T) {
	p, _ := newTestProgress("Removing")
	var details []string
	for i := range 20 {
		details = append(details, fmt.Sprintf("detail %02d", i))
	}
	r := make(chan bool, 1)
	m, _ := feed(p, tea.WindowSizeMsg{Width: 80, Height: 8},
		RowUpdate{Key: "a", Label: "a", State: RowRunning},
		RowUpdate{Key: "b", Label: "b", State: RowRunning},
		RowUpdate{Key: "c", Label: "c", State: RowDone},
		AskMsg{Question: "Remove?", Details: details, Reply: r})
	v := view(m)
	if n := lineCount(v); n > 8 {
		t.Fatalf("%d lines for a height of 8:\n%s", n, v)
	}
	if !strings.Contains(v, "[y/N]") {
		t.Fatalf("question not shown:\n%s", v)
	}
	if strings.Contains(v, "more finished") && !(strings.Contains(v, "a ") && strings.Contains(v, "b ")) {
		t.Fatalf("hidden running rows reported as finished:\n%s", v)
	}
	re := regexp.MustCompile(`… (\d+) more`)
	if mm := re.FindStringSubmatch(v); mm != nil {
		hidden, _ := strconv.Atoi(mm[1])
		shownRows := len(regexp.MustCompile(`(?m)^\S+ [abc]\s*$`).FindAllString(v, -1))
		if hidden+shownRows != 3 {
			t.Fatalf("hidden %d + shown %d != 3 rows:\n%s", hidden, shownRows, v)
		}
	}
}

func TestProgressInterruptedMsgFromSignals(t *testing.T) {
	p, _ := newTestProgress("Updating")
	m, _ := feed(p, RowUpdate{Key: "a", Label: "a", State: RowRunning}, InterruptedMsg{Count: 1})
	if v := view(m); !strings.Contains(v, "Ctrl-C again to force") {
		t.Fatalf("signal interruption not shown:\n%s", v)
	}
	// A key press counted by the controller too is not counted twice.
	m, _ = m.Update(keyMsg("ctrl+c"))
	m, _ = m.Update(InterruptedMsg{Count: 2})
	if v := view(m); !strings.Contains(v, "forcing") {
		t.Fatalf("second interruption not shown:\n%s", v)
	}
	if n := asProgress(t, m).Interrupts(); n != 2 {
		t.Fatalf("Interrupts() = %d, want 2", n)
	}
}

func TestCtrlCMarksMenusAndDialogsInterrupted(t *testing.T) {
	ms, _ := press(NewMultiSelect("Pick", abc()), "ctrl+c")
	if !ms.(MultiSelect).Interrupted() || !ms.(MultiSelect).Cancelled() {
		t.Fatal("ctrl+c: MultiSelect not interrupted and cancelled")
	}
	ms, _ = press(NewMultiSelect("Pick", abc()), "esc")
	if ms.(MultiSelect).Interrupted() {
		t.Fatal("esc: MultiSelect interrupted")
	}
	c, _ := press(NewConfirm("Q?", nil), "ctrl+c")
	if !c.(Confirm).Interrupted() || c.(Confirm).Confirmed() {
		t.Fatal("ctrl+c: Confirm not interrupted or confirmed")
	}
	c, _ = press(NewConfirm("Q?", nil), "n")
	if c.(Confirm).Interrupted() {
		t.Fatal("n: Confirm interrupted")
	}
}
