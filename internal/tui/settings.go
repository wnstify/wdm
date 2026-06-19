package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

type settingsField struct {
	key   string
	value string
	apply func(*types.Settings, string)
}

type settingsLoadedMsg struct {
	settings *types.Settings
	err      error
}

type settingsSavedMsg struct {
	settings types.Settings
	err      error
}

func (m model) loadSettingsCmd() tea.Cmd {
	return func() tea.Msg {
		settings, err := m.eng.Settings(m.ctx)
		if err == nil && settings == nil {
			err = fmt.Errorf("settings unavailable")
		}
		return settingsLoadedMsg{settings: settings, err: err}
	}
}

func newSettingsFields(settings *types.Settings) []settingsField {
	return []settingsField{
		{
			key:   "base_stack_path",
			value: settings.BaseStackPath,
			apply: func(s *types.Settings, value string) {
				s.BaseStackPath = value
			},
		},
		{
			key:   "timezone",
			value: settings.Timezone,
			apply: func(s *types.Settings, value string) {
				s.Timezone = value
			},
		},
		{
			key:   "default_docker_network",
			value: settings.DefaultDockerNetwork,
			apply: func(s *types.Settings, value string) {
				s.DefaultDockerNetwork = value
			},
		},
		{
			key:   "update_check_preference",
			value: settings.UpdateCheckPreference,
			apply: func(s *types.Settings, value string) {
				s.UpdateCheckPreference = value
			},
		},
	}
}

func (m model) updateSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up) && m.settingsCursor > 0:
		m.settingsCursor--
	case key.Matches(msg, m.keys.Down) && m.settingsCursor < len(m.settingsFields):
		m.settingsCursor++
	case key.Matches(msg, m.keys.Select):
		if m.settingsCursor < len(m.settingsFields) {
			m.settingsCursor++
			return m, nil
		}
		return m.saveSettings()
	case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
		m = m.deleteSettingsInputRune()
	case msg.Type == tea.KeySpace:
		m = m.appendSettingsInput(" ")
	case msg.Type == tea.KeyRunes:
		m = m.appendSettingsInput(string(msg.Runes))
	}

	return m, nil
}

func (m model) appendSettingsInput(value string) model {
	if m.settingsCursor >= len(m.settingsFields) {
		return m
	}
	m.settingsFields[m.settingsCursor].value += value
	m.settingsMessage = ""
	return m
}

func (m model) deleteSettingsInputRune() model {
	if m.settingsCursor >= len(m.settingsFields) {
		return m
	}

	value := m.settingsFields[m.settingsCursor].value
	if value == "" {
		return m
	}

	runes := []rune(value)
	m.settingsFields[m.settingsCursor].value = string(runes[:len(runes)-1])
	m.settingsMessage = ""
	return m
}

func (m model) saveSettings() (tea.Model, tea.Cmd) {
	if m.settings == nil {
		m.err = fmt.Errorf("settings unavailable")
		return m, nil
	}

	settings := m.mergedSettings()
	m.busy = true
	m.err = nil
	m.settingsMessage = ""
	return m, m.saveSettingsCmd(settings)
}

func (m model) mergedSettings() types.Settings {
	settings := *m.settings
	for _, field := range m.settingsFields {
		field.apply(&settings, field.value)
	}
	return settings
}

func (m model) saveSettingsCmd(settings types.Settings) tea.Cmd {
	return func() tea.Msg {
		err := m.eng.UpdateSettings(m.ctx, settings)
		return settingsSavedMsg{settings: settings, err: err}
	}
}

func (m model) settingsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Settings"))
	b.WriteString("\n\n")

	switch {
	case m.busy && m.settings == nil:
		b.WriteString("Loading settings...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	case m.busy:
		b.WriteString("Saving settings...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	case m.err != nil:
		b.WriteString("Settings failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}

	if m.settingsMessage != "" {
		b.WriteString(m.settingsMessage)
		b.WriteString("\n\n")
	}

	if m.settings != nil {
		fmt.Fprintf(&b, "schema_version: %d\n", m.settings.SchemaVersion)
		b.WriteString("catalog_channel: ")
		b.WriteString(m.settings.CatalogChannel)
		b.WriteString(" (locked)\n\n")
	}

	for i, field := range m.settingsFields {
		prefix := "  "
		suffix := ""
		if i == m.settingsCursor {
			prefix = "> "
			suffix = " [selected]"
		}
		b.WriteString(prefix)
		b.WriteString(field.key)
		b.WriteString(": ")
		b.WriteString(field.value)
		b.WriteString(suffix)
		b.WriteByte('\n')
	}

	prefix := "  "
	suffix := ""
	if m.settingsCursor == len(m.settingsFields) {
		prefix = "> "
		suffix = " [selected]"
	}
	b.WriteString(prefix)
	b.WriteString("Save settings")
	b.WriteString(suffix)
	b.WriteByte('\n')

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}
