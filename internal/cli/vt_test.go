package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// vtScreen is a minimal terminal emulator for tests: it applies the
// output of a program (text, line feeds, cursor movements, erasures) to a
// screen of rows×cols cells, and keeps the lines scrolled off the top in
// a scrollback, so that a test can check what a user sees in the terminal
// and above it after a session. Attributes (colors, bold) are ignored.
// Sequences it does not implement are counted in unknown, so that a test
// can tell whether its emulation is complete enough.
type vtScreen struct {
	rows, cols int
	// newline makes a line feed also return the carriage, as a terminal
	// with output post-processing (onlcr) does: set it for the output of a
	// program writing to something else than a terminal, which then uses
	// bare line feeds as newlines.
	newline bool
	grid    [][]rune
	x, y    int
	wrap    bool // the cursor is past the last column (deferred wrap)
	sx, sy  int  // saved cursor
	// top and bottom are the rows of the scroll region (DECSTBM): line
	// feeds on the bottom row and scrolls move its rows only.
	top, bottom int

	scrollback []string
	unknown    map[string]int

	alt   bool     // the alternate screen is active (grid)
	other [][]rune // the inactive screen
}

// altActive reports whether the alternate screen is active.
func (s *vtScreen) altActive() bool { return s.alt }

func newVTScreen(rows, cols int) *vtScreen {
	s := &vtScreen{rows: rows, cols: cols, bottom: rows - 1, unknown: map[string]int{}}
	for range rows {
		s.grid = append(s.grid, s.blank())
	}
	return s
}

func (s *vtScreen) blank() []rune {
	l := make([]rune, s.cols)
	for i := range l {
		l[i] = ' '
	}
	return l
}

// write applies out to the screen.
func (s *vtScreen) write(out string) {
	p := ansi.NewParser()
	var state byte
	for len(out) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(out, state, p)
		state = newState
		out = out[n:]
		switch {
		case width > 0:
			s.print(seq, width)
		case len(seq) == 1 && seq[0] < 0x20:
			s.control(seq[0])
		case strings.HasPrefix(seq, "\x1b["):
			s.csi(seq, ansi.Cmd(p.Command()), p.Params())
		case strings.HasPrefix(seq, "\x1b]"), strings.HasPrefix(seq, "\x1bP"), strings.HasPrefix(seq, "\x1b_"):
			// OSC, DCS, APC: no effect on the screen.
		case len(seq) == 2 && seq[0] == 0x1b:
			s.esc(seq[1])
		default:
			s.unknown[seq]++
		}
	}
}

func (s *vtScreen) print(g string, width int) {
	if s.wrap {
		s.x, s.wrap = 0, false
		s.lineFeed()
	}
	r := []rune(g)[0]
	s.grid[s.y][s.x] = r
	for i := 1; i < width && s.x+i < s.cols; i++ {
		s.grid[s.y][s.x+i] = 0 // continuation of a wide character
	}
	s.x += width
	if s.x >= s.cols {
		s.x, s.wrap = s.cols-1, true
	}
}

func (s *vtScreen) lineFeed() {
	switch {
	case s.y == s.bottom:
		s.scrollOff()
	case s.y < s.rows-1:
		s.y++
	}
}

// scrollOff scrolls the scroll region up by one line. When the region is
// the whole screen, its top line goes to the scrollback, unless the
// alternate screen is active.
func (s *vtScreen) scrollOff() {
	if s.top == 0 && s.bottom == s.rows-1 && !s.alt {
		s.scrollback = append(s.scrollback, lineText(s.grid[0]))
	}
	copy(s.grid[s.top:s.bottom], s.grid[s.top+1:s.bottom+1])
	s.grid[s.bottom] = s.blank()
}

// scrollDown scrolls the scroll region down by one line.
func (s *vtScreen) scrollDown() {
	copy(s.grid[s.top+1:s.bottom+1], s.grid[s.top:s.bottom])
	s.grid[s.top] = s.blank()
}

func (s *vtScreen) control(b byte) {
	switch b {
	case '\r':
		s.x, s.wrap = 0, false
	case '\n', '\v', '\f':
		s.wrap = false
		if s.newline {
			s.x = 0
		}
		s.lineFeed()
	case '\b':
		s.wrap = false
		s.x = max(0, s.x-1)
	case '\t':
		s.x = min(s.cols-1, (s.x/8+1)*8)
	case '\a':
	default:
		s.unknown[string(b)]++
	}
}

