package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Row is one row of an Overview table.
type Row struct {
	Key   string   // returned by Overview.Chosen; should be unique
	Cells []string // one per column, matched by the filter
	Alert bool     // the last cell is rendered with the Failed style (e.g. a problem)
	Level Level    // unless Alert, the last cell is rendered in the style of this level
}

// Scope tells which rows an OverviewAction applies to.
type Scope int

const (
	// ScopeRows applies to the selected rows, or the current row when none
	// is selected. Like MultiSelect.SelectedKeys, selected rows count whether
	// the filter shows them or not (the status line gives the number
	// selected).
	ScopeRows Scope = iota
	// ScopeCurrent applies to the current row only.
	ScopeCurrent
	// ScopeNone applies to no row (e.g. an action on the whole bundle).
	ScopeNone
)

// OverviewAction is an action of an Overview, triggered by a key.
type OverviewAction struct {
	Key   string // as tea.KeyPressMsg.String(), e.g. "u" or "enter"
	Name  string // returned by Overview.Chosen
	Help  string // short description in the help line
	Short bool   // also listed in the compact key help (see Overview)
	Scope Scope
	// Check, if set, may refuse the action for its targets: a non-empty
	// message is displayed and the overview stays open.
	Check func(targets []string) string
	// Do, if set, runs the action in the overview instead of choosing it:
	// the overview displays the message it returns, runs its command and
	// stays open (e.g. copying a path to the clipboard).
	Do func(targets []string) (message string, cmd tea.Cmd)
}

// noRowMessage is displayed when an action needing a row has none.
const noRowMessage = "no row to act on"

// Overview is a bubbletea model displaying a table of rows with a cursor,
// a multi-selection and a filter, and quitting with the action chosen by
// the user.
//
// Keys: up/down/k/j, pgup/pgdown, home/end move; space toggles the
// selection of the current row; a selects and n deselects every visible
// row (rows hidden by the filter keep their state, as in MultiSelect);
// / enters filter mode (case-insensitive substring over the cells; editing
// as in MultiSelect: enter keeps the filter, esc clears it); ? toggles
// the full key help (one short line by default, with the Short actions);
// the keys of the actions choose an action (or run it in place, see
// OverviewAction.Do);
// q, esc and ctrl+c quit. In list mode esc first clears an applied filter and only
// quits when no filter is set. Key events of several runes and bracketed
// pastes are ignored in list mode, so pasted text never triggers actions.
// Text fields are displayed with control characters replaced by U+FFFD.
//
// Update returns tea.Quit once an action is chosen or the user quits; later
// messages are ignored and the view is then empty, so that the program
// leaves nothing in the terminal.
type Overview struct {
	title   string
	header  []string
	rows    []Row
	actions []OverviewAction
	message string

	selected map[string]bool
	visible  []int // indices into rows matching the filter
	cursor   int   // index into visible
	offset   int   // index into visible of the first rendered row

	width, height int // 0 = unknown
	altScreen     bool
	grouped       bool  // see WithGrouping
	shrink        []int // see WithShrinkable

	filter    string
	filtering bool
	fullHelp  bool

	chosen                 string
	targets                []string
	cancelled, interrupted bool

	keys   multiSelectKeys
	help   help.Model
	styles Styles
}

// NewOverview returns an Overview of rows (copied) under header.
func NewOverview(title string, header []string, rows []Row, actions []OverviewAction) Overview {
	h := help.New()
	st := DefaultStyles()
	h.Styles = st.Help
	keys := newMultiSelectKeys()
	// The keys are those of MultiSelect, with the same help except that
	// esc and q quit the program rather than cancel a menu.
	keys.Cancel.SetHelp("esc/q", "quit")
	m := Overview{
		title:    title,
		header:   slices.Clone(header),
		rows:     slices.Clone(rows),
		actions:  slices.Clone(actions),
		selected: map[string]bool{},
		keys:     keys,
		help:     h,
		styles:   st,
	}
	m.applyFilter()
	return m
}

// WithMessage returns a copy of m displaying msg below the table, on the
// status line.
func (m Overview) WithMessage(msg string) Overview {
	m.message = msg
	return m
}

// WithAltScreen returns a copy of m rendered on the alternate screen: each
// program starts on a clear screen, and the terminal shows again what it
// showed before once the program ends.
func (m Overview) WithAltScreen() Overview {
	m.altScreen = true
	return m
}

