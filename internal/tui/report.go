package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ReportGrace is the time during which a Report ignores the keys that end
// it (ctrl+c excepted), counted from NewReport.
const ReportGrace = 250 * time.Millisecond

// Report is a bubbletea model displaying a title and lines of text (e.g.
// what a command printed) on the alternate screen until q, esc or enter is
// pressed, or ctrl+c, which ends it as an interruption; other keys are
// ignored. A report longer than the window scrolls (up/down/k/j by a line,
// pgup/pgdown and b/space by a page, home/end and g/G to the top and the
// bottom, the mouse wheel: see WithMouse) and a status line tells
// which lines are shown; lines wider
// than the window are wrapped, their continuation lines indented to their
// text column (see textColumn and WithLevels), by at least 2 and at most
// half the width. With an unknown size every line is shown.
// Keys that end the report are ignored during the first ReportGrace
// (ctrl+c excepted), so that a key typed ahead, e.g. while the work that
// precedes the report ran, does not end it before it can be read; scroll
// keys always work. Pastes, clicks and other messages are ignored. Update returns
// tea.Quit once the report ends; later messages are ignored and the view
// is then empty. The title and lines are displayed with tabs expanded to
// the next multiple of 8 columns and other control characters replaced by
// U+FFFD.
type Report struct {
	title string
	lines []string // as displayed, without the styles of marks
	// marks are the marks of lines (see WithLevels), styled once wrapped,
	// and indents their text columns, to which their continuation lines
	// are indented (see wrapped).
	marks   []Mark
	indents []int
	back    string
	now     func() time.Time
	start   time.Time

	width, height int  // 0 = unknown
	offset        int  // index into wrapped() of the first rendered line
	mouse         bool // see WithMouse

	done, interrupted bool
	keys              reportKeys
	help              help.Model
	styles            Styles
}

// reportKeys are the keys of a Report.
type reportKeys struct {
	Up, Down, PageUp, PageDown, Top, Bottom, Back, ForceQuit key.Binding
}

func newReportKeys() reportKeys {
	return reportKeys{
		Up:        key.NewBinding(key.WithKeys("up", "k")),
		Down:      key.NewBinding(key.WithKeys("down", "j")),
		PageUp:    key.NewBinding(key.WithKeys("pgup", "b")),
		PageDown:  key.NewBinding(key.WithKeys("pgdown", "space")),
		Top:       key.NewBinding(key.WithKeys("home", "g")),
		Bottom:    key.NewBinding(key.WithKeys("end", "G", "shift+g")),
		Back:      key.NewBinding(key.WithKeys("q", "esc", "enter")),
		ForceQuit: key.NewBinding(key.WithKeys("ctrl+c")),
	}
}

// NewReport returns a Report of lines under title; back describes what the
// keys ending it lead to, in the help line (e.g. "back to the overview").
func NewReport(title string, lines []string, back string) Report {
	st := DefaultStyles()
	h := help.New()
	h.Styles = st.Help
	m := Report{title: title, back: back, now: time.Now, keys: newReportKeys(), help: h, styles: st}
	for _, l := range lines {
		l = sanitize(expandTabs(l))
		m.lines = append(m.lines, l)
		m.marks = append(m.marks, Mark{})
		m.indents = append(m.indents, textColumn(l))
	}
	m.start = m.now()
	return m
}

// Mark is the part of a line, from byte Start to End, displayed in the
// style of Level (see Report.WithLevels).
type Mark struct {
	Start, End int
	Level      Level
}

// WithLevels returns a copy of m in which the part of each line that
// classify marks (e.g. the level of a doctor check) is displayed in the
// style of its level. classify receives each line as displayed (tabs
// expanded, control characters replaced); a mark of LevelNone, or out of
// the line's bounds, displays it unstyled. The text column of a marked
// line follows the mark and the spaces after it.
func (m Report) WithLevels(classify func(line string) Mark) Report {
	marks, indents := slices.Clone(m.marks), slices.Clone(m.indents)
	for i, l := range m.lines {
		if k := classify(l); k.Level != LevelNone && 0 <= k.Start && k.Start < k.End && k.End <= len(l) {
			marks[i] = k
			indents[i] = ansi.StringWidth(l[:k.End]) + len(l[k.End:]) - len(strings.TrimLeft(l[k.End:], " "))
		}
	}
	m.marks, m.indents = marks, indents
	return m
}

