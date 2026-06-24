package tui

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_CheckMyAppsLoadsManagedApps(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "n8n", TemplateName: "n8n"},
				State:   "running",
			},
			{
				AppInfo:          types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
				State:            "needs_attention",
				AttentionReasons: []string{"container_exited"},
			},
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
	assert.Equal(t, 1, fake.listStatusCalls)
}

func TestModel_CheckMyAppsListReflectsLiveState(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"},
				State:   "running",
			},
			{
				AppInfo:          types.AppInfo{AppID: "bravo", TemplateName: "Bravo", NeedsAttention: true},
				State:            "needs_attention",
				AttentionReasons: []string{"container_missing"},
			},
			{
				AppInfo: types.AppInfo{AppID: "charlie", TemplateName: "Charlie"},
				State:   "removed",
			},
		},
	}
	m := loadCheckAppsScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "running")
	assert.Contains(t, view, "bravo")
	assert.Contains(t, view, "needs attention")
	assert.Contains(t, view, "charlie")
	assert.Contains(t, view, "removed")
	assert.Equal(t, 1, fake.listStatusCalls)
	assert.Equal(t, 0, fake.listCalls)
}

func TestModel_CheckMyAppsSelectionLoadsStatusAndNextActions(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
				State:   "needs_attention",
			},
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
		"Manage resources",
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
		listStatusApps: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
				State:   "needs_attention",
			},
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

	for checkAppActions[m.actionCursor] != "Restart app" {
		m = updateModel(t, m, downKey())
	}
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	assert.Equal(t, []string{"uptime-kuma"}, fake.restartCalls)
	assert.Contains(t, m.View(), "Restart complete")
	assert.Contains(t, m.View(), "running")

	for checkAppActions[m.actionCursor] != "Validate config" {
		m = updateModel(t, m, downKey())
	}
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	assert.Equal(t, []string{"uptime-kuma"}, fake.validateCalls)
	assert.Contains(t, m.View(), "Compose config is invalid")
	assert.Contains(t, m.View(), "services.app.image is required")
}

// TestModel_CheckMyAppsApplyOverlayChangesUsesEngine proves the "Apply
// overlay changes" action triggers the redeploy Cmd, which calls
// RedeployStack via the engine bridge, and the resulting status is shown.
func TestModel_CheckMyAppsApplyOverlayChangesUsesEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", NeedsAttention: true},
				State:   "needs_attention",
			},
		},
		statuses: map[string]*types.AppStatus{
			"uptime-kuma": {
				AppID:          "uptime-kuma",
				State:          "needs attention",
				NeedsAttention: true,
			},
		},
		redeployResult: &types.RestartResult{
			AppID:             "uptime-kuma",
			RestartedServices: []string{"app"},
			Status:            &types.AppStatus{AppID: "uptime-kuma", State: "running"},
		},
	}
	m := loadCheckAppsStatusScreen(t, fake)

	for checkAppActions[m.actionCursor] != "Apply overlay changes" {
		m = updateModel(t, m, downKey())
	}
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	assert.Equal(t, []string{"uptime-kuma"}, fake.redeployCalls)
	assert.Contains(t, m.View(), "Overlay changes applied")
	assert.Contains(t, m.View(), "running")
}

func TestModel_CheckAppsViewRendersLoadingErrorAndEmptyStates(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		m    model
		want string
	}{
		{
			name: "loading",
			m:    model{busy: true},
			want: "Loading managed apps...",
		},
		{
			name: "error",
			m:    model{err: errors.New("docker daemon unavailable")},
			want: "Could not load managed apps: docker daemon unavailable",
		},
		{
			name: "empty",
			m:    model{},
			want: "No managed apps found.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			view := tt.m.checkAppsView()
			assert.Contains(t, view, "Check my apps")
			assert.Contains(t, view, tt.want)
			assert.Contains(t, view, "Back: b")
			assert.Contains(t, view, "Quit: q")
		})
	}
}

func TestModel_AppActionsViewRendersBusyErrorValidationAndFallbacks(t *testing.T) {
	t.Parallel()

	t.Run("busy", func(t *testing.T) {
		t.Parallel()

		m := model{
			busy: true,
			apps: []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "vaultwarden"}}},
		}

		view := m.appActionsView()
		assert.Contains(t, view, "Working on vaultwarden...")
		assert.Contains(t, view, "Back: b")
		assert.Contains(t, view, "Quit: q")
		assert.NotContains(t, view, "Next actions")
	})

	t.Run("error with unavailable status", func(t *testing.T) {
		t.Parallel()

		m := model{
			err:  errors.New("restart failed"),
			apps: []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "vaultwarden"}}},
		}

		view := m.appActionsView()
		assert.Contains(t, view, "Action failed: restart failed")
		assert.Contains(t, view, "vaultwarden")
		assert.Contains(t, view, "Status unavailable.")
		assert.Contains(t, view, "Next actions")
	})

	t.Run("valid config and action message", func(t *testing.T) {
		t.Parallel()

		m := model{
			status: &types.AppStatus{
				AppID: "vaultwarden",
				State: "running",
				Services: []types.ServiceStatus{
					{Service: "server", State: "running"},
				},
			},
			actionMessage: "Restart complete.",
			validation: &types.ValidationResult{
				Valid:  true,
				Detail: "compose.yaml is valid",
			},
		}

		view := m.appActionsView()
		assert.Contains(t, view, "State: running")
		assert.Contains(t, view, "- server: running")
		assert.Contains(t, view, "Restart complete.")
		assert.Contains(t, view, "Compose config is valid.")
		assert.Contains(t, view, "compose.yaml is valid")
		assert.Contains(t, view, "> View details [selected]")
	})
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
