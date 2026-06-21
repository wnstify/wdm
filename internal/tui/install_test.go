package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_InstallPickerLoadsCatalogApps(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogApps: []types.CatalogApp{
			{AppID: "alpha", Name: "Alpha", Summary: "First app", TemplateVersion: "2026-06-12"},
			{AppID: "bravo", Name: "Bravo", Summary: "Second app", TemplateVersion: "2026-06-13"},
		},
	}
	m := newReadyModel(fake)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())

	view := m.View()
	assert.Contains(t, view, "Install an app")
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "Alpha")
	assert.Contains(t, view, "First app")
	assert.Contains(t, view, "bravo")
	assert.Contains(t, view, "Second app")
	assert.Contains(t, view, "> alpha")
	assert.Contains(t, view, "[selected]")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
	assert.Equal(t, 1, fake.availableAppsCalls)
}

func TestModel_InstallFormRendersCatalogPlaceholderMetadata(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogApps: []types.CatalogApp{
			{AppID: "alpha", Name: "Alpha", Summary: "First app"},
		},
		catalogDetails: map[string]*types.CatalogApp{
			"alpha": catalogDetailFixture(),
		},
	}
	m := loadInstallForm(t, fake)

	view := m.View()
	assert.Contains(t, view, "Alpha")
	assert.Contains(t, view, "Long-form app detail")
	assert.Contains(t, view, "DOMAIN")
	assert.Contains(t, view, "domain")
	assert.Contains(t, view, "required")
	assert.Contains(t, view, "MEDIA_PATH")
	assert.Contains(t, view, "/srv/media")
	assert.Contains(t, view, "SECRET_TOKEN")
	assert.Contains(t, view, "generated")
	assert.Contains(t, view, "8080 -> 80/tcp")
	assert.Contains(t, view, "worker example/worker:1.2.3")
	assert.Contains(t, view, "Esc: back")
	assert.Contains(t, view, "Ctrl+C: quit")
}

func installFormFake() *fakeEngine {
	return &fakeEngine{
		catalogApps: []types.CatalogApp{
			{AppID: "alpha", Name: "Alpha", Summary: "First app"},
		},
		catalogDetails: map[string]*types.CatalogApp{
			"alpha": catalogDetailFixture(),
		},
	}
}

func TestModel_InstallFormAcceptsBAndQAsTypedInput(t *testing.T) {
	t.Parallel()

	m := loadInstallForm(t, installFormFake())
	require.Equal(t, screenInstallForm, m.screen)

	m = updateModel(t, m, runeKey('b'))
	m = updateModel(t, m, runeKey('q'))

	assert.Equal(t, "bq", m.installFields[m.installFieldCursor].value)
	assert.Equal(t, screenInstallForm, m.screen)
	assert.False(t, m.exiting)
}

func TestModel_InstallFormEscStillGoesBack(t *testing.T) {
	t.Parallel()

	m := loadInstallForm(t, installFormFake())

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = assertModel(t, next)
	require.Nil(t, cmd)

	assert.NotEqual(t, screenInstallForm, m.screen)
	assert.False(t, m.exiting)
}

func TestModel_InstallFormCtrlCStillQuits(t *testing.T) {
	t.Parallel()

	m := loadInstallForm(t, installFormFake())

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	require.NotNil(t, cmd)
	assert.Equal(t, tea.Quit(), cmd())
}

func TestModel_InstallFlowCallsEngineInstallWithFormValues(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogApps: []types.CatalogApp{
			{AppID: "alpha", Name: "Alpha", Summary: "First app"},
		},
		catalogDetails: map[string]*types.CatalogApp{
			"alpha": catalogDetailFixture(),
		},
		installResult: &types.InstallResult{
			AppID:     "alpha",
			StackPath: "/srv/alpha",
			PostInstallGuidance: &types.PostInstallGuidance{
				LocalTargetURL: "http://127.0.0.1:8080",
				GeneratedCredentials: []types.GeneratedCredential{
					{
						Label: "Alpha ADMIN_TOKEN",
						Value: "one-time-token",
						Note:  "Store this value now.",
					},
				},
			},
		},
	}
	m := loadInstallForm(t, fake)

	m = typeIntoInstallField(t, m, "alpha.example.com")
	m = updateModel(t, m, downKey())
	m = typeIntoInstallField(t, m, "/srv/alpha")
	m = updateModel(t, m, downKey())
	m = typeIntoInstallField(t, m, "/data/media")
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 1)
	got := fake.installRequests[0]
	assert.Equal(t, "alpha", got.AppID)
	assert.Equal(t, "alpha.example.com", got.Domain)
	assert.Equal(t, "/srv/alpha", got.StackPath)
	assert.Equal(t, map[string]string{"MEDIA_PATH": "/data/media"}, got.PlaceholderValues)
	assert.NotContains(t, got.PlaceholderValues, "SECRET_TOKEN")

	view := m.View()
	assert.Contains(t, view, "Install complete")
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "/srv/alpha")
	assert.Contains(t, view, "http://127.0.0.1:8080")
	assert.Contains(t, view, "SAVE THIS NOW")
	assert.Contains(t, view, "Alpha ADMIN_TOKEN")
	assert.Contains(t, view, "one-time-token")
	assert.Contains(t, view, "Store this value now.")
}

