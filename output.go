package main

// Script- and agent-facing output modes: -plain prints a GitHub-flavoured
// Markdown table (one row per gauge), -json prints the same data as JSON.
// Both share fetchAll with the TUI, so the numbers are identical across modes.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// plainColumns is the fixed schema of the -plain table. Fixed columns keep the
// output parseable by column index and pasteable into issues or agent prompts.
var plainColumns = []string{"Provider", "Plan", "Window", "Used", "Detail", "Resets in"}

// renderPlain writes all panels as one Markdown table:
//
//	| Provider | Plan            | Window | Used | Detail              | Resets in |
//	| -------- | --------------- | ------ | ---- | ------------------- | --------- |
//	| Z.AI     | GLM Coding Lite | 5h     | 82%  | 1659 / 2000 credits | 2h 44m    |
//
// Cells are padded so the raw text also reads as a table in a terminal.
// Gauges with unknown usage (Used < 0, e.g. a plain balance) leave Used empty;
// a failed provider carries its message in the Detail column.
func renderPlain(w io.Writer, panels []Panel) {
	rows := [][]string{}
	for _, p := range panels {
		if p.Err != nil {
			rows = append(rows, []string{p.Name, p.Note, "", "", "ERROR: " + p.Err.Error(), ""})
			continue
		}
		if len(p.Items) == 0 {
			rows = append(rows, []string{p.Name, p.Note, "", "", "(no data)", ""})
			continue
		}
		for _, g := range p.Items {
			used := ""
			if g.Used >= 0 {
				used = fmt.Sprintf("%.0f%%", clampPct(g.Used))
			}
			rows = append(rows, []string{p.Name, p.Note, g.Label, used, g.Detail, resetsIn(g)})
		}
	}

	widths := make([]int, len(plainColumns))
	for i, h := range plainColumns {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if n := len([]rune(escapePipes(cell))); n > widths[i] {
				widths[i] = n
			}
		}
	}

	fmt.Fprintln(w, markdownRow(plainColumns, widths))
	seps := make([]string, len(widths))
	for i, n := range widths {
		seps[i] = strings.Repeat("-", n)
	}
	fmt.Fprintln(w, markdownRow(seps, widths))
	for _, r := range rows {
		fmt.Fprintln(w, markdownRow(r, widths))
	}
}

// resetsIn is the bare remaining time, without the "resets in " prose the TUI
// needs, because the column header already says what the value means.
func resetsIn(g Gauge) string {
	if g.Reset == nil {
		return ""
	}
	d := time.Until(*g.Reset)
	if d <= 0 {
		return "now"
	}
	return formatDuration(d)
}

func markdownRow(cells []string, widths []int) string {
	var b strings.Builder
	b.WriteString("|")
	for i, c := range cells {
		c = escapePipes(c)
		b.WriteString(" ")
		b.WriteString(c)
		b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(c))))
		b.WriteString(" |")
	}
	return b.String()
}

// escapePipes keeps a provider-supplied value from breaking the table.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
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
