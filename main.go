package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

// ---------------------------------------------------------------------------
// Data model

type Gauge struct {
	Label  string  // e.g. "5h", "7d"
	Used   float64 // 0..100
	Reset  *time.Time
	Detail string
	Window time.Duration // window length; >0 with Reset also draws the time bar
}

type Panel struct {
	Name    string
	Note    string
	Cost    string
	Items   []Gauge
	Err     error
	Updated time.Time
}

type Creds struct {
	AnthToken   string
	AnthAccount string
	AnthEmail   string
	CodexToken  string
	CodexAcct   string
	ZaiKey      string
	KimiKey     string
	MmKey       string
	OpenRouter  string
	EleKey      string
	MeshyKey    string
	AliKey      string
	AliBaseURL  string
	AliCookie   string
	Custom      []customProvider
	Costs       map[string]string
	Prefs       dashboardConfig
}

// ---------------------------------------------------------------------------
// HTTP + JSON

var httpc = &http.Client{Timeout: 15 * time.Second}

type httpStatusError struct {
	code       int
	retryAfter string
	expired    bool
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func getJSON(ctx context.Context, url string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		status := &httpStatusError{code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
		if resp.StatusCode == http.StatusUnauthorized && req.URL.Host == "api.anthropic.com" && req.URL.Path == "/api/oauth/usage" {
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&body) == nil {
				status.expired = strings.Contains(strings.ToLower(body.Error.Message), "access token has expired")
			}
		}
		return status
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------------------------------------------------------------------------
// Provider fetchers

type anthBucket struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

func fetchAnthropic(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.AnthToken == "" {
		return nil, "", fmt.Errorf("missing anthropic credential (credentials.json / keychain / -tags omp)")
	}
	cache := readAnthropicCache(c.AnthToken)
	// Successful usage belongs to the account, while authentication failures
	// belong to one token. Keep those separate when Claude rotates tokens.
	if c.AnthAccount != "" {
		accountCache := readAnthropicCache("account:" + c.AnthAccount)
		if accountCache.FetchedAt.After(cache.FetchedAt) {
			cache.Items, cache.FetchedAt = accountCache.Items, accountCache.FetchedAt
		}
	}
	state, release, gateErr := reserveAnthropicRequest(cache)
	if gateErr != nil {
		return nil, "", gateErr
	}
	if release == nil {
		return cooldownAnthropic(cache, state)
	}
	defer release()
	var j struct {
		FiveHour     *anthBucket `json:"five_hour"`
		SevenDay     *anthBucket `json:"seven_day"`
		SevenDayOpus *anthBucket `json:"seven_day_opus"`
	}
	err := getJSON(ctx, "https://api.anthropic.com/api/oauth/usage", map[string]string{
		"Authorization":  "Bearer " + c.AnthToken,
		"anthropic-beta": "oauth-2025-04-20",
		"User-Agent":     "claude-code/2.0",
	}, &j)
	if err != nil {
		cache.Failed = true
		if status, ok := err.(*httpStatusError); ok {
			cache.HTTPCode, cache.Expired = status.code, status.expired
			if status.code == http.StatusTooManyRequests {
				cache.RetryAt = anthropicRetryAt(status.retryAfter)
				if cache.RetryAt.After(state.NextAt) {
					state.NextAt = cache.RetryAt
				}
				state.RateLimited = true
				if writeErr := writeAnthropicState(filepath.Join(anthropicCacheDir, "anthropic-request.json"), state); writeErr != nil {
					_ = writeAnthropicCache(c.AnthToken, cache)
					return nil, "", fmt.Errorf("cannot persist Anthropic rate limit; request skipped")
				}
				_ = writeAnthropicCache(c.AnthToken, cache)
				return cooldownAnthropic(cache, state)
			}
		}
		_ = writeAnthropicCache(c.AnthToken, cache)
		return nil, "", err
	}
	var gs []Gauge
	add := func(label string, b *anthBucket, window time.Duration) {
		if b != nil {
			gs = append(gs, Gauge{Label: label, Used: b.Utilization, Reset: b.ResetsAt, Window: window})
		}
	}
	add("5h", j.FiveHour, 5*time.Hour)
	add("7d", j.SevenDay, 7*24*time.Hour)
	add("opus 7d", j.SevenDayOpus, 7*24*time.Hour)
	if len(gs) == 0 {
		cache.Failed = true
		_ = writeAnthropicCache(c.AnthToken, cache)
		return nil, "", fmt.Errorf("empty response")
	}
	note := ""
	if c.AnthEmail != "" {
		note = c.AnthEmail
	}
	success := anthropicCache{FetchedAt: time.Now(), Items: gs}
	_ = writeAnthropicCache(c.AnthToken, success)
	if c.AnthAccount != "" {
		_ = writeAnthropicCache("account:"+c.AnthAccount, success)
	}
	return gs, note, nil
}

func fetchCodex(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.CodexToken == "" {
		return nil, "", fmt.Errorf("missing ~/.codex/auth.json")
	}
	var j struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
		RateLim  struct {
			Primary   *codexWin `json:"primary_window"`
			Secondary *codexWin `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	err := getJSON(ctx, "https://chatgpt.com/backend-api/wham/usage", map[string]string{
		"Authorization":      "Bearer " + c.CodexToken,
		"ChatGPT-Account-Id": c.CodexAcct,
		"OpenAI-Beta":        "codex-1",
		"originator":         "Codex Desktop",
	}, &j)
	if err != nil {
		return nil, "", err
	}
	var gs []Gauge
	for _, w := range []*codexWin{j.RateLim.Primary, j.RateLim.Secondary} {
		if w == nil {
			continue
		}
		reset := w.Reset()
		gs = append(gs, Gauge{Label: w.Label(), Used: w.UsedPercent, Reset: reset,
			Window: time.Duration(w.WindowSeconds) * time.Second})
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("no windows in response")
	}
	return gs, j.PlanType, nil
}

type codexWin struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int     `json:"limit_window_seconds"`
	ResetAfter    float64 `json:"reset_after_seconds"`
	ResetAt       float64 `json:"reset_at"`
}

func (w *codexWin) Label() string {
	switch w.WindowSeconds {
	case 18000:
		return "5h"
	case 604800:
		return "7d"
	default:
		return fmt.Sprintf("%dh", w.WindowSeconds/3600)
	}
}

func (w *codexWin) Reset() *time.Time {
	if w.ResetAt > 0 {
		t := time.Unix(int64(w.ResetAt), 0)
		return &t
	}
	if w.ResetAfter > 0 {
		t := time.Now().Add(time.Duration(w.ResetAfter) * time.Second)
		return &t
	}
	return nil
}

func fetchZai(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.ZaiKey == "" {
		return nil, "", fmt.Errorf("missing ZAI_API_KEY (env / credentials.json)")
	}
	var j struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Success bool   `json:"success"`
		Data    struct {
			Level  string `json:"level"`
			Limits []struct {
				Type          string  `json:"type"`
				Unit          int     `json:"unit"`
				Number        int     `json:"number"`
				CurrentValue  float64 `json:"currentValue"`
				Usage         float64 `json:"usage"`
				Percentage    float64 `json:"percentage"`
				NextResetTime int64   `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://api.z.ai/api/monitor/usage/quota/limit", map[string]string{
		"Authorization": "Bearer " + c.ZaiKey,
	}, &j); err != nil {
		return nil, "", err
	}
	if !j.Success {
		return nil, "", fmt.Errorf("Z.AI: %s", j.Msg)
	}
	var gs []Gauge
	for _, l := range j.Data.Limits {
		if l.Type != "CREDIT_LIMIT" {
			continue
		}
		var reset *time.Time
		if l.NextResetTime > 0 {
			t := time.UnixMilli(l.NextResetTime)
			reset = &t
		}
		gs = append(gs, Gauge{
			Label:  zaiLabel(l.Unit, l.Number),
			Used:   l.Percentage,
			Reset:  reset,
			Window: zaiWindow(l.Unit, l.Number),
			Detail: fmt.Sprintf("%.0f / %.0f credits", l.CurrentValue, l.Usage),
		})
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("no limits")
	}
	note := ""
	if j.Data.Level != "" {
		note = "GLM Coding " + titleCase(j.Data.Level)
	}
	return gs, note, nil
}

// titleCase upper-cases the first letter of a plan name. Providers send these
// lower-cased ("lite", "starter"), and an absent plan must stay absent rather
// than panic on an empty string.
func titleCase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func zaiLabel(unit, number int) string {
	switch {
	case unit == 3 && number == 5:
		return "5h"
	case unit == 6:
		return "7d"
	default:
		return fmt.Sprintf("%d×%d", number, unit)
	}
}

func zaiWindow(unit, number int) time.Duration {
	switch {
	case unit == 3 && number == 5:
		return 5 * time.Hour
	case unit == 6:
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

func fetchKimi(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.KimiKey == "" {
		return nil, "", fmt.Errorf("missing KIMI_API_KEY (env / credentials.json)")
	}
	// Kimi for Coding plan quota, same endpoint the Kimi Code CLI /usage
	// command reads: https://api.kimi.com/coding/v1/usages (MoonshotAI/kimi-code).
	var j struct {
		Usages struct {
			Limit5h struct {
				UsedRatio float64 `json:"used_ratio"`
				ResetTime string  `json:"reset_time"`
			} `json:"limit_5h"`
			Limit7d struct {
				UsedRatio float64 `json:"used_ratio"`
				ResetTime string  `json:"reset_time"`
			} `json:"limit_7d"`
			MonthTotal struct {
				UsedRatio float64 `json:"used_ratio"`
				ResetTime string  `json:"reset_time"`
			} `json:"limit_month_total"`
			MonthCode struct {
				UsedRatio float64 `json:"used_ratio"`
				ResetTime string  `json:"reset_time"`
			} `json:"limit_month_code"`
		} `json:"usages"`
	}
	if err := getJSON(ctx, "https://api.kimi.com/coding/v1/usages", map[string]string{
		"Authorization": "Bearer " + c.KimiKey,
	}, &j); err != nil {
		return nil, "", err
	}
	add := func(label string, usedRatio float64, resetTime string, window time.Duration) *Gauge {
		if usedRatio <= 0 && resetTime == "" {
			return nil
		}
		g := Gauge{Label: label, Used: usedRatio * 100, Window: window}
		if t, err := time.Parse(time.RFC3339, resetTime); err == nil {
			g.Reset = &t
		}
		return &g
	}
	var gs []Gauge
	// Monthly windows are calendar cycles anchored on the billing day: derive
	// the window length from the reset time so the elapsed bar can render.
	monthWindow := func(resetTime string) time.Duration {
		if t, err := time.Parse(time.RFC3339, resetTime); err == nil {
			return t.Sub(t.AddDate(0, -1, 0))
		}
		return 0
	}
	month := add("month", j.Usages.MonthTotal.UsedRatio, j.Usages.MonthTotal.ResetTime, monthWindow(j.Usages.MonthTotal.ResetTime))
	if month == nil {
		// Some plans report only the code-specific monthly window.
		month = add("month", j.Usages.MonthCode.UsedRatio, j.Usages.MonthCode.ResetTime, monthWindow(j.Usages.MonthCode.ResetTime))
	}
	for _, g := range []*Gauge{
		add("5h", j.Usages.Limit5h.UsedRatio, j.Usages.Limit5h.ResetTime, 5*time.Hour),
		add("7d", j.Usages.Limit7d.UsedRatio, j.Usages.Limit7d.ResetTime, 7*24*time.Hour),
		month,
	} {
		if g != nil {
			gs = append(gs, *g)
		}
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("no quota windows (Kimi for Coding key required)")
	}
	return gs, "Coding Plan", nil
}

func fetchMinimax(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.MmKey == "" {
		return nil, "", fmt.Errorf("missing MINIMAX_API_KEY (env / credentials.json)")
	}
	var j struct {
		ModelRemains []struct {
			ModelName    string  `json:"model_name"`
			IntervalLeft float64 `json:"current_interval_remaining_percent"`
			StartTime    int64   `json:"start_time"`
			EndTime      int64   `json:"end_time"`
			WeeklyLeft   float64 `json:"current_weekly_remaining_percent"`
			WeeklyStart  int64   `json:"weekly_start_time"`
			WeeklyEnd    int64   `json:"weekly_end_time"`
		} `json:"model_remains"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := getJSON(ctx, "https://api.minimax.io/v1/token_plan/remains", map[string]string{
		"Authorization": "Bearer " + c.MmKey,
	}, &j); err != nil {
		return nil, "", err
	}
	if j.BaseResp.StatusCode != 0 {
		return nil, "", fmt.Errorf("MiniMax: %s", j.BaseResp.StatusMsg)
	}
	var gs []Gauge
	for _, m := range j.ModelRemains {
		if m.ModelName != "general" {
			continue
		}
		if m.EndTime > 0 {
			t := time.UnixMilli(m.EndTime)
			win := time.Duration(0)
			if m.StartTime > 0 {
				win = time.Duration(m.EndTime-m.StartTime) * time.Millisecond
			}
			gs = append(gs, Gauge{Label: "5h", Used: 100 - m.IntervalLeft, Reset: &t, Window: win})
		}
		if m.WeeklyEnd > 0 {
			t := time.UnixMilli(m.WeeklyEnd)
			win := time.Duration(0)
			if m.WeeklyStart > 0 {
				win = time.Duration(m.WeeklyEnd-m.WeeklyStart) * time.Millisecond
			}
			gs = append(gs, Gauge{Label: "7d", Used: 100 - m.WeeklyLeft, Reset: &t, Window: win})
		}
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("no 'general' plan")
	}
	return gs, "Coding Plan", nil
}

func fetchOpenRouter(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.OpenRouter == "" {
		return nil, "", fmt.Errorf("missing OPENROUTER_API_KEY")
	}
	var j struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "https://openrouter.ai/api/v1/credits", map[string]string{
		"Authorization": "Bearer " + c.OpenRouter,
	}, &j); err != nil {
		return nil, "", err
	}
	d := j.Data
	if d.TotalCredits <= 0 {
		return nil, "", fmt.Errorf("no credits")
	}
	used := d.TotalUsage / d.TotalCredits * 100
	// An account can run slightly past its credit balance (usage is settled
	// asynchronously). The gauge tops out at 100% — the schema promises 0-100
	// — while the detail states the overrun instead of a negative "left".
	detail := fmt.Sprintf("$%.2f / $%.2f · $%.2f left", d.TotalUsage, d.TotalCredits, d.TotalCredits-d.TotalUsage)
	if d.TotalUsage > d.TotalCredits {
		detail = fmt.Sprintf("$%.2f / $%.2f · $%.2f over", d.TotalUsage, d.TotalCredits, d.TotalUsage-d.TotalCredits)
	}
	return []Gauge{{
		Label:  "credits",
		Used:   clampPct(used),
		Detail: detail,
	}}, "pay-as-you-go", nil
}

func fetchElevenLabs(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.EleKey == "" {
		return nil, "", fmt.Errorf("missing ELEVENLABS_API_KEY (env / credentials.json)")
	}
	var j struct {
		Tier           string `json:"tier"`
		CharacterCount int    `json:"character_count"`
		CharacterLimit int    `json:"character_limit"`
		NextReset      int64  `json:"next_character_count_reset_unix"`
		Status         string `json:"status"`
	}
	if err := getJSON(ctx, "https://api.elevenlabs.io/v1/user/subscription", map[string]string{
		"xi-api-key": c.EleKey,
	}, &j); err != nil {
		return nil, "", err
	}
	var rp *time.Time
	if j.NextReset > 0 {
		t := time.Unix(j.NextReset, 0)
		rp = &t
	}
	// A tierless/unmetered account reports no character limit. There is no
	// percentage to draw then, so the gauge becomes balance-only (Used < 0)
	// rather than a reassuring empty green bar.
	if j.CharacterLimit <= 0 {
		return []Gauge{{
			Label:  "chars",
			Used:   -1,
			Reset:  rp,
			Detail: fmt.Sprintf("%d chars used · no limit reported", j.CharacterCount),
		}}, titleCase(j.Tier), nil
	}
	return []Gauge{{
		Label:  "chars",
		Used:   float64(j.CharacterCount) / float64(j.CharacterLimit) * 100,
		Reset:  rp,
		Window: 30 * 24 * time.Hour, // monthly cycle (approx)
		Detail: fmt.Sprintf("%d / %d chars", j.CharacterCount, j.CharacterLimit),
	}}, titleCase(j.Tier), nil
}

func fetchMeshy(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.MeshyKey == "" {
		return nil, "", fmt.Errorf("missing MESHY_API_KEY (env / credentials.json)")
	}
	var j struct {
		Balance float64 `json:"balance"`
	}
	if err := getJSON(ctx, "https://api.meshy.ai/openapi/v1/balance", map[string]string{
		"Authorization": "Bearer " + c.MeshyKey,
	}, &j); err != nil {
		return nil, "", err
	}
	return []Gauge{{
		Label:  "credits",
		Used:   -1,
		Detail: fmt.Sprintf("%.0f credits left", j.Balance),
	}}, "3D generation", nil
}

// ---------------------------------------------------------------------------
// Fetch orchestration

// serviceDef is one panel source: a built-in provider or a custom service.
// key is the config id used by dashboard layout and cost overrides.
type serviceDef struct {
	name       string
	key        string
	credential string
	fn         func(context.Context, Creds) ([]Gauge, string, error)
}

// arrangeServices applies the dashboard layout: hidden services are dropped
// (and never fetched — a disabled service stops making requests) and the rest
// follow the configured order, with unlisted services keeping their default
// order at the end.
func arrangeServices(defs []serviceDef, prefs dashboardConfig) []serviceDef {
	hidden := make(map[string]bool, len(prefs.Hidden))
	for _, id := range prefs.Hidden {
		hidden[id] = true
	}
	rank := make(map[string]int, len(prefs.Order))
	for i, id := range prefs.Order {
		rank[id] = i
	}
	out := defs[:0]
	for _, d := range defs {
		if !hidden[d.key] {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return orderLess(out[i].key, out[j].key, rank)
	})
	return out
}

// orderLess ranks listed ids before unlisted ones, keeping the incoming order
// within each group (callers sort stably).
func orderLess(a, b string, rank map[string]int) bool {
	ra, aListed := rank[a]
	rb, bListed := rank[b]
	switch {
	case aListed && bListed:
		return ra < rb
	case aListed != bListed:
		return aListed
	default:
		return false
	}
}

func fetchAll(c Creds) []Panel {
	defs := []serviceDef{
		{"Anthropic", "anthropic", c.AnthToken, fetchAnthropic},
		{"OpenAI Codex", "codex", c.CodexToken, fetchCodex},
		{"Z.AI", "zai", c.ZaiKey, fetchZai},
		{"Kimi", "kimi", c.KimiKey, fetchKimi},
		{"MiniMax Code", "minimax", c.MmKey, fetchMinimax},
		{"OpenRouter", "openrouter", c.OpenRouter, fetchOpenRouter},
		{"ElevenLabs", "elevenlabs", c.EleKey, fetchElevenLabs},
		{"Meshy", "meshy", c.MeshyKey, fetchMeshy},
		{"Alibaba", "alibaba", c.AliKey, fetchAlibaba},
	}
	configured := defs[:0]
	for _, d := range defs {
		if d.credential != "" {
			configured = append(configured, d)
		}
	}
	defs = configured
	for _, cp := range c.Custom {
		p := cp
		defs = append(defs, serviceDef{p.Name, "custom:" + p.Name, "", func(ctx context.Context, _ Creds) ([]Gauge, string, error) {
			return fetchCustom(ctx, p)
		}})
	}
	defs = arrangeServices(defs, c.Prefs)
	var wg sync.WaitGroup
	results := make([]Panel, len(defs))
	for i, d := range defs {
		wg.Add(1)
		go func(i int, d serviceDef) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			items, note, err := d.fn(ctx, c)
			p := Panel{Name: d.name, Cost: serviceCost(d.key, note, c.Costs), Updated: time.Now()}
			if err != nil {
				p.Err = err
			} else {
				p.Items, p.Note = items, note
			}
			results[i] = p
		}(i, d)
	}
	wg.Wait()
	return results
}

// ---------------------------------------------------------------------------
// Styling

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	keyStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	headerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	okDotStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("41"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	nameStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	pctStyle     = lipgloss.NewStyle().Bold(true)
	timeFill     = lipgloss.NewStyle().Foreground(lipgloss.Color("69"))
	timeEmpty    = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	timePctStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
)

func pctColor(p float64) lipgloss.Color {
	switch {
	case p >= 90:
		return lipgloss.Color("203")
	case p >= 75:
		return lipgloss.Color("208")
	case p >= 50:
		return lipgloss.Color("220")
	default:
		return lipgloss.Color("41")
	}
}

func bar(p float64, width int, color lipgloss.Color) string {
	if width < 3 {
		width = 3
	}
	filled := int(math.Round(p / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Repeat("░", width-filled))
}

func panelColor(p Panel) (lipgloss.Color, bool) {
	if p.Err != nil {
		return lipgloss.Color("203"), true
	}
	worst := 0.0
	for _, g := range p.Items {
		if g.Used > worst {
			worst = g.Used
		}
	}
	return pctColor(worst), false
}

func resetText(g Gauge) string {
	if g.Reset == nil {
		return ""
	}
	d := time.Until(*g.Reset)
	if d <= 0 {
		return "resets now"
	}
	return "resets in " + formatDuration(d)
}

// formatDuration renders a remaining time as e.g. "1h 58m" or "6d 9h".
func formatDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// ---------------------------------------------------------------------------
// Bubble Tea model

type model struct {
	creds    Creds
	panels   []Panel
	width    int
	height   int
	last     time.Time
	now      time.Time
	loading  bool
	interval time.Duration
	// gen identifies the current refresh chain. Every manual refresh starts a
	// new one, so the timer armed by the previous chain is ignored instead of
	// running a second fetch loop in parallel — without it each "r" press
	// permanently doubles the auto-refresh rate.
	gen int
	// static marks a one-shot render (-once): no key hints, no refresh timer.
	static         bool
	scroll         int
	anthPrompt     bool
	anthRefreshing bool
	anthAsked      string
	anthNotice     string
	anthSource     string
	// aliPrompt/aliLogining drive the bl console login offer: when the Alibaba
	// panel reports an expired console session, the user can run the
	// interactive `bl auth login` without leaving the dashboard.
	aliPrompt   bool
	aliLogining bool
	aliPrompted bool
	aliNotice   string
	// admin is the embedded admin screen ("A"). While it is open it owns the
	// whole frame; closing it reloads credentials and layout and refetches.
	admin *adminModel
}

type tickMsg struct{ gen int }
type clockMsg time.Time
type alibabaLoginMsg struct{ err error }
type adminCloseMsg struct{}
type fetchedMsg struct {
	panels     []Panel
	creds      Creds
	anthSource string
	at         time.Time
	gen        int
}

func initialModel(c Creds) model {
	return model{creds: c, loading: true, interval: 90 * time.Second, now: time.Now()}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmd(m.creds, m.gen), clockCmd())
}

var reloadCreds = resolveCreds

// alibabaLoginCmd runs the interactive bl console login in the foreground.
// tea.ExecProcess suspends the dashboard, attaches the terminal to bl (which
// opens the browser), and resumes the TUI when the command exits.
func alibabaLoginCmd(site string) tea.Cmd {
	c := exec.Command("bl", "auth", "login", "--console", "--config", "default", "--console-site", site)
	return tea.ExecProcess(c, func(err error) tea.Msg { return alibabaLoginMsg{err} })
}

func fetchCmd(c Creds, gen int) tea.Cmd {
	return func() tea.Msg {
		// OAuth tokens can rotate while the dashboard is running.
		// Re-read the existing sources; never refresh or write credentials.
		fresh, src := reloadCreds()
		return fetchedMsg{panels: fetchAll(fresh), creds: fresh, anthSource: src["anthropic"], at: time.Now(), gen: gen}
	}
}

func clockCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockMsg(t) })
}

func refreshTick(d time.Duration, gen int) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg{gen: gen} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.admin != nil {
		switch msg := msg.(type) {
		case adminCloseMsg:
			m.admin = nil
			m.loading = true
			m.gen++
			m.scroll = 0
			return m, fetchCmd(m.creds, m.gen)
		case tea.WindowSizeMsg:
			m.width, m.height = msg.Width, msg.Height
			m.admin.width, m.admin.height = msg.Width, msg.Height
			return m, tea.ClearScreen
		case clockMsg:
			// Keep the clock chain alive; the admin screen pauses fetching.
			m.now = time.Time(msg)
			return m, clockCmd()
		case tickMsg:
			return m, nil
		case fetchedMsg:
			if msg.gen == m.gen {
				m.panels, m.creds, m.anthSource = msg.panels, msg.creds, msg.anthSource
				m.last, m.loading = msg.at, false
			}
			return m, nil
		}
		next, cmd := m.admin.Update(msg)
		if am, ok := next.(*adminModel); ok {
			m.admin = am
		}
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Repaint from scratch: a shrunk window otherwise keeps stale rows
		// from the taller frame on screen.
		return m, tea.ClearScreen
	case tea.KeyMsg:
		if m.anthPrompt {
			switch msg.String() {
			case "y":
				m.anthPrompt, m.anthRefreshing = false, true
				m.gen++
				return m, anthropicRefreshCmd(m.creds.AnthToken, m.anthSource)
			case "n", "enter", "esc":
				m.anthPrompt = false
				m.anthNotice = "Refresh declined. Open Claude Code to renew the token."
				return m, refreshTick(m.interval, m.gen)
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}
		if m.aliPrompt {
			switch msg.String() {
			case "y":
				m.aliPrompt = false
				m.aliLogining = true
				site, _, siteErr := alibabaSite(m.creds.AliBaseURL)
				if siteErr != nil {
					site = "international"
				}
				return m, alibabaLoginCmd(site)
			case "n", "enter", "esc":
				m.aliPrompt = false
				m.aliNotice = "Alibaba login declined. Renew it later with: bl auth login --console --config default"
				return m, refreshTick(m.interval, m.gen)
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}
		if m.anthRefreshing && msg.String() != "ctrl+c" {
			return m, nil
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading = true
			m.gen++
			return m, fetchCmd(m.creds, m.gen)
		case "a", "A":
			m.admin = newAdminModel()
			m.admin.embedded = true
			m.admin.width, m.admin.height = m.width, m.height
			m.scroll = 0
			return m, m.admin.Init()
		case "j", "down":
			m.scroll += 3
		case "k", "up":
			m.scroll -= 3
			if m.scroll < 0 {
				m.scroll = 0
			}
		case "pgdown":
			m.scroll += m.height - 4
		case "pgup":
			m.scroll -= m.height - 4
			if m.scroll < 0 {
				m.scroll = 0
			}
		case "home":
			m.scroll = 0
		}
	case tea.MouseMsg:
		if msg.Type == tea.MouseWheelUp {
			m.scroll -= 3
			if m.scroll < 0 {
				m.scroll = 0
			}
		} else if msg.Type == tea.MouseWheelDown {
			m.scroll += 3
		}
	case fetchedMsg:
		if msg.gen != m.gen {
			return m, nil // superseded by a manual refresh
		}
		m.panels = msg.panels
		m.creds, m.anthSource = msg.creds, msg.anthSource
		if !m.static && !m.anthRefreshing {
			for _, p := range m.panels {
				if p.Name == "Anthropic" && anthropicExpired(p.Err) && m.creds.AnthToken != m.anthAsked {
					m.anthAsked = m.creds.AnthToken
					if supportedAnthropicSource(m.anthSource) {
						m.anthPrompt = true
						m.anthNotice = ""
					} else {
						m.anthNotice = "Anthropic token expired. Renew it in its source; copied tokens cannot be refreshed."
					}
				}
			}
		}
		if !m.static && !m.anthPrompt && !m.aliLogining {
			for _, p := range m.panels {
				if p.Name != "Alibaba" {
					continue
				}
				if p.Err == nil {
					m.aliPrompted = false // session healthy again
				} else if errors.Is(p.Err, errAlibabaLogin) && !m.aliPrompted {
					m.aliPrompted = true // ask once per expiry, not every 90s
					m.aliNotice = ""
					m.aliPrompt = true
				}
			}
		}
		m.last = msg.at
		m.loading = false
		if m.anthPrompt || m.aliPrompt {
			return m, nil
		}
		return m, refreshTick(m.interval, m.gen)
	case alibabaLoginMsg:
		m.aliLogining = false
		if msg.err != nil {
			m.aliNotice = "Alibaba login failed: " + msg.err.Error()
			return m, refreshTick(m.interval, m.gen)
		}
		m.aliNotice = "Alibaba console logged in."
		m.loading = true
		return m, fetchCmd(m.creds, m.gen)
	case anthropicRefreshMsg:
		m.anthRefreshing = false
		if msg.err != nil {
			m.anthNotice = "Anthropic refresh failed: " + msg.err.Error()
			return m, refreshTick(m.interval, m.gen)
		}
		m.anthNotice = "Anthropic token renewed."
		m.loading = true
		return m, fetchCmd(m.creds, m.gen)
	case tickMsg:
		if msg.gen != m.gen || m.anthPrompt || m.aliPrompt || m.aliLogining || m.anthRefreshing {
			return m, nil // timer from an abandoned refresh chain
		}
		return m, fetchCmd(m.creds, m.gen)
	case clockMsg:
		m.now = time.Time(msg)
		return m, clockCmd()
	}
	return m, nil
}

func fmtClock(t time.Time) string {
	return t.Format("15:04:05")
}

func (m model) View() string {
	if m.admin != nil {
		return m.admin.View()
	}
	w := m.width
	if w <= 0 {
		w = 101
	}
	// Never fill the terminal's last column: a line exactly as wide as the
	// window triggers the auto-margin wrap, and the wrapped tail survives as
	// a duplicate row when the window is resized.
	w--
	if w > maxFrame {
		w = maxFrame
	}
	if w < 20 {
		w = 20
	}
	var b strings.Builder

	left := headerStyle.Render("⚡ aimeter")
	if w >= 46 {
		left += dimStyle.Render(" · AI service status")
	}
	right := dimStyle.Render(fmtClock(m.now))
	b.WriteString(placeBetween(left, right, w))
	b.WriteString("\n\n")

	if !m.static {
		message := m.anthNotice
		switch {
		case m.anthPrompt:
			message = "Anthropic access token expired. Refresh it now?\nWARNING: This changes Claude Code credentials. A concurrent refresh may invalidate its login. Close Claude Code first.\ny refresh / n cancel (default: no)"
		case m.anthRefreshing:
			message = "Renewing Anthropic token..."
		case m.aliLogining:
			message = "Logging in to the Alibaba console (finish in the browser)..."
		case m.aliPrompt:
			message = "Alibaba console session expired. Log in with bl? This opens a browser.\ny login / n cancel (default: no)"
		case m.aliNotice != "":
			message = m.aliNotice
		}
		if message != "" {
			b.WriteString(lipgloss.NewStyle().Width(w).Render(message) + "\n\n")
		}
		if m.anthPrompt || m.anthRefreshing || m.aliPrompt || m.aliLogining {
			return strings.TrimRight(b.String(), "\n")
		}
	}
	head := b.String() // pinned header: title, blank line, optional notice
	lay := computeLayout(m.panels, w)
	var sections []string
	for _, p := range m.panels {
		sections = append(sections, renderPanel(p, lay))
	}
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("238")).
		Render(strings.Repeat("─", lay.inner-2))
	b.WriteString(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(0, 1).
		Width(lay.inner).
		Render(strings.Join(sections, "\n"+sep+"\n")))
	b.WriteString("\n")

	if m.loading && m.last.IsZero() {
		b.WriteString(dimStyle.Render("  loading…") + "\n")
	}

	// -once has no key handling and no timer, so its footer states only when
	// the snapshot was taken.
	if m.static {
		if !m.last.IsZero() && w >= 34 {
			b.WriteString(placeBetween("", dimStyle.Render("snapshot "+fmtClock(m.last)), w))
		}
		return strings.TrimRight(b.String(), "\n")
	}

	footer := dimStyle.Render(footerKeys(w))
	var fr string
	if !m.last.IsZero() {
		left_ := int(m.interval.Seconds()) - int(time.Since(m.last).Seconds())
		if left_ < 0 {
			left_ = 0
		}
		// Narrow terminals only get the clock; the full sentence would wrap.
		switch {
		case w >= 70:
			fr = dimStyle.Render(fmt.Sprintf("auto-refresh in %ds · updated %s", left_, fmtClock(m.last)))
		case w >= 34:
			fr = dimStyle.Render(fmt.Sprintf("%ds · %s", left_, fmtClock(m.last)))
		}
	}
	foot := placeBetween(footer, fr, w)

	// Scroll: when the panels exceed the terminal height, the body scrolls
	// between the pinned header and footer instead of losing the tail.
	if m.height <= 0 {
		b.WriteString(foot)
		return strings.TrimRight(b.String(), "\n")
	}
	headLines := strings.Count(head, "\n")
	body := strings.TrimRight(b.String()[len(head):], "\n")
	var bodyLines []string
	if body != "" {
		bodyLines = strings.Split(body, "\n")
	}
	visible := m.height - headLines - 1 // -1 for the footer line
	if visible < 1 {
		visible = 1
	}
	maxScroll := len(bodyLines) - visible
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	end := m.scroll + visible
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	return head + strings.Join(bodyLines[m.scroll:end], "\n") + "\n" + foot
}

// footerKeys shortens the key hints to whatever fits next to the status text.
func footerKeys(w int) string {
	switch {
	case w >= 70:
		return styledKeys("r refresh · a admin · q quit")
	case w >= 60:
		return styledKeys("r · a admin · q quit")
	default:
		return styledKeys("a admin · q quit")
	}
}

// styledKeys highlights the key letters so the hints read as actionable.
func styledKeys(s string) string {
	parts := strings.Split(s, " · ")
	for i, p := range parts {
		fields := strings.SplitN(p, " ", 2)
		if len(fields) == 2 {
			parts[i] = keyStyle.Render(fields[0]) + dimStyle.Render(" "+fields[1])
		} else {
			parts[i] = keyStyle.Render(p)
		}
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

func placeBetween(l, r string, w int) string {
	gap := w - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		gap = 1
	}
	return l + strings.Repeat(" ", gap) + r
}

// frameLayout is computed once for the whole frame so every bar starts and
// ends in the same column, whatever a single panel's captions happen to say.
type frameLayout struct {
	inner   int // panel box width
	content int // usable column inside border + padding
	label   int // label column
	bar     int // bar width, identical in every row
	extras  int // caption column after the percentage; 0 hides captions
}

const (
	rowIndent    = 2
	rowPct       = 5  // leading space plus up to "100%"
	preferredBar = 26 // captions never squeeze the bar below this
	minBar       = 8
	maxFrame     = 220 // beyond this the eye can no longer track a row
)

// computeLayout sizes the columns from the frame width and the widest caption
// run any gauge actually needs, so short captions never leave the bars ragged
// and one verbose row never shrinks every other bar.
func computeLayout(panels []Panel, w int) frameLayout {
	inner := w - 4
	if inner < 20 {
		inner = 20
	}
	l := frameLayout{inner: inner, content: inner - 2, label: 8}
	if l.content < 32 {
		l.label = 5
	}

	budget := l.content - rowIndent - l.label - rowPct
	if budget < 3 {
		budget = 3
	}
	l.bar = budget

	want := 0
	for _, p := range panels {
		if p.Err != nil {
			continue
		}
		for _, g := range p.Items {
			if g.Used < 0 {
				continue
			}
			if n := lipgloss.Width(captionText(g)); n > want {
				want = n
			}
		}
	}
	if want == 0 || budget < minBar+8 {
		return l // no captions to show, or no room for them at all
	}

	// +1 for the space between the percentage and the caption. Captions take
	// exactly what they need; every remaining cell grows the bars, so a wide
	// terminal shows wide bars instead of a wide empty gutter.
	l.extras = min(want+1, budget-preferredBar)
	if l.extras < 0 {
		l.extras = 0
	}
	l.bar = budget - l.extras
	return l
}

// captionText joins a gauge's detail and reset text the way a row renders them.
func captionText(g Gauge) string {
	parts := []string{}
	if g.Detail != "" {
		parts = append(parts, g.Detail)
	}
	if rt := resetText(g); rt != "" {
		parts = append(parts, rt)
	}
	return strings.Join(parts, "  ")
}

func renderPanel(p Panel, lay frameLayout) string {
	_, isErr := panelColor(p)
	content := lay.content

	dot := okDotStyle.Render("●") + " "
	if isErr {
		dot = errStyle.Render("●") + " "
	}
	title := dot + nameStyle.Render(truncateCells(p.Name, content-2))
	// Keep the cost visible even when the plan name does not fit.
	if p.Cost != "" && lipgloss.Width(title)+2+lipgloss.Width(p.Cost) <= content {
		if p.Note != "" && lipgloss.Width(title)+4+lipgloss.Width(p.Note)+lipgloss.Width(p.Cost) <= content {
			title += dimStyle.Render("  " + p.Note)
		}
		title = placeBetween(title, dimStyle.Render(p.Cost), content)
	} else if p.Note != "" && lipgloss.Width(title)+2+lipgloss.Width(p.Note) <= content {
		title += dimStyle.Render("  " + p.Note)
	}
	title = lipgloss.NewStyle().MaxWidth(content).Width(content).Render(title)

	var body string
	if p.Err != nil {
		body = errStyle.Render("  ✗ " + truncateCells(p.Err.Error(), content-4))
	} else {
		var rows []string
		for _, g := range p.Items {
			if g.Used < 0 {
				rows = append(rows, lipgloss.NewStyle().MaxWidth(content).Width(content).Render("  "+
					lipgloss.NewStyle().Width(lay.label).Render(truncateCells(g.Label, lay.label))+
					dimStyle.Render(truncateCells(g.Detail, content-rowIndent-lay.label))))
				continue
			}
			c := pctColor(g.Used)
			line := lipgloss.NewStyle().Width(lay.label).Render(truncateCells(g.Label, lay.label)) +
				bar(g.Used, lay.bar, c) + " " +
				pctStyle.Foreground(c).Render(fmt.Sprintf("%3.0f%%", clampPct(g.Used)))
			if lay.extras > 0 {
				if cap := truncateCells(captionText(g), lay.extras-1); cap != "" {
					line += " " + dimStyle.Render(cap)
				}
			}
			rows = append(rows, lipgloss.NewStyle().MaxWidth(content).Width(content).Render("  "+line))
			if tp, ok := timeElapsedPct(g); ok {
				row := strings.Repeat(" ", lay.label) + thinBar(tp, lay.bar) + " " +
					timePctStyle.Render(fmt.Sprintf("%3.0f%%", tp))
				// The elapsed caption rides in the same column as the others;
				// include the elapsed time when the column has room for it.
				if caption := elapsedCaption(g, lay.extras); caption != "" {
					row += " " + dimStyle.Render(caption)
				}
				rows = append(rows, lipgloss.NewStyle().MaxWidth(content).Width(content).Render("  "+row))
			}
		}
		body = strings.Join(rows, "\n")
	}

	box := title + "\n" + body
	return box
}

func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// truncateCells cuts plain text to at most n printable cells, so a long name
// or detail gets clipped instead of wrapping onto another row.
func truncateCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}

// thinBar is a half-height bar for window progress — the upper half of the
// row, so it optically continues the usage bar above it.
func thinBar(p float64, width int) string {
	filled := int(math.Round(p / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return timeFill.Render(strings.Repeat("▀", filled)) +
		timeEmpty.Render(strings.Repeat("▀", width-filled))
}

func timeElapsedPct(g Gauge) (float64, bool) {
	if g.Window <= 0 || g.Reset == nil {
		return 0, false
	}
	remain := time.Until(*g.Reset)
	pct := 100 * (1 - remain.Minutes()/g.Window.Minutes())
	return clampPct(pct), true
}

// elapsedCaption renders the window-progress caption. The elapsed time rides
// along when the caption column is wide enough; otherwise fall back to the
// bare label, then to nothing on very narrow frames.
func elapsedCaption(g Gauge, extras int) string {
	elapsed := g.Window - time.Until(*g.Reset)
	if elapsed < 0 {
		elapsed = 0
	}
	if long := "elapsed " + formatDuration(elapsed); extras >= lipgloss.Width(long)+1 {
		return long
	}
	if extras >= 9 {
		return "elapsed"
	}
	return ""
}

// ---------------------------------------------------------------------------

// version is overridden at release time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	once := flag.Bool("once", false, "print the dashboard once and exit (no TUI)")
	plain := flag.Bool("plain", false, "print one plain-text line per gauge (no TUI, no colors; for scripts)")
	jsonOut := flag.Bool("json", false, "print all panels as machine-readable JSON (no TUI)")
	show := flag.Bool("show-creds", false, "print where each provider's credentials resolve from and exit")
	admin := flag.Bool("admin", false, "open the admin screen (services and provider credentials) and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("aimeter", version)
		return
	}

	// First run with no config: probe the environment and keychain, persist
	// what is found, and tell the user where it went. Later runs never
	// overwrite an existing file.
	if created, found := firstRunDetect(); created {
		fmt.Fprintf(os.Stderr, "aimeter: detected credentials for %s — wrote %s (manage them with: aimeter -admin)\n",
			strings.Join(found, ", "), configPath)
	}

	if *admin {
		if err := runAdmin(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	creds, src := resolveCreds()

	if *show {
		printCredSources(src, creds.Custom)
		return
	}

	// Auto-detect non-interactive contexts (pipes, CI, cron, deployment
	// scripts): a TUI cannot run without a terminal, so default to plain.
	if !*plain && !*jsonOut && !*once && !*show && !isInteractive() {
		*plain = true
	}

	if *plain || *jsonOut {
		panels := fetchAll(creds)
		if *jsonOut {
			if err := renderJSON(os.Stdout, panels); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			renderPlain(os.Stdout, panels)
		}
		return
	}

	if *once {
		lipgloss.SetColorProfile(termenv.TrueColor)
		panels := fetchAll(creds)
		m := initialModel(creds)
		m.panels = panels
		m.last = time.Now()
		m.loading = false
		m.now = time.Now()
		// Bubble Tea would deliver a WindowSizeMsg; without the event loop the
		// size has to be asked for, or -once always prints a 100-column frame.
		m.width = terminalWidth()
		m.static = true
		fmt.Println(m.View())
		return
	}

	if _, err := tea.NewProgram(initialModel(creds), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// isInteractive reports whether stdin and stdout are both terminals — the TUI
// needs both. Redirected output or a non-TTY stdin (pipes, files, /dev/null,
// CI, cron) means plain output instead.
func isInteractive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// terminalWidth reports the width of the output terminal, or 0 when stdout is
// not a terminal, which makes View fall back to its default frame.
func terminalWidth() int {
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		return 0
	}
	return w
}
