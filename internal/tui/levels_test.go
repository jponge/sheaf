package tui

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The SGR sequences of the level styles (basic ANSI foregrounds, faint).
const (
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrRed    = "\x1b[31m"
	sgrFaint  = "\x1b[2m"
)

// colorSGR matches an SGR sequence setting a foreground color.
var colorSGR = regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?(?:3[0-9]|9[0-7])(?:;[0-9;]*)?m")

func TestStylesLevel(t *testing.T) {
	t.Parallel()
	st := DefaultStyles()
	for _, tc := range []struct {
		l    Level
		want string
	}{{LevelOK, sgrGreen}, {LevelInfo, sgrFaint}, {LevelWarn, sgrYellow}, {LevelFail, sgrRed}} {
		if got := st.Level(tc.l).Render("x"); !strings.HasPrefix(got, tc.want) {
			t.Errorf("level %d renders %q, want it to start with %q", tc.l, got, tc.want)
		}
	}
	if got := st.Level(LevelNone).Render("x"); got != "x" {
		t.Errorf("LevelNone renders %q, want plain", got)
	}
}

// reportLevels marks the level word of doctor-like lines.
func reportLevels(line string) Mark {
	levels := map[string]Level{"OK": LevelOK, "INFO": LevelInfo, "WARN": LevelWarn, "FAIL": LevelFail}
	word, _, _ := strings.Cut(line, " ")
	if l, ok := levels[word]; ok {
		return Mark{Start: 0, End: len(word), Level: l}
	}
	return Mark{}
}

var levelLines = []string{"OK    alpha: fine", "WARN  beta: odd", "      hint: do this", "FAIL  gamma: broken", "INFO  delta: note"}

// The report view displays the part of each line that the classifier
// marks in the style of its level, and nothing else styled; the text is
// unchanged.
func TestReportWithLevelsStylesMarkedParts(t *testing.T) {
	t.Parallel()
	m := NewReport("sheaf doctor: /b", levelLines, "back").WithLevels(reportLevels)
	raw := m.View().Content
	for _, want := range []string{sgrGreen + "OK", sgrYellow + "WARN", sgrRed + "FAIL", sgrFaint + "INFO"} {
		if !strings.Contains(raw, want) {
			t.Errorf("view lacks %q:\n%q", want, raw)
		}
	}
	for _, l := range strings.Split(raw, "\n") {
		if strings.Contains(l, "hint:") && sgr.MatchString(l) {
			t.Errorf("unmarked line styled: %q", l)
		}
		if strings.Contains(l, "alpha") && strings.Contains(l, sgrGreen+"OK    alpha") {
			t.Errorf("style beyond the mark: %q", l)
		}
	}
	for _, l := range levelLines {
		if !hasLine(view(m), l) {
			t.Errorf("text changed: no line %q in\n%s", l, view(m))
		}
	}
	// Without a classifier, no line is styled (the view is as before).
	plain := NewReport("sheaf doctor: /b", levelLines, "back")
	for _, l := range strings.Split(plain.View().Content, "\n") {
		if strings.Contains(l, "alpha") && sgr.MatchString(l) {
			t.Errorf("line styled without a classifier: %q", l)
		}
	}
	// Marks out of bounds are ignored.
	odd := NewReport("t", []string{"OK"}, "back").WithLevels(func(string) Mark { return Mark{Start: 1, End: 9, Level: LevelFail} })
	if !hasLine(view(odd), "OK") {
		t.Errorf("out-of-bounds mark: %q", odd.View().Content)
	}
}

// Styled parts are measured without their escape sequences: wrapping and
// truncation to a narrow width are as for the plain text.
func TestReportWithLevelsWrapsLikePlainText(t *testing.T) {
	t.Parallel()
	for w := 6; w <= 30; w++ {
		var plain tea.Model = NewReport("t", levelLines, "back")
		var styled tea.Model = NewReport("t", levelLines, "back").WithLevels(reportLevels)
		plain, _ = plain.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		styled, _ = styled.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		if view(plain) != view(styled) {
			t.Errorf("width %d: styled view\n%s\nwant\n%s", w, view(styled), view(plain))
		}
		for _, l := range strings.Split(styled.View().Content, "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: line %q too wide", w, l)
			}
		}
	}
}

