package tui

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func asPrompt(t *testing.T, m tea.Model) Prompt {
	t.Helper()
	p, ok := m.(Prompt)
	if !ok {
		t.Fatalf("model is %T, want Prompt", m)
	}
	return p
}

func newPrompt() Prompt { return NewPrompt("New worktree of alpha", []string{"branch", "tag"}) }

func TestPromptTypesAndConfirms(t *testing.T) {
	t.Parallel()
	m, _ := press(newPrompt(), "f", "e", "a", "t", "/", "x", "y", "backspace")
	v := view(m)
	if !hasLine(v, "New worktree of alpha") || !hasLine(v, "branch: feat/x█") {
		t.Fatalf("view:\n%s", v)
	}
	m, cmd := press(m, "enter")
	p := asPrompt(t, m)
	if !isQuit(cmd) || !p.Confirmed() || p.Cancelled() || p.Value() != "feat/x" || p.Mode() != "branch" {
		t.Fatalf("quit=%v confirmed=%v cancelled=%v value=%q mode=%q", isQuit(cmd), p.Confirmed(), p.Cancelled(), p.Value(), p.Mode())
	}
	if m2, _ := press(m, "z"); asPrompt(t, m2).Value() != "feat/x" {
		t.Fatal("input after the answer changed the value")
	}
}

func TestPromptCtrlTCyclesModes(t *testing.T) {
	t.Parallel()
	m, _ := press(newPrompt(), "v", "1", "ctrl+t")
	if p := asPrompt(t, m); p.Mode() != "tag" || !hasLine(view(m), "tag: v1█") {
		t.Fatalf("mode %q:\n%s", p.Mode(), view(m))
	}
	if !strings.Contains(view(m), "ctrl+t") {
		t.Fatalf("help lacks ctrl+t:\n%s", view(m))
	}
	m, _ = press(m, "ctrl+t")
	if p := asPrompt(t, m); p.Mode() != "branch" {
		t.Fatalf("mode %q after a second ctrl+t", p.Mode())
	}
}

func TestPromptBlankEnterIgnored(t *testing.T) {
	t.Parallel()
	m, cmd := press(newPrompt(), "space", "enter")
	if isQuit(cmd) || asPrompt(t, m).Confirmed() {
		t.Fatal("enter with a blank value confirmed")
	}
	// It is not silent: the prompt asks for a value, until one is typed.
	if !hasLine(view(m), "type a branch first (esc cancels)") {
		t.Fatalf("no feedback on a blank enter:\n%s", view(m))
	}
	if m, _ = press(m, "x"); strings.Contains(view(m), "first") {
		t.Fatalf("feedback kept after typing:\n%s", view(m))
	}
}

// Each mode has a hint line below the input, which ctrl+t switches too.
func TestPromptShowsTheHintOfTheMode(t *testing.T) {
	t.Parallel()
	m := newPrompt().WithHints("an existing or new branch", "an existing tag")
	if v := view(m); !hasLine(v, "an existing or new branch") || strings.Contains(v, "an existing tag") {
		t.Fatalf("view:\n%s", v)
	}
	next, _ := press(m, "ctrl+t")
	if v := view(next); !hasLine(v, "an existing tag") || strings.Contains(v, "an existing or new branch") {
		t.Fatalf("view after ctrl+t:\n%s", v)
	}
}

func TestPromptCancelAndInterrupt(t *testing.T) {
	t.Parallel()
	m, cmd := press(newPrompt(), "x", "esc")
	if p := asPrompt(t, m); !isQuit(cmd) || !p.Cancelled() || p.Interrupted() || p.Confirmed() {
		t.Fatalf("esc: quit=%v cancelled=%v interrupted=%v", isQuit(cmd), p.Cancelled(), p.Interrupted())
	}
	m, cmd = press(newPrompt(), "ctrl+c")
	if p := asPrompt(t, m); !isQuit(cmd) || !p.Cancelled() || !p.Interrupted() {
		t.Fatalf("ctrl+c: quit=%v cancelled=%v interrupted=%v", isQuit(cmd), p.Cancelled(), p.Interrupted())
	}
}

