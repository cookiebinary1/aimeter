package main

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points configPath/codexAuthPath at a temp dir and clears all
// provider env vars, so each test controls the whole chain.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldCfg, oldCodex := configPath, codexAuthPath
	configPath = filepath.Join(dir, "credentials.json")
	codexAuthPath = filepath.Join(dir, "auth.json")
	t.Cleanup(func() { configPath, codexAuthPath = oldCfg, oldCodex })
	for _, k := range []string{
		"ZAI_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY",
		"ELEVENLABS_API_KEY", "MESHY_API_KEY",
	} {
		t.Setenv(k, "")
	}
	return dir
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// env beats config for the same provider; config fills the rest.
func TestResolveChainEnvOverConfig(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, `{
		"zai": {"api_key": "cfg-zai"},
		"minimax": {"api_key": "cfg-mm"},
		"openrouter": {"api_key": "cfg-or"}
	}`)
	t.Setenv("ZAI_API_KEY", "env-zai")

	c, src := resolveCreds()

	if c.ZaiKey != "env-zai" || src["zai"] != "env" {
		t.Errorf("zai: key=%q src=%q, want env-zai/env", c.ZaiKey, src["zai"])
	}
	if c.MmKey != "cfg-mm" || src["minimax"] != "config" {
		t.Errorf("minimax: key=%q src=%q, want cfg-mm/config", c.MmKey, src["minimax"])
	}
	if c.OpenRouter != "cfg-or" || src["openrouter"] != "config" {
		t.Errorf("openrouter: key=%q src=%q, want cfg-or/config", c.OpenRouter, src["openrouter"])
	}
	_ = dir
}

// missing config file degrades to env-only resolution, never errors.
func TestResolveChainNoConfig(t *testing.T) {
	isolate(t)
	t.Setenv("ELEVENLABS_API_KEY", "env-ele")

	c, src := resolveCreds()

	if c.EleKey != "env-ele" || src["elevenlabs"] != "env" {
		t.Errorf("elevenlabs: key=%q src=%q, want env-ele/env", c.EleKey, src["elevenlabs"])
	}
	if c.MeshyKey != "" || src["meshy"] != "missing" {
		t.Errorf("meshy: key=%q src=%q, want empty/missing", c.MeshyKey, src["meshy"])
	}
}

// config can also carry the OAuth pairs; codex falls back to its auth.json.
func TestResolveChainOAuth(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, `{
		"anthropic": {"access_token": "tok-a", "email": "a@b.c"},
		"codex": {"access_token": "tok-c", "account_id": "acct-1"}
	}`)

	c, src := resolveCreds()

	if c.AnthToken != "tok-a" || c.AnthEmail != "a@b.c" || src["anthropic"] != "config" {
		t.Errorf("anthropic: token=%q email=%q src=%q", c.AnthToken, c.AnthEmail, src["anthropic"])
	}
	if c.CodexToken != "tok-c" || c.CodexAcct != "acct-1" || src["codex"] != "config" {
		t.Errorf("codex: token=%q acct=%q src=%q", c.CodexToken, c.CodexAcct, src["codex"])
	}
	_ = dir
}

func TestResolveChainCodexAuthFallback(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, `{}`)
	if err := os.WriteFile(codexAuthPath, []byte(
		`{"tokens":{"access_token":"live-tok","account_id":"live-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, src := resolveCreds()

	if c.CodexToken != "live-tok" || c.CodexAcct != "live-acct" || src["codex"] != "codex-auth.json" {
		t.Errorf("codex: token=%q acct=%q src=%q", c.CodexToken, c.CodexAcct, src["codex"])
	}
	_ = dir
}

// unknown fields and a garbage file must not break resolution.
func TestResolveChainBadConfig(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"nonsense": true, "zai": {"api_key": "still-zai"`)
	t.Setenv("MESHY_API_KEY", "env-meshy")

	c, src := resolveCreds()

	if c.ZaiKey != "" || src["zai"] != "missing" {
		t.Errorf("zai should be missing on unparseable config, got %q/%q", c.ZaiKey, src["zai"])
	}
	if c.MeshyKey != "env-meshy" {
		t.Errorf("meshy: %q, want env-meshy", c.MeshyKey)
	}
}