func TestModel_InstallInputBackspaceRemovesLastRune(t *testing.T) {
	t.Parallel()

	m := model{
		installFields: []installField{
			{value: "cafe"},
			{value: "été"},
		},
		installFieldCursor: 1,
	}

	m = m.deleteInstallInputRune()
	assert.Equal(t, "ét", m.installFields[1].value)

	m.installFieldCursor = len(m.installFields)
	unchanged := m.deleteInstallInputRune()
	assert.Equal(t, m.installFields, unchanged.installFields)

	m.installFieldCursor = 0
	m.installFields[0].value = ""
	assert.Empty(t, m.deleteInstallInputRune().installFields[0].value)
}

func TestSelectedCatalogLabelsUseSafeFallbacks(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Install an app", selectedCatalogName(nil))
	assert.Equal(t, "app", selectedCatalogID(nil))

	app := &types.CatalogApp{AppID: "vaultwarden"}
	assert.Equal(t, "vaultwarden", selectedCatalogName(app))
	assert.Equal(t, "vaultwarden", selectedCatalogID(app))

	app.Name = "Vaultwarden"
	assert.Equal(t, "Vaultwarden", selectedCatalogName(app))
}

func TestModel_InstallFormKeyNavigationAndEditing(t *testing.T) {
	t.Parallel()

	// The form key handler is exercised directly: the top-level Update
	// intercepts Back ("b"/"esc") before screen-specific keys, so driving the
	// arms through Update would never reach them for those runes.
	m := model{
		keys: defaultKeyMap(),
		installFields: []installField{
			{label: "DOMAIN", value: ""},
			{label: "MEDIA_PATH", value: ""},
		},
		installFieldCursor: 1,
	}

	// Up moves the cursor toward the first field.
	next, cmd := m.updateInstallFormKey(tea.KeyMsg{Type: tea.KeyUp})
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, 0, m.installFieldCursor, "Up must decrement the install cursor")

	// Rune and Space input both append to the focused field.
	next, cmd = m.updateInstallFormKey(runeKey('a'))
	m = assertModel(t, next)
	require.Nil(t, cmd)
	next, cmd = m.updateInstallFormKey(tea.KeyMsg{Type: tea.KeySpace})
	m = assertModel(t, next)
	require.Nil(t, cmd)
	next, cmd = m.updateInstallFormKey(runeKey('b'))
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, "a b", m.installFields[0].value)

	// Backspace deletes the last rune of the focused field.
	next, cmd = m.updateInstallFormKey(tea.KeyMsg{Type: tea.KeyBackspace})
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, "a ", m.installFields[0].value)
}

// TestModel_InstallFailureShowsLogPathNotice covers the PRD §24 failure UX in
// the TUI: a failed install surfaces the engine log path and the
// review-before-sharing reminder, and stays silent when no engine-owned file
// sink exists (LogPath empty).
func TestModel_InstallFailureShowsLogPathNotice(t *testing.T) {
	t.Parallel()

	t.Run("with file sink", func(t *testing.T) {
		t.Parallel()
		m := model{
			eng:    &fakeEngine{logPath: "/home/user/.local/state/wdm/logs/latest.log"},
			screen: screenInstallForm,
			err:    assert.AnError,
		}
		view := m.installFormView()
		assert.Contains(t, view, "Install failed:")
		assert.Contains(t, view, "/home/user/.local/state/wdm/logs/latest.log")
		assert.Contains(t, view, "review the log before sharing it publicly")
	})

	t.Run("without file sink", func(t *testing.T) {
		t.Parallel()
		m := model{
			eng:    &fakeEngine{logPath: ""},
			screen: screenInstallForm,
			err:    assert.AnError,
		}
		view := m.installFormView()
		assert.Contains(t, view, "Install failed:")
		assert.NotContains(t, view, "review the log before sharing it publicly")
	})
}

func loadInstallForm(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func typeIntoInstallField(t *testing.T, m model, value string) model {
	t.Helper()

	for _, r := range value {
		m = updateModel(t, m, runeKey(r))
	}
	return m
}

func catalogDetailFixture() *types.CatalogApp {
	return &types.CatalogApp{
		AppID:           "alpha",
		Name:            "Alpha",
		Summary:         "First app",
		Description:     "Long-form app detail",
		TemplateName:    "alpha-template",
		TemplateVersion: "2026-06-12",
		Channel:         "stable",
		Placeholders: []types.CatalogPlaceholder{
			{Key: "DOMAIN", Type: "domain", Required: true},
			{Key: "MEDIA_PATH", Type: "path", Required: true, Default: "/srv/media"},
			{Key: "SECRET_TOKEN", Type: "secret", Secret: true},
		},
		Ports: []types.CatalogPort{
			{Service: "web", Host: 8080, Container: 80, Protocol: "tcp"},
		},
		ImagePins: []types.CatalogImagePin{
			{Service: "worker", Image: "example/worker", Tag: "1.2.3"},
		},
	}
}
