package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_CatalogUpdateFlowChecksAndRendersAvailableUpdate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogUpdateStatus: &types.CatalogUpdateStatus{
			Channel:         "stable",
			CurrentVersion:  "2026-06-01",
			LatestVersion:   "2026-06-12",
			UpdateAvailable: true,
			Verified:        true,
			Changes: []types.CatalogChange{
				{AppID: "uptime-kuma", Kind: "updated", Summary: "bump mariadb to 11.8.7"},
				{AppID: "freshrss", Kind: "added", Summary: "new app"},
			},
			CheckedAt: time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC),
		},
	}
	m := loadCatalogUpdateScreen(t, fake)

	assert.Equal(t, 1, fake.checkCatalogUpdateN, "entering the flow issues exactly one check")
	assert.Empty(t, fake.catalogUpdateRequests, "the read-only check never applies")
	assert.Equal(t, 0, fake.listCalls, "catalog update never lists deployed apps")
	assert.Empty(t, fake.updateRequests, "catalog update never updates a deployed app")
	assert.Empty(t, fake.installRequests, "catalog update never installs an app")

	view := m.View()
	assert.Contains(t, view, "Update catalog")
	assert.Contains(t, view, "Channel: stable")
	assert.Contains(t, view, "Current (local): 2026-06-01")
	assert.Contains(t, view, "Latest available: 2026-06-12")
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "bump mariadb to 11.8.7")
	assert.Contains(t, view, "freshrss")
	assert.Contains(t, view, "passed checksum, signature, and attestation")
	assert.Contains(t, view, catalogApplyNetworkNotice)
	assert.Contains(t, view, "Press Enter to download and apply")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
}

func TestModel_CatalogUpdateFlowDisclosesNetworkBeforeCheck(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogUpdateStatus: &types.CatalogUpdateStatus{Channel: "stable"},
	}
	m := newReadyModel(fake)
	m = moveToCatalogUpdateAction(t, m)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	busyView := m.View()
	assert.True(t, m.busy, "the model is busy while the check runs")
	assert.Contains(t, busyView, catalogNetworkNotice, "the network action is disclosed before the check runs")
	assert.Contains(t, busyView, "Checking the catalog server")
}

func TestModel_CatalogUpdateFlowSurfacesUnverifiedAndBlocksApply(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogUpdateStatus: &types.CatalogUpdateStatus{
			Channel:         "stable",
			CurrentVersion:  "2026-06-01",
			LatestVersion:   "2026-06-12",
			UpdateAvailable: true,
			Verified:        false,
			Changes:         []types.CatalogChange{{AppID: "uptime-kuma", Kind: "updated"}},
		},
	}
	m := loadCatalogUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "WARNING")
	assert.Contains(t, view, "NOT verified")
	assert.Contains(t, view, "cannot be applied until the latest catalog verifies")
	assert.NotContains(t, view, "Press Enter to download and apply")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "an unverified update fails closed: Enter does nothing")
	assert.Empty(t, fake.catalogUpdateRequests, "an unverified update is never applied")
	assert.Contains(t, m.View(), "WARNING", "the warning stays visible")
}

func TestModel_CatalogUpdateFlowReportsUpToDate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogUpdateStatus: &types.CatalogUpdateStatus{
			Channel:         "stable",
			CurrentVersion:  "2026-06-12",
			LatestVersion:   "2026-06-12",
			UpdateAvailable: false,
			Verified:        true,
		},
	}
	m := loadCatalogUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "The catalog is up to date.")
	assert.NotContains(t, view, "Press Enter to download and apply")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "with no update available Enter is a no-op")
	assert.Empty(t, fake.catalogUpdateRequests)
	assert.Equal(t, screenCatalogUpdate, m.screen, "the flow stays on the review screen")
}

func TestModel_CatalogUpdateFlowRendersCheckError(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		catalogUpdateStatusErr: errors.New("catalog endpoint unreachable"),
	}
	m := loadCatalogUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Could not check for catalog updates")
	assert.Contains(t, view, "catalog endpoint unreachable")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "a failed check leaves nothing to apply")
	assert.Empty(t, fake.catalogUpdateRequests)
	assert.Equal(t, screenCatalogUpdate, m.screen, "the flow stays on the review screen")
}

