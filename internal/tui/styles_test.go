package tui

import (
	"reflect"
	"strconv"
	"testing"

	"charm.land/lipgloss/v2"
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
		// lipgloss v2 colors are image/color values; the basic ANSI ones are
		// those lipgloss.Color returns for "0" to "15".
		switch c := s.GetForeground(); c.(type) {
		case lipgloss.NoColor:
		default:
			basic := false
			for n := range 16 {
				basic = basic || c == lipgloss.Color(strconv.Itoa(n))
			}
			if !basic {
				t.Errorf("style #%d foreground %v (%T) is not a basic ANSI color (0-15)", i, c, c)
			}
		}
	}
}
