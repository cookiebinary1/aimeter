package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func samplePanels() []Panel {
	reset := time.Now().Add(2 * time.Hour)
	return []Panel{
		{
			Name: "Z.AI", Note: "GLM Coding Lite",
			Items: []Gauge{
				{Label: "5h", Used: 60, Detail: "1207 / 2000 credits", Reset: &reset, Window: 5 * time.Hour},
			},
			Updated: time.Now(),
		},
		{Name: "Meshy", Note: "3D generation", Items: []Gauge{{Label: "credits", Used: -1, Detail: "5500 credits left"}}, Updated: time.Now()},
		{Name: "Broken", Err: errString("missing KEY"), Updated: time.Now()},
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestRenderPlainMarkdownTable(t *testing.T) {
	var b bytes.Buffer
	renderPlain(&b, samplePanels())

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	// header + separator + one row per gauge (2 gauges) + error row
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5:\n%s", len(lines), b.String())
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			t.Errorf("line %d is not a table row: %q", i, line)
		}
		if n := strings.Count(line, "|"); n != len(plainColumns)+1 {
			t.Errorf("line %d has %d pipes, want %d: %q", i, n, len(plainColumns)+1, line)
		}
	}
	for _, h := range plainColumns {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header is missing %q: %q", h, lines[0])
		}
	}
	if !strings.Contains(lines[1], "---") {
		t.Errorf("second line must be the Markdown separator: %q", lines[1])
	}

	cells := splitRow(lines[2])
	if cells[0] != "Z.AI" || cells[1] != "GLM Coding Lite" || cells[2] != "5h" ||
		cells[3] != "60%" || cells[4] != "1207 / 2000 credits" || cells[5] == "" {
		t.Errorf("gauge row: %#v", cells)
	}
	// balance-only gauge leaves Used and Resets empty
	balance := splitRow(lines[3])
	if balance[3] != "" || balance[4] != "5500 credits left" || balance[5] != "" {
		t.Errorf("balance row: %#v", balance)
	}
	// a failed provider reports in the Detail column
	broken := splitRow(lines[4])
	if broken[0] != "Broken" || broken[4] != "ERROR: missing KEY" {
		t.Errorf("error row: %#v", broken)
	}
	if strings.ContainsAny(b.String(), "\x1b") {
		t.Error("plain output must not contain ANSI escapes")
	}
}

// splitRow returns the trimmed cells of a Markdown table row.
func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// a pipe inside provider data must not break the table
func TestRenderPlainEscapesPipes(t *testing.T) {
	var b bytes.Buffer
	renderPlain(&b, []Panel{{Name: "X", Items: []Gauge{{Label: "5h", Used: 1, Detail: "a|b"}}}})
	if !strings.Contains(b.String(), `a\|b`) {
		t.Errorf("pipe not escaped:\n%s", b.String())
	}
}

func TestRenderJSON(t *testing.T) {
	var b bytes.Buffer
	if err := renderJSON(&b, samplePanels()); err != nil {
		t.Fatal(err)
	}
	var panels []jsonPanel
	if err := json.Unmarshal(b.Bytes(), &panels); err != nil {
		t.Fatalf("roundtrip: %v\n%s", err, b.String())
	}
	if len(panels) != 3 {
		t.Fatalf("got %d panels, want 3", len(panels))
	}
	if panels[0].Provider != "Z.AI" || panels[0].Note != "GLM Coding Lite" || len(panels[0].Gauges) != 1 {
		t.Errorf("panel 0: %+v", panels[0])
	}
	g := panels[0].Gauges[0]
	if g.Used != 60 || g.Window != (5*time.Hour).String() || g.ResetsAt == "" || g.ResetsIn == "" {
		t.Errorf("gauge 0: %+v", g)
	}
	if panels[1].Gauges[0].Used != -1 || panels[1].Gauges[0].ResetsAt != "" {
		t.Errorf("balance gauge should have used=-1 and no reset: %+v", panels[1].Gauges[0])
	}
	if panels[2].Error != "missing KEY" || len(panels[2].Gauges) != 0 {
		t.Errorf("error panel: %+v", panels[2])
	}
}