// WithGrouping returns a copy of m that groups rows by their first cell
// (e.g. a repository): the first cell of a rendered row is left blank when
// it equals that of the visible row above it, except on the first row
// rendered, so that each group shown starts with its name. The filter still
// matches the blanked cells.
func (m Overview) WithGrouping() Overview {
	m.grouped = true
	return m
}

// WithShrinkable returns a copy of m in which the columns cols (indices,
// not the last column) are narrowed, in that order, when the table is wider
// than the window: their cells are truncated with an ellipsis, down to
// minShrunkWidth, so that the last column fits. The width of the last
// column is taken over the rows without Alert (their free text, e.g. an
// error, may be long); lines that still do not fit are cut at the window
// width.
func (m Overview) WithShrinkable(cols ...int) Overview {
	m.shrink = slices.Clone(cols)
	return m
}

// minShrunkWidth is the width down to which WithShrinkable narrows a column.
const minShrunkWidth = 8

// WithCursor returns a copy of m whose cursor is on the row key, if visible.
func (m Overview) WithCursor(key string) Overview {
	for i, r := range m.visible {
		if m.rows[r].Key == key {
			m.cursor = i
			m.scrollToCursor()
		}
	}
	return m
}

// WithFilter returns a copy of m filtering its rows with f.
func (m Overview) WithFilter(f string) Overview {
	m.setFilter(dropControl(f))
	return m
}

// WithSelected returns a copy of m in which the rows of keys are selected
// (keys of no row are ignored), e.g. the selection of a previous overview.
func (m Overview) WithSelected(keys []string) Overview {
	m.selected = map[string]bool{}
	for _, r := range m.rows {
		if slices.Contains(keys, r.Key) {
			m.selected[r.Key] = true
		}
	}
	return m
}

// SelectedKeys returns the keys of the selected rows, filtered or not, in
// row order.
func (m Overview) SelectedKeys() []string {
	var keys []string
	for _, r := range m.rows {
		if m.selected[r.Key] {
			keys = append(keys, r.Key)
		}
	}
	return keys
}

// Init implements tea.Model.
func (m Overview) Init() tea.Cmd { return nil }

// Chosen returns the name of the chosen action and its targets (row keys,
// nil for ScopeNone); the name is empty when no action was chosen.
func (m Overview) Chosen() (string, []string) { return m.chosen, m.targets }

// Cancelled reports whether the user quit (q, esc, ctrl+c). A model whose
// program ended for another reason (e.g. input closed) has neither chosen
// an action nor been cancelled.
func (m Overview) Cancelled() bool { return m.cancelled }

// Interrupted reports whether the user quit with ctrl+c, which callers
// treat as an interruption rather than a plain quit.
func (m Overview) Interrupted() bool { return m.interrupted }

// CurrentKey returns the key of the current row, "" when no row is visible.
func (m Overview) CurrentKey() string {
	if m.cursor < len(m.visible) {
		return m.rows[m.visible[m.cursor]].Key
	}
	return ""
}

// Filter returns the filter.
func (m Overview) Filter() string { return m.filter }

// WithFullHelp returns a copy of m showing the full key help when full is
// set, the compact one otherwise (see Overview), e.g. as a previous
// overview did.
func (m Overview) WithFullHelp(full bool) Overview {
	m.fullHelp = full
	return m
}

// FullHelp reports whether m shows the full key help.
func (m Overview) FullHelp() bool { return m.fullHelp }

func (m Overview) done() bool { return m.chosen != "" || m.cancelled }