func TestPromptEditingAndPaste(t *testing.T) {
	t.Parallel()
	m, _ := press(newPrompt(), "a", "space", "b", "ctrl+w")
	if got := asPrompt(t, m).Value(); got != "a " {
		t.Fatalf("ctrl+w: %q", got)
	}
	m, _ = press(m, "ctrl+u")
	m, _ = m.Update(paste("x\ny\x1b"))
	if got := asPrompt(t, m).Value(); got != "xy" {
		t.Fatalf("paste: %q", got)
	}
	if m, _ := m.Update(keyMsg("q")); asPrompt(t, m).Cancelled() || asPrompt(t, m).Value() != "xyq" {
		t.Fatal("q must be typed, not cancel")
	}
}

func TestPromptSanitizesTitle(t *testing.T) {
	t.Parallel()
	m := NewPrompt("t"+injectedSGR+"\n", []string{"branch"})
	assertNoInjectedSGR(t, m)
	assertNoControlChars(t, view(m))
	if !strings.Contains(view(m), "branch: ") {
		t.Fatalf("view:\n%s", view(m))
	}
}

// suggestingPrompt returns newPrompt with suggestions per mode, once its
// Init command (which loads them) has run.
func suggestingPrompt(t *testing.T, byMode ...[]string) tea.Model {
	t.Helper()
	m := newPrompt()
	for i, values := range byMode {
		m = m.WithSuggestions(m.modes[i], func() ([]Suggestion, error) { return values2suggestions(values), nil })
	}
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returns no command to load the suggestions")
	}
	return loadAll(m, cmd)
}

// faintText matches text rendered faint (SGR 2).
func faintText(text string) *regexp.Regexp {
	return regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?2(?:;[0-9;]*)?m" + regexp.QuoteMeta(text))
}

// The prompt suggests the first suggestion of the mode (in their order)
// that starts with the value, case-sensitively, displayed faint after the
// cursor; → or ctrl+e accepts it (B47), ctrl+t switches the mode, enter
// confirms the value as typed.
func TestPromptSuggestsCompletions(t *testing.T) {
	t.Parallel()
	m := suggestingPrompt(t, []string{"feat/x", "feature", "main"}, []string{"v1.0"})
	if v := view(m); !hasLine(v, "branch: █") || strings.Contains(v, "→") {
		t.Fatalf("no value, no suggestion:\n%s", v)
	}
	m, _ = press(m, "f", "e")
	if v := view(m); !hasLine(v, "branch: fe█at/x") || !strings.Contains(v, "→ complete") {
		t.Fatalf("suggestion for fe:\n%s", v)
	}
	if raw := m.View().Content; !faintText("at/x").MatchString(raw) {
		t.Fatalf("suggestion not faint:\n%q", raw)
	}
	accepted, _ := press(m, "right")
	if p := asPrompt(t, accepted); p.Value() != "feat/x" || p.Confirmed() || !hasLine(view(accepted), "branch: feat/x█") {
		t.Fatalf("→: value %q confirmed %v:\n%s", p.Value(), p.Confirmed(), view(accepted))
	}
	if next, _ := press(m, "ctrl+e"); asPrompt(t, next).Value() != "feat/x" {
		t.Fatalf("ctrl+e: value %q", asPrompt(t, next).Value())
	}
	if next, _ := press(m, "u"); !hasLine(view(next), "branch: feu█") || strings.Contains(view(next), "→") {
		t.Fatalf("no suggestion for feu:\n%s", view(next))
	}
	if next, _ := press(m, "backspace", "backspace", "F", "e"); !hasLine(view(next), "branch: Fe█") {
		t.Fatalf("case-insensitive suggestion:\n%s", view(next))
	}
	// A value equal to a suggestion has nothing to complete; → then does
	// nothing.
	if next, _ := press(accepted, "right"); asPrompt(t, next).Value() != "feat/x" || !hasLine(view(next), "branch: feat/x█") || strings.Contains(view(next), "→") {
		t.Fatalf("complete value:\n%s", view(next))
	}
	// Enter confirms the value typed, not the suggestion.
	if next, cmd := press(m, "enter"); !isQuit(cmd) || asPrompt(t, next).Value() != "fe" {
		t.Fatalf("enter: quit %v value %q", isQuit(cmd), asPrompt(t, next).Value())
	}
	// ctrl+t switches the mode, whose suggestions apply.
	tag, _ := press(m, "ctrl+t")
	if p := asPrompt(t, tag); p.Mode() != "tag" || !hasLine(view(tag), "tag: fe█") {
		t.Fatalf("ctrl+t with a suggestion: mode %q:\n%s", p.Mode(), view(tag))
	}
	tag, _ = press(tag, "ctrl+u", "v", "right")
	if p := asPrompt(t, tag); p.Value() != "v1.0" {
		t.Fatalf("tag suggestion: value %q:\n%s", p.Value(), view(tag))
	}
	// A value equal to a suggestion completes to a longer one.
	if m, _ := press(suggestingPrompt(t, nil, []string{"v1", "v1.0"}), "ctrl+t", "v", "1"); !hasLine(view(m), "tag: v1█.0") {
		t.Fatalf("longer suggestion:\n%s", view(m))
	}
}

