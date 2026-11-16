package main

// The admin screen — `aimeter -admin` or "A" in the dashboard. Two tabs share
// one model: Credentials manages the keys aimeter resolves (paste, remove,
// never display), Services chooses which provider panels the dashboard shows
// and in which order. Layout changes persist to credentials.json immediately.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type adminTab int

const (
	adminTabServices adminTab = iota
	adminTabCredentials
)

var adminTabTitles = []string{"Services", "Credentials"}

type adminProvider struct {
	id    string
	name  string
	oauth bool // secret stored as access_token, not api_key
}

// adminProviders is also the default dashboard order.
var adminProviders = []adminProvider{
	{"anthropic", "Anthropic", true},
	{"codex", "OpenAI Codex", true},
	{"zai", "Z.AI", false},
	{"kimi", "Kimi", false},
	{"minimax", "MiniMax Code", false},
	{"openrouter", "OpenRouter", false},
	{"elevenlabs", "ElevenLabs", false},
	{"meshy", "Meshy", false},
	{"alibaba", "Alibaba", false},
}

type adminRow struct {
	provider adminProvider
	source   string // resolved credential source; "missing" when absent
	envBound bool   // env var supplies this key, so config edits would be shadowed
}

// adminService is one row of the Services tab: every known provider (plus
// custom services) whether or not it currently has credentials.
type adminService struct {
	id         string
	name       string
	hidden     bool
	configured bool
	custom     bool
}

type adminModel struct {
	tab      adminTab
	rows     []adminRow
	cursor   int
	editing  bool
	input    textinput.Model
	notice   string
	width    int
	height   int
	services []adminService
	scursor  int
	// embedded is set when the screen is opened from the dashboard; closing
	// then returns to the dashboard instead of quitting the program.
	embedded bool
}

var (
	adminTabOn    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Underline(true)
	adminTabOff   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	adminCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	adminWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	adminErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	adminBox      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)
	adminOnBadge  = lipgloss.NewStyle().Foreground(lipgloss.Color("41")).Bold(true)
	adminOffBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
)

func newAdminModel() *adminModel {
	m := &adminModel{}
	ti := textinput.New()
	ti.Placeholder = "paste the key here"
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	m.input = ti
	m.reload("")
	return m
}

// reload rebuilds both tabs from a fresh credential resolution and config.
func (m *adminModel) reload(notice string) {
	c, src := resolveCreds()
	m.rows = m.rows[:0]
	for _, p := range adminProviders {
		source := src[p.id]
		if source == "" {
			source = "missing"
		}
		m.rows = append(m.rows, adminRow{provider: p, source: source, envBound: source == "env"})
	}
	m.services = adminServicesFrom(c, src)
	if m.scursor >= len(m.services) {
		m.scursor = len(m.services) - 1
	}
	if m.scursor < 0 {
		m.scursor = 0
	}
	m.notice = notice
}

// adminServicesFrom lists every known service, ordered by the saved layout.
func adminServicesFrom(c Creds, src map[string]string) []adminService {
	hidden := make(map[string]bool, len(c.Prefs.Hidden))
	for _, id := range c.Prefs.Hidden {
		hidden[id] = true
	}
	mk := func(id, name string, custom bool) adminService {
		s := src[id]
		return adminService{id: id, name: name, hidden: hidden[id], configured: s != "" && s != "missing", custom: custom}
	}
	out := make([]adminService, 0, len(adminProviders)+len(c.Custom))
	for _, p := range adminProviders {
		out = append(out, mk(p.id, p.name, false))
	}
	for _, p := range c.Custom {
		if p.Name != "" {
			out = append(out, mk("custom:"+p.Name, p.Name, true))
		}
	}
	rank := make(map[string]int, len(c.Prefs.Order))
	for i, id := range c.Prefs.Order {
		rank[id] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		return orderLess(out[i].id, out[j].id, rank)
	})
	return out
}

// layout is the persisted dashboard configuration for the current list.
func (m *adminModel) layout() dashboardConfig {
	var p dashboardConfig
	for _, s := range m.services {
		p.Order = append(p.Order, s.id)
		if s.hidden {
			p.Hidden = append(p.Hidden, s.id)
		}
	}
	return p
}

func (m *adminModel) saveLayout(action string) {
	if err := writeDashboardConfig(m.layout()); err != nil {
		m.notice = "save failed: " + err.Error()
		return
	}
	m.notice = action
}

func (m *adminModel) closeCmd() tea.Cmd {
	if m.embedded {
		return func() tea.Msg { return adminCloseMsg{} }
	}
	return tea.Quit
}

func (m *adminModel) Init() tea.Cmd { return textinput.Blink }