func (s *vtScreen) esc(final byte) {
	switch final {
	case '7':
		s.sx, s.sy = s.x, s.y
	case '8':
		s.x, s.y, s.wrap = s.sx, s.sy, false
	case 'M': // reverse index
		switch {
		case s.y == s.top:
			s.scrollDown()
		case s.y > 0:
			s.y--
		}
	case 'D':
		s.lineFeed()
	case 'E':
		s.x = 0
		s.lineFeed()
	case '=', '>', '\\':
	default:
		s.unknown["\x1b"+string(final)]++
	}
}

func (s *vtScreen) csi(seq string, cmd ansi.Cmd, params ansi.Params) {
	n := func(i, def int) int {
		v, _, _ := params.Param(i, def)
		if v == 0 && def > 0 {
			return def
		}
		return v
	}
	if cmd.Prefix() == '?' && cmd.Intermediate() == 0 && (cmd.Final() == 'h' || cmd.Final() == 'l') {
		params.ForEach(0, func(_, mode int, _ bool) { s.altScreen(mode, cmd.Final() == 'h') })
		return
	}
	if cmd.Prefix() != 0 || cmd.Intermediate() != 0 {
		switch cmd.Final() {
		case 'h', 'l', 'u', 'p', 'm', 'q', 'c', 'n', 'W': // modes, keyboard, queries, tab stops
			return
		}
		s.unknown[seq]++
		return
	}
	clear := func(row []rune, from, to int) {
		for i := max(0, from); i < min(s.cols, to); i++ {
			row[i] = ' '
		}
	}
	s.wrap = false
	switch cmd.Final() {
	case 'A':
		s.y = max(0, s.y-n(0, 1))
	case 'B':
		s.y = min(s.rows-1, s.y+n(0, 1))
	case 'C':
		s.x = min(s.cols-1, s.x+n(0, 1))
	case 'D':
		s.x = max(0, s.x-n(0, 1))
	case 'E':
		s.x, s.y = 0, min(s.rows-1, s.y+n(0, 1))
	case 'F':
		s.x, s.y = 0, max(0, s.y-n(0, 1))
	case 'G', '`':
		s.x = min(s.cols-1, n(0, 1)-1)
	case 'd':
		s.y = min(s.rows-1, n(0, 1)-1)
	case 'H', 'f':
		s.y, s.x = min(s.rows-1, n(0, 1)-1), min(s.cols-1, n(1, 1)-1)
	case 'J':
		switch n(0, 0) {
		case 0:
			clear(s.grid[s.y], s.x, s.cols)
			for y := s.y + 1; y < s.rows; y++ {
				s.grid[y] = s.blank()
			}
		case 1:
			clear(s.grid[s.y], 0, s.x+1)
			for y := 0; y < s.y; y++ {
				s.grid[y] = s.blank()
			}
		case 2, 3:
			for y := range s.grid {
				s.grid[y] = s.blank()
			}
		}
	case 'K':
		switch n(0, 0) {
		case 0:
			clear(s.grid[s.y], s.x, s.cols)
		case 1:
			clear(s.grid[s.y], 0, s.x+1)
		case 2:
			s.grid[s.y] = s.blank()
		}
	case 'X':
		clear(s.grid[s.y], s.x, s.x+n(0, 1))
	case 'P':
		row, k := s.grid[s.y], n(0, 1)
		copy(row[s.x:], row[min(s.cols, s.x+k):])
		clear(row, s.cols-k, s.cols)
	case '@':
		row, k := s.grid[s.y], n(0, 1)
		copy(row[min(s.cols, s.x+k):], row[s.x:])
		clear(row, s.x, s.x+k)
	case 'L', 'M': // insert or delete lines: from the cursor row to the bottom of the region
		if s.y < s.top || s.y > s.bottom {
			break
		}
		top := s.top
		s.top = s.y
		for range n(0, 1) {
			if cmd.Final() == 'L' {
				s.scrollDown()
			} else {
				copy(s.grid[s.top:s.bottom], s.grid[s.top+1:s.bottom+1])
				s.grid[s.bottom] = s.blank()
			}
		}
		s.top = top
		s.x = 0
	case 'S':
		for range n(0, 1) {
			s.scrollOff()
		}
	case 'T':
		for range n(0, 1) {
			s.scrollDown()
		}
	case 'r':
		if top, bottom := n(0, 1)-1, min(s.rows, n(1, s.rows))-1; top < bottom {
			s.top, s.bottom = top, bottom
			s.x, s.y = 0, 0
		}
	case 'm', 'h', 'l', 'n', 'c', 't', 'q':
		// attributes, modes, reports
	default:
		s.unknown[seq]++
	}
}

