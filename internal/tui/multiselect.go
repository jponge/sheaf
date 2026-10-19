package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Item is one selectable row of a MultiSelect.
type Item struct {
	Key      string   // returned by SelectedKeys; should be unique
	Label    string   // main text, matched by the filter
	Detail   string   // secondary text, matched by the filter
	Tags     []string // short markers shown after the text, e.g. "fork", "archived"
	Selected bool     // initial selection state
	Disabled bool     // cannot be toggled; its Selected state is kept as given
}

// Text fields of an Item and the title are displayed with control characters
// replaced by U+FFFD. Check boxes read "[ ]" and "[x]", or "[-]" and "[+]"
// for disabled items, so that they stay distinguishable without colors.

type multiSelectKeys struct {
	Up, Down, PageUp, PageDown, Home, End key.Binding
	Toggle, All, None, Filter             key.Binding
	Confirm, Cancel, ForceQuit            key.Binding
	// Filter mode.
	ApplyFilter, ClearFilter key.Binding
}

func newMultiSelectKeys() multiSelectKeys {
	return multiSelectKeys{
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:      key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
		PageDown:    key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "page down")),
		Home:        key.NewBinding(key.WithKeys("home"), key.WithHelp("home", "first")),
		End:         key.NewBinding(key.WithKeys("end"), key.WithHelp("end", "last")),
		Toggle:      key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")),
		All:         key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "all")),
		None:        key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "none")),
		Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Confirm:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		Cancel:      key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc/q", "cancel")),
		ForceQuit:   key.NewBinding(key.WithKeys("ctrl+c")),
		ApplyFilter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply filter")),
		ClearFilter: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
	}
}

// MultiSelect is a bubbletea model for picking any number of items from a
// list, with a case-insensitive substring filter over labels and details.
//
// Keys: up/down/k/j, pgup/pgdown, home/end move; space toggles; a selects and
// n deselects every visible enabled item; / enters filter mode; enter
// confirms; esc/q/ctrl+c cancel. Printable runes delivered together in one
// key event are handled one by one; a bracketed paste is ignored in list
// mode. In filter mode typed or pasted text (without control characters)
// edits the filter, backspace deletes a rune, ctrl+w a word and ctrl+u the
// whole filter, enter keeps it and returns to the list, esc clears it. In list mode, esc
// first clears an applied filter and only cancels when no filter is set.
//
// Update returns tea.Quit once the selection is confirmed or cancelled; later
// messages are ignored.
type MultiSelect struct {
	title   string
	items   []Item
	visible []int // indices into items matching the filter
	cursor  int   // index into visible
	offset  int   // index into visible of the first rendered row

	width, height int // 0 = unknown

	filter    string
	filtering bool

	confirmed, cancelled, interrupted bool

	lockedLabel string // see WithLockedLabel

	keys   multiSelectKeys
	help   help.Model
	styles Styles
}

// NewMultiSelect returns a MultiSelect over a copy of items.
func NewMultiSelect(title string, items []Item) MultiSelect {
	h := help.New()
	st := DefaultStyles()
	h.Styles = st.Help
	m := MultiSelect{
		title:  title,
		items:  append([]Item(nil), items...),
		keys:   newMultiSelectKeys(),
		help:   h,
		styles: st,
	}
	m.applyFilter()
	return m
}

// WithStyles returns a copy of m using st.
func (m MultiSelect) WithStyles(st Styles) MultiSelect {
	m.styles = st
	m.help.Styles = st.Help
	return m
}

// WithLockedLabel returns a copy of m whose status line counts only the
// enabled items as the user's choices, and the disabled selected items
// (e.g. repositories already present) apart, under label: "0/2 selected ·
// 1 present".
func (m MultiSelect) WithLockedLabel(label string) MultiSelect {
	m.lockedLabel = label
	return m
}

// Init implements tea.Model.
func (m MultiSelect) Init() tea.Cmd { return nil }

// Confirmed reports whether the user confirmed the selection.
func (m MultiSelect) Confirmed() bool { return m.confirmed }

// Cancelled reports whether the user cancelled. A model whose program ended
// for another reason (e.g. input closed) is neither confirmed nor cancelled.
func (m MultiSelect) Cancelled() bool { return m.cancelled }