// Until the suggestions are loaded (or without any), the prompt works
// without them; a prompt without suggestions has nothing to load.
func TestPromptWorksBeforeSuggestionsLoad(t *testing.T) {
	t.Parallel()
	if newPrompt().Init() != nil {
		t.Fatal("Init returns a command without suggestions")
	}
	m := newPrompt().WithSuggestions("branch", func() ([]Suggestion, error) { return []Suggestion{{Value: "feat"}}, nil })
	next, _ := press(m, "f", "right")
	if v := view(next); asPrompt(t, next).Value() != "f" || !hasLine(v, "branch: f█") {
		t.Fatalf("before loading: value %q:\n%s", asPrompt(t, next).Value(), v)
	}
	// Fewer modes than the prompt's: no suggestion in the others.
	tag, _ := press(suggestingPrompt(t, []string{"feat"}), "ctrl+t", "f")
	if !hasLine(view(tag), "tag: f█") {
		t.Fatalf("mode without suggestions:\n%s", view(tag))
	}
}

// A suggestion with control characters is not offered: nothing of it is
// displayed, and → does not accept it.
func TestPromptSanitizesSuggestions(t *testing.T) {
	t.Parallel()
	m, _ := press(suggestingPrompt(t, []string{"f" + injectedSGR + "x\n"}), "f")
	assertNoInjectedSGR(t, m)
	assertNoControlChars(t, view(m))
	if next, _ := press(m, "right"); asPrompt(t, next).Value() != "f" {
		t.Fatalf("accepted: %q", asPrompt(t, next).Value())
	}
}

// A suggestion with control or format characters is never offered, not
// even as the inline completion: → would insert something else than what
// is displayed (or display it reordered, for bidi controls).
func TestPromptInlineCompletionSkipsUnlistable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		suggestions []string
		typed       string
	}{
		{[]string{"fix", "fix\u202eabc"}, "fix"},
		{[]string{"fix\u0085x"}, "fix"},
		{[]string{"8\x1b[31m"}, "8"},
	} {
		m, _ := press(branchPrompt(t, c.suggestions...), c.typed)
		v := view(m)
		assertNoControlChars(t, v)
		if !hasLine(v, "branch: "+c.typed+"█") || strings.ContainsRune(v, '\u202e') || strings.Contains(v, "→") {
			t.Errorf("%q: inline completion offered:\n%q", c.suggestions, v)
		}
		if next, _ := press(m, "right"); asPrompt(t, next).Value() != c.typed {
			t.Errorf("%q: → gave %q", c.suggestions, asPrompt(t, next).Value())
		}
	}
}

// A check refuses a value on enter: its message is displayed, the prompt
// stays open, and the message goes away once the value is edited.
func TestPromptCheckRefusesValues(t *testing.T) {
	t.Parallel()
	digits := func(v string) string {
		if strings.Trim(v, "0123456789") != "" {
			return "not a number: " + v
		}
		return ""
	}
	m, cmd := press(NewPrompt("Pull request of alpha", []string{"number"}).WithCheck(digits), "x", "1", "enter")
	if p := asPrompt(t, m); isQuit(cmd) || p.Confirmed() || p.Cancelled() {
		t.Fatalf("refused value: quit=%v confirmed=%v cancelled=%v", isQuit(cmd), p.Confirmed(), p.Cancelled())
	}
	if !hasLine(view(m), "not a number: x1") {
		t.Fatalf("no message for a refused value:\n%s", view(m))
	}
	m, _ = press(m, "ctrl+u", "4", "2")
	if strings.Contains(view(m), "not a number") {
		t.Fatalf("message kept after editing:\n%s", view(m))
	}
	m, cmd = press(m, "enter")
	if p := asPrompt(t, m); !isQuit(cmd) || !p.Confirmed() || p.Value() != "42" {
		t.Fatalf("accepted value: quit=%v confirmed=%v value=%q", isQuit(cmd), p.Confirmed(), p.Value())
	}
	// A blank value still asks for one; the check does not see it.
	m, _ = press(NewPrompt("t", []string{"number"}).WithCheck(func(string) string { return "checked" }), "enter")
	if v := view(m); !hasLine(v, "type a number first (esc cancels)") || strings.Contains(v, "checked") {
		t.Fatalf("blank value:\n%s", v)
	}
}

