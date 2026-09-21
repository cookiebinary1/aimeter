package main

import (
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
// Expected line count: top border + title + (bar row + elapsed row) + balance
// row + bottom border.
func TestRenderPanelNeverOverflowsOrWraps(t *testing.T) {
	const wantLines = 6
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
	reset := time.Now().Add(44*time.Hour + 59*time.Minute)
	p := Panel{Name: "Z.AI", Items: []Gauge{{
		Label: "7d", Used: 48, Detail: "4806 / 10000 credits", Reset: &reset, Window: 7 * 24 * time.Hour,
	}}}
	out := renderOne(p, 120)
	if want := captionText(p.Items[0]); !strings.Contains(out, want) {
		t.Errorf("caption %q was clipped:\n%s", want, out)
	}
}
