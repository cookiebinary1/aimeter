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

func TestRenderPlain(t *testing.T) {
	var b bytes.Buffer
	renderPlain(&b, samplePanels())

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), b.String())
	}
	if !strings.HasPrefix(lines[0], "Z.AI | GLM Coding Lite | 5h | 60% | 1207 / 2000 credits | resets in") {
		t.Errorf("line 1: %q", lines[0])
	}
	// negative Used (balance-only) must omit the percent
	if lines[1] != "Meshy | 3D generation | credits | 5500 credits left" {
		t.Errorf("line 2: %q", lines[1])
	}
	if lines[2] != "Broken | ERROR: missing KEY" {
		t.Errorf("line 3: %q", lines[2])
	}
	if strings.ContainsAny(b.String(), "\x1b") {
		t.Error("plain output must not contain ANSI escapes")
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
