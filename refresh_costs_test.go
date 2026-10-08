package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchCmdReloadsRotatedToken(t *testing.T) {
	oldReload, oldTransport, oldDir := reloadCreds, httpc.Transport, anthropicCacheDir
	anthropicCacheDir = t.TempDir()
	t.Cleanup(func() { reloadCreds, httpc.Transport, anthropicCacheDir = oldReload, oldTransport, oldDir })
	token := "old"
	reloadCreds = func() (Creds, map[string]string) { return Creds{AnthToken: token}, nil }
	var seen []string
	httpc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.anthropic.com" {
			t.Fatalf("unexpected host %s", r.URL.Host)
		}
		seen = append(seen, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":12}}`)), Header: make(http.Header)}, nil
	})
	cmd := fetchCmd(Creds{AnthToken: "startup"}, 0)
	cmd()
	token = "rotated"
	blocked := cmd().(fetchedMsg)
	if blocked.creds.AnthToken != "rotated" || len(seen) != 1 {
		t.Fatal("rotation must reload credentials without bypassing cooldown")
	}
	if err := writeAnthropicState(filepath.Join(anthropicCacheDir, "anthropic-request.json"), anthropicRequestState{NextAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	cmd()
	if strings.Join(seen, ",") != "Bearer old,Bearer rotated" {
		t.Fatalf("requests = %v", seen)
	}
}

func TestServiceCost(t *testing.T) {
	for _, tc := range []struct{ key, plan, want string }{
		{"codex", "plus", "~$20/mo list"}, {"codex", "prolite", "cost unknown"},
		{"openrouter", "", "usage-based"}, {"minimax", "Coding Plan", "cost unknown"},
	} {
		if got := serviceCost(tc.key, tc.plan, nil); got != tc.want {
			t.Errorf("%s = %q", tc.key, got)
		}
	}
	if got := serviceCost("codex", "plus", map[string]string{"codex": "€23/mo"}); got != "€23/mo" {
		t.Fatal(got)
	}
	if got := serviceCost("codex", "plus", map[string]string{"codex": ""}); got != "" {
		t.Fatal(got)
	}
}

func TestCostHeaderFits(t *testing.T) {
	p := widePanel()
	p.Cost = "€23/mo"
	for width := 24; width <= 100; width++ {
		rendered := renderOne(p, width)
		for _, line := range strings.Split(rendered, "\n") {
			if visualWidth(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
	}
	if !strings.Contains(renderOne(p, 100), p.Cost) {
		t.Fatal("missing cost")
	}
}

func TestCostsInOutputAndConfig(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"costs":{"anthropic":"€100/mo"}}`)
	cfg, err := readConfigFile()
	if err != nil || cfg.Costs["anthropic"] != "€100/mo" {
		t.Fatalf("config: %v, %v", cfg, err)
	}
	p := widePanel()
	p.Cost = "€23/mo"
	var b bytes.Buffer
	if err := renderJSON(&b, []Panel{p}); err != nil {
		t.Fatal(err)
	}
	var out []jsonPanel
	if err := json.Unmarshal(b.Bytes(), &out); err != nil || out[0].Cost != p.Cost {
		t.Fatalf("JSON: %s, %v", b.String(), err)
	}
	b.Reset()
	renderPlain(&b, []Panel{p})
	if !strings.Contains(b.String(), p.Cost) {
		t.Fatal("missing plain cost")
	}
}
