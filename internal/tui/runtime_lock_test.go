package tui

import (
	"testing"

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
