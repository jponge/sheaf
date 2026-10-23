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
	Links []Link   // hyperlinks in the cells, see Overview.WithHyperlinks
	Match string   // also matched by the filter, not displayed (e.g. the text of a blank cell)
	Note  Note     // faint detail after a cell, see Note
}

// Note is a detail rendered faint after the text of the cell Cell of a Row
// (not the last one), separated by a space, e.g. "(in dir)". It is part of
// the cell for the width of its column and the filter, and gives way first
// when the column is narrowed (see WithShrinkable): it is truncated with
// an ellipsis, down to minNoteWidth columns, then left out, before the
// text of the cell is truncated. The filter matches the cell and its note
// as displayed, separated by a space. A Note without Text is not rendered.
type Note struct {
	Cell int
	Text string
}

// minNoteWidth is the narrowest a truncated Note is rendered: five
// characters and "…", e.g. "(in d…", so that it still tells something.
const minNoteWidth = 6

// Link makes the first occurrence of Text in the cell Cell of a Row a
// hyperlink to URL (see Overview.WithHyperlinks). A cell has at most one
// link: the first Link of the cell.
type Link struct {
	Cell      int
	Text, URL string
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
// Keys: up/down/k/j, pgup/pgdown, home/end/g/G (and the mouse wheel, see
// WithMouse) move; space toggles the
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
	path    string // see WithTitlePath
	header  []string
	rows    []Row
	actions []OverviewAction
	message string
	empty   string // see WithEmpty

	selected map[string]bool
	visible  []int // indices into rows matching the filter
	cursor   int   // index into visible
	offset   int   // index into visible of the first rendered row

	width, height int // 0 = unknown
	altScreen     bool
	mouse         bool  // see WithMouse
	grouped       bool  // see WithGroupFaint
	shrink        []int // see WithShrinkable
	hyperlinks    bool  // see WithHyperlinks

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
	// g and G also go to the first and last row, as in the report view.
	keys.Home = key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g/G", "top/bottom"))
	keys.End = key.NewBinding(key.WithKeys("end", "G", "shift+g"))
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

// WithTitlePath returns a copy of m whose title ends with path (e.g. the
// bundle root): a title too wide for the window loses the start of path
// rather than its end (see cutTitle).
func (m Overview) WithTitlePath(path string) Overview {
	m.path = path
	return m
}

// WithEmpty returns a copy of m showing text, one line per line of text,
// instead of "No rows." when it has no rows (e.g. what to run to add
// some).
func (m Overview) WithEmpty(text string) Overview {
	m.empty = text
	return m
}

// WithAltScreen returns a copy of m rendered on the alternate screen: each
// program starts on a clear screen, and the terminal shows again what it
// showed before once the program ends.
func (m Overview) WithAltScreen() Overview {
	m.altScreen = true
	return m
}

// WithMouse returns a copy of m that turns mouse reporting on while it is
// displayed (see tea.MouseModeCellMotion): the wheel moves the cursor by
// wheelStep rows; clicks are ignored. Mouse reporting keeps the terminal
// from selecting text by itself (most terminals still do with a modifier
// held, usually shift).
func (m Overview) WithMouse() Overview {
	m.mouse = true
	return m
}

// wheelStep is the number of rows (lines) by which a turn of the mouse
// wheel moves the cursor of an Overview (scrolls a Report).
const wheelStep = 3

// WithGroupFaint returns a copy of m that groups rows by their first cell
// (e.g. a repository): the first cell of a rendered row is rendered faint
// when it equals that of the visible row above it, except on the first row
// rendered, so that each group shown starts with its name in the style of
// its row. On the current row, the cells after it keep the row's style.
func (m Overview) WithGroupFaint() Overview {
	m.grouped = true
	return m
}

// WithHyperlinks returns a copy of m rendering the Links of its rows as
// hyperlinks (OSC 8), for a terminal that takes terminal controls: the text
// is unchanged, terminals without hyperlinks ignore the sequences. Only a
// URL of printable ASCII characters without spaces is linked; a link is
// ended on its line, also in a cell or a line cut at the window width,
// also in a first cell rendered faint by WithGroupFaint.
func (m Overview) WithHyperlinks() Overview {
	m.hyperlinks = true
	return m
}

// WithShrinkable returns a copy of m in which the columns cols (indices,
// not the last column) are narrowed when the table is wider than the
// window, the widest first (ties: in the order of cols), one column at a
// time: their cells are truncated with an ellipsis, down to
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

// Update implements tea.Model. Moving the cursor to a row with Alert
// clears the message, so that the row's last cell shows instead (see
// shownMessage).
func (m Overview) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	prev := m.CurrentKey()
	next, cmd := m.update(msg)
	if o, ok := next.(Overview); ok && o.CurrentKey() != prev && o.currentAlert() != "" {
		o.message = ""
		next = o
	}
	return next, cmd
}