// values2suggestions returns suggestions of values, without details.
func values2suggestions(values []string) []Suggestion {
	var list []Suggestion
	for _, v := range values {
		list = append(list, Suggestion{Value: v})
	}
	return list
}

// loadAll runs cmd, the command of m's Init, and every command of the
// batches it returns, and feeds the messages to m.
func loadAll(m tea.Model, cmd tea.Cmd) tea.Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = loadAll(m, c)
		}
		return m
	}
	m, _ = m.Update(msg)
	return m
}

// loaded returns m once the loads of its suggestions have run.
func loaded(t *testing.T, m Prompt) tea.Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returns no command to load the suggestions")
	}
	return loadAll(m, cmd)
}

// rows returns the candidate rows of v: the lines starting with "> " (the
// selected one) or "  ".
func rows(v string) []string {
	var r []string
	for _, l := range strings.Split(v, "\n") {
		if strings.HasPrefix(l, "> ") || strings.HasPrefix(l, "  ") {
			r = append(r, l)
		}
	}
	return r
}

func branchPrompt(t *testing.T, values ...string) tea.Model {
	t.Helper()
	return loaded(t, newPrompt().WithSuggestions("branch", func() ([]Suggestion, error) { return values2suggestions(values), nil }))
}

// The candidates of the mode are listed below the input: those whose value
// or detail contains the value, case-insensitively, at most 5.
func TestPromptListsCandidates(t *testing.T) {
	t.Parallel()
	m := branchPrompt(t, "fix/async-idioms", "fix/async-idioms-v2", "fix/ascii-titles", "main")
	want := []string{"> fix/async-idioms", "  fix/async-idioms-v2", "  fix/ascii-titles"}
	for _, typed := range [][]string{{"a", "s"}, {"A", "S"}} {
		if next, _ := press(m, typed...); !slices.Equal(rows(view(next)), want) {
			t.Errorf("rows for %q: %q, want %q:\n%s", strings.Join(typed, ""), rows(view(next)), want, view(next))
		}
	}
	// No value: every candidate (at most 5).
	if got := rows(view(m)); len(got) != 4 || got[3] != "  main" {
		t.Errorf("rows without a value: %q", got)
	}
	m = branchPrompt(t, "m1", "m2", "m3", "m4", "m5", "m6", "m7")
	if next, _ := press(m, "m"); len(rows(view(next))) != 5 {
		t.Errorf("rows for 7 matches: %q", rows(view(next)))
	}
	// No match: no rows (a new branch name).
	if next, _ := press(m, "z"); len(rows(view(next))) != 0 {
		t.Errorf("rows without a match: %q", rows(view(next)))
	}
}

// Candidates whose value starts with the value come first, then the
// others, each in the order of the suggestions.
func TestPromptRanksPrefixFirst(t *testing.T) {
	t.Parallel()
	m, _ := press(branchPrompt(t, "ab-x", "x-ab", "abc"), "a", "b")
	if got, want := rows(view(m)), []string{"> ab-x", "  abc", "  x-ab"}; !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
}

// boldText matches text rendered bold (SGR 1).
func boldText(text string) *regexp.Regexp {
	return regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?1(?:;[0-9;]*)?m" + regexp.QuoteMeta(text))
}