// The colors reach a terminal that allows them; with NO_COLOR the
// program's color profile drops them (faint is kept, see colorProfile).
func TestReportLevelsFollowTheColorProfile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		profile colorprofile.Profile
		colors  bool
	}{{colorprofile.ANSI, true}, {colorprofile.ASCII, false}, {colorprofile.NoTTY, false}} {
		var out bytes.Buffer
		m := NewReport("t", levelLines, "back").WithLevels(reportLevels)
		p, err := NewProgram(m, strings.NewReader(""), &out, tea.WithColorProfile(tc.profile))
		if err != nil {
			t.Fatal(err)
		}
		go func() { p.Quit() }()
		if _, err := p.Run(); err != nil {
			t.Fatal(err)
		}
		if got := colorSGR.MatchString(out.String()); got != tc.colors {
			t.Errorf("profile %v: colors %v, want %v:\n%q", tc.profile, got, tc.colors, out.String())
		}
	}
}

// The STATE cell of a row is rendered in the style of its level (Alert
// wins), on the current row too, and the table stays aligned and within
// the width.
func TestOverviewRendersRowLevels(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{Key: "1", Cells: []string{"a", "main", "clean"}, Level: LevelOK},
		{Key: "2", Cells: []string{"a", "feat", "dirty, ahead 1"}, Level: LevelWarn},
		{Key: "3", Cells: []string{"bravo", "main", "clean, no upstream"}, Level: LevelInfo},
		{Key: "4", Cells: []string{"c", "main", "clean, behind 2"}, Level: LevelFail},
		{Key: "5", Cells: []string{"d", "x", "problem: gone"}, Level: LevelOK, Alert: true},
		{Key: "6", Cells: []string{"e", "main", "plain"}},
	}
	ov := NewOverview("t", []string{"REPO", "DIR", "STATE"}, rows, ovActions())
	raw := ov.View().Content
	for _, want := range []string{sgrGreen + "clean", sgrYellow + "dirty, ahead 1", sgrFaint + "clean, no upstream", sgrRed + "clean, behind 2", sgrRed + "problem: gone"} {
		if !strings.Contains(raw, want) {
			t.Errorf("view lacks %q:\n%q", want, raw)
		}
	}
	for _, l := range strings.Split(raw, "\n") {
		if strings.Contains(l, "plain") && colorSGR.MatchString(l) {
			t.Errorf("row without a level colored: %q", l)
		}
	}
	for _, want := range []string{
		"> [ ] a      main  clean",
		"  [ ] a      feat  dirty, ahead 1",
		"  [ ] bravo  main  clean, no upstream",
	} {
		if !hasLine(view(ov), want) {
			t.Errorf("view lacks line %q:\n%s", want, view(ov))
		}
	}
	for w := 10; w <= 40; w++ {
		m, _ := tea.Model(ov).Update(tea.WindowSizeMsg{Width: w, Height: 30})
		for _, l := range strings.Split(m.View().Content, "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: line %q too wide", w, l)
			}
		}
	}
}

// Painter styles text like a program rendering to the same output: colors
// on a terminal that allows them, attributes only with NO_COLOR or
// CLICOLOR=0, nothing when the output is not a terminal.
func TestPainter(t *testing.T) {
	t.Parallel()
	tty := []string{"TERM=xterm-256color", "TTY_FORCE=1"}
	var out io.Writer = &bytes.Buffer{}
	for _, tc := range []struct {
		env  []string
		want map[Level]string
	}{
		{tty, map[Level]string{LevelOK: sgrGreen, LevelWarn: sgrYellow, LevelFail: sgrRed, LevelInfo: sgrFaint}},
		{append(tty, "NO_COLOR=1"), map[Level]string{LevelInfo: sgrFaint}},
		{append(tty, "CLICOLOR=0"), map[Level]string{LevelInfo: sgrFaint}},
		{[]string{"TERM=xterm-256color"}, nil},
	} {
		paint := Painter(out, tc.env)
		for _, l := range []Level{LevelNone, LevelOK, LevelInfo, LevelWarn, LevelFail} {
			got := paint(l, "word")
			if want, ok := tc.want[l]; ok {
				if !strings.HasPrefix(got, want) || sgr.ReplaceAllString(got, "") != "word" {
					t.Errorf("%v, level %d: %q, want %q + word", tc.env, l, got, want)
				}
			} else if got != "word" {
				t.Errorf("%v, level %d: %q, want plain", tc.env, l, got)
			}
		}
	}
}
