package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Data model

type Gauge struct {
	Label  string  // e.g. "5h", "7 dní"
	Used   float64 // 0..100
	Reset  *time.Time
	Detail string
	Window time.Duration // dĺžka okna; >0 a Reset → kreslí sa aj časový bar
}

type Panel struct {
	Name    string
	Note    string
	Items   []Gauge
	Err     error
	Updated time.Time
}

type Creds struct {
	AnthToken  string
	AnthEmail  string
	CodexToken string
	CodexAcct  string
	ZaiKey     string
	MmKey      string
	OpenRouter string
	EleKey     string
	MeshyKey   string
}

// ---------------------------------------------------------------------------
// Credentials

func ompDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omp", "agent", "agent.db")
}

func loadCreds() Creds {
	var c Creds
	// omp stores provider credentials in its agent db (SQLite, WAL).
	if rows := ompCredentials(ompDBPath()); rows != nil {
		if d, ok := rows["anthropic"].(map[string]any); ok {
			c.AnthToken, _ = d["access"].(string)
			c.AnthEmail, _ = d["email"].(string)
		}
		if d, ok := rows["zai"].(map[string]any); ok {
			c.ZaiKey, _ = d["key"].(string)
		}
		if d, ok := rows["minimax-code"].(map[string]any); ok {
			c.MmKey, _ = d["key"].(string)
		}
	}
	// Fallback for Anthropic: Claude Code keychain entry.
	if c.AnthToken == "" {
		if out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output(); err == nil {
			var kc struct {
				ClaudeAiOauth struct {
					AccessToken string `json:"accessToken"`
				} `json:"claudeAiOauth"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &kc) == nil {
				c.AnthToken = kc.ClaudeAiOauth.AccessToken
			}
		}
	}
	// Codex: its own auth.json.
	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json")); err == nil {
		var a struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
				AccountID   string `json:"account_id"`
			} `json:"tokens"`
		}
		if json.Unmarshal(b, &a) == nil {
			c.CodexToken = a.Tokens.AccessToken
			c.CodexAcct = a.Tokens.AccountID
		}
	}
	c.OpenRouter = findOpenRouterKey(home)
	// Extra services (ElevenLabs, Meshy): ~/.config/aimeter/keys, KEY=VALUE per line.
	if b, err := os.ReadFile(filepath.Join(home, ".config", "aimeter", "keys")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "ELEVENLABS_API_KEY":
				if c.EleKey == "" {
					c.EleKey = strings.TrimSpace(v)
				}
			case "MESHY_API_KEY":
				if c.MeshyKey == "" {
					c.MeshyKey = strings.TrimSpace(v)
				}
			}
		}
	}
	return c
}

func sqlOpenRO(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}

func ompCredentials(path string) map[string]any {
	db, err := sqlOpenRO(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	rws, err := db.Query("SELECT provider, data FROM auth_credentials")
	if err != nil {
		return nil
	}
	defer rws.Close()
	out := map[string]any{}
	for rws.Next() {
		var provider, data string
		if rws.Scan(&provider, &data) != nil {
			continue
		}
		var v any
		if json.Unmarshal([]byte(data), &v) == nil {
			out[provider] = v
		}
	}
	return out
}

func findOpenRouterKey(home string) string {
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		return k
	}
	re := regexp.MustCompile(`OPENROUTER_API_KEY[^\w-]*["']?(sk-or-[A-Za-z0-9_-]+)`)
	for _, f := range []string{".zshrc", ".zshenv", ".zprofile", filepath.Join(".codex", "config.toml")} {
		b, err := os.ReadFile(filepath.Join(home, f))
		if err != nil {
			continue
		}
		if m := re.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// HTTP + JSON

var httpc = &http.Client{Timeout: 15 * time.Second}

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
		return fmt.Errorf("HTTP %d", resp.StatusCode)
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
		return nil, "", fmt.Errorf("chýba credential — prihlás sa v omp/claude")
	}
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
		return nil, "", err
	}
	var gs []Gauge
	add := func(label string, b *anthBucket, window time.Duration) {
		if b != nil {
			gs = append(gs, Gauge{Label: label, Used: b.Utilization, Reset: b.ResetsAt, Window: window})
		}
	}
	add("5h", j.FiveHour, 5*time.Hour)
	add("7 dní", j.SevenDay, 7*24*time.Hour)
	add("opus 7d", j.SevenDayOpus, 7*24*time.Hour)
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("prázdna odpoveď")
	}
	note := ""
	if c.AnthEmail != "" {
		note = c.AnthEmail
	}
	return gs, note, nil
}

func fetchCodex(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.CodexToken == "" {
		return nil, "", fmt.Errorf("chýba ~/.codex/auth.json")
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
		return nil, "", fmt.Errorf("žiadne okná v odpovedi")
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
		return "7 dní"
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
		return nil, "", fmt.Errorf("chýba ZAI API key (omp)")
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
			Detail: fmt.Sprintf("%.0f / %.0f kreditov", l.CurrentValue, l.Usage),
		})
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("žiadne limity")
	}
	note := ""
	if j.Data.Level != "" {
		note = "GLM Coding " + strings.ToUpper(j.Data.Level[:1]) + j.Data.Level[1:]
	}
	return gs, note, nil
}