// Interrupted reports whether the menu was cancelled with ctrl+c, which
// callers treat as an interruption of the command rather than a plain
// cancellation.
func (m MultiSelect) Interrupted() bool { return m.interrupted }

// SelectedKeys returns the keys of the currently selected items, filtered or
// not, in their original order. It does not check Confirmed.
func (m MultiSelect) SelectedKeys() []string {
	var keys []string
	for _, it := range m.items {
		if it.Selected {
			keys = append(keys, it.Key)
		}
	}
	return keys
}

// Update implements tea.Model.
func (m MultiSelect) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.confirmed || m.cancelled {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		m.scrollToCursor()
		return m, nil
	case tea.PasteMsg:
		if m.filtering {
			m.setFilter(m.filter + dropControl(msg.Content))
		}
		return m, nil
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

func (m MultiSelect) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if utf8.RuneCountInString(msg.Text) > 1 {
		return m.updateRunes([]rune(msg.Text))
	}
	switch {
	case key.Matches(msg, m.keys.Confirm):
		m.confirmed = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Cancel):
		if msg.String() == "esc" && m.filter != "" {
			m.setFilter("")
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Toggle):
		if len(m.visible) > 0 && !m.items[m.visible[m.cursor]].Disabled {
			m.cloneItems()
			it := &m.items[m.visible[m.cursor]]
			it.Selected = !it.Selected
		}
	case key.Matches(msg, m.keys.All):
		m.setVisible(true)
	case key.Matches(msg, m.keys.None):
		m.setVisible(false)
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.scrollToCursor() // the filter line takes a row
	default:
		m.move(msg)
	}
	return m, nil
}

// updateRunes handles each rune as its own key press. A '/' among them enters
// filter mode, and the following runes then edit the filter.
func (m MultiSelect) updateRunes(runes []rune) (tea.Model, tea.Cmd) {
	var model tea.Model = m
	var cmd tea.Cmd
	for _, r := range runes {
		var c tea.Cmd // a ' ' rune is the space key
		model, c = model.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		if c != nil {
			cmd = c
		}
	}
	return model, cmd
}