// ↑/↓ move the selection, within the rows displayed; tab replaces the
// value with the selected row's value; enter confirms the value typed,
// never the selected row (spec anti-goal).
func TestPromptTabTakesSelected(t *testing.T) {
	t.Parallel()
	m, _ := press(branchPrompt(t, "fix/a", "fix/b", "main"), "f", "i", "x")
	down, _ := press(m, "down")
	if got := rows(view(down)); !slices.Equal(got, []string{"  fix/a", "> fix/b"}) {
		t.Fatalf("rows after ↓: %q", got)
	}
	if raw := down.View().Content; !boldText("fix/b").MatchString(raw) {
		t.Fatalf("selected row not bold:\n%q", raw)
	}
	taken, cmd := press(down, "tab")
	if p := asPrompt(t, taken); p.Value() != "fix/b" || p.Confirmed() || isQuit(cmd) || !hasLine(view(taken), "branch: fix/b█") {
		t.Fatalf("tab: value %q confirmed %v:\n%s", p.Value(), p.Confirmed(), view(taken))
	}
	// Enter confirms what is typed, whatever row is selected.
	if next, cmd := press(down, "enter"); !isQuit(cmd) || asPrompt(t, next).Value() != "fix" {
		t.Fatalf("enter with a selected row: quit %v value %q", isQuit(cmd), asPrompt(t, next).Value())
	}
	if next, cmd := press(m, "enter"); !isQuit(cmd) || asPrompt(t, next).Value() != "fix" {
		t.Fatalf("enter: quit %v value %q", isQuit(cmd), asPrompt(t, next).Value())
	}
	// ↓ stops at the last row, ↑ at the first.
	if next, _ := press(m, "down", "down", "down"); !slices.Equal(rows(view(next)), []string{"  fix/a", "> fix/b"}) {
		t.Fatalf("rows after ↓↓↓: %q", rows(view(next)))
	}
	if next, _ := press(down, "up", "up"); !slices.Equal(rows(view(next)), []string{"> fix/a", "  fix/b"}) {
		t.Fatalf("rows after ↓↑↑: %q", rows(view(next)))
	}
	// Editing the value selects the first row again.
	if next, _ := press(down, "/"); !slices.Equal(rows(view(next)), []string{"> fix/a", "  fix/b"}) {
		t.Fatalf("rows after editing: %q", rows(view(next)))
	}
	// The inline completion follows the selected row, when it is a prefix
	// match.
	if !hasLine(view(down), "branch: fix█/b") {
		t.Fatalf("inline completion of the selected row:\n%s", view(down))
	}
	if next, _ := press(down, "right"); asPrompt(t, next).Value() != "fix/b" {
		t.Fatalf("→ after ↓: value %q", asPrompt(t, next).Value())
	}
	// Without rows, tab does nothing (it no longer switches the mode).
	if next, _ := press(m, "z", "tab"); asPrompt(t, next).Value() != "fixz" || asPrompt(t, next).Mode() != "branch" {
		t.Fatalf("tab without rows: value %q mode %q", asPrompt(t, next).Value(), asPrompt(t, next).Mode())
	}
}

func issuePrompt() Prompt {
	return NewPrompt("New worktree of alpha", []string{"branch", "tag", "issue"})
}

// Details are listed after the value and matched too; with WithExactDetail,
// the detail of the value is displayed below the input, or the missing
// text once the suggestions are loaded.
func TestPromptDetailMatched(t *testing.T) {
	t.Parallel()
	load := func() ([]Suggestion, error) { return []Suggestion{{"7", "Fix crash"}, {"12", "Other thing"}}, nil }
	m, _ := press(loaded(t, issuePrompt().WithSuggestions("issue", load).WithExactDetail("issue", "not an open issue")), "ctrl+t", "ctrl+t")
	typed, _ := press(m, "c", "r", "a", "s", "h")
	if got := rows(view(typed)); !slices.Equal(got, []string{"> 7  Fix crash"}) {
		t.Fatalf("rows for crash: %q", got)
	}
	taken, _ := press(typed, "tab")
	if p := asPrompt(t, taken); p.Value() != "7" || !hasLine(view(taken), "Fix crash") {
		t.Fatalf("tab: value %q:\n%s", p.Value(), view(taken))
	}
	if next, _ := press(m, "#", "7"); !hasLine(view(next), "Fix crash") {
		t.Fatalf("#7:\n%s", view(next))
	}
	// The candidates are filtered on the same number: without white space
	// around it and a leading "#".
	for _, typed := range [][]string{{"#", "7"}, {"space", "#", "7", "space"}} {
		next, _ := press(m, typed...)
		if got := rows(view(next)); !slices.Equal(got, []string{"> 7  Fix crash"}) {
			t.Fatalf("rows for %q: %q", asPrompt(t, next).Value(), got)
		}
		if next, _ = press(next, "tab"); asPrompt(t, next).Value() != "7" {
			t.Fatalf("tab after %q: value %q", typed, asPrompt(t, next).Value())
		}
	}
	// Not in another mode: "#" is part of a branch name.
	if next, _ := press(branchPrompt(t, "x7", "#7"), "#", "7"); !slices.Equal(rows(view(next)), []string{"> #7"}) {
		t.Fatalf("#7 in branch mode: %q", rows(view(next)))
	}
	if next, _ := press(m, "8"); !hasLine(view(next), "not an open issue") || hasLine(view(next), "Fix crash") {
		t.Fatalf("8:\n%s", view(next))
	}
	// No value: neither line.
	if v := view(m); strings.Contains(v, "not an open issue") {
		t.Fatalf("no value:\n%s", v)
	}
	// Only "#" (or white space): no number yet, neither line.
	if next, _ := press(m, "#", "space"); strings.Contains(view(next), "not an open issue") {
		t.Fatalf("#:\n%s", view(next))
	}
	// Not loaded yet: no missing line.
	unloaded, _ := press(issuePrompt().WithSuggestions("issue", load).WithExactDetail("issue", "not an open issue"), "ctrl+t", "ctrl+t", "8")
	if v := view(unloaded); strings.Contains(v, "not an open issue") {
		t.Fatalf("before loading:\n%s", v)
	}
	// Other modes: no exact detail.
	if next, _ := press(m, "ctrl+t", "8"); strings.Contains(view(next), "not an open issue") {
		t.Fatalf("branch mode:\n%s", view(next))
	}
}

