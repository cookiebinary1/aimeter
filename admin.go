package main

// aimeter -admin: a small TUI for managing provider credentials. Lists every
// known provider with its resolved source, lets the user paste a key (stored
// in credentials.json) or remove the stored copy. Secrets are never displayed.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type adminProvider struct {
	id    string
	name  string
	oauth bool // secret stored as access_token, not api_key
}

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

type adminModel struct {
	rows    []adminRow
	cursor  int
	editing bool
	input   textinput.Model
	notice  string
	width   int
	height  int
}

func newAdminModel() adminModel {
	m := adminModel{}
	ti := textinput.New()
	ti.Placeholder = "paste the key here"
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	m.input = ti
	m.reload("")
	return m
}

// refresh rows from a fresh credential resolution.
func (m *adminModel) reload(notice string) {
	_, src := resolveCreds()
	m.rows = m.rows[:0]
	for _, p := range adminProviders {
		source := src[p.id]
		if source == "" {
			source = "missing"
		}
		m.rows = append(m.rows, adminRow{provider: p, source: source, envBound: source == "env"})
	}
	m.notice = notice
}

func (m adminModel) Init() tea.Cmd { return textinput.Blink }

func (m adminModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
				id := m.rows[m.cursor].provider.id
				if err := writeProviderSecret(id, secret); err != nil {
					m.reload("save failed: " + err.Error())
					return m, nil
				}
				m.reload(fmt.Sprintf("%s saved to %s", m.rows[m.cursor].provider.name, configPath))
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
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
		}
	}
	return m, nil
}

func (m adminModel) View() string {
	w := m.width
	if w <= 0 {
		w = 101
	}
	w--
	var b strings.Builder

	left := headerStyle.Render("⚡ aimeter admin")
	if w >= 46 {
		left += dimStyle.Render(" · provider credentials")
	}
	b.WriteString(left)
	b.WriteString("\n\n")

	nameW := 0
	for _, p := range adminProviders {
		if len(p.name) > nameW {
			nameW = len(p.name)
		}
	}
	for i, row := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = headerStyle.Render("> ")
		}
		name := lipgloss.NewStyle().Width(nameW).Render(row.provider.name)
		status := row.source
		switch {
		case row.envBound:
			status = dimStyle.Render(status + " (env manages this)")
		case row.source == "missing":
			status = dimStyle.Render(status + " — press Enter to add a key")
		default:
			status = okDotStyle.Render(status)
		}
		b.WriteString(cursor + name + "  " + status + "\n")
	}

	if m.notice != "" {
		b.WriteString("\n" + lipgloss.NewStyle().Width(w).Render(m.notice))
	}
	if m.editing {
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("key for %s (input hidden · Enter saves · Esc cancels):", m.rows[m.cursor].provider.name)) + "\n")
		b.WriteString(m.input.View())
	}

	b.WriteString("\n\n" + dimStyle.Render("↑/↓ select · Enter edit/add · x remove · q quit"))
	return b.String()
}

func runAdmin() error {
	m := newAdminModel()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