func TestModel_CatalogUpdateApplyRaisesConfirmationAndRendersResult(t *testing.T) {
	t.Parallel()

	const verificationDetail = "checksum, signature, and attestation verified"
	sender := newRecordingSender()
	fake := &confirmingCatalogUpdateEngine{
		fakeEngine: &fakeEngine{
			catalogUpdateStatus: &types.CatalogUpdateStatus{
				Channel:         "stable",
				CurrentVersion:  "2026-06-01",
				LatestVersion:   "2026-06-12",
				UpdateAvailable: true,
				Verified:        true,
			},
			catalogUpdateResult: &types.CatalogUpdateResult{
				Channel:            "stable",
				PreviousVersion:    "2026-06-01",
				AppliedVersion:     "2026-06-12",
				VerificationDetail: verificationDetail,
				Changes:            []types.CatalogChange{{AppID: "uptime-kuma", Kind: "updated", Summary: "bump mariadb"}},
				AppliedAt:          time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC),
			},
		},
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindCatalogUpdate,
			Title:   "Update catalog stable",
			Message: "This downloads and verifies the catalog before writing it.",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToCatalogUpdateAction(t, m)

	// Enter the flow: the check runs synchronously through the command.
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())
	require.True(t, m.catalogUpdateApplyable())

	// Authorize the apply.
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	request := sender.waitConfirmation(t)
	assert.Equal(t, types.ConfirmationKindCatalogUpdate, request.confirmation.Kind)
	m = updateModel(t, m, request)

	modalView := m.View()
	assert.Contains(t, modalView, "Update catalog stable")
	assert.Contains(t, modalView, "Yes: y")
	assert.Contains(t, modalView, "No: n")

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("catalog update command did not finish after confirmation")
	}

	require.Len(t, fake.catalogUpdateRequests, 1)
	assert.Equal(t, "stable", fake.catalogUpdateRequests[0].Channel)
	assert.Equal(t, "2026-06-12", fake.catalogUpdateRequests[0].TargetVersion)

	view := m.View()
	assert.Contains(t, view, "Catalog updated")
	assert.Contains(t, view, "2026-06-01 -> 2026-06-12")
	assert.Contains(t, view, verificationDetail)
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "bump mariadb")
}

func TestModel_CatalogUpdateApplyDeclineDoesNotShowResult(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &confirmingCatalogUpdateEngine{
		fakeEngine: &fakeEngine{
			catalogUpdateStatus: &types.CatalogUpdateStatus{
				Channel:         "stable",
				CurrentVersion:  "2026-06-01",
				LatestVersion:   "2026-06-12",
				UpdateAvailable: true,
				Verified:        true,
			},
		},
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindCatalogUpdate,
			Title:   "Update catalog stable",
			Message: "This downloads and verifies the catalog before writing it.",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToCatalogUpdateAction(t, m)

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

	m = updateModel(t, m, runeKey('n'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("catalog update command did not finish after decline")
	}

	require.Len(t, fake.catalogUpdateRequests, 1, "the engine is still consulted so it can map the decline")
	assert.NotEqual(t, screenCatalogUpdateResult, m.screen, "a declined apply does not reach the result screen")
	assert.Error(t, m.err, "the declined apply surfaces an error")
	assert.Contains(t, m.View(), "Could not apply the catalog update", "a declined apply attributes the failure to the apply phase, not the check")
}

type confirmingCatalogUpdateEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingCatalogUpdateEngine) ApplyCatalogUpdate(
	ctx context.Context,
	req types.CatalogUpdateRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	e.catalogUpdateRequests = append(e.catalogUpdateRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, types.NewError(
				types.ErrCodeUserCanceled,
				"catalog update canceled",
				"confirm the catalog update to continue",
			)
		}
	}
	return e.catalogUpdateResult, e.catalogUpdateApplyErr
}

func moveToCatalogUpdateAction(t *testing.T, m model) model {
	t.Helper()

	for dashboardActions[m.cursor] != "Update catalog" {
		m = updateModel(t, m, downKey())
	}
	return m
}

func loadCatalogUpdateScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	m = moveToCatalogUpdateAction(t, m)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
