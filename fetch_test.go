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

// An account with no character limit has no percentage to show. The gauge must
// go balance-only (Used < 0) instead of rendering a reassuring empty bar that
// reads as "0% used".
func TestElevenLabsWithoutLimitIsBalanceOnly(t *testing.T) {
	withStub(t, `{"tier":"free","character_count":1200,"character_limit":0}`)

	gs, _, err := fetchElevenLabs(context.Background(), Creds{EleKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(gs) != 1 {
		t.Fatalf("gauges = %+v, want one", gs)
	}
	if gs[0].Used >= 0 {
		t.Errorf("Used = %v, want negative (balance-only)", gs[0].Used)
	}
	if !strings.Contains(gs[0].Detail, "1200 chars used") {
		t.Errorf("detail = %q, want the raw character count", gs[0].Detail)
	}
}

// Usage settles asynchronously, so an account can end up past its balance.
// The gauge stays inside the documented 0-100 range and the detail says how
// far over it went, rather than reporting a negative amount "left".
func TestOpenRouterOverrunStaysInRange(t *testing.T) {
	withStub(t, `{"data":{"total_credits":10,"total_usage":12.5}}`)

	gs, _, err := fetchOpenRouter(context.Background(), Creds{OpenRouter: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gs[0].Used != 100 {
		t.Errorf("Used = %v, want 100", gs[0].Used)
	}
	if !strings.Contains(gs[0].Detail, "$2.50 over") {
		t.Errorf("detail = %q, want the overrun", gs[0].Detail)
	}
}

func TestOpenRouterNormalBalance(t *testing.T) {
	withStub(t, `{"data":{"total_credits":10,"total_usage":1.22}}`)

	gs, _, err := fetchOpenRouter(context.Background(), Creds{OpenRouter: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(gs[0].Detail, "$8.78 left") {
		t.Errorf("detail = %q, want the remaining balance", gs[0].Detail)
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

// Kimi reports used_ratio fractions per quota window; the fetcher turns them
// into percentage gauges with parsed reset times.
func TestKimiQuotaWindows(t *testing.T) {
	withStub(t, `{"usages":{
		"limit_5h":{"used_ratio":0.1164,"reset_time":"2026-11-16T00:40:13Z"},
		"limit_7d":{"used_ratio":0.5,"reset_time":"2026-11-20T00:00:00Z"},
		"limit_month_total":{"used_ratio":0.0143,"reset_time":"2026-12-16T00:00:00Z"}}}`)

	gs, note, err := fetchKimi(context.Background(), Creds{KimiKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if note != "Coding Plan" {
		t.Errorf("note = %q, want Coding Plan", note)
	}
	if len(gs) != 3 {
		t.Fatalf("gauges = %+v, want 5h/7d/month", gs)
	}
	if gs[0].Label != "5h" || gs[0].Used < 11.6 || gs[0].Used > 11.7 || gs[0].Window != 5*time.Hour {
		t.Errorf("5h gauge = %+v, want ~11.64%% with a 5h window", gs[0])
	}
	if gs[0].Reset == nil || gs[0].Reset.Format(time.RFC3339) != "2026-11-16T00:40:13Z" {
		t.Errorf("5h reset = %v, want parsed reset time", gs[0].Reset)
	}
	if gs[2].Label != "month" || gs[2].Used < 1.42 || gs[2].Used > 1.44 {
		t.Errorf("month gauge = %+v, want ~1.43%%", gs[2])
	}
	// 2026-12-16 minus one calendar month is 30 days, so the elapsed bar
	// gets a real window instead of 0.
	if gs[2].Window != 30*24*time.Hour {
		t.Errorf("month window = %v, want 720h", gs[2].Window)
	}
}

// Plans without a month_total entry fall back to the code-specific window.
func TestKimiMonthCodeFallback(t *testing.T) {
	withStub(t, `{"usages":{
		"limit_5h":{"used_ratio":0.2,"reset_time":"2026-11-16T00:40:13Z"},
		"limit_month_code":{"used_ratio":0.3,"reset_time":"2026-12-16T00:00:00Z"}}}`)

	gs, _, err := fetchKimi(context.Background(), Creds{KimiKey: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(gs) != 2 || gs[1].Label != "month" || gs[1].Used != 30 {
		t.Fatalf("gauges = %+v, want 5h plus month from limit_month_code", gs)
	}
}

func TestKimiEmptyUsagesFails(t *testing.T) {
	withStub(t, `{"usages":{}}`)

	if _, _, err := fetchKimi(context.Background(), Creds{KimiKey: "k"}); err == nil {
		t.Fatal("fetch succeeded, want an error for empty usages")
	}
}

// An expired Alibaba console session must surface the login offer once per
// expiry (not every refresh), hold the auto-refresh timer while prompting,
// and clear after a successful panel.
func TestAlibabaLoginPromptFlow(t *testing.T) {
	m := initialModel(Creds{})
	aliErr := func() []Panel {
		return []Panel{{Name: "Alibaba", Err: errAlibabaLogin, Updated: time.Now()}}
	}
	next, cmd := m.Update(fetchedMsg{panels: aliErr(), at: time.Now(), gen: 0})
	m = next.(model)
	if !m.aliPrompt {
		t.Fatal("login prompt not offered on expired session")
	}
	if cmd != nil {
		t.Error("auto-refresh was armed while the login prompt is open")
	}

	// Declining resumes the refresh chain.
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = next.(model)
	if m.aliPrompt || cmd == nil {
		t.Errorf("decline: aliPrompt=%v cmd=%v, want prompt closed and timer armed", m.aliPrompt, cmd)
	}

	// The prompt does not re-appear for the same expiry...
	next, _ = m.Update(fetchedMsg{panels: aliErr(), at: time.Now(), gen: 0})
	m = next.(model)
	if m.aliPrompt {
		t.Error("prompt re-appeared for the same expiry")
	}
	// ...but a healthy panel resets it, so a later expiry prompts again.
	next, _ = m.Update(fetchedMsg{panels: []Panel{{Name: "Alibaba", Updated: time.Now()}}, at: time.Now(), gen: 0})
	m = next.(model)
	next, _ = m.Update(fetchedMsg{panels: aliErr(), at: time.Now(), gen: 0})
	m = next.(model)
	if !m.aliPrompt {
		t.Error("prompt did not return after a new expiry")
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
