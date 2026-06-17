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

func TestModel_SelfUpdateFlowChecksAndRendersAvailableUpdate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		selfUpdateStatus: &types.SelfUpdateStatus{
			CurrentVersion:  "1.2.0",
			LatestVersion:   "1.3.0",
			UpdateAvailable: true,
			Verified:        true,
			Notes:           []string{"release notes at example.com/changelog"},
			CheckedAt:       time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC),
		},
	}
	m := loadSelfUpdateScreen(t, fake)

	assert.Equal(t, 1, fake.checkSelfUpdateN, "entering the flow issues exactly one check")
	assert.Empty(t, fake.selfUpdateRequests, "the read-only check never applies")
	assert.Equal(t, 0, fake.listCalls, "self-update never lists deployed apps")
	assert.Empty(t, fake.updateRequests, "self-update never updates a deployed app")
	assert.Empty(t, fake.installRequests, "self-update never installs an app")
	assert.Empty(t, fake.removeRequests, "self-update never removes an app")

	view := m.View()
	assert.Contains(t, view, "Update wdm")
	assert.Contains(t, view, "Current (this binary): 1.2.0")
	assert.Contains(t, view, "Latest release: 1.3.0")
	assert.Contains(t, view, "passed checksum, signature, and attestation")
	assert.Contains(t, view, "release notes at example.com/changelog")
	assert.Contains(t, view, selfUpdateApplyNetworkNotice)
	assert.Contains(t, view, "Press Enter to download and apply")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
}

func TestModel_SelfUpdateFlowDisclosesNetworkBeforeCheck(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		selfUpdateStatus: &types.SelfUpdateStatus{CurrentVersion: "1.2.0"},
	}
	m := newReadyModel(fake)
	m = moveToSelfUpdateAction(t, m)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	busyView := m.View()
	assert.True(t, m.busy, "the model is busy while the check runs")
	assert.Contains(t, busyView, selfUpdateNetworkNotice, "the network action is disclosed before the check runs")
	assert.Contains(t, busyView, "Checking the release server")
}

func TestModel_SelfUpdateFlowSurfacesUnverifiedAndBlocksApply(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		selfUpdateStatus: &types.SelfUpdateStatus{
			CurrentVersion:  "1.2.0",
			LatestVersion:   "1.3.0",
			UpdateAvailable: true,
			Verified:        false,
		},
	}
	m := loadSelfUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "WARNING")
	assert.Contains(t, view, "NOT verified")
	assert.Contains(t, view, "cannot be applied until the latest release verifies")
	assert.NotContains(t, view, "Press Enter to download and apply")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "an unverified update fails closed: Enter does nothing")
	assert.Empty(t, fake.selfUpdateRequests, "an unverified update is never applied")
	assert.Contains(t, m.View(), "WARNING", "the warning stays visible")
}

func TestModel_SelfUpdateFlowReportsUpToDate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		selfUpdateStatus: &types.SelfUpdateStatus{
			CurrentVersion:  "1.3.0",
			LatestVersion:   "1.3.0",
			UpdateAvailable: false,
			Verified:        true,
		},
	}
	m := loadSelfUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "wdm is up to date.")
	assert.NotContains(t, view, "Press Enter to download and apply")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "with no update available Enter is a no-op")
	assert.Empty(t, fake.selfUpdateRequests)
	assert.Equal(t, screenSelfUpdate, m.screen, "the flow stays on the review screen")
}

func TestModel_SelfUpdateFlowRendersCheckError(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		selfUpdateStatusErr: errors.New("release endpoint unreachable"),
	}
	m := loadSelfUpdateScreen(t, fake)

	view := m.View()
	assert.Contains(t, view, "Could not check for a wdm update")
	assert.Contains(t, view, "release endpoint unreachable")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	assert.Nil(t, cmd, "a failed check leaves nothing to apply")
	assert.Empty(t, fake.selfUpdateRequests)
	assert.Equal(t, screenSelfUpdate, m.screen, "the flow stays on the review screen")
}