// Update implements tea.Model.
func (m Overview) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done() {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		m.scrollToCursor()
	case tea.PasteMsg:
		if m.filtering {
			m.setFilter(m.filter + dropControl(msg.Content))
		}
	case tea.KeyPressMsg:
		if key.Matches(msg, m.keys.ForceQuit) {
			m.cancelled, m.interrupted = true, true
			return m, tea.Quit
		}
		if m.filtering {
			return m.updateFilter(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

func (m Overview) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if utf8.RuneCountInString(msg.Text) > 1 {
		return m, nil
	}
	s := msg.String()
	for _, a := range m.actions {
		if a.Key == s {
			return m.choose(a)
		}
	}
	switch {
	case key.Matches(msg, m.keys.Cancel):
		if s == "esc" && m.filter != "" {
			m.setFilter("")
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Toggle):
		if k := m.CurrentKey(); k != "" {
			m.selected = cloneSet(m.selected)
			if m.selected[k] {
				delete(m.selected, k)
			} else {
				m.selected[k] = true
			}
		}
	case key.Matches(msg, m.keys.All):
		m.setVisible(true)
	case key.Matches(msg, m.keys.None):
		m.setVisible(false)
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.scrollToCursor() // the filter line takes a row
	case key.Matches(msg, m.keys.More):
		m.fullHelp = !m.fullHelp
		m.scrollToCursor() // the help lines changed
	default:
		m.move(msg)
	}
	return m, nil
}

// setVisible selects or deselects every visible row.
func (m *Overview) setVisible(selected bool) {
	m.selected = cloneSet(m.selected)
	for _, i := range m.visible {
		if selected {
			m.selected[m.rows[i].Key] = true
		} else {
			delete(m.selected, m.rows[i].Key)
		}
	}
}

// cloneSet returns a copy of s: models are values in bubbletea, and earlier
// copies must keep their selection.
func cloneSet(s map[string]bool) map[string]bool {
	c := make(map[string]bool, len(s))
	maps.Copy(c, s)
	return c
}

// choose quits with a, or displays why it cannot apply.
func (m Overview) choose(a OverviewAction) (tea.Model, tea.Cmd) {
	var targets []string
	switch a.Scope {
	case ScopeRows:
		for _, r := range m.rows {
			if m.selected[r.Key] {
				targets = append(targets, r.Key)
			}
		}
		if len(targets) == 0 {
			if k := m.CurrentKey(); k != "" {
				targets = []string{k}
			}
		}
	case ScopeCurrent:
		if k := m.CurrentKey(); k != "" {
			targets = []string{k}
		}
	}
	if a.Scope != ScopeNone && len(targets) == 0 {
		m.message = noRowMessage
		return m, nil
	}
	if a.Check != nil {
		if why := a.Check(targets); why != "" {
			m.message = why
			return m, nil
		}
	}
	if a.Do != nil {
		var cmd tea.Cmd
		m.message, cmd = a.Do(targets)
		return m, cmd
	}
	m.chosen, m.targets = a.Name, targets
	return m, tea.Quit
}

func (m Overview) updateFilter(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); {
	case key.Matches(msg, m.keys.ApplyFilter):
		m.filtering = false
		m.scrollToCursor()
	case key.Matches(msg, m.keys.ClearFilter):
		m.filtering = false
		m.setFilter("")
	case s == "ctrl+u":
		m.setFilter("")
	case s == "ctrl+w":
		m.setFilter(deleteLastWord(m.filter))
	case s == "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.setFilter(string(r[:len(r)-1]))
		}
	case typedText(msg) != "":
		m.setFilter(m.filter + dropControl(typedText(msg)))
	case s == "up", s == "down", s == "pgup", s == "pgdown", s == "home", s == "end":
		m.move(msg)
	}
	return m, nil
}

// move handles navigation keys; other keys are ignored.
func (m *Overview) move(msg tea.KeyPressMsg) {
	switch {
	case key.Matches(msg, m.keys.Up):
		m.cursor--
	case key.Matches(msg, m.keys.Down):
		m.cursor++
	case key.Matches(msg, m.keys.PageUp):
		m.cursor -= m.layout().rows
	case key.Matches(msg, m.keys.PageDown):
		m.cursor += m.layout().rows
	case key.Matches(msg, m.keys.Home):
		m.cursor = 0
	case key.Matches(msg, m.keys.End):
		m.cursor = len(m.visible) - 1
	default:
		return
	}
	m.scrollToCursor()
}

func (m *Overview) setFilter(f string) {
	m.filter = f
	m.applyFilter()
}

// applyFilter recomputes the visible rows, keeping the cursor on the same
// row when it is still visible.
func (m *Overview) applyFilter() {
	current := -1
	if m.cursor >= 0 && m.cursor < len(m.visible) {
		current = m.visible[m.cursor]
	}
	needle := strings.ToLower(m.filter)
	m.visible = nil // fresh slice: copies of the model must not share it
	m.cursor = 0
	for i, r := range m.rows {
		if needle == "" || strings.Contains(strings.ToLower(strings.Join(r.Cells, "\x00")), needle) {
			if i == current {
				m.cursor = len(m.visible)
			}
			m.visible = append(m.visible, i)
		}
	}
	m.offset = 0
	m.scrollToCursor()
}

