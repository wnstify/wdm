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

func TestModel_BackupsFlowListsAppsAndConfigBackups(t *testing.T) {
	t.Parallel()

	fake := backupsFlowFake()
	m := loadBackupsAppsScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Backups")
	assert.Contains(t, view, "alpha")
	assert.Contains(t, view, "[selected]")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	assert.Equal(t, []string{"alpha"}, fake.listBackupCalls)
	view = m.View()
	assert.Contains(t, view, "Config backups for alpha")
	assert.Contains(t, view, "2026-update")
	assert.Contains(t, view, "update")
	assert.Contains(t, view, "2 file(s)")
	assert.NotContains(t, view, "Create backup")
	assert.NotContains(t, view, "Prune")
	assert.NotContains(t, view, "Delete backup")
}

func TestModel_BackupsRestoreCallsEngineAndRendersConfigRestoreResult(t *testing.T) {
	t.Parallel()

	fake := backupsFlowFake()
	fake.restoreResult = &types.RestoreBackupResult{
		AppID:          "alpha",
		SnapshotID:     "2026-update",
		RestoredFiles:  []string{"compose.yaml", ".env"},
		BoundaryNotice: "Config files were restored; app data, databases, uploads, and volumes were not restored.",
		NextAction:     "Run wdm apps update alpha to recreate containers and apply the restored config.",
		Status:         &types.AppStatus{AppID: "alpha", State: "needs attention", Message: "containers still use the old config"},
	}
	m := loadBackupsListScreen(t, fake)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.restoreRequests, 1)
	assert.Equal(t, "alpha", fake.restoreRequests[0].AppID)
	assert.Equal(t, "2026-update", fake.restoreRequests[0].SnapshotID)
	assert.Empty(t, fake.restoreRequests[0].StackPath)

	view := m.View()
	assert.Contains(t, view, "Config restore complete")
	assert.Contains(t, view, "alpha config restored from snapshot 2026-update")
	assert.Contains(t, view, "compose.yaml")
	assert.Contains(t, view, fake.restoreResult.BoundaryNotice)
	assert.Contains(t, view, "Next: "+fake.restoreResult.NextAction)
	assert.Contains(t, view, "needs attention")
	assert.Contains(t, view, "containers still use the old config")
}

func TestModel_BackupsRestoreRendersEngineConfirmationVerbatim(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &confirmingRestoreEngine{
		fakeEngine: backupsFlowFake(),
		confirmation: types.Confirmation{
			Kind:    "restore_config",
			Title:   "Restore config for alpha",
			Message: "This config restore rewrites managed files only.",
		},
	}
	fake.restoreResult = &types.RestoreBackupResult{AppID: "alpha", SnapshotID: "2026-update"}
	m := loadBackupsListScreenWithSender(t, fake, sender.Send)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	request := sender.waitConfirmation(t)
	m = updateModel(t, m, request)
	assert.Contains(t, m.View(), fake.confirmation.Title)
	assert.Contains(t, m.View(), fake.confirmation.Message)

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("restore command did not finish after confirmation")
	}

	require.Len(t, fake.restoreRequests, 1)
	assert.Contains(t, m.View(), "Config restore complete")
}

func TestFormatBackupTimeRendersUTCOrUnknown(t *testing.T) {
	t.Parallel()

	nonUTC := time.Date(2026, time.June, 18, 9, 30, 0, 0, time.FixedZone("CEST", 2*60*60))

	assert.Equal(t, "2026-06-18T07:30:00Z", formatBackupTime(nonUTC))
	assert.Equal(t, "unknown time", formatBackupTime(time.Time{}))
}

func backupsFlowFake() *fakeEngine {
	return &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"},
		},
		backups: []types.BackupInfo{
			{
				SnapshotID: "2026-update",
				Operation:  "update",
				CreatedAt:  time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
				Files:      []string{"compose.yaml", ".env"},
			},
		},
	}
}

type confirmingRestoreEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingRestoreEngine) RestoreBackup(
	ctx context.Context,
	req types.RestoreBackupRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestoreBackupResult, error) {
	e.restoreRequests = append(e.restoreRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, types.NewError(types.ErrCodeUserCanceled, "restore canceled", "confirm the config restore to continue")
		}
	}
	return e.restoreResult, e.restoreErr
}

func loadBackupsAppsScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	return loadBackupsAppsScreenWithSender(t, eng, nil)
}

func loadBackupsAppsScreenWithSender(t *testing.T, eng engine.Engine, send func(tea.Msg)) model {
	t.Helper()

	m := newModelWithContextSender(t.Context(), eng, send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	for dashboardActions[m.cursor] != "Backups" {
		m = updateModel(t, m, downKey())
	}
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func loadBackupsListScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	return loadBackupsListScreenWithSender(t, eng, nil)
}

func loadBackupsListScreenWithSender(t *testing.T, eng engine.Engine, send func(tea.Msg)) model {
	t.Helper()

	m := loadBackupsAppsScreenWithSender(t, eng, send)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
