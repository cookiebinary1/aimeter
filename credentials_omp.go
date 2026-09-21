//go:build omp

package main

// Personal-machine fallbacks, opt-in via `go build -tags omp`: the local OMP
// agent database and (openrouter only) a scan of shell rc files. Off by
// default so public builds carry no machine-specific heuristics.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	_ "modernc.org/sqlite"
)

// ompFallback fills providers still missing after env/config/keychain.
func ompFallback(c *Creds, src map[string]string) {
	rows := ompCredentials(ompDBPath)
	if rows == nil {
		rows = map[string]any{}
	}
	if c.AnthToken == "" {
		if d, ok := rows["anthropic"].(map[string]any); ok {
			c.AnthToken, _ = d["access"].(string)
			c.AnthEmail, _ = d["email"].(string)
			if c.AnthToken != "" {
				src["anthropic"] = "omp"
			}
		}
	}
	if c.ZaiKey == "" {
		if d, ok := rows["zai"].(map[string]any); ok {
			if k, _ := d["key"].(string); k != "" {
				c.ZaiKey = k
				src["zai"] = "omp"
			}
		}
	}
	if c.MmKey == "" {
		if d, ok := rows["minimax-code"].(map[string]any); ok {
			if k, _ := d["key"].(string); k != "" {
				c.MmKey = k
				src["minimax"] = "omp"
			}
		}
	}
	if c.OpenRouter == "" {
		if k := rcScanOpenRouter(); k != "" {
			c.OpenRouter = k
			src["openrouter"] = "rc-scan"
		}
	}
}

// Injectable for tests, matching configPath and friends.
var ompDBPath = defaultOMPDBPath()

func defaultOMPDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omp", "agent", "agent.db")
}

func sqlOpenRO(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}

// ompCredentials reads the auth_credentials table of OMP's agent.db.
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
	// A truncated read must not masquerade as "this provider has no entry":
	// report nothing so the caller falls through to its other sources.
	if rws.Err() != nil {
		return nil
	}
	return out
}

// rcScanOpenRouter greps the usual shell rc files and codex config for an
// exported OpenRouter key. Last resort only.
func rcScanOpenRouter() string {
	home, _ := os.UserHomeDir()
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