func TestModel_SelfUpdateApplyRaisesConfirmationAndRendersResult(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &confirmingSelfUpdateEngine{
		fakeEngine: &fakeEngine{
			selfUpdateStatus: &types.SelfUpdateStatus{
				CurrentVersion:  "1.2.0",
				LatestVersion:   "1.3.0",
				UpdateAvailable: true,
				Verified:        true,
			},
			selfUpdateResult: &types.SelfUpdateResult{
				PreviousVersion:    "1.2.0",
				AppliedVersion:     "1.3.0",
				Replaced:           true,
				SmokeOK:            true,
				RolledBack:         false,
				PreviousBinaryPath: "/home/user/.local/bin/wdm.previous",
				Message:            "wdm updated to 1.3.0.",
			},
		},
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindSelfUpdate,
			Title:   "Update wdm to 1.3.0",
			Message: "This downloads and verifies the new binary before replacing wdm.",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToSelfUpdateAction(t, m)

	// Enter the flow: the check runs synchronously through the command.
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())
	require.True(t, m.selfUpdateApplyable())

	// Authorize the apply.
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	request := sender.waitConfirmation(t)
	assert.Equal(t, types.ConfirmationKindSelfUpdate, request.confirmation.Kind)
	m = updateModel(t, m, request)

	modalView := m.View()
	assert.Contains(t, modalView, "Update wdm to 1.3.0")
	assert.Contains(t, modalView, "Yes: y")
	assert.Contains(t, modalView, "No: n")

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("self-update command did not finish after confirmation")
	}

	require.Len(t, fake.selfUpdateRequests, 1)
	assert.Equal(t, "1.3.0", fake.selfUpdateRequests[0].TargetVersion)

	view := m.View()
	assert.Equal(t, screenSelfUpdateResult, m.screen)
	assert.Contains(t, view, "wdm update")
	assert.Contains(t, view, "1.2.0 -> 1.3.0")
	assert.Contains(t, view, "Replaced: yes")
	assert.Contains(t, view, "Smoke check: ok")
	assert.Contains(t, view, "Rolled back: no")
	assert.Contains(t, view, "wdm updated to 1.3.0.")
	assert.NotContains(t, view, "Self-update did not complete")
}

func TestModel_SelfUpdateApplyDeclineDoesNotShowResult(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &confirmingSelfUpdateEngine{
		fakeEngine: &fakeEngine{
			selfUpdateStatus: &types.SelfUpdateStatus{
				CurrentVersion:  "1.2.0",
				LatestVersion:   "1.3.0",
				UpdateAvailable: true,
				Verified:        true,
			},
		},
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindSelfUpdate,
			Title:   "Update wdm to 1.3.0",
			Message: "This downloads and verifies the new binary before replacing wdm.",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToSelfUpdateAction(t, m)

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
		t.Fatal("self-update command did not finish after decline")
	}

	require.Len(t, fake.selfUpdateRequests, 1, "the engine is still consulted so it can map the decline")
	assert.NotEqual(t, screenSelfUpdateResult, m.screen, "a declined apply does not reach the result screen")
	assert.Error(t, m.err, "the declined apply surfaces an error")
	assert.Contains(t, m.View(), "Could not update wdm", "a declined apply attributes the failure to the apply phase, not the check")
}

