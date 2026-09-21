package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// stubTransport answers every request with one canned JSON body, so a fetcher
// can be exercised without touching the network.
type stubTransport string

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(s))),
	}, nil
}

// withStub points the shared client at a canned response for one test.
func withStub(t *testing.T, body string) {
	t.Helper()
	prev := httpc.Transport
	httpc.Transport = stubTransport(body)
	t.Cleanup(func() { httpc.Transport = prev })
}

// A free ElevenLabs account reports no tier at all. Slicing that empty string
// for the plan note used to panic and take the whole dashboard down.
func TestElevenLabsWithoutTier(t *testing.T) {
	withStub(t, `{"tier":"","character_count":120,"character_limit":10000,"status":"active"}`)

	gs, note, err := fetchElevenLabs(context.Background(), Creds{EleKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want empty for a tierless account", note)
	}
	if len(gs) != 1 || gs[0].Used < 1.19 || gs[0].Used > 1.21 {
		t.Fatalf("gauges = %+v, want one gauge at ~1.2%%", gs)
	}
}

func TestElevenLabsTierIsCapitalised(t *testing.T) {
	withStub(t, `{"tier":"starter","character_count":1,"character_limit":2}`)

	_, note, err := fetchElevenLabs(context.Background(), Creds{EleKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if note != "Starter" {
		t.Errorf("note = %q, want %q", note, "Starter")
	}
}

// Z.AI keeps its product prefix in front of the capitalised plan level.
func TestZaiNoteKeepsProductPrefix(t *testing.T) {
	withStub(t, `{"success":true,"data":{"level":"lite","limits":[
		{"type":"CREDIT_LIMIT","unit":3,"number":5,"currentValue":1659,"usage":2000,"percentage":82}]}}`)

	gs, note, err := fetchZai(context.Background(), Creds{ZaiKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if note != "GLM Coding Lite" {
		t.Errorf("note = %q, want %q", note, "GLM Coding Lite")
	}
	if len(gs) != 1 || gs[0].Label != "5h" || gs[0].Detail != "1659 / 2000 credits" {
		t.Fatalf("gauges = %+v", gs)
	}
}

// A manual refresh must replace the auto-refresh chain, not add a second one:
// the timer armed before the key press has to be dropped, or every "r" press
// permanently doubles how often the dashboard hits every provider.
func TestManualRefreshSupersedesPendingTimer(t *testing.T) {
	m := initialModel(Creds{})

	// The initial fetch lands and arms the auto-refresh timer for gen 0.
	next, _ := m.Update(fetchedMsg{panels: samplePanels(), at: time.Now(), gen: 0})
	m = next.(model)

	// User presses "r": a new chain starts.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(model)
	if cmd == nil {
		t.Fatal("manual refresh issued no command")
	}

	// The timer from the abandoned chain fires: it must not start a fetch.
	if _, cmd := m.Update(tickMsg{gen: 0}); cmd != nil {
		t.Error("stale timer started a second refresh chain")
	}
	// Its in-flight result must not re-arm a timer either.
	if _, cmd := m.Update(fetchedMsg{panels: samplePanels(), at: time.Now(), gen: 0}); cmd != nil {
		t.Error("stale fetch result re-armed the auto-refresh timer")
	}
	// The current chain still works.
	if _, cmd := m.Update(tickMsg{gen: m.gen}); cmd == nil {
		t.Error("current timer did not trigger a refresh")
	}
}
