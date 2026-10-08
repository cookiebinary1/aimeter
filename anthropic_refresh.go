package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const anthropicTokenURL = "https://platform.claude.com/v1/oauth/token"
const anthropicClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

var anthropicRefreshMu sync.Mutex
var readClaudeKeychain = func() ([]byte, error) {
	b, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return nil, errors.New("cannot read Claude Code Keychain credentials")
	}
	return bytes.TrimSpace(b), nil
}
var writeClaudeKeychain = func(b []byte) error {
	// Preserve the existing account and feed the secret on stdin, never argv.
	meta, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials").Output()
	if err != nil {
		return errors.New("cannot find Claude Code Keychain account")
	}
	match := regexp.MustCompile(`"acct"<blob>="([^"\r\n]*)"`).FindSubmatch(meta)
	if len(match) != 2 {
		return errors.New("cannot identify Claude Code Keychain account")
	}
	quote := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader("add-generic-password -U -s \"Claude Code-credentials\" -a " + quote(string(match[1])) + " -w " + quote(string(b)) + "\n")
	// security -i can exit successfully after a failed individual command.
	out, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("SecKeychain")) || bytes.Contains(out, []byte("error")) || bytes.Contains(out, []byte("usage:")) {
		return errors.New("cannot update Claude Code Keychain credentials; reopen Claude Code and log in")
	}
	return nil
}

type claudeCredentialDocument struct {
	raw             map[string]json.RawMessage
	oauth           map[string]json.RawMessage
	access, refresh string
	scopes          []string
}

func parseClaudeCredentials(b []byte) (claudeCredentialDocument, error) {
	var d claudeCredentialDocument
	if json.Unmarshal(b, &d.raw) != nil || json.Unmarshal(d.raw["claudeAiOauth"], &d.oauth) != nil {
		return d, errors.New("invalid Claude Code credentials")
	}
	_ = json.Unmarshal(d.oauth["accessToken"], &d.access)
	_ = json.Unmarshal(d.oauth["refreshToken"], &d.refresh)
	_ = json.Unmarshal(d.oauth["scopes"], &d.scopes)
	if d.access == "" || d.refresh == "" {
		return d, errors.New("no refresh token available; log in with Claude Code")
	}
	return d, nil
}

func supportedAnthropicSource(source string) bool {
	return source == "claude-code:keychain" || source == "claude-code:credentials-file"
}

func anthropicExpired(err error) bool {
	var status *httpStatusError
	return errors.As(err, &status) && status.code == http.StatusUnauthorized && status.expired
}

type anthropicRefreshMsg struct{ err error }

var renewAnthropic = refreshAnthropicToken

func anthropicRefreshCmd(token, source string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return anthropicRefreshMsg{err: renewAnthropic(ctx, token, source)}
	}
}

func refreshAnthropicToken(ctx context.Context, expiredToken, source string) error {
	anthropicRefreshMu.Lock()
	defer anthropicRefreshMu.Unlock()
	if !supportedAnthropicSource(source) {
		return errors.New("only Claude Code credentials can be renewed")
	}
	// Abort if the selected source changed since the user saw the prompt.
	fresh, sources := reloadCreds()
	if sources["anthropic"] != source {
		return errors.New("credential source changed; refresh the dashboard")
	}
	if fresh.AnthToken != expiredToken {
		return nil
	}
	read := func() ([]byte, error) {
		if source == "claude-code:keychain" {
			return readClaudeKeychain()
		}
		return os.ReadFile(claudeCredsPath)
	}
	original, err := read()
	if err != nil {
		return errors.New("cannot read Claude Code credentials")
	}
	doc, err := parseClaudeCredentials(original)
	if err != nil {
		return err
	}
	if doc.access != expiredToken {
		return nil
	}
	payload := map[string]any{"grant_type": "refresh_token", "refresh_token": doc.refresh, "client_id": anthropicClientID}
	if len(doc.scopes) > 0 {
		payload["scope"] = strings.Join(doc.scopes, " ")
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicTokenURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create token refresh request")
	}
	req.Header.Set("Content-Type", "application/json")
	// Never forward the refresh token through a redirect.
	client := *httpc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("token refresh request failed; check your connection")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh returned HTTP %d; log in with Claude Code", resp.StatusCode)
	}
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || result.Access == "" || result.Refresh == "" || result.Expires <= 0 || result.Expires > 365*24*3600 {
		return errors.New("invalid refresh response; log in with Claude Code")
	}
	// Re-read immediately before writing to avoid overwriting a concurrent rotation.
	latest, err := read()
	if err != nil {
		return errors.New("cannot re-read credentials; log in with Claude Code")
	}
	current, err := parseClaudeCredentials(latest)
	if err != nil {
		return err
	}
	if current.access != doc.access || current.refresh != doc.refresh {
		return errors.New("Claude Code changed credentials during renewal; refresh the dashboard")
	}
	set := func(key string, value any) { current.oauth[key], _ = json.Marshal(value) }
	set("accessToken", result.Access)
	set("refreshToken", result.Refresh)
	set("expiresAt", time.Now().Add(time.Duration(result.Expires)*time.Second).UnixMilli())
	if result.Scope != "" {
		set("scopes", strings.Fields(result.Scope))
	}
	current.raw["claudeAiOauth"], _ = json.Marshal(current.oauth)
	updated, _ := json.Marshal(current.raw)
	if source == "claude-code:keychain" {
		if err := writeClaudeKeychain(updated); err != nil {
			return err
		}
		saved, err := read()
		if err != nil {
			return errors.New("cannot verify Keychain update; log in with Claude Code")
		}
		d, err := parseClaudeCredentials(saved)
		if err != nil || d.access != result.Access || d.refresh != result.Refresh {
			return errors.New("Keychain update was not saved; log in with Claude Code")
		}
		return nil
	}
	return writeClaudeCredentialFile(claudeCredsPath, updated)
}

func writeClaudeCredentialFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".aimeter-oauth-*")
	if err != nil {
		return errors.New("cannot create credentials file; log in with Claude Code")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return errors.New("cannot save renewed credentials; log in with Claude Code")
	}
	return nil
}
