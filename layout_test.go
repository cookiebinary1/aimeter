package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// visualWidth counts printable cells, skipping the ANSI sequences lipgloss
// emits, so the measurement matches what a terminal actually shows.
func visualWidth(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		default:
			n++
		}
	}
	return n
}

func widePanel() Panel {
	reset := time.Now().Add(2 * time.Hour)
	return Panel{Name: "Z.AI", Note: "GLM Coding Lite", Items: []Gauge{
		{Label: "5h", Used: 74, Detail: "1482 / 2000 credits", Reset: &reset, Window: 5 * time.Hour},
		{Label: "credits", Used: -1, Detail: "5500 credits left"},
	}}
}

// terse has a much shorter caption than widePanel, so a frame containing both
// exposes whether bar widths are decided per row or once per frame.
func tersePanel() Panel {
	reset := time.Now().Add(90 * time.Minute)
	return Panel{Name: "Anthropic", Items: []Gauge{
		{Label: "5h", Used: 20, Reset: &reset, Window: 5 * time.Hour},
	}}
}

// renderOne renders a single panel at frame width w, as View would.
func renderOne(p Panel, w int) string {
	return renderPanel(p, computeLayout([]Panel{p}, w))
}

// barRuns returns the length of every usage/elapsed bar in a rendered frame.
func barRuns(s string) []int {
	var runs []int
	for _, line := range strings.Split(s, "\n") {
		n := 0
		for _, r := range line {
			switch r {
			case '█', '░', '▀':
				n++
			}
		}
		if n > 0 {
			runs = append(runs, n)
		}
	}
	return runs
}

// A panel must fit its terminal without overflowing AND without wrapping:
// narrow windows drop the grey captions instead of pushing them to new lines.
// renderPanel returns unboxed content: title + (bar row + elapsed row) +
// balance row.
func TestRenderPanelNeverOverflowsOrWraps(t *testing.T) {
	const wantLines = 4
	for _, w := range []int{24, 30, 40, 50, 60, 80, 100} {
		lines := strings.Split(renderOne(widePanel(), w), "\n")
		if len(lines) != wantLines {
			t.Errorf("width %d: %d lines, want %d (content wrapped):\n%s",
				w, len(lines), wantLines, renderOne(widePanel(), w))
		}
		for _, line := range lines {
			if got := visualWidth(line); got > w {
				t.Errorf("width %d: line renders %d cells (+%d): %q", w, got, got-w, line)
			}
		}
	}
}

// The whole frame must fit with the last column left free: a line exactly as
// wide as the terminal wraps, and the wrapped tail survives a resize as a
// duplicated row.
func TestViewLeavesLastColumnFree(t *testing.T) {
	for _, w := range []int{30, 40, 46, 50, 60, 80, 100, 120} {
		m := initialModel(Creds{})
		m.width, m.panels, m.loading = w, []Panel{widePanel()}, false
		m.last, m.now = time.Now().Add(-30*time.Second), time.Now()
		for _, line := range strings.Split(m.View(), "\n") {
			if got := visualWidth(line); got > w-1 {
				t.Errorf("width %d: line renders %d cells (max %d): %q", w, got, w-1, line)
			}
		}
	}
}

// Wide terminals must keep the detail and reset captions.
func TestWideKeepsCaptions(t *testing.T) {
	out := renderOne(widePanel(), 100)
	for _, want := range []string{"1482 / 2000 credits", "resets in", "elapsed"} {
		if !strings.Contains(out, want) {
			t.Errorf("wide render lost %q", want)
		}
	}
}

// Narrow terminals drop those captions but keep the bar and the percentage.
func TestNarrowDropsCaptions(t *testing.T) {
	out := renderOne(widePanel(), 34)
	for _, unwanted := range []string{"1482 / 2000 credits", "resets in"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("narrow render kept %q", unwanted)
		}
	}
	if !strings.Contains(out, "74%") || !strings.Contains(out, "█") {
		t.Errorf("narrow render lost the bar or percentage:\n%s", out)
	}
}

// Every bar in a frame must share one width, so the bars, percentages and
// captions line up vertically even when one panel's caption is much longer
// than another's. A verbose row must not shrink the bars of quiet rows.
func TestBarsShareOneWidthAcrossFrame(t *testing.T) {
	panels := []Panel{widePanel(), tersePanel()}
	for _, w := range []int{60, 80, 100, 140} {
		m := initialModel(Creds{})
		m.width, m.panels, m.loading = w, panels, false
		m.last, m.now = time.Now().Add(-30*time.Second), time.Now()

		runs := barRuns(m.View())
		if len(runs) < 4 {
			t.Fatalf("width %d: expected at least 4 bars, got %d", w, len(runs))
		}
		for i, n := range runs {
			if n != runs[0] {
				t.Errorf("width %d: bar %d is %d cells, first is %d — bars are ragged",
					w, i, n, runs[0])
			}
		}
		if runs[0] < minBar {
			t.Errorf("width %d: bar shrank to %d, below the %d minimum", w, runs[0], minBar)
		}
	}
}