// currentAlert returns the last cell of the current row if it has Alert
// (e.g. a problem), else "".
func (m Overview) currentAlert() string {
	if m.cursor >= len(m.visible) {
		return ""
	}
	r := m.rows[m.visible[m.cursor]]
	if !r.Alert || len(r.Cells) == 0 {
		return ""
	}
	return r.Cells[len(r.Cells)-1]
}

// shownMessage returns the message displayed on the status line: the
// message, else the whole last cell of the current row if it has Alert
// (B62 M5: a problem is often cut in the table).
func (m Overview) shownMessage() string {
	if m.message != "" {
		return sanitize(m.message)
	}
	return sanitize(m.currentAlert())
}

func (m Overview) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.cursor -= wheelStep
		case tea.MouseWheelDown:
			m.cursor += wheelStep
		}
		m.scrollToCursor()
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
		cs := slices.Clone(r.Cells)
		if n := r.Note; n.Text != "" && n.Cell >= 0 && n.Cell < len(cs) {
			cs[n.Cell] += " " + n.Text // as displayed
		}
		if needle == "" || strings.Contains(strings.ToLower(strings.Join(append(cs, r.Match), "\x00")), needle) {
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
	message2                            bool // the second line of the message, see statusLines
	gaps                                int  // blank lines below the title and of the footer, see splitGaps
	rows                                int
}

// layout fits the view into the window height. The blank lines below the
// title and of the footer are dropped (the one above the help first, then
// the one below the title) when that lets every visible row fit (see
// fitGaps and splitGaps). When the window is too small for at least one
// row, the blank lines, then the second line of the message, the help,
// title, status (with the message), filter and header lines are dropped in
// that order. With an unknown height every visible row is shown.
func (m Overview) layout() ovLayout {
	l := ovLayout{title: true, filter: m.filtering || m.filter != "", header: true, status: true, help: true, gaps: 3, message2: m.messageWraps()}
	body := max(1, len(m.visible))
	if len(m.rows) == 0 {
		body = len(m.emptyLines())
	}
	if m.height <= 0 {
		l.rows = body
		return l
	}
	chrome := func() int {
		n := l.gaps
		for _, shown := range []bool{l.title, l.filter, l.header, l.status, l.status && l.message2} {
			if shown {
				n++
			}
		}
		if l.help {
			n += len(m.helpLines())
		}
		return n
	}
	l.gaps = fitGaps(l.gaps, m.height, chrome()-l.gaps, body)
	for _, line := range []*bool{&l.message2, &l.help, &l.title, &l.status, &l.filter, &l.header} {
		if m.height-chrome() >= 1 {
			break
		}
		*line = false
	}
	l.message2 = l.message2 && l.status
	l.rows = max(1, m.height-chrome())
	return l
}

// emptyLines returns the lines shown without rows (see WithEmpty).
func (m Overview) emptyLines() []string {
	if m.empty == "" {
		return []string{"No rows."}
	}
	return strings.Split(m.empty, "\n")
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
	return fitColumns(m.header, m.rows, m.width, len("> [ ] "), m.shrink)
}

// fitColumns returns the width of each column over header and rows, with
// the columns shrink narrowed, the widest first (ties: in the order of
// shrink) and down to minShrunkWidth,
// so that prefix and the table, its last column as wide as the widest
// last cell of the header and the rows without Alert, fit width (0 =
// unknown: nothing is narrowed); see WithShrinkable.
func fitColumns(header []string, rows []Row, width, prefix int, shrink []int) []int {
	w, _ := fitWidths(header, rows, width, prefix, shrink)
	return w
}

