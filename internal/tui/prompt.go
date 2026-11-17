package tui

import (
	"maps"
	"slices"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Prompt is a bubbletea model asking for one line of text, in one of
// several modes (e.g. "branch" or "tag") cycled with ctrl+t.
//
// Typed or pasted text (without control characters) is appended;
// backspace deletes a rune, ctrl+w a word and ctrl+u everything (as the
// filter of MultiSelect). With suggestions (WithSuggestions), the
// candidates of the mode are listed below the input (see candidates), at
// most maxCandidateRows, fewer when the window is too short: ↑ and ↓ move
// the selected row (the first one again once the value or the mode
// changes), and tab replaces the value with the selected row's. The rest
// of the selected candidate, when it starts with the value
// (case-sensitively), else, when the selected candidate equals the value
// or no row is displayed, the rest of the first suggestion of the mode that
// starts with the value and is longer, is displayed faint after the
// cursor, and → or ctrl+e appends it. A suggestion with control or format
// characters is never offered (see listable). enter confirms the value typed (never
// the selected row), if it is not blank (on a blank one, a line asks for a
// value) and accepted by its check, if any (WithCheck, WithModeCheck); esc and ctrl+c
// cancel. A hint per mode (WithHints) is displayed below the input while it
// is edited. Update returns tea.Quit once answered; later messages are
// ignored. The title is displayed with control characters replaced by
// U+FFFD.
type Prompt struct {
	title         string
	modes         []string
	mode          int
	value         string
	width, height int // 0 = unknown
	selected      int // index of the selected candidate row

	hints   []string            // by mode, see WithHints
	blank   bool                // enter was pressed on a blank value
	check   func(string) string // see WithCheck
	refused string              // the message of check for the value, until edited

	loads       map[int]func() ([]Suggestion, error) // by mode, see WithSuggestions
	suggestions map[int]loadedSuggestions            // by mode, once loaded
	jumps       []modeJump                           // see WithModeJump
	exact       map[int]string                       // missing text by mode, see WithExactDetail
	empty       map[int]string                       // by mode, see WithEmptyText
	checks      map[int]func(string) string          // by mode, see WithModeCheck

	confirmed, cancelled, interrupted bool

	keys   promptKeys
	help   help.Model
	styles Styles
}

// Suggestion is a value that a Prompt suggests, with an optional detail
// displayed after it (e.g. the title of an issue whose number is Value).
type Suggestion struct{ Value, Detail string }

// loadedSuggestions is the result of the load of a mode's suggestions.
type loadedSuggestions struct {
	list []Suggestion
	err  error
}

// modeJump is a rune that switches mode from to mode to (see WithModeJump).
type modeJump struct {
	r        string
	from, to int
}

// maxCandidateRows is the number of candidate rows listed at most.
const maxCandidateRows = 5

type promptKeys struct{ Confirm, Mode, Take, Up, Down, Complete, Cancel key.Binding }

// NewPrompt returns a Prompt titled title, starting in the first of modes
// (at least one).
func NewPrompt(title string, modes []string) Prompt {
	h := help.New()
	st := DefaultStyles()
	h.Styles = st.Help
	modes = slices.Clone(modes)
	if len(modes) == 0 {
		modes = []string{"value"}
	}
	k := promptKeys{
		Confirm:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		Mode:     key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", strings.Join(modes, "/"))),
		Take:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "take")),
		Up:       key.NewBinding(key.WithKeys("up"), key.WithHelp("↑/↓", "select")),
		Down:     key.NewBinding(key.WithKeys("down")),
		Complete: key.NewBinding(key.WithKeys("right", "ctrl+e"), key.WithHelp("→", "complete")),
		Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
	return Prompt{title: title, modes: modes, keys: k, help: h, styles: st}
}

// WithHints returns a copy of m displaying hints[i] below the input in the
// i-th mode (e.g. what a branch name means).
func (m Prompt) WithHints(hints ...string) Prompt {
	m.hints = slices.Clone(hints)
	return m
}

// WithCheck returns a copy of m in which enter on a value that is not
// blank calls check: a non-empty result refuses the value and is displayed
// below the input until the value is edited (e.g. a value that is not a
// number).
func (m Prompt) WithCheck(check func(value string) string) Prompt {
	m.check = check
	return m
}