// altScreen applies DECSET (set) or DECRST of mode when it switches
// screens, as xterm does: 47 switches; 1047 also clears the alternate
// screen when leaving it; 1049 saves the cursor and clears the alternate
// screen when entering it, and restores the cursor when leaving it. Other
// private modes have no effect on the screen.
func (s *vtScreen) altScreen(mode int, set bool) {
	if mode != 47 && mode != 1047 && mode != 1049 {
		return
	}
	if mode == 1049 && set {
		s.esc('7')
	}
	if mode == 1047 && !set && s.alt {
		for y := range s.grid {
			s.grid[y] = s.blank()
		}
	}
	if set != s.alt {
		if s.other == nil {
			for range s.rows {
				s.other = append(s.other, s.blank())
			}
		}
		s.grid, s.other, s.alt = s.other, s.grid, set
		s.wrap = false
	}
	if mode == 1049 && set {
		for y := range s.grid {
			s.grid[y] = s.blank()
		}
	}
	if mode == 1049 && !set {
		s.esc('8')
	}
}

func lineText(row []rune) string {
	var b strings.Builder
	for _, r := range row {
		if r != 0 {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// lines returns the scrollback followed by the normal screen (see
// normalLines), then the alternate screen while it is active (see
// altLines): the screen the user sees then, the normal one coming back
// when the program leaves it.
func (s *vtScreen) lines() []string { return append(s.normalLines(), s.altLines()...) }

// normalLines returns the scrollback followed by the normal screen,
// trailing empty lines removed, whichever screen is active.
func (s *vtScreen) normalLines() []string {
	normal := s.grid
	if s.alt {
		normal = s.other
	}
	return trimmedLines(append([]string(nil), s.scrollback...), normal)
}

// altLines returns the alternate screen while it is active, trailing empty
// lines removed, and nil otherwise.
func (s *vtScreen) altLines() []string {
	if !s.alt {
		return nil
	}
	return trimmedLines(nil, s.grid)
}

// trimmedLines appends the rows of grid to out, and removes the trailing
// empty lines.
func trimmedLines(out []string, grid [][]rune) []string {
	for _, row := range grid {
		out = append(out, lineText(row))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// The emulator itself: erasures, cursor movements, deferred wrap, line
// feeds with and without carriage return, and the scrollback.
func TestVTScreen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		newline   bool
		want      []string
	}{
		{"erase line right", "abcd\r\x1b[2C\x1b[K", false, []string{"ab"}},
		{"erase line left", "abcd\x1b[2D\x1b[1K", false, []string{"  cd"}},
		{"erase whole line", "abcd\x1b[2K", false, nil},
		{"erase below", "a\r\nb\r\nc\x1b[A\r\x1b[J", false, []string{"a"}},
		{"erase above", "a\r\nb\r\ncde\x1b[2D\x1b[1J", false, []string{"", "", "  e"}},
		{"cursor moves", "ab\x1b[2;3Hx\x1b[Ay\x1b[1Gz", false, []string{"zb y", "  x"}},
		{"deferred wrap", "abcd", false, []string{"abcd"}},
		{"wrap", "abcde", false, []string{"abcd", "e"}},
		{"bare line feed", "ab\ncd", false, []string{"ab", "  cd"}},
		{"newline mode", "ab\ncd", true, []string{"ab", "cd"}},
		{"scrollback", "1\r\n2\r\n3\r\n4", false, []string{"1", "2", "3", "4"}},
	} {
		vt := newVTScreen(3, 4)
		vt.newline = tc.newline
		vt.write(tc.out)
		if got := vt.lines(); strings.Join(got, "|") != strings.Join(tc.want, "|") || len(vt.unknown) > 0 {
			t.Errorf("%s: lines %q (unknown %v), want %q", tc.name, got, vt.unknown, tc.want)
		}
	}
	vt := newVTScreen(3, 4)
	vt.write("1\r\n2\r\n3\r\n4")
	if strings.Join(vt.scrollback, "|") != "1" {
		t.Errorf("scrollback %q, want [1]", vt.scrollback)
	}
}

// A scroll region (DECSTBM, which homes the cursor): line feeds on its
// bottom row and CSI S/T scroll its rows only, without scrollback.
func TestVTScreenScrollRegion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"line feed on the bottom row", "1\r\n2\r\n3\r\n4\x1b[2;3rx\x1b[3;1H\n", []string{"x", "3", "", "4"}},
		{"scroll up", "1\r\n2\r\n3\r\n4\x1b[2;3r\x1b[S", []string{"1", "3", "", "4"}},
		{"scroll down", "1\r\n2\r\n3\r\n4\x1b[2;3r\x1b[T", []string{"1", "", "2", "4"}},
		{"reset", "1\r\n2\r\n3\r\n4\x1b[2;3r\x1b[r\x1b[4;1H\n5", []string{"2", "3", "4", "5"}},
	} {
		vt := newVTScreen(4, 4)
		vt.write(tc.out)
		if got := vt.grid; strings.Join(trimmedLines(nil, got), "|") != strings.Join(tc.want, "|") || len(vt.unknown) > 0 {
			t.Errorf("%s: screen %q (unknown %v), want %q", tc.name, trimmedLines(nil, got), vt.unknown, tc.want)
		}
		if tc.name != "reset" && len(vt.scrollback) > 0 {
			t.Errorf("%s: scrollback %q, want none", tc.name, vt.scrollback)
		}
	}
}

