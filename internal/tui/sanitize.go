package tui

import (
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// sanitize replaces control characters (C0, DEL, C1, including newlines and
// ESC) with U+FFFD so that caller-provided text cannot inject terminal escape
// sequences or extra lines into a view.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return unicode.ReplacementChar
		}
		return r
	}, s)
}

// dropControl removes control characters, e.g. newlines from pasted filter
// text.
func dropControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// deleteLastWord removes trailing whitespace and the word before it, like
// ctrl+w in a shell.
func deleteLastWord(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	return s[:strings.LastIndexFunc(s, unicode.IsSpace)+1]
}

// wrap splits s into lines at most width cells wide, breaking at spaces
// and, for words longer than width (than the room after the indentation of
// s on the first line), inside words. width <= 0 disables wrapping.
// Trailing spaces of the lines are removed.
func wrap(s string, width int) []string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return []string{s}
	}
	var out []string
	line := ""
	flush := func() {
		out = append(out, strings.TrimRight(line, " "))
		line = ""
	}
	// lead returns the indentation of s while the first line holds nothing
	// else, if the first character of word fits after it: it is kept before
	// word broken there instead of making a blank first line.
	lead := func(word string) string {
		first, _ := splitAtWidth(word, 1)
		if len(out) == 0 && strings.TrimLeft(line, " ") == "" && lipgloss.Width(line+first) <= width {
			return line
		}
		return ""
	}
	for _, word := range strings.SplitAfter(s, " ") {
		for pad := lead(word); lipgloss.Width(pad+strings.TrimRight(word, " ")) > width; pad = lead(word) {
			if line != "" && pad == "" {
				flush()
			}
			head, rest := splitAtWidth(word, width-lipgloss.Width(pad))
			out = append(out, pad+head)
			line, word = "", rest
		}
		if line != "" && lipgloss.Width(line+strings.TrimRight(word, " ")) > width {
			flush()
		}
		line += word
	}
	if strings.TrimSpace(line) != "" || len(out) == 0 {
		flush()
	}
	return out
}

// splitAtWidth returns the longest prefix of s at most width cells wide
// (at least one rune) and the rest.
func splitAtWidth(s string, width int) (string, string) {
	w := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width && i > 0 {
			return s[:i], s[i:]
		}
		w += rw
	}
	return s, ""
}

// cutTitle returns title, the text of a title line ending with path (see
// Overview.WithTitlePath), fitted to width (when known, > 0) by cutting
// the start of path rather than the end of the title: "sheaf:
// …/bundle/work", so that the bundle name stays shown. The path keeps
// whole components after "…" when its last one fits, else as many of its
// last grapheme clusters as fit (widths as the terminal displays them,
// never splitting a cluster, e.g. an emoji and its variation selector). A
// title that fits, does not end with a non-empty path or leaves no room
// for "…" and a column of the path is returned unchanged (and cut at the
// end with the other lines, see cutLines); when no cluster of the path
// fits after "…", the title ends with "…".
func cutTitle(title, path string, width int) string {
	head, ok := strings.CutSuffix(title, path)
	if width <= 0 || lipgloss.Width(title) <= width || path == "" || !ok {
		return title
	}
	room := width - lipgloss.Width(head) - lipgloss.Width("…")
	if room < 1 {
		return title
	}
	// The longest suffix that fits: TruncateLeft drops whole clusters, and
	// keeps a wide one straddling the cut, dropped then.
	tail := path
	for drop := lipgloss.Width(path) - room; tail != "" && lipgloss.Width(tail) > room; drop++ {
		tail = ansi.TruncateLeft(path, drop, "")
	}
	// Then the longest one of it starting at a separator.
	if k := strings.Index(tail, "/"); k >= 0 && k+1 < len(tail) {
		tail = tail[k:]
	}
	return head + "…" + tail
}

// cutLines cuts the lines wider than width (when known, > 0) at the width,
// ending them with "…" so that the cut shows (one column short when the
// cut would follow another "…").
func cutLines(lines []string, width int) {
	if width <= 0 {
		return
	}
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = ansi.Truncate(l, width, "…")
			if strings.HasSuffix(ansi.Strip(lines[i]), "……") && width > 1 {
				// The line is cut right after a cell cut with "…": one
				// ellipsis shows both.
				lines[i] = ansi.Truncate(l, width-1, "…")
			}
		}
	}
}