// WithSuggestions returns a copy of m that suggests values in mode (see
// Prompt), which load returns; a mode that m does not have is ignored.
// Init runs the load of each mode in its own command, so that the prompt
// opens without waiting and a slow load does not delay the others. Until
// a mode's load returns, a line says that its suggestions are loading;
// then its error, if any, is displayed as its first line, and an empty
// list as "no <mode>s" (e.g. "no branches"), or as WithEmptyText says.
func (m Prompt) WithSuggestions(mode string, load func() ([]Suggestion, error)) Prompt {
	if i := slices.Index(m.modes, mode); i >= 0 {
		m.loads = maps.Clone(m.loads)
		if m.loads == nil {
			m.loads = map[int]func() ([]Suggestion, error){}
		}
		m.loads[i] = load
	}
	return m
}

// WithModeJump returns a copy of m in which typing r on an empty value in
// mode from switches to mode to, without inserting r (e.g. '#' for an
// issue number). A pasted r is inserted.
func (m Prompt) WithModeJump(r rune, from, to string) Prompt {
	f, t := slices.Index(m.modes, from), slices.Index(m.modes, to)
	if f >= 0 && t >= 0 {
		m.jumps = append(slices.Clip(m.jumps), modeJump{r: string(r), from: f, to: t})
	}
	return m
}

// WithExactDetail returns a copy of m that displays, in mode, below the
// input, the detail of the suggestion whose value equals the value (without
// surrounding white space and a leading "#"), else, once the suggestions
// are loaded, missing.
func (m Prompt) WithExactDetail(mode, missing string) Prompt {
	if i := slices.Index(m.modes, mode); i >= 0 {
		m.exact = maps.Clone(m.exact)
		if m.exact == nil {
			m.exact = map[int]string{}
		}
		m.exact[i] = missing
	}
	return m
}

// WithModeCheck returns a copy of m in which check checks the values of
// mode, as WithCheck does, instead of the check of WithCheck (e.g. an
// issue number in a mode, branch names in the others).
func (m Prompt) WithModeCheck(mode string, check func(value string) string) Prompt {
	if i := slices.Index(m.modes, mode); i >= 0 {
		m.checks = maps.Clone(m.checks)
		if m.checks == nil {
			m.checks = map[int]func(string) string{}
		}
		m.checks[i] = check
	}
	return m
}

// WithEmptyText returns a copy of m that displays text in mode once its
// suggestions are loaded and empty, instead of "no <mode>s" (e.g. "no open
// issues").
func (m Prompt) WithEmptyText(mode, text string) Prompt {
	if i := slices.Index(m.modes, mode); i >= 0 {
		m.empty = maps.Clone(m.empty)
		if m.empty == nil {
			m.empty = map[int]string{}
		}
		m.empty[i] = text
	}
	return m
}

// suggestionsMsg carries the suggestions of a mode loaded by
// WithSuggestions' load.
type suggestionsMsg struct {
	mode int
	list []Suggestion
	err  error
}

// Init implements tea.Model: it loads the suggestions of each mode, if
// any (see WithSuggestions), in a command of its own.
func (m Prompt) Init() tea.Cmd {
	var cmds []tea.Cmd
	for i := range m.modes {
		if load := m.loads[i]; load != nil {
			cmds = append(cmds, func() tea.Msg {
				list, err := load()
				return suggestionsMsg{mode: i, list: list, err: err}
			})
		}
	}
	return tea.Batch(cmds...)
}

// listable reports whether v can be offered (listed as a candidate, or as
// the inline completion): tab and → insert it as it is displayed, so a
// value with control or format characters (which would be displayed
// altered, reordered or not at all) is never offered.
func listable(v string) bool {
	return v != "" && !strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}

// typed returns the value that the suggestions are matched against: in a
// mode with WithExactDetail, without surrounding white space and a leading
// "#" (e.g. "#7" for the issue 7).
func (m Prompt) typed() string {
	if _, ok := m.exact[m.mode]; ok {
		return strings.TrimPrefix(strings.TrimSpace(m.value), "#")
	}
	return m.value
}

// candidates returns the suggestions of the mode whose value or detail
// contains the typed value (see typed), case-insensitively: the one equal
// to it first (so that the row taken with tab stays the selected one),
// then those whose value starts with it, then the others, each in the
// order of the suggestions. Values that are not listable are left out.
func (m Prompt) candidates() []Suggestion {
	typed := m.typed()
	v := strings.ToLower(typed)
	var exact, prefix, others []Suggestion
	for _, s := range m.suggestions[m.mode].list {
		if !listable(s.Value) {
			continue
		}
		value := strings.ToLower(s.Value)
		switch {
		case s.Value == typed:
			exact = append(exact, s)
		case strings.HasPrefix(value, v):
			prefix = append(prefix, s)
		case strings.Contains(value, v) || strings.Contains(strings.ToLower(oneLine(s.Detail)), v):
			others = append(others, s)
		}
	}
	return slices.Concat(exact, prefix, others)
}