func (m MultiSelect) updateFilter(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

// typedText returns the text a key types into the filter: its text, or for
// alt+<printable> (which has none) the printable rune, as the filter took the
// runes of alt-modified keys with bubbletea v1.
func typedText(msg tea.KeyPressMsg) string {
	if msg.Text != "" {
		return msg.Text
	}
	if msg.Mod&^(tea.ModShift|tea.ModCapsLock) != tea.ModAlt {
		return ""
	}
	r := msg.Code
	if msg.Mod.Contains(tea.ModShift) && msg.ShiftedCode != 0 {
		r = msg.ShiftedCode
	}
	if r == tea.KeySpace || (r < tea.KeyExtended && unicode.IsPrint(r)) {
		return string(r)
	}
	return ""
}

// move handles navigation keys; other keys are ignored.
func (m *MultiSelect) move(msg tea.KeyPressMsg) {
	switch {
	case key.Matches(msg, m.keys.Up):
		m.cursor--
	case key.Matches(msg, m.keys.Down):
		m.cursor++
	case key.Matches(msg, m.keys.PageUp):
		m.cursor -= m.listHeight()
	case key.Matches(msg, m.keys.PageDown):
		m.cursor += m.listHeight()
	case key.Matches(msg, m.keys.Home):
		m.cursor = 0
	case key.Matches(msg, m.keys.End):
		m.cursor = len(m.visible) - 1
	default:
		return
	}
	m.scrollToCursor()
}

// cloneItems gives m its own items slice before a mutation, so that earlier
// copies of the model keep their state (models are values in bubbletea).
func (m *MultiSelect) cloneItems() {
	m.items = slices.Clone(m.items)
}

func (m *MultiSelect) setVisible(selected bool) {
	m.cloneItems()
	for _, i := range m.visible {
		if !m.items[i].Disabled {
			m.items[i].Selected = selected
		}
	}
}

func (m *MultiSelect) setFilter(f string) {
	m.filter = f
	m.applyFilter()
}

// applyFilter recomputes the visible rows, keeping the cursor on the same item
// when it is still visible.
func (m *MultiSelect) applyFilter() {
	current := -1
	if m.cursor >= 0 && m.cursor < len(m.visible) {
		current = m.visible[m.cursor]
	}
	needle := strings.ToLower(m.filter)
	m.visible = nil // fresh slice: copies of the model must not share it
	m.cursor = 0
	for i, it := range m.items {
		if needle == "" || strings.Contains(strings.ToLower(it.Label), needle) ||
			strings.Contains(strings.ToLower(it.Detail), needle) {
			if i == current {
				m.cursor = len(m.visible)
			}
			m.visible = append(m.visible, i)
		}
	}
	m.offset = 0
	m.scrollToCursor()
}

// layout tells which lines View renders around the item rows, and how many
// item rows fit.
type layout struct {
	title, filter, status, help bool
	gaps                        int // blank lines of the footer, see footer
	rows                        int
}

// layout fits the view into the window height. The blank lines of the
// footer are dropped (the one above the help first) when that lets every
// visible item fit (see fitGaps). When the window is too small for at least
// one item row, the blank lines, then the help, title, status and filter
// lines are dropped in that order. With an unknown height every visible
// item is shown.
func (m MultiSelect) layout() layout {
	l := layout{title: true, filter: m.filtering || m.filter != "", status: true, help: true, gaps: 2}
	if m.height <= 0 {
		l.rows = max(1, len(m.visible))
		return l
	}
	chrome := func() int {
		n := l.gaps
		for _, shown := range []bool{l.title, l.filter, l.status} {
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
	for _, line := range []*bool{&l.help, &l.title, &l.status, &l.filter} {
		if m.height-chrome() >= 1 {
			break
		}
		*line = false
	}
	l.rows = max(1, m.height-chrome())
	return l
}

// fitGaps returns how many of the gaps blank lines of a footer a view of
// height keeps, with lines other lines and n rows to show: all of them when
// the rows fit, as many as let every row fit otherwise (the rows then do
// not scroll), and when they scroll anyway, as many as leave room for a
// row.
func fitGaps(gaps, height, lines, n int) int {
	room := height - lines
	switch {
	case room-gaps >= n:
	case room >= n:
		gaps = room - n
	default:
		gaps = max(0, min(gaps, room-1))
	}
	return gaps
}

// footer returns the lines below the rows of a list view: its sections
// (e.g. the status line, the key help) without the empty ones, the first
// gaps of them preceded by a blank line.
func footer(gaps int, sections ...[]string) []string {
	var lines []string
	for _, s := range sections {
		if len(s) == 0 {
			continue
		}
		if gaps > 0 {
			lines = append(lines, "")
			gaps--
		}
		lines = append(lines, s...)
	}
	return lines
}

// fitStatus returns how many of parts (none empty), joined with " · " into
// a status line, to keep within width (0 = unknown): the first part always
// (truncated by the caller if need be), each next one only if it fits
// whole with its separator, so that no part is cut partway (a cut part
// could read as another number, "12 shown" as "12"); the parts after the
// first that does not fit are dropped too.
func fitStatus(width int, parts []string) int {
	if len(parts) == 0 {
		return 0
	}
	n, w := 1, lipgloss.Width(parts[0])
	for ; n < len(parts); n++ {
		next := w + lipgloss.Width(" · "+parts[n])
		if width > 0 && next > width {
			break
		}
		w = next
	}
	return n
}

// wrapHelp renders the key help of bindings on as many lines of width as
// needed so that none is cut off (on one line when the width is unknown);
// only a binding wider than the width alone is truncated by h.
func wrapHelp(h help.Model, width int, bindings []key.Binding) []string {
	unbounded := h // measures lines without the ellipsis
	unbounded.SetWidth(0)
	var lines []string
	var line []key.Binding
	for _, b := range bindings {
		if len(line) > 0 && width > 0 && lipgloss.Width(unbounded.ShortHelpView(append(slices.Clone(line), b))) > width {
			lines = append(lines, h.ShortHelpView(line))
			line = nil
		}
		line = append(line, b)
	}
	if len(line) > 0 {
		lines = append(lines, h.ShortHelpView(line))
	}
	return lines
}

// joinStatus joins the non-empty parts of a status line with " · ".
func joinStatus(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

// listHeight is the number of item rows that fit in the view.
func (m MultiSelect) listHeight() int { return m.layout().rows }

// scrollToCursor clamps the cursor and adjusts the offset so it is rendered.
func (m *MultiSelect) scrollToCursor() {
	n := len(m.visible)
	m.cursor = min(max(m.cursor, 0), max(n-1, 0))
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = min(max(m.offset, 0), max(n-h, 0))
}

// View implements tea.Model.
func (m MultiSelect) View() tea.View {
	st := m.styles
	l := m.layout()
	var lines []string
	if l.title {
		lines = append(lines, st.Title.Render(sanitize(m.title)))
	}
	if l.filter {
		f := st.FilterPrompt.Render("/") + m.filter
		if m.filtering {
			f += "█"
		}
		lines = append(lines, f)
	}

	switch {
	case len(m.items) == 0:
		lines = append(lines, st.Empty.Render("No items."))
	case len(m.visible) == 0:
		lines = append(lines, st.Empty.Render("No matches."))
	default:
		end := min(m.offset+l.rows, len(m.visible))
		for row := m.offset; row < end; row++ {
			lines = append(lines, m.renderRow(row))
		}
	}

	var status, help []string
	if l.status {
		parts := m.status()
		status = []string{st.Status.Render(joinStatus(parts[:fitStatus(m.width, parts)]...))}
	}
	if l.help {
		help = m.helpLines()
	}
	lines = append(lines, footer(l.gaps, status, help)...)

	if m.width > 0 {
		trunc := lipgloss.NewStyle().MaxWidth(m.width)
		for i, line := range lines {
			lines[i] = trunc.Render(line)
		}
	}
	return tea.NewView(strings.Join(lines, "\n"))
}

// helpLines renders the key help, wrapped to the width so that no key is
// cut off.
func (m MultiSelect) helpLines() []string {
	if m.filtering {
		return wrapHelp(m.help, m.width, []key.Binding{m.keys.ApplyFilter, m.keys.ClearFilter})
	}
	return wrapHelp(m.help, m.width, []key.Binding{
		m.keys.Up, m.keys.Down, m.keys.Toggle, m.keys.All, m.keys.None,
		m.keys.Filter, m.keys.Confirm, m.keys.Cancel,
	})
}

func (m MultiSelect) renderRow(row int) string {
	st := m.styles
	it := m.items[m.visible[row]]
	current := row == m.cursor

	cursor := "  "
	if current {
		cursor = st.Cursor.Render("> ")
	}
	// A disabled row is rendered plain, then faint as a whole: nested styles
	// would reset the faint attribute midway.
	style := func(s lipgloss.Style, text string) string {
		if it.Disabled {
			return text
		}
		return s.Render(text)
	}
	var box string
	switch {
	case it.Disabled && it.Selected:
		box = "[+]"
	case it.Disabled:
		box = "[-]"
	case it.Selected:
		box = st.Checked.Render("[x]")
	default:
		box = st.Unchecked.Render("[ ]")
	}
	label := sanitize(it.Label)
	if current {
		label = style(st.CurrentRow, label)
	}
	parts := []string{box, label}
	if it.Detail != "" {
		parts = append(parts, style(st.Detail, sanitize(it.Detail)))
	}
	for _, t := range it.Tags {
		parts = append(parts, style(st.Tag, "["+sanitize(t)+"]"))
	}
	line := strings.Join(parts, " ")
	if it.Disabled {
		line = st.Disabled.Render(line)
	}
	return cursor + line
}

// status returns the parts of the status line.
func (m MultiSelect) status() []string {
	selected, total, locked := len(m.SelectedKeys()), len(m.items), 0
	if m.lockedLabel != "" {
		// Only the enabled items are the user's choices.
		selected, total = 0, 0
		for _, it := range m.items {
			switch {
			case it.Disabled && it.Selected:
				locked++
			case !it.Disabled:
				total++
				if it.Selected {
					selected++
				}
			}
		}
	}
	parts := []string{fmt.Sprintf("%d/%d selected", selected, total)}
	if locked > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", locked, m.lockedLabel))
	}
	if m.filter != "" {
		parts = append(parts, fmt.Sprintf("%d shown", len(m.visible)))
	}
	if h := m.listHeight(); len(m.visible) > h {
		parts = append(parts, fmt.Sprintf("rows %d-%d of %d", m.offset+1, m.offset+h, len(m.visible)))
	}
	return parts
}
