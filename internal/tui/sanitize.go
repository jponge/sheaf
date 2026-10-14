package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
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
// and, for words longer than width, inside words. width <= 0 disables
// wrapping. Trailing spaces of the lines are removed.
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
	for _, word := range strings.SplitAfter(s, " ") {
		for lipgloss.Width(strings.TrimRight(word, " ")) > width {
			if line != "" {
				flush()
			}
			head, rest := splitAtWidth(word, width)
			out = append(out, head)
			word = rest
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
