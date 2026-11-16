package main

// Credential resolution: a chain of non-invasive sources, first hit wins per
// provider. Resolution only reads; interactive renewal is a separate opt-in flow.
//
// Static API-key providers (zai, minimax, openrouter, elevenlabs, meshy, alibaba):
//
//	1. env       ZAI_API_KEY, KIMI_API_KEY (or MOONSHOT_API_KEY), MINIMAX_API_KEY,
//	             OPENROUTER_API_KEY, ELEVENLABS_API_KEY, MESHY_API_KEY,
//	             ALIBABA_API_KEY
//	2. config    ~/.config/aimeter/credentials.json  (chmod 600)
//	3. keychain  macOS only: `security find-generic-password -s aimeter -a <name>`
//	4. omp       local OMP agent.db (build with -tags omp)
//	5. rc-scan   openrouter only: shell rc files / codex config (-tags omp)
//
// OAuth providers rotate their tokens, so live sources beat frozen copies:
//
//	anthropic:  config → Claude Code's own storage → OMP db (-tags omp)
//	            (macOS keychain entry, or ~/.claude/.credentials.json)
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
	configPath        = defaultConfigPath()
	codexAuthPath     = defaultCodexAuthPath()
	claudeCredsPath   = defaultClaudeCredsPath()
	claudeProfilePath = defaultClaudeProfilePath()
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

func defaultClaudeProfilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

func defaultClaudeCredsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

func defaultCodexAuthPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// credentialsFile is the on-disk schema of credentials.json.
type credentialsFile struct {
	Anthropic  oauthPair         `json:"anthropic"`
	Codex      oauthPair         `json:"codex"`
	Zai        apiKey            `json:"zai"`
	Kimi       apiKey            `json:"kimi"`
	Minimax    apiKey            `json:"minimax"`
	OpenRouter apiKey            `json:"openrouter"`
	ElevenLabs apiKey            `json:"elevenlabs"`
	Meshy      apiKey            `json:"meshy"`
	Alibaba    alibabaCreds      `json:"alibaba"`
	Custom     []customProvider  `json:"custom"`
	Costs      map[string]string `json:"costs"`
	Dashboard  dashboardConfig   `json:"dashboard,omitempty"`
}

// dashboardConfig controls which services the dashboard shows and in what
// order. Both lists hold provider ids ("zai", "custom:My Service"); ids that
// no longer resolve are ignored, so a stale entry can never hide a panel.
type dashboardConfig struct {
	Order  []string `json:"order,omitempty"`
	Hidden []string `json:"hidden,omitempty"`
}

type apiKey struct {
	APIKey string `json:"api_key,omitempty"`
}

type alibabaCreds struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url,omitempty"`
	Cookie  string `json:"cookie,omitempty"`
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

// claudeOAuthToken is the Anthropic OAuth fallback, reading Claude Code's own
// storage: the macOS keychain entry, or ~/.claude/.credentials.json (which is
// where Claude Code stores it on Linux and in other setups).
func claudeOAuthToken() (string, string) {
	if runtime.GOOS == "darwin" {
		if t := claudeKeychainToken(); t != "" {
			return t, "claude-code:keychain"
		}
	}
	if t := claudeTokenFromFile(claudeCredsPath); t != "" {
		return t, "claude-code:credentials-file"
	}
	return "", ""
}

// claudeTokenFromFile parses Claude Code's .credentials.json.
func claudeTokenFromFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Same warning as for our own config: this file holds a bearer token in
	// plaintext, and Claude Code does not always create it with 0600.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "aimeter: warning: %s is group/world readable — chmod 600 it\n", path)
	}
	var kc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &kc) != nil {
		return ""
	}
	return kc.ClaudeAiOauth.AccessToken
}