func zaiLabel(unit, number int) string {
	switch {
	case unit == 3 && number == 5:
		return "5h"
	case unit == 6:
		return "7 dní"
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

func fetchMinimax(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.MmKey == "" {
		return nil, "", fmt.Errorf("chýba MiniMax API key (omp)")
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
			gs = append(gs, Gauge{Label: "7 dní", Used: 100 - m.WeeklyLeft, Reset: &t, Window: win})
		}
	}
	if len(gs) == 0 {
		return nil, "", fmt.Errorf("žiadny 'general' plán")
	}
	return gs, "Coding Plan", nil
}

func fetchOpenRouter(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.OpenRouter == "" {
		return nil, "", fmt.Errorf("chýba OPENROUTER_API_KEY")
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
		return nil, "", fmt.Errorf("žiadne credits")
	}
	used := d.TotalUsage / d.TotalCredits * 100
	return []Gauge{{
		Label:  "kredit",
		Used:   used,
		Detail: fmt.Sprintf("$%.2f / $%.2f · zostáva $%.2f", d.TotalUsage, d.TotalCredits, d.TotalCredits-d.TotalUsage),
	}}, "pay-as-you-go", nil
}

func fetchElevenLabs(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.EleKey == "" {
		return nil, "", fmt.Errorf("chýba ELEVENLABS_API_KEY (~/.config/aimeter/keys)")
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
	pct := 0.0
	if j.CharacterLimit > 0 {
		pct = float64(j.CharacterCount) / float64(j.CharacterLimit) * 100
	}
	return []Gauge{{
		Label:  "znaky",
		Used:   pct,
		Reset:  rp,
		Window: 30 * 24 * time.Hour, // mesačný cyklus (approx)
		Detail: fmt.Sprintf("%d / %d znakov", j.CharacterCount, j.CharacterLimit),
	}}, strings.ToUpper(j.Tier[:1]) + j.Tier[1:], nil
}

func fetchMeshy(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.MeshyKey == "" {
		return nil, "", fmt.Errorf("chýba MESHY_API_KEY (~/.config/aimeter/keys)")
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
		Label:  "kredity",
		Used:   -1,
		Detail: fmt.Sprintf("%.0f kreditov zostáva", j.Balance),
	}}, "3D generovanie", nil
}

// ---------------------------------------------------------------------------
// Fetch orchestration

func fetchAll(c Creds) []Panel {
	type def struct {
		name string
		fn   func(context.Context, Creds) ([]Gauge, string, error)
	}
	defs := []def{
		{"Anthropic", fetchAnthropic},
		{"OpenAI Codex", fetchCodex},
		{"Z.AI", fetchZai},
		{"MiniMax Code", fetchMinimax},
		{"OpenRouter", fetchOpenRouter},
		{"ElevenLabs", fetchElevenLabs},
		{"Meshy", fetchMeshy},
	}
	panels := make([]Panel, len(defs))
	var wg sync.WaitGroup
	for i, d := range defs {
		wg.Add(1)
		go func(i int, d def) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			items, note, err := d.fn(ctx, c)
			p := Panel{Name: d.name, Updated: time.Now()}
			if err != nil {
				p.Err = err
			} else {
				p.Items, p.Note = items, note
			}
			panels[i] = p
		}(i, d)
	}
	wg.Wait()
	return panels
}

// ---------------------------------------------------------------------------
// Styling

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
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
	filled := int(mathRound(p / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Repeat("░", width-filled))
}

func mathRound(v float64) float64 {
	if v < 0 {
		return -float64(int(-v + 0.5))
	}
	return float64(int(v + 0.5))
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
		return "reset teraz"
	}
	var s string
	switch {
	case d >= 48*time.Hour:
		s = fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		s = fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return "reset za " + s
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
}

type tickMsg struct{}
type clockMsg time.Time
type fetchedMsg struct {
	panels []Panel
	at     time.Time
}

func initialModel(c Creds) model {
	return model{creds: c, loading: true, interval: 90 * time.Second, now: time.Now()}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmd(m.creds), clockCmd())
}

