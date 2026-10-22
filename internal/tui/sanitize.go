package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

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

// cutTitle returns title, the text of a title line, fitted to width (when
// known, > 0) by cutting the start of its path, the text after its last
// ": ", rather than its end: "sheaf: …/bundle/work", so that the bundle
// name stays shown. The path keeps whole components after "…" when its
// last one fits, else as many of its last characters as fit. A title that
// fits, has no path or no room for "…" and one character of it is
// returned unchanged (and cut at the end with the other lines, see
// cutLines).
func cutTitle(title string, width int) string {
	i := strings.LastIndex(title, ": ")
	if width <= 0 || lipgloss.Width(title) <= width || i < 0 {
		return title
	}
	head, path := title[:i+2], title[i+2:]
	room := width - lipgloss.Width(head) - lipgloss.Width("…")
	if room < 1 {
		return title
	}
	// The longest suffix that fits, then the longest one of it starting at
	// a separator.
	start := len(path)
	for j, w := len(path), 0; j > 0; {
		r, size := utf8.DecodeLastRuneInString(path[:j])
		if w += lipgloss.Width(string(r)); w > room {
			break
		}
		j -= size
		start = j
	}
	tail := path[start:]
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