// The alternate screen (DECSET/DECRST 1049, 1047 and 47, as xterm
// implements them): while it is active, lines ends with it; leaving it
// brings back the normal screen as it was (and the cursor for 1049), and
// what scrolls off the alternate screen does not go to the scrollback.
func TestVTScreenAlternateScreen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		alt       bool
		want      []string
	}{
		{"1049 enters a clear screen", "ab\r\ncd\x1b[?1049h\x1b[Hx", true, []string{"ab", "cd", "x"}},
		{"1049 restores screen and cursor", "ab\r\ncd\x1b[?1049h\x1b[2J\x1b[Hxyz\r\n1\r\n2\r\n3\r\n4\x1b[?1049lE", false, []string{"ab", "cdE"}},
		{"1049 clears again on re-entry", "ab\x1b[?1049h\x1b[Hxy\x1b[?1049l\x1b[?1049h", true, []string{"ab"}},
		{"1047 clears on leaving", "ab\x1b[?1047hx\x1b[?1047l\x1b[?1047h", true, []string{"ab"}},
		{"47 keeps the alternate screen", "ab\x1b[?47hx\x1b[?47l\x1b[?47h", true, []string{"ab", "  x"}},
	} {
		vt := newVTScreen(3, 4)
		vt.write(tc.out)
		if got := vt.lines(); strings.Join(got, "|") != strings.Join(tc.want, "|") || vt.altActive() != tc.alt || len(vt.unknown) > 0 {
			t.Errorf("%s: lines %q, alternate screen %v (unknown %v), want %q, %v", tc.name, got, vt.altActive(), vt.unknown, tc.want, tc.alt)
		}
		if len(vt.scrollback) > 0 {
			t.Errorf("%s: scrollback %q, want none", tc.name, vt.scrollback)
		}
	}
}

// Reverse index (ESC M) and line insertion and deletion (CSI L, CSI M)
// honour the scroll region, as in xterm: ESC M on the top row of the
// region scrolls it down; CSI L and CSI M move the rows from the cursor
// to the bottom of the region only, and do nothing outside the region.
func TestVTScreenScrollRegionEditing(t *testing.T) {
	t.Parallel()
	const fill = "1\r\n2\r\n3\r\n4\r\n5"
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"reverse index on the top row", fill + "\x1b[2;4r\x1b[2;1H\x1bM", []string{"1", "", "2", "3", "5"}},
		{"reverse index inside", fill + "\x1b[2;4r\x1b[3;1H\x1bMx", []string{"1", "x", "3", "4", "5"}},
		{"reverse index without region", fill + "\x1b[1;1H\x1bM", []string{"", "1", "2", "3", "4"}},
		{"insert line", fill + "\x1b[2;4r\x1b[3;1H\x1b[L", []string{"1", "2", "", "3", "5"}},
		{"delete line", fill + "\x1b[2;4r\x1b[2;1H\x1b[M", []string{"1", "3", "4", "", "5"}},
		{"insert line outside", fill + "\x1b[2;4r\x1b[5;1H\x1b[L", []string{"1", "2", "3", "4", "5"}},
		{"delete line outside", fill + "\x1b[2;4r\x1b[1;1H\x1b[M", []string{"1", "2", "3", "4", "5"}},
	} {
		vt := newVTScreen(5, 4)
		vt.write(tc.out)
		got := make([]string, len(vt.grid))
		for i, row := range vt.grid {
			got[i] = lineText(row)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(vt.unknown) > 0 {
			t.Errorf("%s: screen %q (unknown %v), want %q", tc.name, got, vt.unknown, tc.want)
		}
	}
}
