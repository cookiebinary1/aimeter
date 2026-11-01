package main

// Token Plan inference keys do not expose usage through DashScope's API.
// Quotas use the authenticated console gateway, as in OMP's Token Plan provider:
// https://github.com/can1357/oh-my-pi/blob/95db18ef9936bff96fbd33268b2363ab25a9bb9d/packages/ai/src/usage/alibaba-token-plan.ts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	alibabaInternationalURL = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
	alibabaChinaURL         = "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"
	alibabaUsageAPI         = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/usage"
	alibabaUserAgent        = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
)

var alibabaSecurityToken = regexp.MustCompile(`\bSEC_TOKEN\s*:\s*"([^"]+)"`)

func fetchAlibaba(ctx context.Context, c Creds) ([]Gauge, string, error) {
	if c.AliKey == "" {
		return nil, "", fmt.Errorf("missing ALIBABA_API_KEY (env / credentials.json / omp)")
	}
	if strings.TrimSpace(c.AliCookie) == "" {
		return fetchAlibabaCLI(ctx, c)
	}

	origin := "https://home.qwencloud.com"
	sessionURL := origin + "/tool/user/info.json"
	dashboardURL := origin + "/billing/subscription/token-plan-individual"
	gateway := "https://cs-data.qwencloud.com/data/api.json"
	action, region := "IntlBroadScopeAspnGateway", "ap-southeast-1"
	var cornerstone map[string]any
	china := false
	switch strings.TrimRight(strings.TrimSpace(c.AliBaseURL), "/") {
	case "", alibabaInternationalURL:
		cornerstone = map[string]any{
			"domain": "home.qwencloud.com", "consoleSite": "QWENCLOUD", "console": "ONE_CONSOLE",
			"xsp_lang": "en-US", "protocol": "V2", "productCode": "p_efm",
		}
	case alibabaChinaURL:
		china = true
		origin = "https://bailian.console.aliyun.com"
		sessionURL = origin + "/cn-beijing?tab=plan"
		dashboardURL = sessionURL
		gateway = "https://bailian-cs.console.aliyun.com/data/api.json"
		action, region = "BroadScopeAspnGateway", "cn-beijing"
		var trace [16]byte
		if _, err := rand.Read(trace[:]); err != nil {
			return nil, "", err
		}
		trace[6], trace[8] = trace[6]&0x0f|0x40, trace[8]&0x3f|0x80
		cornerstone = map[string]any{
			"feTraceId": fmt.Sprintf("%x-%x-%x-%x-%x", trace[:4], trace[4:6], trace[6:8], trace[8:10], trace[10:]),
			"feURL":     sessionURL + "#/efm/subscription/token-plan/personal", "protocol": "V2",
			"console": "ONE_CONSOLE", "productCode": "p_efm", "switchUserType": 3,
			"domain": "bailian.console.aliyun.com", "consoleSite": "BAILIAN_ALIYUN",
			"userNickName": "", "userPrincipalName": "", "xsp_lang": "zh-CN",
		}
	default:
		return nil, "", fmt.Errorf("unsupported Token Plan base_url; use the Singapore or Beijing endpoint")
	}

	secToken, err := alibabaSession(ctx, sessionURL, origin, c.AliCookie, china)
	if err != nil {
		return nil, "", err
	}
	params, err := json.Marshal(map[string]any{
		"Api":  alibabaUsageAPI,
		"Data": map[string]any{"cornerstoneParam": cornerstone},
		"V":    "1.0",
	})
	if err != nil {
		return nil, "", err
	}
	form := url.Values{
		"product": {"sfm_bailian"}, "action": {action}, "region": {region},
		"sec_token": {secToken}, "params": {string(params)},
	}
	query := url.Values{"product": {"sfm_bailian"}, "action": {action}, "api": {alibabaUsageAPI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway+"?"+query.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Cookie", c.AliCookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", dashboardURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	var csrf string
	for _, cookie := range req.Cookies() {
		if cookie.Name == "login_aliyunid_csrf" && cookie.Value != "" {
			csrf = cookie.Value
			break
		}
		if cookie.Name == "csrf" {
			csrf = cookie.Value
		}
	}
	if csrf != "" {
		req.Header.Set("x-xsrf-token", csrf)
		req.Header.Set("x-csrf-token", csrf)
	}
	resp, err := alibabaRequest(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("invalid Alibaba quota response; renew ALIBABA_COOKIE")
	}
	gauges, err := alibabaGauges(payload)
	return gauges, "Token Plan", err
}

// Console cookies must never follow a redirect to a login page or another host.
func alibabaRequest(req *http.Request) (*http.Response, error) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", alibabaUserAgent)
	client := *httpc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("Alibaba console HTTP %d; renew ALIBABA_COOKIE if session expired", resp.StatusCode)
	}
	return resp, nil
}