// fitWidths is fitColumns, also returning the columns by which the table
// is still wider than width (0 or less when it fits).
func fitWidths(header []string, rows []Row, width, prefix int, shrink []int) ([]int, int) {
	var w []int
	grow := func(cells []string) {
		for i, c := range cells {
			if i >= len(w) {
				w = append(w, 0)
			}
			w[i] = max(w[i], lipgloss.Width(sanitize(c)))
		}
	}
	grow(header)
	for _, r := range rows {
		grow(r.Cells)
		if n := r.Note; n.Text != "" && n.Cell >= 0 && n.Cell < len(r.Cells)-1 {
			w[n.Cell] = max(w[n.Cell], lipgloss.Width(sanitize(r.Cells[n.Cell]))+1+lipgloss.Width(sanitize(n.Text)))
		}
	}
	if width <= 0 || len(w) == 0 {
		return w, 0
	}
	last := len(w) - 1
	excess := prefix + 2*last - width
	for _, cw := range w[:last] {
		excess += cw
	}
	lastWidth := 0
	if last < len(header) {
		lastWidth = lipgloss.Width(sanitize(header[last]))
	}
	for _, r := range rows {
		if !r.Alert && len(r.Cells) == len(w) {
			lastWidth = max(lastWidth, lipgloss.Width(sanitize(r.Cells[last])))
		}
	}
	excess += lastWidth
	for ; excess > 0; excess-- {
		widest := -1
		for _, c := range shrink {
			if c >= 0 && c < last && w[c] > minShrunkWidth && (widest < 0 || w[c] > w[widest]) {
				widest = c
			}
		}
		if widest < 0 {
			break
		}
		w[widest]--
	}
	return w, excess
}

// Table renders header and rows as an Overview renders its table (see
// WithGroupFaint and WithShrinkable), for output that is not a bubbletea
// program (e.g. a command's table on a terminal), without cursor,
// selection boxes or header style: the columns shrink are narrowed to fit
// width (0 = unknown: nothing is narrowed or cut), the first cell is
// rendered by paint at LevelInfo (faint) when it repeats the one of the
// row above, the last cell is rendered by paint (nil: unstyled) at the
// level of its row (LevelFail for Alert), and
// lines still wider than width are cut with "…". Cells are sanitized.
// When narrowing the columns shrink to minShrunkWidth is not enough, every
// column but the last gives way, the widest first (ties: the columns
// shrink, then the others from the right), down to one character, so that
// the last column stays whole as long as possible.
func Table(header []string, rows []Row, width int, shrink []int, paint func(Level, string) string) []string {
	widths, excess := fitWidths(header, rows, width, 0, shrink)
	order := slices.Clone(shrink)
	for c := len(widths) - 2; c >= 0; c-- {
		if !slices.Contains(shrink, c) {
			order = append(order, c)
		}
	}
	for ; excess > 0; excess-- {
		widest := -1
		for _, c := range order {
			if c >= 0 && c < len(widths)-1 && widths[c] > 1 && (widest < 0 || widths[c] > widths[widest]) {
				widest = c
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
	}
	lines := []string{cells(header, widths, nil, nil, nil, Note{}, nil)}
	for i, r := range rows {
		var first, last func(string) string
		if paint != nil && i > 0 && len(r.Cells) > 0 && len(rows[i-1].Cells) > 0 && rows[i-1].Cells[0] == r.Cells[0] {
			first = func(s string) string { return paint(LevelInfo, s) }
		}
		if paint != nil {
			l := r.Level
			if r.Alert {
				l = LevelFail
			}
			last = func(s string) string { return paint(l, s) }
		}
		lines = append(lines, cells(r.Cells, widths, first, last, nil, r.Note, nil))
	}
	cutLines(lines, width)
	return lines
}

// cells pads the cells to the column widths (not the last one), truncating
// those wider than their column, and joins them; first and last, if set,
// render the first cell (when not the last one) and the last cell. The first link of each cell is rendered as a hyperlink (see
// link). note follows its cell, rendered by faint (nil: unstyled), as far
// as it fits (see Note).
func cells(cs []string, widths []int, first, last func(string) string, links []Link, note Note, faint func(string) string) string {
	var b strings.Builder
	for i, c := range cs {
		c = sanitize(c)
		for _, l := range links {
			if l.Cell == i {
				c = link(c, l)
				break
			}
		}
		if i == note.Cell && i < len(cs)-1 && note.Text != "" {
			text := sanitize(note.Text)
			if room := widths[i] - lipgloss.Width(c) - 1; room >= min(lipgloss.Width(text), minNoteWidth) {
				n := ansi.Truncate(text, room, "…")
				if faint != nil {
					n = faint(n)
				}
				c += " " + n
			}
		}
		if i < len(cs)-1 && lipgloss.Width(c) > widths[i] {
			c = ansi.Truncate(c, widths[i], "…")
		}
		if i > 0 {
			b.WriteString("  ")
		}
		if i == 0 && i < len(cs)-1 && first != nil {
			c = first(c)
		}
		if i == len(cs)-1 {
			if last != nil {
				c = last(c)
			}
			b.WriteString(c)
			break
		}
		b.WriteString(c + strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(c))))
	}
	return b.String()
}