// ovLayout tells which lines View renders around the rows, and how many
// rows fit.
type ovLayout struct {
	title, filter, header, status, help bool // status: with the message
	gaps                                int  // blank lines below the title and of the footer, see splitGaps
	rows                                int
}

// layout fits the view into the window height. The blank lines below the
// title and of the footer are dropped (the one above the help first, then
// the one below the title) when that lets every visible row fit (see
// fitGaps and splitGaps). When the window is too small for at least one
// row, the blank lines, then the help, title, status (with the message),
// filter and header lines are dropped in that order. With an unknown
// height every visible row is shown.
func (m Overview) layout() ovLayout {
	l := ovLayout{title: true, filter: m.filtering || m.filter != "", header: true, status: true, help: true, gaps: 3}
	if m.height <= 0 {
		l.rows = max(1, len(m.visible))
		return l
	}
	chrome := func() int {
		n := l.gaps
		for _, shown := range []bool{l.title, l.filter, l.header, l.status} {
			if shown {
				n++
			}
		}
		if l.help {
			n += len(m.helpLines())
		}
		return n
	}
	l.gaps = fitGaps(l.gaps, m.height, chrome()-l.gaps, max(1, len(m.visible)))
	for _, line := range []*bool{&l.help, &l.title, &l.status, &l.filter, &l.header} {
		if m.height-chrome() >= 1 {
			break
		}
		*line = false
	}
	l.rows = max(1, m.height-chrome())
	return l
}

// scrollToCursor clamps the cursor and adjusts the offset so it is rendered.
func (m *Overview) scrollToCursor() {
	n := len(m.visible)
	m.cursor = min(max(m.cursor, 0), max(n-1, 0))
	h := m.layout().rows
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = min(max(m.offset, 0), max(n-h, 0))
}

// columnWidths returns the width of each column over the header and every
// row (not only the visible ones, so that filtering does not shift them),
// the shrinkable columns narrowed to the window width (see WithShrinkable).
func (m Overview) columnWidths() []int {
	var w []int
	grow := func(cells []string) {
		for i, c := range cells {
			if i >= len(w) {
				w = append(w, 0)
			}
			w[i] = max(w[i], lipgloss.Width(sanitize(c)))
		}
	}
	grow(m.header)
	for _, r := range m.rows {
		grow(r.Cells)
	}
	if m.width <= 0 || len(w) == 0 {
		return w
	}
	last := len(w) - 1
	excess := len("> [ ] ") + 2*last - m.width
	for _, cw := range w[:last] {
		excess += cw
	}
	lastWidth := 0
	if last < len(m.header) {
		lastWidth = lipgloss.Width(sanitize(m.header[last]))
	}
	for _, r := range m.rows {
		if !r.Alert && len(r.Cells) == len(w) {
			lastWidth = max(lastWidth, lipgloss.Width(sanitize(r.Cells[last])))
		}
	}
	excess += lastWidth
	for _, c := range m.shrink {
		if excess <= 0 {
			break
		}
		if c >= 0 && c < last {
			cut := min(excess, max(0, w[c]-minShrunkWidth))
			w[c] -= cut
			excess -= cut
		}
	}
	return w
}

// cells pads the cells to the column widths (not the last one), truncating
// those wider than their column, and joins them; style, if set, renders the
// last cell.
func cells(cs []string, widths []int, last *lipgloss.Style) string {
	var b strings.Builder
	for i, c := range cs {
		c = sanitize(c)
		if i < len(cs)-1 && lipgloss.Width(c) > widths[i] {
			c = ansi.Truncate(c, widths[i], "…")
		}
		if i > 0 {
			b.WriteString("  ")
		}
		if i == len(cs)-1 {
			if last != nil {
				c = last.Render(c)
			}
			b.WriteString(c)
			break
		}
		b.WriteString(c + strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(c))))
	}
	return b.String()
}