func (m *adminModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = k.Width, k.Height
		return m, nil
	}

	if m.editing {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "esc":
				m.editing = false
				m.input.SetValue("")
				m.input.Blur()
				return m, nil
			case "enter":
				secret := strings.TrimSpace(m.input.Value())
				m.editing = false
				m.input.SetValue("")
				m.input.Blur()
				if secret == "" {
					return m, nil
				}
				row := m.rows[m.cursor]
				if err := writeProviderSecret(row.provider.id, secret); err != nil {
					m.reload("save failed: " + err.Error())
					return m, nil
				}
				m.reload(fmt.Sprintf("%s saved to %s", row.provider.name, configPath))
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			return m, m.closeCmd()
		case "tab":
			m.tab = (m.tab + 1) % adminTab(len(adminTabTitles))
			m.notice = ""
			return m, nil
		case "shift+tab":
			m.tab = (m.tab - 1 + adminTab(len(adminTabTitles))) % adminTab(len(adminTabTitles))
			m.notice = ""
			return m, nil
		case "1":
			m.tab, m.notice = adminTabCredentials, ""
			return m, nil
		case "2":
			m.tab, m.notice = adminTabServices, ""
			return m, nil
		}
	}

	if m.tab == adminTabServices {
		return m.updateServices(msg)
	}
	return m.updateCredentials(msg)
}

func (m *adminModel) updateCredentials(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		m.notice = ""
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		m.notice = ""
	case "enter":
		row := m.rows[m.cursor]
		if row.envBound {
			m.notice = fmt.Sprintf("%s comes from an environment variable — unset it to manage the key here", row.provider.name)
			return m, nil
		}
		m.editing = true
		m.notice = ""
		m.input.Focus()
		return m, textinput.Blink
	case "x":
		row := m.rows[m.cursor]
		if row.envBound {
			m.notice = fmt.Sprintf("%s comes from an environment variable — unset it to manage the key here", row.provider.name)
			return m, nil
		}
		if row.source == "missing" {
			return m, nil
		}
		if err := writeProviderSecret(row.provider.id, ""); err != nil {
			m.reload("remove failed: " + err.Error())
			return m, nil
		}
		m.reload(fmt.Sprintf("%s removed from %s", row.provider.name, configPath))
	case "h", "s":
		row := m.rows[m.cursor]
		for i := range m.services {
			if m.services[i].id == row.provider.id {
				m.services[i].hidden = !m.services[i].hidden
				if m.services[i].hidden {
					m.saveLayout(fmt.Sprintf("%s hidden from the dashboard", row.provider.name))
				} else {
					m.saveLayout(fmt.Sprintf("%s shown on the dashboard", row.provider.name))
				}
				break
			}
		}
	}
	return m, nil
}

func (m *adminModel) updateServices(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		if m.scursor > 0 {
			m.scursor--
		}
		m.notice = ""
	case "down", "j":
		if m.scursor < len(m.services)-1 {
			m.scursor++
		}
		m.notice = ""
	case "K", "shift+up":
		m.moveService(-1)
	case "J", "shift+down":
		m.moveService(1)
	case " ", "enter":
		s := &m.services[m.scursor]
		s.hidden = !s.hidden
		if s.hidden {
			m.saveLayout(fmt.Sprintf("%s hidden from the dashboard", s.name))
		} else {
			m.saveLayout(fmt.Sprintf("%s shown on the dashboard", s.name))
		}
	}
	return m, nil
}

// moveService swaps the selected service with its neighbour and persists the
// new order.
func (m *adminModel) moveService(delta int) {
	to := m.scursor + delta
	if to < 0 || to >= len(m.services) {
		return
	}
	m.services[m.scursor], m.services[to] = m.services[to], m.services[m.scursor]
	m.scursor = to
	m.saveLayout(fmt.Sprintf("%s moved to position %d", m.services[to].name, to+1))
}

func (m *adminModel) View() string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	w--
	if w > 96 {
		w = 96
	}
	if w < 20 {
		w = 20
	}
	var b strings.Builder

	left := headerStyle.Render("⚡ aimeter admin")
	if w >= 54 {
		left += dimStyle.Render(" · services and credentials")
	}
	b.WriteString(lipgloss.NewStyle().MaxWidth(w).Render(left) + "\n\n")

	var tabs []string
	for i, title := range adminTabTitles {
		style := adminTabOff
		if adminTab(i) == m.tab {
			style = adminTabOn
		}
		tabs = append(tabs, style.Render(fmt.Sprintf("%d %s", i+1, title)))
	}
	b.WriteString(lipgloss.NewStyle().MaxWidth(w).Render("  "+strings.Join(tabs, dimStyle.Render("   "))) + "\n\n")

	if m.tab == adminTabServices {
		b.WriteString(m.servicesView(w))
	} else {
		b.WriteString(m.credentialsView(w))
	}

	if m.notice != "" {
		style := dimStyle
		if strings.Contains(m.notice, "failed") {
			style = adminErr
		}
		b.WriteString("\n\n" + style.Render("  "+truncateCells(m.notice, w-4)))
	}
	if m.editing {
		hint := fmt.Sprintf("  key for %s · input hidden · Enter saves · Esc cancels", m.rows[m.cursor].provider.name)
		b.WriteString("\n\n" + lipgloss.NewStyle().MaxWidth(w).Render(dimStyle.Render(hint)))
		b.WriteString("\n  " + m.input.View())
	}

	b.WriteString("\n\n" + lipgloss.NewStyle().MaxWidth(w).Render("  "+styledKeys(m.hint(w))))
	return b.String()
}