// styled returns s, a prefix of line i (e.g. its first wrapped line), with
// the part of the mark of the line it contains styled.
func (m Report) styled(i int, s string) string {
	k := m.marks[i]
	if k.Level == LevelNone || k.Start >= len(s) {
		return s
	}
	end := min(k.End, len(s))
	return s[:k.Start] + m.styles.Level(k.Level).Render(s[k.Start:end]) + s[end:]
}

// maxLabelWidth is the width of the widest first word ending with ":" that
// textColumn takes for a label.
const maxLabelWidth = 12

// textColumn returns the column at which the text of line l starts: after
// its indentation and, when its first word is a label (ends with ":", is
// at most maxLabelWidth columns wide and has no "/", e.g. "hint:" or
// "sheaf:", not a path such as "/x/main:") or is followed by at least two
// spaces (a padded column, e.g. "OK    "), after that word and the spaces
// following it.
func textColumn(l string) int {
	rest := strings.TrimLeft(l, " ")
	col := len(l) - len(rest)
	label := func(word string) bool {
		return strings.HasSuffix(word, ":") && ansi.StringWidth(word) <= maxLabelWidth && !strings.Contains(word, "/")
	}
	if word, _, ok := strings.Cut(rest, " "); ok && (label(word) || strings.HasPrefix(rest[len(word):], "  ")) {
		after := strings.TrimLeft(rest[len(word):], " ")
		col += ansi.StringWidth(rest[:len(rest)-len(after)])
	}
	return col
}

// expandTabs replaces the tabs of s by spaces up to the next multiple of 8
// columns.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += ansi.StringWidth(string(r))
	}
	return b.String()
}

// WithMouse returns a copy of m that turns mouse reporting on while it is
// displayed (see tea.MouseModeCellMotion): the wheel scrolls by wheelStep
// lines; clicks are ignored, and neither ends the report.
func (m Report) WithMouse() Report {
	m.mouse = true
	return m
}

// WithClock returns a copy of m using now as its clock; the grace period
// counts from now().
func (m Report) WithClock(now func() time.Time) Report {
	m.now = now
	m.start = now()
	return m
}

// Init implements tea.Model.
func (m Report) Init() tea.Cmd { return nil }

// Done reports whether a key ended the report. A model whose program ended
// for another reason (e.g. input closed) is not done.
func (m Report) Done() bool { return m.done }

// Interrupted reports whether the key was ctrl+c, which callers treat as an
// interruption.
func (m Report) Interrupted() bool { return m.interrupted }

