package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_RuntimeLockInitRendersStaleRecoveryPrompt(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockStatus: staleRuntimeLockFixture()}
	m := newReadyModel(fake)

	m = runInit(t, m)

	view := m.View()
	assert.Contains(t, view, "Runtime lock recovery")
	assert.Contains(t, view, "exists: true")
	assert.Contains(t, view, "held: true")
	assert.Contains(t, view, "stale: true")
	assert.Contains(t, view, "holder_pid: 1234")
	assert.Contains(t, view, "holder_command: install")
	assert.Contains(t, view, "holder_alive: false")
	assert.Contains(t, view, "Clear stale lock")
	assert.Contains(t, view, "Return to dashboard")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
	assert.Equal(t, 1, fake.runtimeLockStatusCalls)
}

func TestModel_RuntimeLockClearCallsEngineAndRendersPostClearStatus(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		runtimeLockStatus:      staleRuntimeLockFixture(),
		clearRuntimeLockResult: &types.RuntimeLockStatus{},
	}
	m := loadRuntimeLockPrompt(t, fake)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	assert.Equal(t, 1, fake.clearRuntimeLockCalls)
	view := m.View()
	assert.Contains(t, view, "Runtime lock cleared")
	assert.Contains(t, view, "exists: false")
	assert.Contains(t, view, "held: false")
	assert.Contains(t, view, "stale: false")
}

func TestModel_RuntimeLockViewRendersBusyErrorMessageAndMissingStatus(t *testing.T) {
	t.Parallel()

	t.Run("busy", func(t *testing.T) {
		t.Parallel()

		m := model{busy: true}

		view := m.runtimeLockView()
		assert.Contains(t, view, "Runtime lock recovery")
		assert.Contains(t, view, "Clearing runtime lock...")
		assert.Contains(t, view, "Back: b")
		assert.Contains(t, view, "Quit: q")
		assert.NotContains(t, view, "Actions")
	})

	t.Run("error message without status", func(t *testing.T) {
		t.Parallel()

		m := model{
			err:                errors.New("lock file is not stale"),
			runtimeLockMessage: "Runtime lock clear was skipped.",
		}

		view := m.runtimeLockView()
		assert.Contains(t, view, "Runtime lock action failed: lock file is not stale")
		assert.Contains(t, view, "Runtime lock clear was skipped.")
		assert.Contains(t, view, "No runtime lock status available.")
		assert.Contains(t, view, "Actions")
	})
}

func TestModel_RuntimeLockReturnToDashboardSelectsAndNavigates(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockStatus: staleRuntimeLockFixture()}
	m := loadRuntimeLockPrompt(t, fake)
	require.Equal(t, 0, m.runtimeLockCursor)
	require.Equal(t, screenRuntimeLock, m.screen)
	require.Equal(t, runtimeLockRecoveryActions, m.runtimeLockActions())

	// Down moves onto "Return to dashboard"; Up returns to "Clear stale lock".
	m = updateModel(t, m, downKey())
	assert.Equal(t, 1, m.runtimeLockCursor)
	next, cmd := m.updateRuntimeLockKey(tea.KeyMsg{Type: tea.KeyUp})
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, 0, m.runtimeLockCursor, "Up must move the runtime-lock cursor back toward the first action")

	// Selecting "Return to dashboard" leaves the recovery screen without
	// clearing the lock.
	m = updateModel(t, m, downKey())
	require.Equal(t, "Return to dashboard", m.runtimeLockActions()[m.runtimeLockCursor])
	m.runtimeLockMessage = "Runtime lock cleared."
	next, cmd = m.updateRuntimeLockKey(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd, "Return to dashboard must not emit a command")
	assert.Equal(t, screenDashboard, m.screen, "Return to dashboard must navigate back to the dashboard")
	assert.Equal(t, 0, fake.clearRuntimeLockCalls, "Return to dashboard must not clear the runtime lock")
	assert.Empty(t, m.runtimeLockMessage, "Return to dashboard must clear any prior status message")
}

func loadRuntimeLockPrompt(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	return runInit(t, m)
}

func staleRuntimeLockFixture() *types.RuntimeLockStatus {
	return &types.RuntimeLockStatus{
		Exists:        true,
		Held:          true,
		Stale:         true,
		HolderPID:     1234,
		HolderCommand: "install",
		HolderAlive:   false,
		WDMVersion:    "test",
	}
}
