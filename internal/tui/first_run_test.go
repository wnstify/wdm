package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_FirstRunWizardWelcomeListsPRDStepsAndLoadsCatalog(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogApps: []types.CatalogApp{
			{AppID: "alpha", Name: "Alpha", Summary: "First app"},
		},
	}
	m := newFirstRunModel(fake)

	view := m.View()
	assert.Contains(t, view, "First-run wizard")
	for _, step := range []string{
		"Welcome",
		"Check system requirements",
		"Choose app",
		"Enter app domain",
		"Choose stack path",
		"Confirm generated settings",
		"Generate files and secrets",
		"Validate Compose and Docker readiness",
		"Deploy after confirmation",
		"Show local URL and Pangolin next step",
	} {
		assert.Contains(t, view, step)
	}
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, 0, fake.availableAppsCalls, "welcome must advance to a real system-check step before catalog loading")

	view = m.View()
	assert.Contains(t, view, "First-run wizard")
	assert.Contains(t, view, "Check system requirements")
	assert.Contains(t, view, "Docker and Compose readiness")
	assert.Contains(t, view, "Continue to choose app")

	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	assert.Equal(t, 1, fake.availableAppsCalls)
	view = m.View()
	assert.Contains(t, view, "First-run wizard")
	assert.Contains(t, view, "Install an app")
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "Alpha")
	assert.Contains(t, view, "[selected]")
}

func TestModel_FirstRunWizardMapsInstallProgressAndConfirmationToSteps(t *testing.T) {
	t.Parallel()

	m := newFirstRunModel(&fakeEngine{})
	m.screen = screenInstallForm
	m.firstRun = true
	m.busy = true
	m.catalogDetail = catalogDetailFixture()

	m = updateModel(t, m, progressMsg{
		step:    types.StepInstallRender,
		pct:     25,
		message: "rendering install",
	})
	view := m.View()
	assert.Contains(t, view, "First-run wizard")
	assert.Contains(t, view, "Generate files and secrets")
	assert.Contains(t, view, "rendering install")

	m = updateModel(t, m, progressMsg{
		step:    types.StepInstallComposeValidate,
		pct:     30,
		message: "validating compose config",
	})
	view = m.View()
	assert.Contains(t, view, "Validate Compose and Docker readiness")
	assert.Contains(t, view, "validating compose config")

	confirmation := types.Confirmation{
		Kind:    "install_deploy",
		Title:   "Deploy alpha",
		Message: "Deploy the stack after confirmation.",
	}
	m = updateModel(t, m, confirmationRequestedMsg{
		confirmation: confirmation,
		reply:        make(chan confirmationReply, 1),
	})
	view = m.View()
	assert.Contains(t, view, "First-run wizard")
	assert.Contains(t, view, "Deploy after confirmation")
	assert.Contains(t, view, confirmation.Message)
}

func TestModel_FirstRunWizardDelegatesToInstallFlow(t *testing.T) {
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
				Pangolin: &types.PangolinGuidance{
					TargetURL:            "http://127.0.0.1:8080",
					RecommendedSubdomain: "alpha.example.com",
					Notes:                []string{"Create the Pangolin resource after install."},
				},
				FirstRunNotes: []string{"Open the admin setup screen."},
			},
		},
	}
	m := loadFirstRunInstallForm(t, fake)

	assert.Contains(t, m.View(), "First-run wizard")
	assert.Contains(t, m.View(), "Install inputs")

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
	assert.Contains(t, view, "First-run wizard")
	assert.Contains(t, view, "Install complete")
	assert.Contains(t, view, "http://127.0.0.1:8080")
	assert.Contains(t, view, "alpha.example.com")
	assert.Contains(t, view, "Create the Pangolin resource after install.")
	assert.Contains(t, view, "Open the admin setup screen.")
}

func loadFirstRunInstallForm(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newFirstRunModel(eng)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)

	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
