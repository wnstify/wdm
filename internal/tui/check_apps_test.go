package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_CheckMyAppsLoadsManagedApps(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listApps: []types.AppInfo{
			{AppID: "n8n", TemplateName: "n8n", NeedsAttention: false},
			{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
		},
	}
	m := newReadyModel(fake)

	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())

	view := m.View()
	assert.Contains(t, view, "Check my apps")
	assert.Contains(t, view, "n8n")
	assert.Contains(t, view, "running")
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "needs attention")
	assert.Contains(t, view, "> n8n")
	assert.Contains(t, view, "[selected]")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
	assert.Equal(t, 1, fake.listCalls)
}

func TestModel_CheckMyAppsSelectionLoadsStatusAndNextActions(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listApps: []types.AppInfo{
			{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
		},
		statuses: map[string]*types.AppStatus{
			"uptime-kuma": {
				AppID:            "uptime-kuma",
				State:            "needs attention",
				Message:          "container exited unexpectedly",
				NeedsAttention:   true,
				AttentionReasons: []string{"container_exited"},
				Services: []types.ServiceStatus{
					{Service: "app", State: "exited", NeedsAttention: true, Message: "exit 1"},
				},
			},
		},
	}
	m := loadCheckAppsScreen(t, fake)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())

	view := m.View()
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "container exited unexpectedly")
	assert.Contains(t, view, "container_exited")
	for _, action := range []string{
		"View details",
		"Restart app",
		"Remove app",
		"Validate config",
		"Return to dashboard",
	} {
		assert.Contains(t, view, action)
	}
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
	assert.Equal(t, []string{"uptime-kuma"}, fake.statusCalls)
}

func TestModel_CheckMyAppsRestartAndValidateUseEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listApps: []types.AppInfo{
			{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
		},
		statuses: map[string]*types.AppStatus{
			"uptime-kuma": {
				AppID:          "uptime-kuma",
				State:          "needs attention",
				NeedsAttention: true,
			},
		},
		restartResult: &types.RestartResult{
			AppID:             "uptime-kuma",
			RestartedServices: []string{"app"},
			Status:            &types.AppStatus{AppID: "uptime-kuma", State: "running"},
		},
		validationResult: &types.ValidationResult{
			AppID:  "uptime-kuma",
			Valid:  false,
			Detail: "services.app.image is required",
		},
	}
	m := loadCheckAppsStatusScreen(t, fake)

	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	assert.Equal(t, []string{"uptime-kuma"}, fake.restartCalls)
	assert.Contains(t, m.View(), "Restart complete")
	assert.Contains(t, m.View(), "running")

	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	assert.Equal(t, []string{"uptime-kuma"}, fake.validateCalls)
	assert.Contains(t, m.View(), "Compose config is invalid")
	assert.Contains(t, m.View(), "services.app.image is required")
}

func newReadyModel(eng *fakeEngine) model {
	m := newModel(eng)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	return m
}

func loadCheckAppsScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func loadCheckAppsStatusScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := loadCheckAppsScreen(t, eng)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