// claudeKeychainToken reads the macOS keychain entry Claude Code itself stores.
func claudeKeychainToken() string {
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
	c.Costs = cfg.Costs
	c.Prefs = cfg.Dashboard

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
	c.KimiKey = resolveKey("kimi", "KIMI_API_KEY", cfg.Kimi.APIKey)
	if c.KimiKey == "" {
		// Moonshot's own docs name the variable; accept it as an alias.
		c.KimiKey = resolveKey("kimi", "MOONSHOT_API_KEY", cfg.Kimi.APIKey)
	}
	c.MmKey = resolveKey("minimax", "MINIMAX_API_KEY", cfg.Minimax.APIKey)
	c.OpenRouter = resolveKey("openrouter", "OPENROUTER_API_KEY", cfg.OpenRouter.APIKey)
	c.EleKey = resolveKey("elevenlabs", "ELEVENLABS_API_KEY", cfg.ElevenLabs.APIKey)
	c.MeshyKey = resolveKey("meshy", "MESHY_API_KEY", cfg.Meshy.APIKey)
	c.AliKey = resolveKey("alibaba", "ALIBABA_API_KEY", cfg.Alibaba.APIKey)
	c.AliBaseURL = os.Getenv("ALIBABA_BASE_URL")
	if c.AliBaseURL == "" {
		c.AliBaseURL = cfg.Alibaba.BaseURL
	}
	c.AliCookie = os.Getenv("ALIBABA_COOKIE")
	if c.AliCookie == "" {
		c.AliCookie = cfg.Alibaba.Cookie
	}

	// OAuth providers: explicit config copy first, then live sources.
	if cfg.Anthropic.AccessToken != "" {
		c.AnthToken, c.AnthEmail = cfg.Anthropic.AccessToken, cfg.Anthropic.Email
		src["anthropic"] = "config"
	}
	if c.AnthToken == "" {
		if t, s := claudeOAuthToken(); t != "" {
			c.AnthToken = t
			c.AnthAccount = claudeAccountIdentity()
			src["anthropic"] = s
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

	// Personal-machine fallbacks; no-op in default builds.
	ompFallback(&c, src)

	// User-defined providers come from the config file only.
	skipped := 0
	for _, p := range cfg.Custom {
		if p.Name == "" || p.URL == "" {
			skipped++
			continue
		}
		c.Custom = append(c.Custom, p)
		src["custom:"+p.Name] = "config"
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "aimeter: warning: %d custom provider(s) skipped (need name and url)\n", skipped)
	}

	for _, p := range credProviders {
		if src[p] == "" {
			src[p] = "missing"
		}
	}
	return c, src
}

// credProviders is the display/report order.
var credProviders = []string{"anthropic", "codex", "zai", "kimi", "minimax", "openrouter", "elevenlabs", "meshy", "alibaba"}

// staticProviderEnvs lists the environment variables probed for each static
// (API-key) provider, in priority order. OAuth providers are absent: their
// tokens rotate, so first-run detection never freezes a copy.
var staticProviderEnvs = map[string][]string{
	"zai":        {"ZAI_API_KEY"},
	"kimi":       {"KIMI_API_KEY", "MOONSHOT_API_KEY"},
	"minimax":    {"MINIMAX_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"elevenlabs": {"ELEVENLABS_API_KEY"},
	"meshy":      {"MESHY_API_KEY"},
	"alibaba":    {"ALIBABA_API_KEY"},
}

// keychainLookup is injectable so tests do not touch the real keychain.
var keychainLookup = keychainGet

// firstRunDetect runs when the config file does not exist yet: it probes the
// environment and keychain for static API keys and persists them to
// credentials.json. Returns true when it created the file, plus the provider
// ids it found. OAuth providers are deliberately skipped — their live sources
// (Claude Code storage, codex auth.json) keep working without a frozen copy.
func firstRunDetect() (created bool, found []string) {
	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	}
	cfg := &credentialsFile{}
	for _, id := range credProviders {
		envs, ok := staticProviderEnvs[id]
		if !ok {
			continue
		}
		for _, envName := range envs {
			if v := os.Getenv(envName); v != "" {
				setStaticKey(cfg, id, v)
				found = append(found, id)
				break
			}
		}
		if getStaticKey(cfg, id) == "" {
			if v := keychainLookup(id); v != "" {
				setStaticKey(cfg, id, v)
				found = append(found, id)
			}
		}
	}
	if len(found) == 0 {
		return false, nil
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return false, nil
	}
	if err := os.WriteFile(configPath, b, 0o600); err != nil {
		return false, nil
	}
	return true, found
}

func getStaticKey(cfg *credentialsFile, id string) string {
	switch id {
	case "zai":
		return cfg.Zai.APIKey
	case "kimi":
		return cfg.Kimi.APIKey
	case "minimax":
		return cfg.Minimax.APIKey
	case "openrouter":
		return cfg.OpenRouter.APIKey
	case "elevenlabs":
		return cfg.ElevenLabs.APIKey
	case "meshy":
		return cfg.Meshy.APIKey
	case "alibaba":
		return cfg.Alibaba.APIKey
	}
	return ""
}

func setStaticKey(cfg *credentialsFile, id, value string) {
	switch id {
	case "zai":
		cfg.Zai.APIKey = value
	case "kimi":
		cfg.Kimi.APIKey = value
	case "minimax":
		cfg.Minimax.APIKey = value
	case "openrouter":
		cfg.OpenRouter.APIKey = value
	case "elevenlabs":
		cfg.ElevenLabs.APIKey = value
	case "meshy":
		cfg.Meshy.APIKey = value
	case "alibaba":
		cfg.Alibaba.APIKey = value
	}
}

// loadConfigForWrite returns the current config, tolerating a missing file
// (empty config) but refusing to overwrite an unparsable one.
func loadConfigForWrite() (*credentialsFile, error) {
	cfg, err := readConfigFile()
	if err != nil {
		// Missing file is fine (cfg is empty); a parse error must not clobber.
		if _, statErr := os.Stat(configPath); statErr == nil {
			return nil, err
		}
		return &credentialsFile{}, nil
	}
	return cfg, nil
}

// saveConfigFile writes credentials.json atomically enough for a single-user
// CLI: create the directory 0700 and the file 0600.
func saveConfigFile(cfg *credentialsFile) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, b, 0o600)
}