// View implements tea.Model.
func (m Overview) View() tea.View {
	if m.done() {
		v := tea.NewView("")
		v.AltScreen = m.altScreen
		return v
	}
	st := m.styles
	l := m.layout()
	titleGap, gaps := splitGaps(l.gaps)
	widths := m.columnWidths()
	var lines []string
	if l.title {
		lines = append(lines, st.Title.Render(sanitize(m.title)))
		if titleGap {
			lines = append(lines, "")
		}
	}
	if l.filter {
		f := st.FilterPrompt.Render("/") + m.filter
		if m.filtering {
			f += "█"
		}
		lines = append(lines, f)
	}
	if l.header {
		lines = append(lines, "      "+st.Detail.Render(cells(m.header, widths, nil)))
	}
	switch {
	case len(m.rows) == 0:
		lines = append(lines, st.Empty.Render("No rows."))
	case len(m.visible) == 0:
		lines = append(lines, st.Empty.Render("No matches."))
	default:
		end := min(m.offset+l.rows, len(m.visible))
		for i := m.offset; i < end; i++ {
			lines = append(lines, m.renderRow(i, widths))
		}
	}
	var status, help []string
	if l.status {
		// The message follows the parts, unstyled; free text, it may be
		// cut partway, but is dropped with its separator when not even
		// its start fits.
		parts := m.status()
		parts = parts[:fitStatus(m.width, parts)]
		line := st.Status.Render(joinStatus(parts...))
		msg := sanitize(m.message)
		if msg != "" && (m.width <= 0 || lipgloss.Width(joinStatus(parts...)+" · ") < m.width) {
			line = joinStatus(line, msg)
		}
		status = []string{line}
	}
	if l.help {
		help = m.helpLines()
	}
	lines = append(lines, footer(gaps, status, help)...)
	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, line := range lines {
			lines[i] = trunc.Render(line)
		}
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = m.altScreen
	return v
}

// helpLines renders the key help, wrapped to the width so that no key is
// cut off: in filter mode the filter keys; otherwise the compact help
// (toggle, the Short actions, filter, ? more, quit), or with the full help
// the movement keys and quit, then every action and ? less.
func (m Overview) helpLines() []string {
	if m.filtering {
		return wrapHelp(m.help, m.width, []key.Binding{m.keys.ApplyFilter, m.keys.ClearFilter})
	}
	binding := func(a OverviewAction) key.Binding {
		return key.NewBinding(key.WithKeys(a.Key), key.WithHelp(a.Key, a.Help))
	}
	if !m.fullHelp {
		short := []key.Binding{m.keys.Toggle}
		for _, a := range m.actions {
			if a.Short {
				short = append(short, binding(a))
			}
		}
		quit := key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit"))
		return wrapHelp(m.help, m.width, append(short, m.keys.Filter, m.keys.More, quit))
	}
	lines := wrapHelp(m.help, m.width, []key.Binding{m.keys.Up, m.keys.Down, m.keys.Toggle, m.keys.All, m.keys.None, m.keys.Filter, m.keys.Cancel})
	var actions []key.Binding
	for _, a := range m.actions {
		actions = append(actions, binding(a))
	}
	return append(lines, wrapHelp(m.help, m.width, append(actions, m.keys.Less))...)
}

func (m Overview) renderRow(i int, widths []int) string {
	st := m.styles
	r := m.rows[m.visible[i]]
	cursor := "  "
	if i == m.cursor {
		cursor = st.Cursor.Render("> ")
	}
	box := st.Unchecked.Render("[ ]")
	if m.selected[r.Key] {
		box = st.Checked.Render("[x]")
	}
	var last *lipgloss.Style
	switch {
	case r.Alert:
		last = &st.Failed
	case r.Level != LevelNone:
		s := st.Level(r.Level)
		last = &s
	}
	cs := r.Cells
	if prev := m.rows[m.visible[max(i-1, 0)]].Cells; m.grouped && i > m.offset && len(cs) > 0 && len(prev) > 0 && prev[0] == cs[0] {
		cs = append([]string{""}, cs[1:]...)
	}
	text := cells(cs, widths, last)
	if i == m.cursor {
		text = st.CurrentRow.Render(text)
	}
	return cursor + box + " " + text
}

// status returns the parts of the status line, without the message.
func (m Overview) status() []string {
	n := 0
	for _, r := range m.rows {
		if m.selected[r.Key] {
			n++
		}
	}
	parts := []string{fmt.Sprintf("%d/%d selected", n, len(m.rows))}
	if m.filter != "" {
		parts = append(parts, fmt.Sprintf("%d shown", len(m.visible)))
	}
	if h := m.layout().rows; len(m.visible) > h {
		parts = append(parts, fmt.Sprintf("rows %d-%d of %d", m.offset+1, m.offset+h, len(m.visible)))
	}
	return parts
}