func (m *adminModel) hint(w int) string {
	closeHint := "Esc close"
	if m.embedded {
		closeHint = "Esc dashboard"
	}
	if w >= 76 {
		if m.tab == adminTabServices {
			return "↑/↓ move · Space show/hide · K/J reorder · Tab credentials · " + closeHint
		}
		return "↑/↓ move · Enter add/edit key · x remove · h show/hide · Tab services · " + closeHint
	}
	if m.tab == adminTabServices {
		return "↑↓ · Space toggle · K/J order · Tab · " + closeHint
	}
	return "↑↓ · Enter key · x remove · h show/hide · Tab · " + closeHint
}

func (m *adminModel) serviceById(id string) *adminService {
	for i := range m.services {
		if m.services[i].id == id {
			return &m.services[i]
		}
	}
	return nil
}

func (m *adminModel) credentialsView(w int) string {
	content := w - 4 // row budget inside border and padding
	showSource := content >= 56
	nameW := 20
	if !showSource {
		nameW = content - 12
		if nameW > 20 {
			nameW = 20
		}
		if nameW < 8 {
			nameW = 8
		}
	}
	srcW := 16

	header := "  " + padCell("PROVIDER", nameW+3)
	if showSource {
		header += padCell("SOURCE", srcW+3)
	}
	lines := []string{dimStyle.Render(header + "STATUS")}
	for i, r := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = adminCursor.Render("▸ ")
		}
		name := nameStyle.Render(truncateCells(r.provider.name, nameW))
		name += strings.Repeat(" ", nameW-lipgloss.Width(name)+3)
		source := ""
		if showSource {
			source = dimStyle.Render(truncateCells(r.source, srcW))
			source += strings.Repeat(" ", srcW-lipgloss.Width(truncateCells(r.source, srcW))+3)
		}

		var status string
		switch {
		case r.envBound:
			status = adminWarn.Render("◆ env")
		case r.source == "missing":
			status = adminOffBadge.Render("○ no key")
		default:
			status = adminOnBadge.Render("● ready")
		}
		if s := m.serviceById(r.provider.id); s != nil && s.hidden {
			status += adminOffBadge.Render("  hidden")
		}
		lines = append(lines, cursor+name+source+status)
	}
	return adminBox.Width(content + 2).Render(clipLines(lines, content))
}

func (m *adminModel) servicesView(w int) string {
	content := w - 4
	showID := content >= 60
	nameW := 22
	if !showID {
		nameW = content - 16
		if nameW > 22 {
			nameW = 22
		}
		if nameW < 8 {
			nameW = 8
		}
	}

	header := strings.Repeat(" ", 8) + padCell("SERVICE", nameW+3) + padCell("SHOW", 9)
	if showID {
		header += "KEY"
	}
	lines := []string{dimStyle.Render(header)}
	for i, s := range m.services {
		cursor := "  "
		if i == m.scursor {
			cursor = adminCursor.Render("▸ ")
		}
		dot := adminOffBadge.Render("○")
		if s.configured {
			dot = adminOnBadge.Render("●")
		}
		box := adminOffBadge.Render("[ ]")
		state := adminOffBadge.Render(padCell("hidden", 9))
		if !s.hidden {
			box = adminOnBadge.Render("[x]")
			state = adminOnBadge.Render(padCell("shown", 9))
		}
		nameText := truncateCells(s.name, nameW)
		name := nameStyle.Render(nameText)
		if s.hidden {
			name = dimStyle.Render(nameText)
		}
		name += strings.Repeat(" ", nameW-lipgloss.Width(nameText)+3)
		line := cursor + dot + " " + box + " " + name + state
		if showID {
			line += dimStyle.Render(truncateCells(s.id, 24))
		}
		lines = append(lines, line)
	}
	body := adminBox.Width(content + 2).Render(clipLines(lines, content))
	legend := "  ● credentials found   ○ no credentials   Space toggles, K/J reorders"
	if w < 70 {
		legend = "  ● found   ○ no credentials"
	}
	return body + "\n\n" + lipgloss.NewStyle().MaxWidth(w).Render(dimStyle.Render(legend))
}

// clipLines keeps every row inside the box, even in a very narrow terminal.
func clipLines(lines []string, content int) string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = lipgloss.NewStyle().MaxWidth(content).Render(line)
	}
	return strings.Join(out, "\n")
}

// padCell pads plain text to n cells (lipgloss-aware, no styling involved).
func padCell(s string, n int) string {
	if lipgloss.Width(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-lipgloss.Width(s))
}

func runAdmin() error {
	m := newAdminModel()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