// A long caption may not squeeze the bar below the preferred width while
// there is still room; captions get truncated into their column instead.
func TestCaptionsNeverSqueezeBarBelowPreferred(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour)
	verbose := Panel{Name: "Verbose", Items: []Gauge{{
		Label: "5h", Used: 50, Reset: &reset, Window: 5 * time.Hour,
		Detail: "a very long caption that would happily eat the whole row if allowed",
	}}}
	lay := computeLayout([]Panel{verbose}, 100)
	if lay.bar < preferredBar {
		t.Errorf("bar is %d cells, want at least the preferred %d", lay.bar, preferredBar)
	}
}

// A wide terminal must spend its surplus on the bars: captions take only the
// cells they need, the rest grows every bar, so the frame never shows a wide
// empty gutter beside stubby bars.
func TestWideFrameGrowsBars(t *testing.T) {
	panels := []Panel{widePanel(), tersePanel()}
	prev := 0
	for _, w := range []int{100, 140, 180} {
		lay := computeLayout(panels, w)
		if lay.bar <= prev {
			t.Errorf("width %d: bar %d did not grow past %d", w, lay.bar, prev)
		}
		prev = lay.bar
	}
}

// While the frame has room, a caption is shown whole — an off-by-one in the
// caption column used to clip the last character of the longest caption.
func TestLongestCaptionRendersWhole(t *testing.T) {
	// No Reset: the caption is then a fixed string. A relative "resets in"
	// caption is recomputed when the expectation is built, so it flips a
	// minute on a slow runner and the test fails for the wrong reason.
	caption := "4806 / 10000 credits  resets in 44h 58m"
	p := Panel{Name: "Z.AI", Items: []Gauge{{Label: "7d", Used: 48, Detail: caption}}}

	out := renderOne(p, 120)
	if !strings.Contains(out, caption) {
		t.Errorf("caption %q was clipped:\n%s", caption, out)
	}
}

// A window shorter than the panels must scroll: the footer stays visible on
// the last row and an oversized scroll offset clamps to the real content.
func TestViewScrollClampsToContent(t *testing.T) {
	m := initialModel(Creds{})
	for i := 0; i < 24; i++ {
		m.panels = append(m.panels, Panel{
			Name:    fmt.Sprintf("Panel-%02d", i),
			Updated: time.Now(),
			Items:   []Gauge{{Label: "5h", Used: 10}},
		})
	}
	m.width = 100
	m.height = 20
	m.last = time.Now()
	m.scroll = 1000

	out := m.View()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > m.height {
		t.Fatalf("rendered %d lines into a %d-line window", len(lines), m.height)
	}
	if !strings.Contains(lines[len(lines)-1], "q quit") {
		t.Errorf("last line = %q, want the pinned footer", lines[len(lines)-1])
	}
	// The clamped view must reach content the un-scrolled view cannot show.
	m.scroll = 0
	if top := m.View(); strings.Contains(top, "Panel-23") {
		t.Error("un-scrolled view already shows the last panel; nothing was clipped")
	}
	if !strings.Contains(out, "Panel-23") {
		t.Error("scrolled view does not reach the last panel")
	}
}

// The elapsed caption should carry the elapsed time when the caption column
// is wide enough, degrade to the bare label, then disappear.
func TestElapsedCaptionIncludesDuration(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour) // 3h elapsed of a 5h window
	g := Gauge{Label: "5h", Used: 60, Reset: &reset, Window: 5 * time.Hour}

	if cap := elapsedCaption(g, 30); !strings.HasPrefix(cap, "elapsed ") || !strings.Contains(cap, "3h") {
		t.Errorf("wide caption = %q, want the elapsed duration", cap)
	}
	if cap := elapsedCaption(g, 9); cap != "elapsed" {
		t.Errorf("medium caption = %q, want bare label", cap)
	}
	if cap := elapsedCaption(g, 8); cap != "" {
		t.Errorf("narrow caption = %q, want empty", cap)
	}

	// A reset further out than the window (period not started) clamps to 0m.
	future := time.Now().Add(30 * time.Hour)
	g.Reset = &future
	if cap := elapsedCaption(g, 30); !strings.Contains(cap, "0m") {
		t.Errorf("future-reset caption = %q, want elapsed 0m", cap)
	}
}