// rows returns the candidates displayed, given room lines for them (< 0:
// unlimited), and the index of the selected one among them.
func (m Prompt) rows(room int) ([]Suggestion, int) {
	c := m.candidates()
	n := min(len(c), maxCandidateRows)
	if room >= 0 {
		n = min(n, room)
	}
	return c[:n], max(0, min(m.selected, n-1))
}

// shownRows returns the candidates View displays and the index of the
// selected one.
func (m Prompt) shownRows() ([]Suggestion, int) {
	head, tail := m.lines()
	return m.rows(m.room(head, tail))
}

// room returns the number of lines left for the candidate rows by the
// lines head and tail in the window's height (-1 when it is unknown).
func (m Prompt) room(head, tail []string) int {
	if m.height <= 0 {
		return -1
	}
	return max(0, m.height-len(head)-len(tail))
}

// suggestion returns what the inline completion adds to the value (see
// Prompt): "" for an empty value or no such suggestion.
func (m Prompt) suggestion() string {
	if m.value == "" {
		return ""
	}
	if rows, sel := m.shownRows(); len(rows) > 0 {
		s := rows[sel].Value
		if rest, ok := strings.CutPrefix(s, m.value); ok && rest != "" {
			return rest
		}
		if s != m.value {
			return ""
		}
	}
	for _, s := range m.suggestions[m.mode].list {
		if rest, ok := strings.CutPrefix(s.Value, m.value); ok && rest != "" && listable(s.Value) {
			return rest
		}
	}
	return ""
}

// Value returns the text entered.
func (m Prompt) Value() string { return m.value }

// Mode returns the current mode.
func (m Prompt) Mode() string { return m.modes[m.mode] }

// Confirmed reports whether the text was confirmed with enter.
func (m Prompt) Confirmed() bool { return m.confirmed }

// Cancelled reports whether the prompt was cancelled (esc, ctrl+c). A model
// whose program ended for another reason (e.g. input closed) is neither
// confirmed nor cancelled.
func (m Prompt) Cancelled() bool { return m.cancelled }

// Interrupted reports whether the prompt was cancelled with ctrl+c, which
// callers treat as an interruption rather than a plain cancellation.
func (m Prompt) Interrupted() bool { return m.interrupted }

// jump returns the mode that typing text switches to (see WithModeJump),
// or -1.
func (m Prompt) jump(text string) int {
	if m.value != "" {
		return -1
	}
	for _, j := range m.jumps {
		if j.from == m.mode && j.r == text {
			return j.to
		}
	}
	return -1
}

// Update implements tea.Model.
func (m Prompt) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.confirmed || m.cancelled {
		return m, nil
	}
	value, mode := m.value, m.mode
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
	case suggestionsMsg:
		m.suggestions = maps.Clone(m.suggestions)
		if m.suggestions == nil {
			m.suggestions = map[int]loadedSuggestions{}
		}
		m.suggestions[msg.mode] = loadedSuggestions{list: msg.list, err: msg.err}
		if msg.mode == m.mode {
			m.selected = 0
		}
	case tea.PasteMsg:
		m.value += dropControl(msg.Content)
		m.blank, m.refused = false, ""
	case tea.KeyPressMsg:
		m.blank, m.refused = false, ""
		switch s := msg.String(); {
		case s == "ctrl+c":
			m.cancelled, m.interrupted = true, true
			return m, tea.Quit
		case key.Matches(msg, m.keys.Cancel):
			m.cancelled = true
			return m, tea.Quit
		case key.Matches(msg, m.keys.Confirm):
			if strings.TrimSpace(m.value) == "" {
				m.blank = true
				break
			}
			check := m.check
			if c, ok := m.checks[m.mode]; ok {
				check = c
			}
			if check != nil {
				m.refused = check(m.value)
			}
			if m.refused == "" {
				m.confirmed = true
				return m, tea.Quit
			}
		case key.Matches(msg, m.keys.Mode):
			m.mode = (m.mode + 1) % len(m.modes)
		case key.Matches(msg, m.keys.Take):
			if rows, sel := m.shownRows(); len(rows) > 0 {
				m.value = rows[sel].Value
			}
		case key.Matches(msg, m.keys.Up):
			_, sel := m.shownRows()
			m.selected = max(0, sel-1)
		case key.Matches(msg, m.keys.Down):
			rows, sel := m.shownRows()
			m.selected = max(0, min(sel+1, len(rows)-1))
		case key.Matches(msg, m.keys.Complete):
			m.value += dropControl(m.suggestion())
		case s == "ctrl+u":
			m.value = ""
		case s == "ctrl+w":
			m.value = deleteLastWord(m.value)
		case s == "backspace":
			if r := []rune(m.value); len(r) > 0 {
				m.value = string(r[:len(r)-1])
			}
		case m.jump(typedText(msg)) >= 0:
			m.mode = m.jump(typedText(msg))
		case typedText(msg) != "":
			m.value += dropControl(typedText(msg))
		}
	}
	if m.value != value || m.mode != mode {
		m.selected = 0
	}
	return m, nil
}

