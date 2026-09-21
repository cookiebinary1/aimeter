package main

// Credential resolution: a chain of non-invasive sources, first hit wins per
// provider. Nothing is ever written or modified — the tool only reads.
//
// Static API-key providers (zai, minimax, openrouter, elevenlabs, meshy):
//
//	1. env       ZAI_API_KEY, MINIMAX_API_KEY, OPENROUTER_API_KEY,
//	             ELEVENLABS_API_KEY, MESHY_API_KEY
//	2. config    ~/.config/aimeter/credentials.json  (chmod 600)
//	3. keychain  macOS only: `security find-generic-password -s aimeter -a <name>`
//	4. omp       local OMP agent.db (build with -tags omp)
//	5. rc-scan   openrouter only: shell rc files / codex config (-tags omp)
//
// OAuth providers rotate their tokens, so live sources beat frozen copies:
//
//	anthropic:  config → OMP db (-tags omp) → macOS keychain Claude Code entry
//	codex:      config → ~/.codex/auth.json (Codex CLI's own file)

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Injectable for tests.
var (
	configPath    = defaultConfigPath()
	codexAuthPath = defaultCodexAuthPath()
)

func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "aimeter", "credentials.json")
	}
	// Deliberately ~/.config on every platform (including macOS, where
	// os.UserConfigDir would return ~/Library/Application Support): it keeps
	// the historical ~/.config/aimeter/ location and matches user expectation.
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "aimeter", "credentials.json")
}

func defaultCodexAuthPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// credentialsFile is the on-disk schema of credentials.json.
type credentialsFile struct {
	Anthropic  oauthPair `json:"anthropic"`
	Codex      oauthPair `json:"codex"`
	Zai        apiKey    `json:"zai"`
	Minimax    apiKey    `json:"minimax"`
	OpenRouter apiKey    `json:"openrouter"`
	ElevenLabs apiKey    `json:"elevenlabs"`
	Meshy      apiKey    `json:"meshy"`
}

type apiKey struct {
	APIKey string `json:"api_key"`
}

type oauthPair struct {
	AccessToken string `json:"access_token"`
	Email       string `json:"email,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
}

// readConfigFile loads credentials.json; any error yields an empty (optional)
// config. Loose permissions warn on stderr but do not block.
func readConfigFile() (*credentialsFile, error) {
	f := &credentialsFile{}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return f, err
	}
	if fi, err := os.Stat(configPath); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "aimeter: warning: %s is group/world readable — chmod 600 it\n", configPath)
	}
	if err := json.Unmarshal(b, f); err != nil {
		return &credentialsFile{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	return f, nil
}

// keychainGet reads one secret from the OS keychain. Only macOS is supported
// (stock `security` CLI, no cgo); elsewhere it is a silent no-op.
func keychainGet(account string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "aimeter", "-a", account, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// claudeKeychainToken is the macOS fallback for the Anthropic OAuth token,
// reading the entry Claude Code itself stores.
func claudeKeychainToken() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return ""
	}
	var kc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(out, &kc) != nil {
		return ""
	}
	return kc.ClaudeAiOauth.AccessToken
}

// readCodexAuth reads Codex CLI's own auth.json (live tokens, no copy).
func readCodexAuth() (token, acct string) {
	b, err := os.ReadFile(codexAuthPath)
	if err != nil {
		return "", ""
	}
	var a struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &a) != nil {
		return "", ""
	}
	return a.Tokens.AccessToken, a.Tokens.AccountID
}

// resolveCreds fills Creds from the chain above and reports each provider's
// winning source ("env", "config", "keychain", "omp", "rc-scan",
// "keychain:claude-code", "codex-auth.json" or "missing").
func resolveCreds() (Creds, map[string]string) {
	var c Creds
	src := map[string]string{}
	cfg, _ := readConfigFile() // optional; empty on any error

	resolveKey := func(name, envName, cfgVal string) string {
		switch {
		case os.Getenv(envName) != "":
			src[name] = "env"
			return os.Getenv(envName)
		case cfgVal != "":
			src[name] = "config"
			return cfgVal
		default:
			if v := keychainGet(name); v != "" {
				src[name] = "keychain"
				return v
			}
			return ""
		}
	}
	c.ZaiKey = resolveKey("zai", "ZAI_API_KEY", cfg.Zai.APIKey)
	c.MmKey = resolveKey("minimax", "MINIMAX_API_KEY", cfg.Minimax.APIKey)
	c.OpenRouter = resolveKey("openrouter", "OPENROUTER_API_KEY", cfg.OpenRouter.APIKey)
	c.EleKey = resolveKey("elevenlabs", "ELEVENLABS_API_KEY", cfg.ElevenLabs.APIKey)
	c.MeshyKey = resolveKey("meshy", "MESHY_API_KEY", cfg.Meshy.APIKey)

	// Personal-machine fallbacks; no-op in default builds.
	ompFallback(&c, src)

	// OAuth providers: explicit config copy first, then live sources.
	if cfg.Anthropic.AccessToken != "" {
		c.AnthToken, c.AnthEmail = cfg.Anthropic.AccessToken, cfg.Anthropic.Email
		src["anthropic"] = "config"
	}
	if c.AnthToken == "" {
		if t := claudeKeychainToken(); t != "" {
			c.AnthToken = t
			src["anthropic"] = "keychain:claude-code"
		}
	}
	if cfg.Codex.AccessToken != "" {
		c.CodexToken, c.CodexAcct = cfg.Codex.AccessToken, cfg.Codex.AccountID
		src["codex"] = "config"
	}
	if c.CodexToken == "" {
		if t, a := readCodexAuth(); t != "" {
			c.CodexToken, c.CodexAcct = t, a
			src["codex"] = "codex-auth.json"
		}
	}

	for _, p := range credProviders {
		if src[p] == "" {
			src[p] = "missing"
		}
	}
	return c, src
}

// credProviders is the display/report order.
var credProviders = []string{"anthropic", "codex", "zai", "minimax", "openrouter", "elevenlabs", "meshy"}

// printCredSources reports where each provider resolved from — never the keys.
func printCredSources(src map[string]string) {
	w := 0
	for _, p := range credProviders {
		if len(p) > w {
			w = len(p)
		}
	}
	for _, p := range credProviders {
		fmt.Printf("%-*s  %s\n", w, p, src[p])
	}
}
