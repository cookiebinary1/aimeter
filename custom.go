package main

// User-defined providers: when aimeter has no built-in support for a service,
// the user describes it in credentials.json under "custom" — one GET with a
// Bearer key, then RFC 6901 JSON-pointer extraction into a gauge. Deliberately
// minimal: no POST bodies, no auth flavors, no response templates.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// customProvider is one entry of the "custom" array in credentials.json.
// Required: name, url, api_key, and either percent or used+total pointers.
type customProvider struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	APIKey  string `json:"api_key"`
	Label   string `json:"label,omitempty"`   // gauge label; default "usage"
	Percent string `json:"percent,omitempty"` // pointer to a 0-100 number
	Used    string `json:"used,omitempty"`    // pointer to used amount
	Total   string `json:"total,omitempty"`   // pointer to total amount
	Detail  string `json:"detail,omitempty"`  // pointer to an extra text line
}

// fetchCustom fetches one user-defined provider.
func fetchCustom(ctx context.Context, p customProvider) ([]Gauge, string, error) {
	if p.URL == "" {
		return nil, "", fmt.Errorf("missing url")
	}
	if p.APIKey == "" {
		return nil, "", fmt.Errorf("missing api_key")
	}
	var raw any
	if err := getJSON(ctx, p.URL, map[string]string{"Authorization": "Bearer " + p.APIKey}, &raw); err != nil {
		return nil, "", err
	}

	label := p.Label
	if label == "" {
		label = "usage"
	}

	var used float64
	switch {
	case p.Percent != "":
		v, ok := jsonPointer(raw, p.Percent)
		if !ok {
			return nil, "", fmt.Errorf("percent pointer %q not found", p.Percent)
		}
		f, ok := toFloat(v)
		if !ok {
			return nil, "", fmt.Errorf("percent pointer %q is not a number", p.Percent)
		}
		used = f
	case p.Used != "" && p.Total != "":
		uv, okU := jsonPointer(raw, p.Used)
		tv, okT := jsonPointer(raw, p.Total)
		if !okU || !okT {
			return nil, "", fmt.Errorf("used/total pointer not found")
		}
		uf, okU := toFloat(uv)
		tf, okT := toFloat(tv)
		if !okU || !okT {
			return nil, "", fmt.Errorf("used/total pointer is not a number")
		}
		if tf <= 0 {
			return nil, "", fmt.Errorf("total must be > 0")
		}
		used = uf / tf * 100
	default:
		return nil, "", fmt.Errorf("needs a percent pointer, or used+total pointers")
	}

	detail := ""
	if p.Detail != "" {
		if v, ok := jsonPointer(raw, p.Detail); ok {
			detail = toDisplay(v)
		}
	}
	return []Gauge{{Label: label, Used: used, Detail: detail}}, "", nil
}

// jsonPointer resolves an RFC 6901 pointer (e.g. "/data/limits/0/used")
// against decoded JSON; ok=false when any step is missing.
func jsonPointer(doc any, ptr string) (any, bool) {
	if ptr == "" {
		return doc, true
	}
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(tok, "~1", "/")
		tok = strings.ReplaceAll(tok, "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// toFloat accepts JSON numbers and numeric strings.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// toDisplay renders a pointer value for the detail line.
func toDisplay(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(n)
	default:
		b, err := json.Marshal(n)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
