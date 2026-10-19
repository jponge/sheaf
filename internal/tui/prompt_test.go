package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestPromptTabCyclesModes(t *testing.T) {
	t.Parallel()
	m, _ := press(newPrompt(), "v", "1", "tab")
	if p := asPrompt(t, m); p.Mode() != "tag" || !hasLine(view(m), "tag: v1█") {
		t.Fatalf("mode %q:\n%s", p.Mode(), view(m))
	}
	if !strings.Contains(view(m), "tab") {
		t.Fatalf("help lacks tab:\n%s", view(m))
	}
	m, _ = press(m, "tab")
	if p := asPrompt(t, m); p.Mode() != "branch" {
		t.Fatalf("mode %q after a second tab", p.Mode())
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

// Each mode has a hint line below the input, which tab switches too.
func TestPromptShowsTheHintOfTheMode(t *testing.T) {
	t.Parallel()
	m := newPrompt().WithHints("an existing or new branch", "an existing tag")
	if v := view(m); !hasLine(v, "an existing or new branch") || strings.Contains(v, "an existing tag") {
		t.Fatalf("view:\n%s", v)
	}
	next, _ := press(m, "tab")
	if v := view(next); !hasLine(v, "an existing tag") || strings.Contains(v, "an existing or new branch") {
		t.Fatalf("view after tab:\n%s", v)
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
	m := newPrompt().WithSuggestions(func() [][]string { return byMode })
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returns no command to load the suggestions")
	}
	next, _ := m.Update(cmd())
	return next
}

// faintText matches text rendered faint (SGR 2).
func faintText(text string) *regexp.Regexp {
	return regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?2(?:;[0-9;]*)?m" + regexp.QuoteMeta(text))
}

// The prompt suggests the first suggestion of the mode (in their order)
// that starts with the value, case-sensitively, displayed faint after the
// cursor; → or ctrl+e accepts it (B47), tab still switches the mode, enter
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
	// Tab switches the mode, whose suggestions apply.
	tag, _ := press(m, "tab")
	if p := asPrompt(t, tag); p.Mode() != "tag" || !hasLine(view(tag), "tag: fe█") {
		t.Fatalf("tab with a suggestion: mode %q:\n%s", p.Mode(), view(tag))
	}
	tag, _ = press(tag, "ctrl+u", "v", "right")
	if p := asPrompt(t, tag); p.Value() != "v1.0" {
		t.Fatalf("tag suggestion: value %q:\n%s", p.Value(), view(tag))
	}
	// A value equal to a suggestion completes to a longer one.
	if m, _ := press(suggestingPrompt(t, nil, []string{"v1", "v1.0"}), "tab", "v", "1"); !hasLine(view(m), "tag: v1█.0") {
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
	m := newPrompt().WithSuggestions(func() [][]string { return [][]string{{"feat"}} })
	next, _ := press(m, "f", "right")
	if v := view(next); asPrompt(t, next).Value() != "f" || !hasLine(v, "branch: f█") {
		t.Fatalf("before loading: value %q:\n%s", asPrompt(t, next).Value(), v)
	}
	// Fewer modes than the prompt's: no suggestion in the others.
	tag, _ := press(suggestingPrompt(t, []string{"feat"}), "tab", "f")
	if !hasLine(view(tag), "tag: f█") {
		t.Fatalf("mode without suggestions:\n%s", view(tag))
	}
}

// Suggestions are displayed with control characters replaced, and
// accepted without them.
func TestPromptSanitizesSuggestions(t *testing.T) {
	t.Parallel()
	m, _ := press(suggestingPrompt(t, []string{"f" + injectedSGR + "x\n"}), "f")
	assertNoInjectedSGR(t, m)
	assertNoControlChars(t, view(m))
	if next, _ := press(m, "right"); asPrompt(t, next).Value() != "f[38;5;201mx" {
		t.Fatalf("accepted: %q", asPrompt(t, next).Value())
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
