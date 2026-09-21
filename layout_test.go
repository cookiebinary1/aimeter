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

// A panel must fit its terminal without overflowing AND without wrapping:
// narrow windows drop the grey captions instead of pushing them to new lines.
// Expected line count: top border + title + (bar row + elapsed row) + balance
// row + bottom border.
func TestRenderPanelNeverOverflowsOrWraps(t *testing.T) {
	const wantLines = 6
	for _, w := range []int{24, 30, 40, 50, 60, 80, 100} {
		lines := strings.Split(renderPanel(widePanel(), w), "\n")
		if len(lines) != wantLines {
			t.Errorf("width %d: %d lines, want %d (content wrapped):\n%s",
				w, len(lines), wantLines, renderPanel(widePanel(), w))
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
	out := renderPanel(widePanel(), 100)
	for _, want := range []string{"1482 / 2000 credits", "resets in", "elapsed"} {
		if !strings.Contains(out, want) {
			t.Errorf("wide render lost %q", want)
		}
	}
}

// Narrow terminals drop those captions but keep the bar and the percentage.
func TestNarrowDropsCaptions(t *testing.T) {
	out := renderPanel(widePanel(), 34)
	for _, unwanted := range []string{"1482 / 2000 credits", "resets in"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("narrow render kept %q", unwanted)
		}
	}
	if !strings.Contains(out, "74%") || !strings.Contains(out, "█") {
		t.Errorf("narrow render lost the bar or percentage:\n%s", out)
	}
}