func fetchCmd(c Creds) tea.Cmd {
	return func() tea.Msg {
		return fetchedMsg{panels: fetchAll(c), at: time.Now()}
	}
}

func clockCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockMsg(t) })
}

func refreshTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, fetchCmd(m.creds)
		}
	case fetchedMsg:
		m.panels = msg.panels
		m.last = msg.at
		m.loading = false
		return m, refreshTick(m.interval)
	case tickMsg:
		return m, fetchCmd(m.creds)
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
	w := m.width
	if w <= 0 || w > 100 {
		w = 100
	}
	var b strings.Builder

	left := headerStyle.Render("⚡ aimeter") + dimStyle.Render(" · stav AI služieb")
	right := dimStyle.Render(fmtClock(m.now))
	b.WriteString(placeBetween(left, right, w))
	b.WriteString("\n\n")

	for _, p := range m.panels {
		b.WriteString(renderPanel(p, w))
		b.WriteString("\n")
	}

	if m.loading && m.last.IsZero() {
		b.WriteString(dimStyle.Render("  načítavam…") + "\n")
	}

	footer := dimStyle.Render("r obnoviť · q koniec")
	var fr string
	if !m.last.IsZero() {
		left_ := int(m.interval.Seconds()) - int(time.Since(m.last).Seconds())
		if left_ < 0 {
			left_ = 0
		}
		fr = dimStyle.Render(fmt.Sprintf("auto-obnova za %ds · aktualizované %s", left_, fmtClock(m.last)))
	}
	b.WriteString(placeBetween(footer, fr, w))
	return b.String()
}

func placeBetween(l, r string, w int) string {
	gap := w - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		gap = 1
	}
	return l + strings.Repeat(" ", gap) + r
}

func renderPanel(p Panel, w int) string {
	color, isErr := panelColor(p)
	inner := w - 4
	if inner < 40 {
		inner = 40
	}

	title := okDotStyle.Render("●") + " "
	if isErr {
		title = errStyle.Render("●") + " "
	}
	title += nameStyle.Render(p.Name)
	if p.Note != "" {
		title += dimStyle.Render("  " + p.Note)
	}
	title = lipgloss.NewStyle().Width(inner).Render(title)

	var body string
	if p.Err != nil {
		body = errStyle.Render("  ✗ " + p.Err.Error())
	} else {
		var rows []string
		for _, g := range p.Items {
			if g.Used < 0 {
				rows = append(rows, lipgloss.NewStyle().Width(inner).Render("  "+
					lipgloss.NewStyle().Width(8).Render(g.Label)+dimStyle.Render(g.Detail)))
				continue
			}
			c := pctColor(g.Used)
			line := lipgloss.NewStyle().Width(8).Render(g.Label) +
				bar(g.Used, 26, c) + " " +
				pctStyle.Foreground(c).Render(fmt.Sprintf("%3.0f%%", clampPct(g.Used)))
			extras := []string{}
			if g.Detail != "" {
				extras = append(extras, dimStyle.Render(g.Detail))
			}
			if rt := resetText(g); rt != "" {
				extras = append(extras, dimStyle.Render(rt))
			}
			if len(extras) > 0 {
				line += " " + strings.Join(extras, "  ")
			}
			rows = append(rows, lipgloss.NewStyle().Width(inner).Render("  "+line))
			if tp, ok := timeElapsedPct(g); ok {
				rows = append(rows, lipgloss.NewStyle().Width(inner).Render("  "+
					strings.Repeat(" ", 8)+
					thinBar(tp, 26)+" "+
					timePctStyle.Render(fmt.Sprintf("%3.0f%%", tp))+
					dimStyle.Render(" času")))
			}
		}
		body = strings.Join(rows, "\n")
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Padding(0, 1).
		Width(inner).
		Render(title + "\n" + body)
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

// thinBar je polovičný (nízky) bar pre priebeh času v okne — horná polovica
// riadku, takže opticky nadväzuje na usage bar nad ním.
func thinBar(p float64, width int) string {
	filled := int(mathRound(p / 100 * float64(width)))
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

// ---------------------------------------------------------------------------

func main() {
	once := flag.Bool("once", false, "vypíš dashboard raz a skonči (bez TUI)")
	flag.Parse()

	creds := loadCreds()

	if *once {
		lipgloss.SetColorProfile(termenv.TrueColor)
		panels := fetchAll(creds)
		m := initialModel(creds)
		m.panels = panels
		m.last = time.Now()
		m.loading = false
		m.now = time.Now()
		fmt.Println(m.View())
		return
	}

	if _, err := tea.NewProgram(initialModel(creds), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chyba:", err)
		os.Exit(1)
	}
}