// plural returns the plural of the English noun mode.
func plural(mode string) string {
	for _, suffix := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(mode, suffix) {
			return mode + "es"
		}
	}
	return mode + "s"
}

// article returns the indefinite article of the English noun mode: "an"
// before a vowel letter, else "a".
func article(mode string) string {
	if mode != "" && strings.ContainsRune("aeiouAEIOU", rune(mode[0])) {
		return "an"
	}
	return "a"
}

// errorLine returns the first line of err that is not blank, on one row
// (see oneLine), or blank when there is none.
func errorLine(err error, blank string) string {
	for l := range strings.SplitSeq(err.Error(), "\n") {
		if l = oneLine(l); l != "" {
			return l
		}
	}
	return blank
}

// lines returns the lines of the view above the candidate rows and below
// them (the inline completion and the key help excepted, see View).
func (m Prompt) lines() (head, tail []string) {
	st := m.styles
	input := st.FilterPrompt.Render(sanitize(m.Mode())+":") + " " + m.value
	done := m.confirmed || m.cancelled
	if !done {
		input += "█"
	}
	head = []string{st.Title.Render(sanitize(m.title)), input}
	if done {
		return head, nil
	}
	loaded, isLoaded := m.suggestions[m.mode]
	if missing, ok := m.exact[m.mode]; ok && isLoaded && loaded.err == nil && m.typed() != "" {
		line := st.Detail.Render(sanitize(missing))
		for _, s := range loaded.list {
			if s.Value == m.typed() {
				line = oneLine(s.Detail)
				break
			}
		}
		if line != "" {
			head = append(head, line)
		}
	}
	if m.mode < len(m.hints) && m.hints[m.mode] != "" {
		head = append(head, st.Detail.Render(sanitize(m.hints[m.mode])))
	}
	if m.loads[m.mode] != nil {
		mode := sanitize(plural(m.Mode()))
		switch {
		case !isLoaded:
			head = append(head, st.Detail.Render("loading "+mode+"…"))
		case loaded.err != nil:
			head = append(head, st.Warn.Render(errorLine(loaded.err, "cannot load "+mode)))
		case len(loaded.list) == 0:
			empty, ok := m.empty[m.mode]
			if !ok {
				empty = "no " + mode
			}
			head = append(head, st.Empty.Render(sanitize(empty)))
		}
	}
	if m.blank {
		tail = append(tail, st.Question.Render("type "+article(m.Mode())+" "+sanitize(m.Mode())+" first (esc cancels)"))
	}
	if m.refused != "" {
		tail = append(tail, st.Question.Render(sanitize(m.refused)))
	}
	return head, append(tail, "") // the key help, see View
}

// View implements tea.Model.
func (m Prompt) View() tea.View {
	st := m.styles
	head, tail := m.lines()
	if m.confirmed || m.cancelled {
		cutLines(head, m.width)
		return tea.NewView(strings.Join(head, "\n"))
	}
	suggestion := m.suggestion()
	if suggestion != "" {
		head[1] += st.Detail.Render(sanitize(suggestion))
	}
	rows, sel := m.rows(m.room(head, tail))
	lines := head
	for i, s := range rows {
		line := "  " + s.Value
		if i == sel {
			line = st.Cursor.Render("> ") + st.CurrentRow.Render(s.Value)
		}
		if d := oneLine(s.Detail); d != "" {
			line += "  " + st.Detail.Render(d)
		}
		lines = append(lines, line)
	}
	bindings := []key.Binding{m.keys.Confirm}
	if len(rows) > 0 && rows[sel].Value != m.value {
		bindings = append(bindings, m.keys.Take)
	}
	if len(rows) > 1 {
		bindings = append(bindings, m.keys.Up)
	}
	if suggestion != "" {
		bindings = append(bindings, m.keys.Complete)
	}
	if len(m.modes) > 1 {
		bindings = append(bindings, m.keys.Mode)
	}
	tail[len(tail)-1] = m.help.ShortHelpView(append(bindings, m.keys.Cancel))
	lines = append(lines, tail...)
	cutLines(lines, m.width)
	return tea.NewView(strings.Join(lines, "\n"))
}
