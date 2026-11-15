package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolate points configPath/codexAuthPath at a temp dir and clears all
// provider env vars, so each test controls the whole chain.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldCfg, oldCodex, oldClaude, oldProfile := configPath, codexAuthPath, claudeCredsPath, claudeProfilePath
	configPath = filepath.Join(dir, "credentials.json")
	codexAuthPath = filepath.Join(dir, "auth.json")
	claudeCredsPath = filepath.Join(dir, "claude-creds.json")
	claudeProfilePath = filepath.Join(dir, "claude-profile.json")
	t.Cleanup(func() {
		configPath, codexAuthPath, claudeCredsPath, claudeProfilePath = oldCfg, oldCodex, oldClaude, oldProfile
	})
	for _, k := range []string{
		"ZAI_API_KEY", "KIMI_API_KEY", "MOONSHOT_API_KEY", "MINIMAX_API_KEY", "OPENROUTER_API_KEY",
		"ELEVENLABS_API_KEY", "MESHY_API_KEY", "ALIBABA_API_KEY", "ALIBABA_BASE_URL", "ALIBABA_COOKIE",
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
	isolate(t)
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

// MOONSHOT_API_KEY is the name Moonshot's own docs use; it must resolve the
// Kimi panel when KIMI_API_KEY is unset.
func TestResolveChainKimiMoonshotAlias(t *testing.T) {
	isolate(t)
	t.Setenv("MOONSHOT_API_KEY", "env-moonshot")

	c, src := resolveCreds()

	if c.KimiKey != "env-moonshot" || src["kimi"] != "env" {
		t.Errorf("kimi: key=%q src=%q, want env-moonshot/env", c.KimiKey, src["kimi"])
	}
}

// config can also carry the OAuth pairs; codex falls back to its auth.json.
func TestResolveChainOAuth(t *testing.T) {
	isolate(t)
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
}

func TestResolveChainCodexAuthFallback(t *testing.T) {
	isolate(t)
	writeConfig(t, `{}`)
	if err := os.WriteFile(codexAuthPath, []byte(
		`{"tokens":{"access_token":"live-tok","account_id":"live-acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, src := resolveCreds()

	if c.CodexToken != "live-tok" || c.CodexAcct != "live-acct" || src["codex"] != "codex-auth.json" {
		t.Errorf("codex: token=%q acct=%q src=%q", c.CodexToken, c.CodexAcct, src["codex"])
	}
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

// First run: keys found in the environment are persisted to a fresh
// credentials.json with 0600 permissions; OAuth providers are never copied.
func TestFirstRunDetectWritesDetectedKeys(t *testing.T) {
	isolate(t)
	oldLookup := keychainLookup
	keychainLookup = func(string) string { return "" }
	t.Cleanup(func() { keychainLookup = oldLookup })

	t.Setenv("ZAI_API_KEY", "detected-zai")
	t.Setenv("KIMI_API_KEY", "detected-kimi")

	created, found := firstRunDetect()
	if !created {
		t.Fatal("config was not created")
	}
	if len(found) != 2 {
		t.Errorf("found = %v, want zai+kimi", found)
	}
	fi, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not carry POSIX permission bits.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Zai.APIKey != "detected-zai" || cfg.Kimi.APIKey != "detected-kimi" {
		t.Errorf("persisted zai=%q kimi=%q", cfg.Zai.APIKey, cfg.Kimi.APIKey)
	}
	if cfg.Anthropic.AccessToken != "" || cfg.Codex.AccessToken != "" {
		t.Error("OAuth tokens must not be frozen into the config")
	}

	created, _ = firstRunDetect()
	if created {
		t.Error("second run overwrote the existing config")
	}
}

func TestFirstRunDetectSkipsExistingFile(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"zai":{"api_key":"keep-me"}}`)
	t.Setenv("KIMI_API_KEY", "newer-env-key")

	created, _ := firstRunDetect()
	if created {
		t.Fatal("existing config was overwritten")
	}
	cfg, err := readConfigFile()
	if err != nil || cfg.Zai.APIKey != "keep-me" {
		t.Errorf("config = %+v err=%v, want untouched", cfg, err)
	}
}

func TestFirstRunDetectNoKeysCreatesNothing(t *testing.T) {
	isolate(t)
	oldLookup := keychainLookup
	keychainLookup = func(string) string { return "" }
	t.Cleanup(func() { keychainLookup = oldLookup })

	created, found := firstRunDetect()
	if created || len(found) != 0 {
		t.Errorf("created=%v found=%v, want nothing", created, found)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("config file created without any detected credentials")
	}
}

// Admin edits go through writeProviderSecret: set one key, keep everything
// else in the file, and remove again by writing an empty value.
func TestWriteProviderSecretPreservesOtherFields(t *testing.T) {
	isolate(t)
	writeConfig(t, `{
		"zai": {"api_key": "z"},
		"alibaba": {"api_key": "a", "base_url": "https://example.invalid/v1"},
		"costs": {"zai": "$9/mo"},
		"custom": [{"name": "X", "url": "https://example.invalid/u", "api_key": "c", "percent": "/p"}]
	}`)

	if err := writeProviderSecret("kimi", "k"); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kimi.APIKey != "k" || cfg.Zai.APIKey != "z" || cfg.Alibaba.BaseURL != "https://example.invalid/v1" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Costs["zai"] != "$9/mo" || len(cfg.Custom) != 1 {
		t.Errorf("costs/custom not preserved: %+v", cfg)
	}

	if err := writeProviderSecret("kimi", ""); err != nil {
		t.Fatal(err)
	}
	cfg, _ = readConfigFile()
	if cfg.Kimi.APIKey != "" {
		t.Error("kimi key not removed")
	}
}

// A garbage config must not be clobbered by an admin write.
func TestWriteProviderSecretRefusesParseError(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"zai": {"api_key": "broken"`)
	before, _ := os.ReadFile(configPath)
	if err := writeProviderSecret("kimi", "k"); err == nil {
		t.Fatal("write succeeded over an unparseable config")
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Error("unparseable config was modified")
	}
}

func TestClaudeTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")

	if got := claudeTokenFromFile(filepath.Join(dir, "missing.json")); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
	if err := os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"tok-cc","refreshToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeTokenFromFile(p); got != "tok-cc" {
		t.Errorf("parsed: got %q, want tok-cc", got)
	}
	if err := os.WriteFile(p, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeTokenFromFile(p); got != "" {
		t.Errorf("garbage: got %q, want empty", got)
	}
}

func TestAlibabaExplicitURLPrecedence(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"alibaba":{"api_key":"config-key","base_url":"https://config.example/compatible-mode/v1","cookie":"session=config"}}`)
	t.Setenv("ALIBABA_API_KEY", "env-key")

	c, src := resolveCreds()
	if c.AliKey != "env-key" || src["alibaba"] != "env" || c.AliBaseURL != "https://config.example/compatible-mode/v1" {
		t.Fatalf("Alibaba did not preserve env key with configured endpoint: source=%s url=%s", src["alibaba"], c.AliBaseURL)
	}
	if c.AliCookie != "session=config" {
		t.Fatal("configured console session was not resolved")
	}

	t.Setenv("ALIBABA_BASE_URL", "https://env.example/compatible-mode/v1")
	t.Setenv("ALIBABA_COOKIE", "session=env")
	c, _ = resolveCreds()
	if c.AliBaseURL != "https://env.example/compatible-mode/v1" {
		t.Fatalf("environment endpoint did not override config: %s", c.AliBaseURL)
	}
	if c.AliCookie != "session=env" {
		t.Fatal("environment console session did not override config")
	}
}

func TestClaudeAccountIdentity(t *testing.T) {
	old := claudeProfilePath
	claudeProfilePath = filepath.Join(t.TempDir(), "profile.json")
	t.Cleanup(func() { claudeProfilePath = old })
	if got := claudeAccountIdentity(); got != "" {
		t.Fatal("missing profile returned identity")
	}
	for _, tc := range []struct{ body, want string }{
		{`{"oauthAccount":{"accountUuid":"user","organizationUuid":"org"}}`, "user:org"},
		{`{"oauthAccount":{"organizationUuid":"org"}}`, ""},
		{`invalid`, ""},
	} {
		if err := os.WriteFile(claudeProfilePath, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		if got := claudeAccountIdentity(); got != tc.want {
			t.Fatalf("identity=%q want=%q", got, tc.want)
		}
	}
}
