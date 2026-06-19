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

func TestModel_RemoveFlowOffersSafeAndDestructiveChoices(t *testing.T) {
	t.Parallel()

	fake := removeFlowFake()
	m := loadRemoveActionsScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Remove uptime-kuma")
	assert.Contains(t, view, "Safe remove")
	assert.Contains(t, view, "Permanently delete")
	assert.Contains(t, view, "[selected]")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
}

func TestModel_RemoveFlowCallsEngineRemoveAndRendersKeptArtifacts(t *testing.T) {
	t.Parallel()

	fake := removeFlowFake()
	fake.removeResult = &types.RemoveResult{
		AppID:                 "uptime-kuma",
		StackPath:             "/srv/wdm/uptime-kuma",
		PreservedPaths:        []string{"/srv/wdm/uptime-kuma/.env"},
		RemainingNamedVolumes: []string{"wdm_uptime-kuma_data"},
		RemainingNetworks:     []string{"wdm"},
		Status:                &types.AppStatus{AppID: "uptime-kuma", State: "removed"},
	}
	m := loadRemoveActionsScreen(t, fake)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.removeRequests, 1)
	assert.Equal(t, "uptime-kuma", fake.removeRequests[0].AppID)
	assert.Empty(t, fake.removeRequests[0].StackPath)
	assert.Empty(t, fake.deleteRequests)

	view := m.View()
	assert.Contains(t, view, "Safe remove complete")
	assert.Contains(t, view, "Files and data were kept")
	assert.Contains(t, view, "/srv/wdm/uptime-kuma/.env")
	assert.Contains(t, view, "wdm_uptime-kuma_data")
	assert.Contains(t, view, "wdm")
	assert.Contains(t, view, "removed")
}

func TestModel_DestructiveDeleteFlowPassesTypedNameAndRendersEngineConfirmation(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &confirmingDeleteEngine{
		fakeEngine: removeFlowFake(),
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindDeleteDestructive,
			Title:   "Permanently delete uptime-kuma",
			Message: "This permanently deletes files for uptime-kuma.",
		},
	}
	fake.deleteResult = &types.DeleteResult{
		AppID:                 "uptime-kuma",
		DeletedPaths:          []string{"/srv/wdm/uptime-kuma"},
		RemainingNamedVolumes: []string{"wdm_uptime-kuma_data"},
		RemovedNetworks:       []string{"wdm"},
		RetainedNetworks: []types.RetainedNetwork{
			{Name: "shared", Reason: "network shared has active endpoints"},
		},
	}
	m := loadRemoveActionsScreenWithSender(t, fake, sender.Send)
	m = updateModel(t, m, downKey())

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Contains(t, m.View(), "Type uptime-kuma")

	for _, r := range "uptime-kuma" {
		m = updateModel(t, m, runeKey(r))
	}
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
	assert.Contains(t, view, "Permanently delete uptime-kuma")
	assert.Contains(t, view, "This permanently deletes files for uptime-kuma.")

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("delete command did not finish after confirmation")
	}

	require.Len(t, fake.deleteRequests, 1)
	assert.Equal(t, "uptime-kuma", fake.deleteRequests[0].AppID)
	assert.Equal(t, "uptime-kuma", fake.deleteRequests[0].ConfirmationName)
	assert.False(t, fake.deleteRequests[0].DeleteNamedVolumes)

	view = m.View()
	assert.Contains(t, view, "permanently deleted")
	assert.Contains(t, view, "/srv/wdm/uptime-kuma")
	assert.Contains(t, view, "wdm_uptime-kuma_data")
	assert.Contains(t, view, "Networks removed:")
	assert.Contains(t, view, "could not be removed")
	assert.Contains(t, view, "docker network rm shared")
	assert.Empty(t, fake.removeRequests)
}

func TestModel_DestructiveDeleteFlowPassesMismatchedNameToEngine(t *testing.T) {
	t.Parallel()

	fake := removeFlowFake()
	fake.deleteErr = types.NewError(types.ErrCodeUsageValidation, "delete refused", "typed name did not match")
	m := loadRemoveActionsScreen(t, fake)
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)

	for _, r := range "wrong-name" {
		m = updateModel(t, m, runeKey(r))
	}
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.deleteRequests, 1)
	assert.Equal(t, "uptime-kuma", fake.deleteRequests[0].AppID)
	assert.Equal(t, "wrong-name", fake.deleteRequests[0].ConfirmationName)
	assert.Contains(t, m.View(), "Delete failed")
	assert.Contains(t, m.View(), "delete refused")
}

func TestModel_DeleteNameBackspaceRemovesLastRune(t *testing.T) {
	t.Parallel()

	m := model{deleteNameInput: "café"}

	m = m.deleteDeleteNameRune()
	assert.Equal(t, "caf", m.deleteNameInput)

	m.deleteNameInput = ""
	assert.Empty(t, m.deleteDeleteNameRune().deleteNameInput)
}

func removeFlowFake() *fakeEngine {
	return &fakeEngine{
		listStatusApps: []types.AppRuntimeStatus{
			{AppInfo: types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma"}, State: "running"},
		},
		statuses: map[string]*types.AppStatus{
			"uptime-kuma": {AppID: "uptime-kuma", State: "running"},
		},
	}
}

type confirmingDeleteEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingDeleteEngine) DeleteApp(
	ctx context.Context,
	req types.DeleteRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.DeleteResult, error) {
	e.deleteRequests = append(e.deleteRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, types.NewError(types.ErrCodeUserCanceled, "delete canceled", "confirm the deletion to continue")
		}
	}
	return e.deleteResult, e.deleteErr
}

func loadDeleteNameScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := loadRemoveActionsScreen(t, eng)
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)
	require.Equal(t, screenDeleteName, m.screen)
	return m
}

func TestModel_DeleteNameAcceptsBAndQAsTypedInput(t *testing.T) {
	t.Parallel()

	m := loadDeleteNameScreen(t, removeFlowFake())

	m = updateModel(t, m, runeKey('b'))
	m = updateModel(t, m, runeKey('q'))

	assert.Equal(t, "bq", m.deleteNameInput)
	assert.Equal(t, screenDeleteName, m.screen)
	assert.False(t, m.exiting)
}

func TestModel_DeleteNameTypesBeszelRegression(t *testing.T) {
	t.Parallel()

	m := loadDeleteNameScreen(t, removeFlowFake())

	for _, r := range "beszel" {
		m = updateModel(t, m, runeKey(r))
	}

	assert.Equal(t, "beszel", m.deleteNameInput)
	assert.Equal(t, screenDeleteName, m.screen)
}

func TestModel_DeleteNameEscStillGoesBack(t *testing.T) {
	t.Parallel()

	m := loadDeleteNameScreen(t, removeFlowFake())

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = assertModel(t, next)
	require.Nil(t, cmd)

	assert.NotEqual(t, screenDeleteName, m.screen)
	assert.False(t, m.exiting)
}

func TestModel_DeleteNameCtrlCStillQuits(t *testing.T) {
	t.Parallel()

	m := loadDeleteNameScreen(t, removeFlowFake())

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	require.NotNil(t, cmd)
	assert.Equal(t, tea.Quit(), cmd())
}

func loadRemoveActionsScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	return loadRemoveActionsScreenWithSender(t, eng, nil)
}

func loadRemoveActionsScreenWithSender(t *testing.T, eng engine.Engine, send func(tea.Msg)) model {
	t.Helper()

	m := newModelWithContextSender(t.Context(), eng, send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)
	return m
}
