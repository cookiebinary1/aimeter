package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
)

// Hidden services are dropped before the fan-out, so a disabled service also
// stops making requests; listed services lead in the configured order.
func TestFetchAllAppliesDashboardLayout(t *testing.T) {
	withStub(t, `{"data":{"total_credits":10,"total_usage":1},"tier":"free","character_count":1,"character_limit":10}`)
	panels := fetchAll(Creds{
		OpenRouter: "test-key",
		EleKey:     "test-key",
		MeshyKey:   "test-key",
		Prefs: dashboardConfig{
			Order:  []string{"elevenlabs", "openrouter"},
			Hidden: []string{"meshy"},
		},
	})
	if len(panels) != 2 {
		t.Fatalf("panels = %+v, want the two visible providers", panels)
	}
	if panels[0].Name != "ElevenLabs" || panels[1].Name != "OpenRouter" {
		t.Errorf("order = %q, %q; want ElevenLabs then OpenRouter", panels[0].Name, panels[1].Name)
	}
	for _, p := range panels {
		if p.Name == "Meshy" {
			t.Error("hidden service was fetched and shown")
		}
	}
}

// Unlisted services keep their default order after the ranked ones, so adding
// a new provider never shuffles the existing layout.
func TestArrangeServicesUnlistedKeepDefaultOrder(t *testing.T) {
	defs := []serviceDef{{name: "A", key: "a"}, {name: "B", key: "b"}, {name: "C", key: "c"}}
	got := arrangeServices(defs, dashboardConfig{Order: []string{"c"}})
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d services, want %d", len(got), len(want))
	}
	for i, key := range want {
		if got[i].key != key {
			t.Errorf("position %d = %q, want %q", i, got[i].key, key)
		}
	}
}

// The Services tab lists every known service, ordered and marked from the
// persisted layout, including custom entries.
func TestAdminServicesFromLayout(t *testing.T) {
	c := Creds{
		ZaiKey: "z",
		Prefs: dashboardConfig{
			Order:  []string{"meshy", "zai"},
			Hidden: []string{"meshy"},
		},
		Custom: []customProvider{{Name: "My Service", URL: "https://example.invalid"}},
	}
	src := map[string]string{"zai": "config", "custom:My Service": "config", "meshy": "missing"}

	services := adminServicesFrom(c, src)
	if len(services) != len(adminProviders)+1 {
		t.Fatalf("got %d services, want %d", len(services), len(adminProviders)+1)
	}
	if services[0].id != "meshy" || !services[0].hidden {
		t.Errorf("first service = %+v, want hidden meshy", services[0])
	}
	if services[1].id != "zai" || !services[1].configured {
		t.Errorf("second service = %+v, want configured zai", services[1])
	}
	last := services[len(services)-1]
	if last.id != "custom:My Service" || !last.custom {
		t.Errorf("last service = %+v, want the custom provider", last)
	}
}

// Toggling visibility in the Services tab persists immediately and preserves
// the rest of credentials.json.
func TestAdminServicesTogglePersists(t *testing.T) {
	isolate(t)
	writeConfig(t, `{
		"zai": {"api_key": "z"},
		"costs": {"zai": "$9/mo"},
		"dashboard": {"order": ["zai", "meshy"]}
	}`)

	m := newAdminModel()
	m.tab = adminTabServices
	if m.services[0].id != "zai" {
		t.Fatalf("services[0] = %q, want zai", m.services[0].id)
	}
	m.scursor = 0
	m.Update(tea.KeyMsg{Type: tea.KeySpace})

	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dashboard.Hidden) != 1 || cfg.Dashboard.Hidden[0] != "zai" {
		t.Errorf("hidden = %v, want [zai]", cfg.Dashboard.Hidden)
	}
	if cfg.Zai.APIKey != "z" || cfg.Costs["zai"] != "$9/mo" {
		t.Errorf("credentials/costs not preserved: %+v", cfg)
	}
	if len(cfg.Dashboard.Order) != len(m.services) {
		t.Errorf("order = %v, want every service persisted", cfg.Dashboard.Order)
	}
}

// Reordering moves the selected service and writes the new order.
func TestAdminServicesReorderPersists(t *testing.T) {
	isolate(t)
	writeConfig(t, `{"dashboard": {"order": ["zai", "meshy"]}}`)

	m := newAdminModel()
	m.tab = adminTabServices
	m.scursor = 0
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("J")})

	if m.services[0].id != "meshy" || m.services[1].id != "zai" {
		t.Fatalf("order after move = %q, %q", m.services[0].id, m.services[1].id)
	}
	cfg, err := readConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dashboard.Order) < 2 || cfg.Dashboard.Order[0] != "meshy" || cfg.Dashboard.Order[1] != "zai" {
		t.Errorf("persisted order = %v", cfg.Dashboard.Order)
	}
}

// "A" opens the embedded admin; closing it returns to the dashboard and
// schedules a refetch so layout edits apply without a restart.
func TestDashboardAdminKeyRoundTrip(t *testing.T) {
	m := initialModel(Creds{})
	m.width, m.height = 100, 30

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")})
	opened, ok := next.(model)
	if !ok || opened.admin == nil {
		t.Fatal("A did not open the admin screen")
	}
	if !opened.admin.embedded {
		t.Error("admin opened from the dashboard is not embedded")
	}
	if view := opened.View(); !strings.Contains(view, "aimeter admin") {
		t.Errorf("admin view missing title:\n%s", view)
	}

	next, cmd := opened.Update(adminCloseMsg{})
	closed := next.(model)
	if closed.admin != nil {
		t.Error("admin screen stayed open after close")
	}
	if cmd == nil {
		t.Error("closing the admin did not schedule a refetch")
	}
}

// Every admin frame must fit the terminal: rows clip instead of wrapping and
// the key hints shorten on narrow terminals.
func TestAdminViewFitsWidth(t *testing.T) {
	m := &adminModel{}
	for _, p := range adminProviders {
		m.rows = append(m.rows, adminRow{provider: p, source: "codex-auth.json"})
		m.services = append(m.services, adminService{id: p.id, name: p.name, configured: true})
	}
	m.rows[2].source = "missing"
	m.services[3].hidden = true

	for _, tab := range []adminTab{adminTabCredentials, adminTabServices} {
		m.tab = tab
		for _, w := range []int{30, 40, 50, 60, 80, 100} {
			m.width, m.height = w, 40
			for _, line := range strings.Split(m.View(), "\n") {
				if got := visualWidth(line); got > w-1 {
					t.Errorf("tab %d width %d: line renders %d cells: %q", tab, w, got, line)
				}
			}
		}
	}
}