// link returns the sanitized cell c with the first occurrence of the text
// of l (sanitized) made a hyperlink to its URL (OSC 8), if its URL is safe
// to send (see safeURL). Truncation (ansi.Truncate) keeps the sequence
// that ends the link, which takes no width.
func link(c string, l Link) string {
	text := sanitize(l.Text)
	i := strings.Index(c, text)
	if text == "" || i < 0 || !safeURL(l.URL) {
		return c
	}
	return c[:i] + ansi.SetHyperlink(l.URL) + text + ansi.ResetHyperlink() + c[i+len(text):]
}

// safeURL reports whether url is not empty and only made of printable
// ASCII characters other than space, so that it can neither end the OSC 8
// sequence early nor inject another sequence.
func safeURL(url string) bool {
	for _, r := range url {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return url != ""
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
		lines = append(lines, st.Title.Render(cutTitle(sanitize(m.title), sanitize(m.path), m.width)))
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
		lines = append(lines, "      "+st.Detail.Render(cells(m.header, widths, nil, nil, nil, Note{}, nil)))
	}
	switch {
	case len(m.rows) == 0:
		empty := m.emptyLines()
		for _, e := range empty[:min(l.rows, len(empty))] {
			lines = append(lines, st.Empty.Render(sanitize(e)))
		}
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
		status = m.statusLines(l.message2)
	}
	if l.help {
		help = m.helpLines()
	}
	lines = append(lines, footer(gaps, status, help)...)
	cutLines(lines, m.width)
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = m.altScreen
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// helpLines renders the key help, wrapped to the width so that no key is
// cut off: in filter mode the filter keys; otherwise the compact help
// (toggle, a/n, the Short actions, ? more, quit), or with the full help
// the movement keys (g/G for both ends) and quit, then every action and
// ? less.
func (m Overview) helpLines() []string {
	if m.filtering {
		return wrapHelp(m.help, m.width, []key.Binding{m.keys.ApplyFilter, m.keys.ClearFilter})
	}
	binding := func(a OverviewAction) key.Binding {
		return key.NewBinding(key.WithKeys(a.Key), key.WithHelp(a.Key, a.Help))
	}
	if !m.fullHelp {
		// a and n share one entry, to fit 80 columns.
		allNone := key.NewBinding(key.WithKeys("a", "n"), key.WithHelp("a/n", "all/none"))
		short := []key.Binding{m.keys.Toggle, allNone}
		for _, a := range m.actions {
			if a.Short {
				short = append(short, binding(a))
			}
		}
		quit := key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit"))
		return wrapHelp(m.help, m.width, append(short, m.keys.More, quit))
	}
	lines := wrapHelp(m.help, m.width, []key.Binding{m.keys.Up, m.keys.Down, m.keys.Home, m.keys.Toggle, m.keys.All, m.keys.None, m.keys.Filter, m.keys.Cancel})
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
	var last func(string) string
	switch {
	case r.Alert:
		last = func(s string) string { return st.Failed.Render(s) }
	case r.Level != LevelNone:
		style := st.Level(r.Level)
		last = func(s string) string { return style.Render(s) }
	}
	cs := r.Cells
	var links []Link
	if m.hyperlinks {
		links = r.Links
	}
	faint := func(s string) string { return st.Detail.Render(s) }
	if i == m.cursor {
		// The reset that ends the note would also end the style of the
		// current row for the cells after it: open it again.
		open, _, _ := strings.Cut(st.CurrentRow.Render("x"), "x")
		faint = func(s string) string { return st.Detail.Render(s) + open }
	}
	var first func(string) string
	if prev := m.rows[m.visible[max(i-1, 0)]].Cells; m.grouped && i > m.offset && len(cs) > 0 && len(prev) > 0 && prev[0] == cs[0] {
		first = faint
	}
	text := cells(cs, widths, first, last, links, r.Note, faint)
	if i == m.cursor {
		text = st.CurrentRow.Render(text)
	}
	return cursor + box + " " + text
}

// selectedPart returns the first part of the status line, always shown:
// the number of rows selected.
func (m Overview) selectedPart() string {
	n := 0
	for _, r := range m.rows {
		if m.selected[r.Key] {
			n++
		}
	}
	return fmt.Sprintf("%d/%d selected", n, len(m.rows))
}

// messageWraps reports whether the message displayed (see shownMessage)
// does not fit on the status line after its first part (see statusLines).
func (m Overview) messageWraps() bool {
	msg := m.shownMessage()
	return m.width > 0 && msg != "" && lipgloss.Width(joinStatus(m.selectedPart(), msg)) > m.width
}

// statusLines renders the status line: its parts (see status), each shown
// whole or dropped (see fitStatus), then the message (see shownMessage),
// unstyled. The message wins over the parts after the first (B62 M6): they
// are dropped, the last first, until the message fits. When it does not
// fit after the first part, it wraps (at spaces, see wrap) to a second
// line if two is set, after the parts that leave it whole on the two
// lines, its first word going to that line when it is not wider than the
// window; the lines are then cut at the width (ending with "…", see
// cutLines). Without the second line, the message is cut on the
// first one, and dropped with its separator when not even its first
// character fits with the ellipsis.
func (m Overview) statusLines(two bool) []string {
	st := m.styles
	parts, msg := m.status(), m.shownMessage()
	if msg == "" {
		return []string{st.Status.Render(joinStatus(parts[:fitStatus(m.width, parts)]...))}
	}
	for n := len(parts); n >= 1; n-- {
		if m.width <= 0 || lipgloss.Width(joinStatus(append(parts[:n:n], msg)...)) <= m.width {
			return []string{joinStatus(st.Status.Render(joinStatus(parts[:n]...)), msg)}
		}
	}
	if !two {
		room := m.width - lipgloss.Width(parts[0]+" · ")
		if room >= min(2, lipgloss.Width(msg)) {
			return []string{joinStatus(st.Status.Render(parts[0]), msg)}
		}
		return []string{st.Status.Render(parts[0])}
	}
	// The parts that fit before the message stay on the first line (the
	// last given up first) as long as the message stays whole on the two
	// lines (V3 review: "rows a-b of n" vanished on problem rows).
	for n := len(parts); ; n-- {
		lines := m.wrapMessage(st.Status.Render(joinStatus(parts[:n]...)), lipgloss.Width(joinStatus(parts[:n]...)), msg)
		if n == 1 || lipgloss.Width(lines[len(lines)-1]) <= m.width && lipgloss.Width(joinStatus(parts[:n]...)+" · ") < m.width {
			return lines
		}
	}
}

// wrapMessage returns the status line made of prefix (prefixWidth columns
// wide) and msg, wrapped to a second line (see statusLines).
func (m Overview) wrapMessage(prefix string, prefixWidth int, msg string) []string {
	room := m.width - prefixWidth - lipgloss.Width(" · ")
	word, _, _ := strings.Cut(msg, " ")
	if room < 1 || lipgloss.Width(word) > room && lipgloss.Width(word) <= m.width {
		return []string{prefix, msg}
	}
	head := wrap(msg, room)[0]
	return []string{joinStatus(prefix, head), strings.TrimLeft(msg[len(head):], " ")}
}

// status returns the parts of the status line, without the message.
func (m Overview) status() []string {
	parts := []string{m.selectedPart()}
	if m.filter != "" {
		parts = append(parts, fmt.Sprintf("%d shown", len(m.visible)))
	}
	if h := m.layout().rows; len(m.visible) > h {
		parts = append(parts, fmt.Sprintf("rows %d-%d of %d", m.offset+1, m.offset+h, len(m.visible)))
	}
	return parts
}
