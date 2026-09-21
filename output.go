package main

// Script- and agent-facing output modes: -plain prints one " | "-separated
// line per gauge (no TUI, no colors, no box drawing), -json prints the same
// data machine-readable. Both share fetchAll with the TUI, so the numbers are
// identical across modes.

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// renderPlain writes one line per gauge, " | "-separated:
//
//	Provider | note | label | 42% | detail | resets in 1h 58m
//	Provider | ERROR: message
//
// Gauges with unknown usage (Used < 0, e.g. a plain balance) omit the percent.
func renderPlain(w io.Writer, panels []Panel) {
	for _, p := range panels {
		if p.Err != nil {
			fmt.Fprintf(w, "%s | ERROR: %s\n", p.Name, p.Err.Error())
			continue
		}
		head := p.Name
		if p.Note != "" {
			head += " | " + p.Note
		}
		for _, g := range p.Items {
			parts := []string{head, g.Label}
			if g.Used >= 0 {
				parts = append(parts, fmt.Sprintf("%.0f%%", clampPct(g.Used)))
			}
			if g.Detail != "" {
				parts = append(parts, g.Detail)
			}
			if rt := resetText(g); rt != "" {
				parts = append(parts, rt)
			}
			fmt.Fprintln(w, joinNonEmpty(parts, " | "))
		}
		if len(p.Items) == 0 {
			fmt.Fprintf(w, "%s | (no data)\n", head)
		}
	}
}

func joinNonEmpty(parts []string, sep string) string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	s := ""
	for i, p := range out {
		if i > 0 {
			s += sep
		}
		s += p
	}
	return s
}

// jsonGauge/jsonPanel are the -json schema. Used is 0-100, or -1 when the
// service reports only a balance and no percentage.
type jsonGauge struct {
	Label    string  `json:"label"`
	Used     float64 `json:"used"`
	Detail   string  `json:"detail,omitempty"`
	ResetsAt string  `json:"resets_at,omitempty"` // RFC 3339
	ResetsIn string  `json:"resets_in,omitempty"` // e.g. "1h 58m", "now"
	Window   string  `json:"window,omitempty"`    // window length, e.g. "5h0m0s"
}

type jsonPanel struct {
	Provider string      `json:"provider"`
	Note     string      `json:"note,omitempty"`
	Error    string      `json:"error,omitempty"`
	Gauges   []jsonGauge `json:"gauges"`
}

// renderJSON writes all panels as one JSON array.
func renderJSON(w io.Writer, panels []Panel) error {
	out := make([]jsonPanel, 0, len(panels))
	for _, p := range panels {
		jp := jsonPanel{Provider: p.Name, Note: p.Note, Gauges: []jsonGauge{}}
		if p.Err != nil {
			jp.Error = p.Err.Error()
		}
		for _, g := range p.Items {
			jg := jsonGauge{Label: g.Label, Used: g.Used, Detail: g.Detail}
			if g.Window > 0 {
				jg.Window = g.Window.String()
			}
			if g.Reset != nil {
				jg.ResetsAt = g.Reset.Format(time.RFC3339)
				d := time.Until(*g.Reset)
				if d <= 0 {
					jg.ResetsIn = "now"
				} else {
					jg.ResetsIn = formatDuration(d)
				}
			}
			jp.Gauges = append(jp.Gauges, jg)
		}
		out = append(out, jp)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
