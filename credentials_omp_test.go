//go:build omp

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// The omp build resolves credentials from this machine's real OMP database and
// shell rc files. Every test in the package must see a clean machine instead,
// or results depend on whoever runs them — the fallbacks are exercised
// explicitly with fixtures below.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aimeter-omp")
	if err != nil {
		panic(err)
	}
	ompDBPath = filepath.Join(dir, "absent.db")
	os.Setenv("HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newAgentDB builds a throwaway agent.db with the columns ompCredentials reads.
func newAgentDB(t *testing.T, rows map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE auth_credentials (provider TEXT, data TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for provider, data := range rows {
		if _, err := db.Exec(`INSERT INTO auth_credentials VALUES (?, ?)`, provider, data); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return path
}

func TestOMPCredentialsFillsMissingProviders(t *testing.T) {
	path := newAgentDB(t, map[string]string{
		"anthropic":    `{"access":"sk-ant-oauth","email":"user@example.com"}`,
		"zai":          `{"key":"zai-key"}`,
		"minimax-code": `{"key":"mm-key"}`,
		"garbage":      `{not json`,
	})

	rows := ompCredentials(path)
	if rows == nil {
		t.Fatal("no rows read")
	}
	if _, ok := rows["garbage"]; ok {
		t.Error("unparseable row was kept")
	}
	d, ok := rows["anthropic"].(map[string]any)
	if !ok || d["access"] != "sk-ant-oauth" {
		t.Fatalf("anthropic row = %#v", rows["anthropic"])
	}
}

// A provider already resolved from an earlier source must win over the OMP db.
func TestOMPFallbackDoesNotOverrideEarlierSources(t *testing.T) {
	path := newAgentDB(t, map[string]string{"zai": `{"key":"from-omp"}`})
	prev := ompDBPath
	ompDBPath = path
	t.Cleanup(func() { ompDBPath = prev })

	c := Creds{ZaiKey: "from-env"}
	src := map[string]string{"zai": "env"}
	ompFallback(&c, src)

	if c.ZaiKey != "from-env" || src["zai"] != "env" {
		t.Errorf("env credential was overwritten: key=%q src=%q", c.ZaiKey, src["zai"])
	}
}

func TestOMPFallbackReportsItsSource(t *testing.T) {
	path := newAgentDB(t, map[string]string{"minimax-code": `{"key":"mm-key"}`})
	prev := ompDBPath
	ompDBPath = path
	t.Cleanup(func() { ompDBPath = prev })

	c := Creds{}
	src := map[string]string{}
	ompFallback(&c, src)

	if c.MmKey != "mm-key" || src["minimax"] != "omp" {
		t.Errorf("minimax = %q from %q, want %q from %q", c.MmKey, src["minimax"], "mm-key", "omp")
	}
}

// A missing or unreadable database is a normal case — no credentials, no panic.
func TestOMPCredentialsMissingDB(t *testing.T) {
	if rows := ompCredentials(filepath.Join(t.TempDir(), "absent.db")); len(rows) != 0 {
		t.Errorf("rows = %#v, want none", rows)
	}
}

func TestRCScanOpenRouter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("# comment\nexport OPENROUTER_API_KEY=\"sk-or-v1-abc123_XY\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := rcScanOpenRouter(); got != "sk-or-v1-abc123_XY" {
		t.Errorf("key = %q, want %q", got, "sk-or-v1-abc123_XY")
	}
}

// Anything that is not an OpenRouter key must be ignored, so an unrelated
// export never gets sent to openrouter.ai as a bearer token.
func TestRCScanOpenRouterIgnoresForeignKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("export OPENROUTER_API_KEY=$OTHER_KEY\nexport ANTHROPIC_API_KEY=sk-ant-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := rcScanOpenRouter(); got != "" {
		t.Errorf("key = %q, want empty", got)
	}
}