// writeProviderSecret sets or clears one provider's secret in credentials.json,
// preserving every other field (OAuth pairs, Alibaba endpoint, custom
// providers, costs, dashboard layout). An empty value removes the config entry.
// OAuth providers store an access_token; the rest an api_key.
func writeProviderSecret(id, value string) error {
	cfg, err := loadConfigForWrite()
	if err != nil {
		return err
	}
	switch id {
	case "anthropic":
		if value == "" {
			cfg.Anthropic = oauthPair{}
		} else {
			cfg.Anthropic.AccessToken = value
		}
	case "codex":
		if value == "" {
			cfg.Codex = oauthPair{}
		} else {
			cfg.Codex.AccessToken = value
		}
	default:
		setStaticKey(cfg, id, value)
	}
	return saveConfigFile(cfg)
}

// writeDashboardConfig persists the dashboard layout (service order and
// hidden services), preserving every other field in credentials.json.
func writeDashboardConfig(prefs dashboardConfig) error {
	cfg, err := loadConfigForWrite()
	if err != nil {
		return err
	}
	cfg.Dashboard = prefs
	return saveConfigFile(cfg)
}

// printCredSources reports where each provider resolved from — never the keys.
func printCredSources(src map[string]string, custom []customProvider) {
	w := 0
	rows := append([]string{}, credProviders...)
	for _, p := range custom {
		rows = append(rows, "custom:"+p.Name)
	}
	for _, p := range rows {
		if len(p) > w {
			w = len(p)
		}
	}
	for _, p := range rows {
		fmt.Printf("%-*s  %s\n", w, p, src[p])
	}
}

// Include the organization because usage limits can differ between memberships.
// Never use this identity for explicit tokens from another credential source.
func claudeAccountIdentity() string {
	b, err := os.ReadFile(claudeProfilePath)
	if err != nil {
		return ""
	}
	var profile struct {
		Account struct {
			ID  string `json:"accountUuid"`
			Org string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &profile) != nil || profile.Account.ID == "" {
		return ""
	}
	return profile.Account.ID + ":" + profile.Account.Org
}
