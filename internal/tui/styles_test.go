package tui

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The default styles must stay readable on light and dark terminals: colors
// are limited to the 16 basic ANSI colors (remapped by the terminal theme) and
// no style sets a background.
func TestDefaultStylesUseOnlyBasicANSIForegrounds(t *testing.T) {
	st := DefaultStyles()
	var styles []lipgloss.Style
	collect := func(v reflect.Value) {
		for i := range v.NumField() {
			if s, ok := v.Field(i).Interface().(lipgloss.Style); ok {
				styles = append(styles, s)
			}
		}
	}
	collect(reflect.ValueOf(st))
	collect(reflect.ValueOf(st.Help))
	if len(styles) < 10 {
		t.Fatalf("collected only %d styles; reflection walk is broken", len(styles))
	}
	for i, s := range styles {
		if _, ok := s.GetBackground().(lipgloss.NoColor); !ok {
			t.Errorf("style #%d sets background %v", i, s.GetBackground())
		}
		switch c := s.GetForeground().(type) {
		case lipgloss.NoColor:
		case lipgloss.Color:
			if n, err := strconv.Atoi(string(c)); err != nil || n < 0 || n > 15 {
				t.Errorf("style #%d foreground %q is not a basic ANSI color (0-15)", i, c)
			}
		default:
			t.Errorf("style #%d foreground has type %T, want lipgloss.Color", i, c)
		}
	}
}