// WithModeJump: the rune typed on an empty value of the mode switches the
// mode, without being inserted; elsewhere, or pasted, it is inserted.
func TestPromptModeJump(t *testing.T) {
	t.Parallel()
	m := issuePrompt().WithModeJump('#', "branch", "issue")
	if next, _ := press(m, "#"); asPrompt(t, next).Mode() != "issue" || asPrompt(t, next).Value() != "" {
		t.Fatalf("#: mode %q value %q", asPrompt(t, next).Mode(), asPrompt(t, next).Value())
	}
	if next, _ := press(m, "a", "#"); asPrompt(t, next).Mode() != "branch" || asPrompt(t, next).Value() != "a#" {
		t.Fatalf("a#: mode %q value %q", asPrompt(t, next).Mode(), asPrompt(t, next).Value())
	}
	if next, _ := press(m, "ctrl+t", "#"); asPrompt(t, next).Mode() != "tag" || asPrompt(t, next).Value() != "#" {
		t.Fatalf("# in tag mode: mode %q value %q", asPrompt(t, next).Mode(), asPrompt(t, next).Value())
	}
	if next, _ := m.Update(paste("#x")); asPrompt(t, next).Mode() != "branch" || asPrompt(t, next).Value() != "#x" {
		t.Fatalf("pasted #x: mode %q value %q", asPrompt(t, next).Mode(), asPrompt(t, next).Value())
	}
}

// startLoads runs the command of m's Init as bubbletea does, each command
// of a batch in its own goroutine, and returns the channel of their
// messages.
func startLoads(t *testing.T, m tea.Model) <-chan tea.Msg {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returns no command to load the suggestions")
	}
	msgs := make(chan tea.Msg, 16)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		go func() {
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					run(c)
				}
				return
			}
			msgs <- msg
		}()
	}
	run(cmd)
	return msgs
}

func receive(t *testing.T, msgs <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-msgs:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("no suggestions loaded within 10 s")
		return nil
	}
}