// TestModel_SelfUpdateApplyRollbackShowsResultAndFailure verifies that
// ApplySelfUpdate returns a result ALONGSIDE an error on the
// rollback path (smoke check failed). The model MUST surface the result
// screen so the rollback is visible and the failure is evident, rather
// than hiding it behind a bare error on the check screen.
func TestModel_SelfUpdateApplyRollbackShowsResultAndFailure(t *testing.T) {
	t.Parallel()

	rollbackErr := types.NewError(
		types.ErrCodeGeneric,
		"the new wdm binary failed its version smoke check; restored the previous binary",
		"try the update again or check the release",
	)
	sender := newRecordingSender()
	fake := &confirmingSelfUpdateEngine{
		fakeEngine: &fakeEngine{
			selfUpdateStatus: &types.SelfUpdateStatus{
				CurrentVersion:  "1.2.0",
				LatestVersion:   "1.3.0",
				UpdateAvailable: true,
				Verified:        true,
			},
			selfUpdateResult: &types.SelfUpdateResult{
				PreviousVersion:    "1.2.0",
				AppliedVersion:     "1.3.0",
				Replaced:           false,
				SmokeOK:            false,
				RolledBack:         true,
				PreviousBinaryPath: "/home/user/.local/bin/wdm.previous",
				Message:            "Smoke check failed; rolled back to the previous binary.",
			},
			selfUpdateApplyErr: rollbackErr,
		},
		confirmation: types.Confirmation{
			Kind:    types.ConfirmationKindSelfUpdate,
			Title:   "Update wdm to 1.3.0",
			Message: "This downloads and verifies the new binary before replacing wdm.",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToSelfUpdateAction(t, m)

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
	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("self-update command did not finish after confirmation")
	}

	// The result screen IS shown even though the apply returned an error.
	assert.Equal(t, screenSelfUpdateResult, m.screen, "a rollback result is surfaced even on the error path")
	assert.Error(t, m.err, "the rollback still carries the engine error")

	view := m.View()
	// The failure is evident.
	assert.Contains(t, view, "Self-update did not complete", "the failure is evident, not hidden as success")
	assert.Contains(t, view, "failed its version smoke check", "the engine error is shown")
	// The structured rollback outcome is visible.
	assert.Contains(t, view, "Rolled back: yes")
	assert.Contains(t, view, "Smoke check: failed")
	assert.Contains(t, view, "Replaced: no")
	assert.Contains(t, view, "1.2.0 -> 1.3.0")
	assert.Contains(t, view, "Smoke check failed; rolled back to the previous binary.")
}

// TestModel_SelfUpdateApplyFailedRollbackShowsManualRecovery covers the
// second rollback arm: the rollback itself failed, so the new binary is in
// place (Replaced) but the smoke check failed. The manual-recovery message
// must be visible alongside the error.
func TestModel_SelfUpdateApplyFailedRollbackShowsManualRecovery(t *testing.T) {
	t.Parallel()

	recoverErr := types.NewError(
		types.ErrCodeGeneric,
		"smoke check failed and restoring the previous binary failed",
		"restore wdm.previous manually",
	)
	sender := newRecordingSender()
	fake := &confirmingSelfUpdateEngine{
		fakeEngine: &fakeEngine{
			selfUpdateStatus: &types.SelfUpdateStatus{
				CurrentVersion:  "1.2.0",
				LatestVersion:   "1.3.0",
				UpdateAvailable: true,
				Verified:        true,
			},
			selfUpdateResult: &types.SelfUpdateResult{
				PreviousVersion:    "1.2.0",
				AppliedVersion:     "1.3.0",
				Replaced:           true,
				SmokeOK:            false,
				RolledBack:         false,
				PreviousBinaryPath: "/home/user/.local/bin/wdm.previous",
				Message:            "Smoke check failed and rollback failed. Restore wdm.previous manually.",
			},
			selfUpdateApplyErr: recoverErr,
		},
		confirmation: types.Confirmation{
			Kind:  types.ConfirmationKindSelfUpdate,
			Title: "Update wdm to 1.3.0",
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m = moveToSelfUpdateAction(t, m)

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
	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("self-update command did not finish after confirmation")
	}

	assert.Equal(t, screenSelfUpdateResult, m.screen, "the manual-recovery result is surfaced even on the error path")

	view := m.View()
	assert.Contains(t, view, "Self-update did not complete")
	assert.Contains(t, view, "Replaced: yes")
	assert.Contains(t, view, "Smoke check: failed")
	assert.Contains(t, view, "Rolled back: no")
	assert.Contains(t, view, "Restore wdm.previous manually.")
}

type confirmingSelfUpdateEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingSelfUpdateEngine) ApplySelfUpdate(
	ctx context.Context,
	req types.SelfUpdateRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.SelfUpdateResult, error) {
	e.selfUpdateRequests = append(e.selfUpdateRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, types.NewError(
				types.ErrCodeUserCanceled,
				"self-update canceled",
				"confirm the self-update to continue",
			)
		}
	}
	return e.selfUpdateResult, e.selfUpdateApplyErr
}

func moveToSelfUpdateAction(t *testing.T, m model) model {
	t.Helper()

	for dashboardActions[m.cursor] != "Update wdm" {
		m = updateModel(t, m, downKey())
	}
	return m
}

func loadSelfUpdateScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	m = moveToSelfUpdateAction(t, m)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}
