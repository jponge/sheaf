// Package tui provides reusable bubbletea widgets (multi-select list, confirm
// dialog) and a helper to run a model as a program. It has no knowledge of
// bundles, git or hosting.
package tui

import (
	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
)

// Styles groups every lipgloss style used by the widgets of this package.
//
// The defaults only use text attributes (bold, faint, reverse) and the 16
// basic ANSI colors, which terminals remap to fit their own light or dark
// theme, so they stay readable on both.
type Styles struct {
	Title        lipgloss.Style
	Cursor       lipgloss.Style // the "> " marker of the current row
	CurrentRow   lipgloss.Style // label of the current row
	Checked      lipgloss.Style // "[x]" marker
	Unchecked    lipgloss.Style // "[ ]" marker
	Disabled     lipgloss.Style // whole row of a disabled item
	Detail       lipgloss.Style
	Tag          lipgloss.Style
	Empty        lipgloss.Style // "No items." / "No matches."
	FilterPrompt lipgloss.Style
	Status       lipgloss.Style // "N/M selected" line
	Question     lipgloss.Style
	Failed       lipgloss.Style // failure marker of a Progress row
	Help         help.Styles
}

// DefaultStyles returns the package's default styles.
func DefaultStyles() Styles {
	faint := lipgloss.NewStyle().Faint(true)
	h := help.Styles{
		ShortKey:       lipgloss.NewStyle().Bold(true),
		ShortDesc:      faint,
		ShortSeparator: faint,
		Ellipsis:       faint,
		FullKey:        lipgloss.NewStyle().Bold(true),
		FullDesc:       faint,
		FullSeparator:  faint,
	}
	return Styles{
		Title:        lipgloss.NewStyle().Bold(true),
		Cursor:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		CurrentRow:   lipgloss.NewStyle().Bold(true),
		Checked:      lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		Unchecked:    lipgloss.NewStyle(),
		Disabled:     faint,
		Detail:       faint,
		Tag:          lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		Empty:        faint.Italic(true),
		FilterPrompt: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		Status:       faint,
		Question:     lipgloss.NewStyle().Bold(true),
		Failed:       lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		Help:         h,
	}
}
