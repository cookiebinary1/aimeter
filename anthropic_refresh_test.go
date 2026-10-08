package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type refreshTransport func(*http.Request) (*http.Response, error)

func (f refreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func refreshFixture(t *testing.T) {
	t.Helper()
	isolate(t)
	oldReload, oldTransport := reloadCreds, httpc.Transport
	reloadCreds = func() (Creds, map[string]string) {
		return Creds{AnthToken: "old"}, map[string]string{"anthropic": "claude-code:credentials-file"}
	}
	t.Cleanup(func() { reloadCreds, httpc.Transport = oldReload, oldTransport })
	if err := os.WriteFile(claudeCredsPath, []byte(`{"other":{"keep":true},"claudeAiOauth":{"accessToken":"old","refreshToken":"refresh-old","expiresAt":1,"scopes":["user:profile"],"subscriptionType":"max"}}`), 0600); err != nil {
		t.Fatal(err)
	}
}
func refreshResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestAnthropicRefreshPersistsRotationAndMetadata(t *testing.T) {
	refreshFixture(t)
	httpc.Transport = refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != anthropicTokenURL {
			t.Fatalf("wrong refresh destination: %s %s", r.Method, r.URL)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["refresh_token"] != "refresh-old" || payload["grant_type"] != "refresh_token" || payload["client_id"] != anthropicClientID || payload["scope"] != "user:profile" {
			t.Fatal("incorrect refresh payload")
		}
		return refreshResponse(200, `{"access_token":"new","refresh_token":"refresh-new","expires_in":3600,"scope":"user:profile user:inference"}`), nil
	})
	if err := refreshAnthropicToken(context.Background(), "old", "claude-code:credentials-file"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(claudeCredsPath)
	d, err := parseClaudeCredentials(b)
	if err != nil || d.access != "new" || d.refresh != "refresh-new" || len(d.scopes) != 2 {
		t.Fatal("rotation not saved")
	}
	if string(d.raw["other"]) != `{"keep":true}` || string(d.oauth["subscriptionType"]) != `"max"` {
		t.Fatal("metadata lost")
	}
	var expiry int64
	_ = json.Unmarshal(d.oauth["expiresAt"], &expiry)
	if time.Until(time.UnixMilli(expiry)) < 59*time.Minute {
		t.Fatal("incorrect expiry")
	}
	info, _ := os.Stat(claudeCredsPath)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credentials permissions")
	}
}
func TestAnthropicRefreshRefusesConcurrentRotation(t *testing.T) {
	refreshFixture(t)
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		_ = os.WriteFile(claudeCredsPath, []byte(`{"claudeAiOauth":{"accessToken":"claude-new","refreshToken":"claude-refresh"}}`), 0600)
		return refreshResponse(200, `{"access_token":"new","refresh_token":"refresh-new","expires_in":3600}`), nil
	})
	if err := refreshAnthropicToken(context.Background(), "old", "claude-code:credentials-file"); err == nil {
		t.Fatal("overwrote concurrent rotation")
	}
	b, _ := os.ReadFile(claudeCredsPath)
	d, _ := parseClaudeCredentials(b)
	if d.access != "claude-new" {
		t.Fatal("concurrent credentials lost")
	}
}
func TestAnthropicRefreshFailuresDoNotWriteOrLeak(t *testing.T) {
	for _, body := range []string{`{"access_token":"secret-new"}`, `{"access_token":"secret-new","refresh_token":"secret-refresh","expires_in":-1}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			refreshFixture(t)
			before, _ := os.ReadFile(claudeCredsPath)
			httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) { return refreshResponse(200, body), nil })
			err := refreshAnthropicToken(context.Background(), "old", "claude-code:credentials-file")
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("unsafe error")
			}
			after, _ := os.ReadFile(claudeCredsPath)
			if string(before) != string(after) {
				t.Fatal("changed credentials on failure")
			}
		})
	}
}
func TestAnthropicRefreshSkipsAlreadyRotatedToken(t *testing.T) {
	refreshFixture(t)
	reloadCreds = func() (Creds, map[string]string) {
		return Creds{AnthToken: "new"}, map[string]string{"anthropic": "claude-code:credentials-file"}
	}
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unnecessary refresh request")
		return nil, errors.New("unexpected")
	})
	if err := refreshAnthropicToken(context.Background(), "old", "claude-code:credentials-file"); err != nil {
		t.Fatal(err)
	}
}
func TestAnthropicExpiryRequiresExplicitAPIError(t *testing.T) {
	old := httpc.Transport
	t.Cleanup(func() { httpc.Transport = old })
	for _, tc := range []struct {
		message string
		expired bool
	}{{"OAuth access token has expired. Re-authenticate to continue.", true}, {"Invalid token", false}} {
		httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
			return refreshResponse(401, `{"error":{"message":"`+tc.message+`"}}`), nil
		})
		err := getJSON(context.Background(), "https://api.anthropic.com/api/oauth/usage", nil, nil)
		if anthropicExpired(err) != tc.expired {
			t.Fatalf("expiry detection: %v", err)
		}
	}
}
func TestAnthropicPromptRequiresConsentAndDoesNotRepeat(t *testing.T) {
	msg := fetchedMsg{creds: Creds{AnthToken: "expired"}, anthSource: "claude-code:credentials-file", panels: []Panel{{Name: "Anthropic", Err: &httpStatusError{code: 401, expired: true}}}}
	m := initialModel(Creds{})
	updated, _ := m.Update(msg)
	m = updated.(model)
	if !m.anthPrompt {
		t.Fatal("missing consent prompt")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.anthPrompt || m.anthRefreshing || cmd == nil {
		t.Fatal("enter must decline")
	}
	updated, _ = m.Update(msg)
	m = updated.(model)
	if m.anthPrompt {
		t.Fatal("repeated declined prompt")
	}
	m.anthAsked = ""
	updated, _ = m.Update(msg)
	m = updated.(model)
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(model)
	if !m.anthRefreshing || m.anthPrompt || cmd == nil {
		t.Fatal("confirmation must start renewal")
	}
}
func TestAnthropicPromptFitsNarrowTerminals(t *testing.T) {
	for _, w := range []int{24, 30, 40, 80, 100} {
		m := model{width: w, anthPrompt: true}
		for _, line := range strings.Split(m.View(), "\n") {
			if visualWidth(line) >= w {
				t.Fatalf("prompt overflow at %d: %q", w, line)
			}
		}
	}
}
func TestAnthropicKeychainRefreshPreservesMetadata(t *testing.T) {
	refreshFixture(t)
	oldRead, oldWrite := readClaudeKeychain, writeClaudeKeychain
	t.Cleanup(func() { readClaudeKeychain, writeClaudeKeychain = oldRead, oldWrite })
	saved, _ := os.ReadFile(claudeCredsPath)
	readClaudeKeychain = func() ([]byte, error) { return saved, nil }
	writeClaudeKeychain = func(b []byte) error { saved = b; return nil }
	reloadCreds = func() (Creds, map[string]string) {
		return Creds{AnthToken: "old"}, map[string]string{"anthropic": "claude-code:keychain"}
	}
	httpc.Transport = refreshTransport(func(*http.Request) (*http.Response, error) {
		return refreshResponse(200, `{"access_token":"new","refresh_token":"refresh-new","expires_in":3600}`), nil
	})
	if err := refreshAnthropicToken(context.Background(), "old", "claude-code:keychain"); err != nil {
		t.Fatal(err)
	}
	d, _ := parseClaudeCredentials(saved)
	if d.access != "new" || string(d.raw["other"]) != `{"keep":true}` {
		t.Fatal("Keychain rotation not saved")
	}
}

func TestAnthropicRefreshRejectsRedirectAndHTTPFailure(t *testing.T) {
	for _, code := range []int{302, 401, 429, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			refreshFixture(t)
			before, _ := os.ReadFile(claudeCredsPath)
			requests := 0
			httpc.Transport = refreshTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := refreshResponse(code, `{"error":"refresh-old"}`)
				resp.Header.Set("Location", "https://example.com/leak")
				return resp, nil
			})
			err := refreshAnthropicToken(context.Background(), "old", "claude-code:credentials-file")
			if err == nil || strings.Contains(err.Error(), "refresh-old") || requests != 1 {
				t.Fatal("unsafe failure handling")
			}
			after, _ := os.ReadFile(claudeCredsPath)
			if string(before) != string(after) {
				t.Fatal("credentials changed")
			}
		})
	}
}
func TestAnthropicPromptUnavailableForCopiedTokenOrGeneric401(t *testing.T) {
	for _, tc := range []struct {
		source  string
		expired bool
		static  bool
	}{{"config", true, false}, {"omp", true, false}, {"claude-code:keychain", false, false}, {"claude-code:keychain", true, true}} {
		m := initialModel(Creds{})
		m.static = tc.static
		next, _ := m.Update(fetchedMsg{creds: Creds{AnthToken: "old"}, anthSource: tc.source, panels: []Panel{{Name: "Anthropic", Err: &httpStatusError{code: 401, expired: tc.expired}}}})
		if next.(model).anthPrompt {
			t.Fatal("unexpected renewal prompt")
		}
	}
}