// Each mode loads its suggestions on its own: branches are listed (and
// tab completes them) while the issues still load (spec anti-goal: tab in
// branch mode never waits for the network); an error is displayed in its
// mode only, as its first line; an empty list says so.
func TestPromptPerModeLoading(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	m := tea.Model(issuePrompt().
		WithSuggestions("branch", func() ([]Suggestion, error) { return []Suggestion{{Value: "main"}}, nil }).
		WithSuggestions("tag", func() ([]Suggestion, error) { return nil, nil }).
		WithSuggestions("issue", func() ([]Suggestion, error) {
			<-release
			return nil, errors.New("gh: not logged in\nmore")
		}))
	if v := view(m); !hasLine(v, "loading branches…") {
		t.Fatalf("before loading:\n%s", v)
	}
	msgs := startLoads(t, m)
	m, _ = m.Update(receive(t, msgs))
	m, _ = m.Update(receive(t, msgs))
	if v := view(m); !slices.Equal(rows(v), []string{"> main"}) || strings.Contains(v, "loading") {
		t.Fatalf("branch mode while issues load:\n%s", v)
	}
	if next, _ := press(m, "m", "a", "tab"); asPrompt(t, next).Value() != "main" {
		t.Fatalf("tab while issues load: value %q", asPrompt(t, next).Value())
	}
	if next, _ := press(m, "ctrl+t"); !hasLine(view(next), "no tags") || len(rows(view(next))) != 0 {
		t.Fatalf("tag mode without tags:\n%s", view(next))
	}
	issue, _ := press(m, "ctrl+t", "ctrl+t")
	if v := view(issue); !hasLine(v, "loading issues…") {
		t.Fatalf("issue mode while loading:\n%s", v)
	}
	once.Do(func() { close(release) })
	msg := receive(t, msgs)
	// Received in issue mode, or in another mode.
	issue, _ = issue.Update(msg)
	if v := view(issue); !hasLine(v, "gh: not logged in") || strings.Contains(v, "more") || strings.Contains(v, "loading") {
		t.Fatalf("issue mode after the error:\n%s", v)
	}
	m, _ = m.Update(msg)
	if v := view(m); strings.Contains(v, "not logged in") || !slices.Equal(rows(v), []string{"> main"}) {
		t.Fatalf("branch mode after the issue error:\n%s", v)
	}
	if next, _ := press(m, "ctrl+t", "ctrl+t"); !hasLine(view(next), "gh: not logged in") {
		t.Fatalf("issue mode after the error arrived in branch mode:\n%s", view(next))
	}
	// After the answer, a late load changes nothing.
	done, _ := press(issue, "4", "enter")
	if late, _ := done.Update(suggestionsMsg{mode: 2, list: []Suggestion{{"4", "x"}}}); view(late) != view(done) {
		t.Fatalf("late load changed the view:\n%s", view(late))
	}
}

// Details are displayed on one row: control characters and runs of white
// space as one space, format characters (bidi controls) dropped. Values
// with control or format characters are not listed: tab would insert
// something else than what the row displays.
func TestPromptSanitizesCandidates(t *testing.T) {
	t.Parallel()
	load := func() ([]Suggestion, error) {
		return []Suggestion{
			{"7", "Fix\tthing\nnow" + injectedSGR + "x\u202edrawkcab\u2066"},
			{"8\x1b[31m", "esc"},
			{"9\u202e", "bidi"},
		}, nil
	}
	m := loaded(t, issuePrompt().WithSuggestions("issue", load).WithExactDetail("issue", "missing"))
	m, _ = press(m, "ctrl+t", "ctrl+t")
	assertNoInjectedSGR(t, m)
	assertNoControlChars(t, view(m))
	if got, want := rows(view(m)), []string{"> 7  Fix thing now [38;5;201mxdrawkcab"}; !slices.Equal(got, want) {
		t.Fatalf("rows %q, want %q", got, want)
	}
	m, _ = press(m, "7")
	assertNoInjectedSGR(t, m)
	if v := view(m); !hasLine(v, "Fix thing now [38;5;201mxdrawkcab") || strings.ContainsAny(v, "\u202e\u2066") {
		t.Fatalf("exact detail:\n%q", v)
	}
}

// Rows wider than the window are cut with "…"; on a short window, fewer
// rows are listed so that the view fits, and ↓ and tab stay on the rows
// displayed.
func TestPromptRowsFitTheWindow(t *testing.T) {
	t.Parallel()
	long := longText("fix/long", 200)
	load := func() ([]Suggestion, error) {
		return []Suggestion{{Value: long}, {"b2", longText("detail", 200)}, {Value: "b3"}, {Value: "b4"}, {Value: "b5"}, {Value: "b6"}}, nil
	}
	m := loaded(t, newPrompt().WithSuggestions("branch", load))
	for w := 20; w <= 160; w++ {
		next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		r := rows(view(next))
		if len(r) != 5 {
			t.Fatalf("width %d: rows %q", w, r)
		}
		for i, l := range r[:2] {
			if lipgloss.Width(l) != w || !strings.HasSuffix(l, "…") {
				t.Fatalf("width %d: row %d not cut with …: %q", w, i, l)
			}
		}
	}
	// Title, input, the 5 rows and the key help: 8 lines.
	for h, want := range map[int]int{8: 5, 6: 3, 4: 1, 3: 0} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: h})
		if r := rows(view(next)); len(r) != want {
			t.Errorf("height %d: %d rows, want %d:\n%s", h, len(r), want, view(next))
		}
		if lineCount(view(next)) > h {
			t.Errorf("height %d: %d lines:\n%s", h, lineCount(view(next)), view(next))
		}
	}
	short, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 5})
	short, _ = press(short, "down", "down", "down", "tab")
	if got := asPrompt(t, short).Value(); got != "b2" {
		t.Fatalf("tab on 2 displayed rows after ↓↓↓: %q", got)
	}
	// ↓ did not move past the last row displayed: in a taller window, the
	// second row is still the selected one.
	short, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 5})
	short, _ = press(short, "down", "down", "down")
	if tall, _ := short.Update(tea.WindowSizeMsg{Width: 200, Height: 40}); !strings.HasPrefix(rows(view(tall))[1], "> b2") {
		t.Fatalf("selection after ↓↓↓ on 2 rows, then a taller window: %q", rows(view(tall)))
	}
	// Nothing displayed: tab takes nothing.
	tiny, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 3})
	if next, _ := press(tiny, "tab"); asPrompt(t, next).Value() != "" {
		t.Fatalf("tab without rows displayed: %q", asPrompt(t, next).Value())
	}
}

