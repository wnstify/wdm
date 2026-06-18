package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_SettingsScreenLoadsCurrentSettings(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{settings: settingsFixture()}
	m := loadSettingsScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Settings")
	assert.Contains(t, view, "base_stack_path")
	assert.Contains(t, view, "~/docker")
	assert.Contains(t, view, "timezone")
	assert.Contains(t, view, "default_docker_network")
	assert.Contains(t, view, "wdm")
	assert.Contains(t, view, "catalog_channel")
	assert.Contains(t, view, "stable")
	assert.Contains(t, view, "locked")
	assert.Contains(t, view, "update_check_preference")
	assert.Contains(t, view, "manual")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
	assert.Equal(t, 1, fake.settingsCalls)
}

func TestModel_SettingsScreenPersistsMergedSettingsWithoutReread(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{settings: settingsFixture()}
	m := loadSettingsScreen(t, fake)

	m = updateModel(t, m, downKey())
	m = typeIntoSettingsField(t, m, "UTC")
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())

	require.Len(t, fake.updatedSettings, 1)
	want := *settingsFixture()
	want.Timezone = "UTC"
	assert.Equal(t, want, fake.updatedSettings[0])
	assert.Equal(t, 1, fake.settingsCalls, "UpdateSettings must not be followed by a stale Settings re-read")

	view := m.View()
	assert.Contains(t, view, "Settings saved")
	assert.Contains(t, view, "timezone: UTC")
	assert.Contains(t, view, "catalog_channel: stable")
}

func TestModel_SettingsInputBackspaceRemovesLastRuneAndClearsMessage(t *testing.T) {
	t.Parallel()

	m := model{
		settingsFields: []settingsField{
			{key: "timezone", value: "UTC"},
			{key: "base_stack_path", value: "/srv/donnée"},
		},
		settingsCursor:  1,
		settingsMessage: "Settings saved",
	}

	m = m.deleteSettingsInputRune()
	assert.Equal(t, "/srv/donné", m.settingsFields[1].value)
	assert.Empty(t, m.settingsMessage)

	m.settingsCursor = len(m.settingsFields)
	unchanged := m.deleteSettingsInputRune()
	assert.Equal(t, m.settingsFields, unchanged.settingsFields)

	m.settingsCursor = 0
	m.settingsFields[0].value = ""
	assert.Empty(t, m.deleteSettingsInputRune().settingsFields[0].value)
}

func loadSettingsScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	for range 4 {
		m = updateModel(t, m, downKey())
	}

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func typeIntoSettingsField(t *testing.T, m model, value string) model {
	t.Helper()

	for _, r := range value {
		m = updateModel(t, m, runeKey(r))
	}
	return m
}

func settingsFixture() *types.Settings {
	return &types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "~/docker",
		Timezone:              "",
		DefaultDockerNetwork:  "wdm",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "manual",
	}
}