// Update implements tea.Model.
func (m Report) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		m.scroll(0)
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll(-wheelStep)
		case tea.MouseWheelDown:
			m.scroll(wheelStep)
		}
	case tea.KeyPressMsg:
		page := m.layout().rows
		switch {
		case key.Matches(msg, m.keys.ForceQuit):
			m.done, m.interrupted = true, true
			return m, tea.Quit
		case key.Matches(msg, m.keys.Up):
			m.scroll(-1)
		case key.Matches(msg, m.keys.Down):
			m.scroll(1)
		case key.Matches(msg, m.keys.PageUp):
			m.scroll(-page)
		case key.Matches(msg, m.keys.PageDown):
			m.scroll(page)
		case key.Matches(msg, m.keys.Top):
			m.scroll(-len(m.wrapped()))
		case key.Matches(msg, m.keys.Bottom):
			m.scroll(len(m.wrapped()))
		case key.Matches(msg, m.keys.Back) && m.now().Sub(m.start) >= ReportGrace:
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// scroll moves the first rendered line by n, within bounds.
func (m *Report) scroll(n int) {
	m.offset = min(max(m.offset+n, 0), max(len(m.wrapped())-m.layout().rows, 0))
}

// wrapped returns the lines wrapped to the width, if known, their
// continuation lines indented to their text column (see Report): broken at
// spaces only (see wrap), a token being broken only when wider than the
// room; the marks are styled afterwards (see styled).
func (m Report) wrapped() []string {
	var out []string
	for i, l := range m.lines {
		if m.width <= 0 || ansi.StringWidth(l) <= m.width {
			out = append(out, m.styled(i, l))
			continue
		}
		head := wrap(l, m.width)[0]
		out = append(out, m.styled(i, head))
		indent := min(max(m.indents[i], 2), m.width/2)
		for _, c := range wrap(strings.TrimLeft(l[len(head):], " "), m.width-indent) {
			// Spaces at a break (e.g. trailing spaces) are not shown, nor
			// is a line of spaces only.
			if c = strings.TrimLeft(c, " "); c != "" {
				out = append(out, strings.Repeat(" ", indent)+c)
			}
		}
	}
	return out
}

// reportLayout tells which lines View renders around the report lines, and
// how many fit.
type reportLayout struct {
	title, status, help bool
	gaps                int // blank lines below the title and of the footer, see splitGaps
	rows                int
}

// layout fits the view into the window height: the blank lines below the
// title and of the footer are dropped (the one above the help first, then
// the one below the title; without the status line, the one below the
// title first) when that lets every line fit (see fitGaps and splitGaps),
// and the status line is shown when the lines do not fit. When the window
// is too small for at least one line, the blank lines, then the help,
// title and status lines are dropped in that order. An empty report has
// no blank line below the title: it would be next to the one above the
// help. With an unknown height every line is shown.
func (m Report) layout() reportLayout {
	n := len(m.wrapped())
	if m.height <= 0 {
		return reportLayout{title: true, help: true, gaps: min(2, 1+n), rows: max(1, n)}
	}
	l := m.fit(false)
	if n > l.rows {
		l = m.fit(true)
	}
	return l
}

// fit fits the view into the window height, with or without the status
// line (see layout).
func (m Report) fit(status bool) reportLayout {
	l := reportLayout{title: true, status: status, help: true, gaps: min(2, 1+len(m.wrapped()))}
	if status {
		l.gaps = 3
	}
	chrome := func() int {
		c := l.gaps
		for _, shown := range []bool{l.title, l.status} {
			if shown {
				c++
			}
		}
		if l.help {
			c += len(m.helpLines())
		}
		return c
	}
	l.gaps = fitGaps(l.gaps, m.height, chrome()-l.gaps, len(m.wrapped()))
	for _, line := range []*bool{&l.help, &l.title, &l.status} {
		if m.height-chrome() >= 1 {
			break
		}
		*line = false
	}
	l.rows = max(1, m.height-chrome())
	return l
}

// helpLines renders the key help, wrapped to the width so that no key is
// cut off (the one ending the report included).
func (m Report) helpLines() []string {
	return wrapHelp(m.help, m.width, []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "scroll")),
		key.NewBinding(key.WithKeys("space", "b"), key.WithHelp("space/b", "page")),
		key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom")),
		key.NewBinding(key.WithKeys("q", "enter"), key.WithHelp("q/enter", m.back)),
	})
}

// View implements tea.Model.
func (m Report) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	if m.done {
		return v
	}
	st := m.styles
	l := m.layout()
	titleGap, gaps := splitGaps(l.gaps)
	all := m.wrapped()
	var lines []string
	if l.title {
		lines = append(lines, st.Title.Render(cutTitle(sanitize(m.title), m.width)))
		if titleGap {
			lines = append(lines, "")
		}
	}
	end := min(m.offset+l.rows, len(all))
	lines = append(lines, all[m.offset:end]...)
	var status, help []string
	if l.status {
		status = []string{st.Status.Render(fmt.Sprintf("lines %d-%d of %d", m.offset+1, end, len(all)))}
	}
	if l.help {
		help = m.helpLines()
	}
	lines = append(lines, footer(gaps, status, help)...)
	cutLines(lines, m.width)
	v.SetContent(strings.Join(lines, "\n"))
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}