// The key help lists tab and ↑/↓ only when they do something (rows to
// take, several rows), ctrl+t only with several modes.
func TestPromptKeyHelp(t *testing.T) {
	t.Parallel()
	m := branchPrompt(t, "fix/a", "fix/b")
	if v := view(m); !strings.Contains(v, "tab take") || !strings.Contains(v, "↑/↓ select") || !strings.Contains(v, "ctrl+t branch/tag") {
		t.Fatalf("with rows:\n%s", v)
	}
	if next, _ := press(m, "fix/a"); strings.Contains(view(next), "↑/↓") {
		t.Fatalf("one row:\n%s", view(next))
	}
	if next, _ := press(m, "f", "i", "x", "/", "a"); strings.Contains(view(next), "tab") {
		t.Fatalf("value equal to the only row:\n%s", view(next))
	}
	if next, _ := press(m, "z"); strings.Contains(view(next), "tab") || strings.Contains(view(next), "↑/↓") {
		t.Fatalf("no rows:\n%s", view(next))
	}
	single := loaded(t, NewPrompt("t", []string{"branch"}).WithSuggestions("branch", func() ([]Suggestion, error) { return []Suggestion{{Value: "a"}}, nil }))
	if v := view(single); strings.Contains(v, "ctrl+t") || !strings.Contains(v, "tab take") {
		t.Fatalf("one mode:\n%s", v)
	}
}

// After tab takes a row, that row stays the selected one (a candidate
// equal to the value comes first), so that the highlighted row is the
// value taken, not another one completing it.
func TestPromptTabKeepsTheTakenRowSelected(t *testing.T) {
	t.Parallel()
	m, _ := press(branchPrompt(t, "fix/a-v2", "fix/a", "main"), "f", "i", "x", "down", "tab")
	if p := asPrompt(t, m); p.Value() != "fix/a" || !slices.Equal(rows(view(m)), []string{"> fix/a", "  fix/a-v2"}) {
		t.Fatalf("after tab: value %q rows %q", p.Value(), rows(view(m)))
	}
	// The same when the value is typed.
	if next, _ := press(branchPrompt(t, "fix/a-v2", "fix/a"), "fix/a"); !slices.Equal(rows(view(next)), []string{"> fix/a", "  fix/a-v2"}) {
		t.Fatalf("typed fix/a: rows %q", rows(view(next)))
	}
}

// The line asking for a value uses the article of the mode's noun: "an
// issue", "a branch".
func TestPromptBlankAsksWithTheArticle(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]string{"issue": "type an issue first (esc cancels)", "branch": "type a branch first (esc cancels)", "number": "type a number first (esc cancels)"} {
		if m, _ := press(NewPrompt("t", []string{mode}), "enter"); !hasLine(view(m), want) {
			t.Errorf("%s:\n%s", mode, view(m))
		}
	}
}

// A load error is displayed as its first line that is not blank; an error
// without text says what could not be loaded.
func TestPromptLoadErrorFirstNonBlankLine(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]string{"\n \t\ngh: not logged in\nmore": "gh: not logged in", "\n": "cannot load issues"} {
		m := loaded(t, NewPrompt("t", []string{"issue"}).WithSuggestions("issue", func() ([]Suggestion, error) { return nil, errors.New(msg) }))
		if v := view(m); !hasLine(v, want) || strings.Contains(v, "more") {
			t.Errorf("error %q:\n%s", msg, v)
		}
	}
}