func alibabaSession(ctx context.Context, endpoint, origin, cookie string, china bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", origin+"/")
	resp, err := alibabaRequest(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if china {
		html, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", err
		}
		if match := alibabaSecurityToken.FindSubmatch(html); len(match) == 2 {
			return string(match[1]), nil
		}
	} else {
		var payload struct {
			Data struct {
				SecToken string `json:"secToken"`
			} `json:"data"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) == nil && payload.Data.SecToken != "" {
			return payload.Data.SecToken, nil
		}
	}
	return "", fmt.Errorf("Alibaba console session expired; renew ALIBABA_COOKIE")
}

func alibabaGauges(payload map[string]any) ([]Gauge, error) {
	if payload["successResponse"] == false {
		return nil, fmt.Errorf("Alibaba console rejected quota request; renew ALIBABA_COOKIE")
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid Alibaba quota response")
	}
	if data["success"] == false {
		return nil, fmt.Errorf("Alibaba quota access rejected (%v)", data["errorCode"])
	}
	if encoded, ok := data["Data"].(string); ok {
		if err := json.Unmarshal([]byte(encoded), &data); err != nil {
			return nil, fmt.Errorf("invalid Alibaba quota data")
		}
	}
	if v2, ok := data["DataV2"].(map[string]any); ok {
		if nested, ok := v2["data"].(map[string]any); ok {
			data = nested
		}
	}
	if nested, ok := data["data"].(map[string]any); ok {
		data = nested
	}
	windows := [...]struct {
		label, percent, reset string
		span                  time.Duration
	}{
		{"5h", "per5HourPercentage", "per5HourResetTime", 5 * time.Hour},
		{"7d", "per1WeekPercentage", "per1WeekResetTime", 7 * 24 * time.Hour},
		{"monthly", "per1MonthPercentage", "per1MonthResetTime", 0},
	}
	gauges := make([]Gauge, 0, len(windows))
	for _, w := range windows {
		used, ok := toFloat(data[w.percent])
		if !ok || used < 0 || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		if used <= 1 {
			used *= 100
		}
		g := Gauge{Label: w.label, Used: clampPct(used), Window: w.span}
		if reset, ok := toFloat(data[w.reset]); ok && reset > 0 && !math.IsInf(reset, 0) {
			if reset < 1e12 {
				reset *= 1000
			}
			t := time.UnixMilli(int64(reset))
			g.Reset = &t
		}
		gauges = append(gauges, g)
	}
	if len(gauges) == 0 {
		return nil, fmt.Errorf("Alibaba console returned no quota windows")
	}
	return gauges, nil
}

// Use the official CLI's console session without copying its credentials.
var runAlibabaCLI = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "bl", args...).Output()
}

func fetchAlibabaCLI(ctx context.Context, c Creds) ([]Gauge, string, error) {
	site, region := "international", "ap-southeast-1"
	switch strings.TrimRight(strings.TrimSpace(c.AliBaseURL), "/") {
	case "", alibabaInternationalURL:
	case alibabaChinaURL:
		site, region = "domestic", "cn-beijing"
	default:
		return nil, "", fmt.Errorf("unsupported Token Plan base_url; use the Singapore or Beijing endpoint")
	}
	body, err := runAlibabaCLI(ctx, "usage", "token-plan", "--config", "default", "--console-site", site, "--console-region", region, "--output", "json", "--timeout", "10")
	if err != nil {
		// CLI stderr can contain account details. Keep errors actionable and safe.
		return nil, "", fmt.Errorf("quota unavailable: log in with bl auth login --console --config default --console-site %s, or set ALIBABA_COOKIE", site)
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil || data == nil {
		return nil, "", fmt.Errorf("invalid quota response from bl")
	}
	if data["error"] != nil || data["hasQuotaData"] == false {
		return nil, "", fmt.Errorf("bl returned no Token Plan quota; check console account and region")
	}
	gs, err := alibabaGauges(map[string]any{"data": data})
	return gs, "Token Plan · bl", err
}
