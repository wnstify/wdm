package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_UpdateFlowListsManagedAppsAndCallsEngineUpdate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listApps: []types.AppInfo{
			{AppID: "alpha", TemplateName: "Alpha", NeedsAttention: false},
		},
		updateResult: &types.UpdateResult{
			AppID:                   "alpha",
			PreviousTemplateVersion: "2026-06-01",
			NewTemplateVersion:      "2026-06-12",
			UpdatedServices:         []string{"web example/web:1 -> example/web:2"},
			RiskClassifications:     []string{"safe"},
			BackupPath:              "/srv/alpha/.wdm-backups/1-update",
			Status:                  &types.AppStatus{AppID: "alpha", State: "running"},
		},
	}
	m := loadUpdateAppsScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Update apps")
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "[selected]")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.updateRequests, 1)
	assert.Equal(t, "alpha", fake.updateRequests[0].AppID)
	assert.False(t, fake.updateRequests[0].DryRun, "TUI update flow applies through the engine path")
	assert.Empty(t, fake.updateRequests[0].TargetTemplateVersion, "TUI update flow has no update-source awareness")

	view = m.View()
	assert.Contains(t, view, "Update complete")
	assert.Contains(t, view, "2026-06-01 -> 2026-06-12")
	assert.Contains(t, view, "web example/web:1 -> example/web:2")
	assert.Contains(t, view, "/srv/alpha/.wdm-backups/1-update")
	assert.Contains(t, view, "running")
}

func TestModel_UpdateFlowRendersDatabaseRiskConfirmationMessageVerbatim(t *testing.T) {
	t.Parallel()

	const databaseRiskMessage = "This update may change the app database.\nTake your own backup before continuing."
	sender := newRecordingSender()
	fake := &confirmingUpdateEngine{
		fakeEngine: &fakeEngine{
			listApps: []types.AppInfo{
				{AppID: "alpha", TemplateName: "Alpha"},
			},
			updateResult: &types.UpdateResult{AppID: "alpha"},
		},
		confirmation: types.Confirmation{
			Kind:    "update_database_risk",
			Title:   "database-risk update for alpha",
			Message: databaseRiskMessage,
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.cursor = 1

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	request := sender.waitConfirmation(t)
	m = updateModel(t, m, request)
	view := m.View()
	assert.Contains(t, view, "database-risk update for alpha")
	assert.Contains(t, view, databaseRiskMessage)
	assert.Contains(t, view, "Yes: y")
	assert.Contains(t, view, "No: n")
	assert.NotContains(t, view, "Take a database backup", "TUI must not re-type the warning")

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("update command did not finish after confirmation")
	}

	require.Len(t, fake.updateRequests, 1)
	assert.Contains(t, m.View(), "Update complete")
}

type confirmingUpdateEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingUpdateEngine) Update(
	ctx context.Context,
	req types.UpdateRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.UpdateResult, error) {
	e.updateRequests = append(e.updateRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, types.NewError(types.ErrCodeUserCanceled, "update canceled", "confirm the update to continue")
		}
	}
	return e.updateResult, e.updateErr
}

func loadUpdateAppsScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
