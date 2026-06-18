package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func moveToStopAllAction(t *testing.T, m model) model {
	t.Helper()

	for dashboardActions[m.cursor] != "Stop all apps" {
		m = updateModel(t, m, downKey())
	}
	return m
}

// loadStopAllScreen drives the dashboard "Stop all apps" entry through
// Enter and the dispatched command, returning the model after the
// stopAllFinishedMsg lands. The fakeEngine ignores the confirmer and
// returns its configured result synchronously, so no modal handshake is
// needed for the flow test.
func loadStopAllScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := newReadyModel(eng)
	m = moveToStopAllAction(t, m)
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func TestModel_StopAll_DispatchesEngineAndShowsResult(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		stopAllResult: &types.StopAllResult{
			Stopped: []types.StoppedApp{{AppID: "uptime-kuma"}, {AppID: "freshrss"}},
		},
	}

	m := loadStopAllScreen(t, eng)

	assert.Equal(t, 1, eng.stopAllCalls, "selecting the action must call Engine.StopAll exactly once")
	assert.Equal(t, screenStopAllResult, m.screen, "a returned result must advance to the result screen")

	view := m.View()
	assert.Contains(t, view, "All managed apps were stopped.")
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "freshrss")
}

func TestModel_StopAll_PartialFailureRendersFailedStacks(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		stopAllResult: &types.StopAllResult{
			Stopped: []types.StoppedApp{{AppID: "uptime-kuma"}},
			Failed:  []types.StoppedApp{{AppID: "freshrss", Error: "daemon unreachable"}},
		},
	}

	m := loadStopAllScreen(t, eng)

	view := m.View()
	assert.Contains(t, view, "Some managed apps failed to stop")
	assert.Contains(t, view, "freshrss")
	assert.Contains(t, view, "daemon unreachable")
}
