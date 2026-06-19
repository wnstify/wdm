package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func moveToUninstallAction(t *testing.T, m model) model {
	t.Helper()

	for dashboardActions[m.cursor] != "Uninstall wdm" {
		m = updateModel(t, m, downKey())
	}
	return m
}

// dispatchUninstall drives the dashboard "Uninstall wdm" entry through
// Enter and the dispatched command, then feeds the resulting
// uninstallFinishedMsg back through Update WITHOUT asserting the follow-up
// command is nil — a full-success uninstall returns tea.Quit. It returns the
// settled model and the command Update produced for the finished message so
// the caller can assert on quit behavior.
func dispatchUninstall(t *testing.T, eng *fakeEngine) (model, tea.Cmd) {
	t.Helper()

	m := newReadyModel(eng)
	m = moveToUninstallAction(t, m)

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd, "selecting Uninstall wdm must dispatch a command")

	next, finishedCmd := m.Update(cmd())
	return assertModel(t, next), finishedCmd
}

func TestModel_Uninstall_FullSuccessQuits(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown:      []types.TornDownApp{{AppID: "uptime-kuma"}, {AppID: "freshrss"}},
			KeptDataPaths: []string{"/home/u/docker/uptime-kuma"},
			RemovedPaths:  []string{"/home/u/.local/bin/wdm"},
		},
	}

	m, cmd := dispatchUninstall(t, eng)

	assert.Equal(t, 1, eng.uninstallCalls, "selecting the action must call Engine.Uninstall exactly once")
	assert.Equal(t, screenUninstallResult, m.screen, "a full success must advance to the result screen")
	require.NotNil(t, cmd, "a full success must return a quit command")
	assert.IsType(t, tea.QuitMsg{}, cmd(), "the full-success command must be tea.Quit")

	view := m.View()
	assert.Contains(t, view, "wdm was uninstalled.")
	assert.Contains(t, view, "uptime-kuma")
	assert.Contains(t, view, "named volumes and per-app stack data")
}

// The result screen shows the removed-network count and lists any retained
// network with its manual `docker network rm` hint.
func TestModel_Uninstall_ResultRendersNetworks(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown:        []types.TornDownApp{{AppID: "uptime-kuma"}},
			RemovedNetworks: []string{"wdm_proxy", "wdm_kuma"},
			RetainedNetworks: []types.RetainedNetwork{
				{Name: "wdm_db", Reason: "active endpoints"},
			},
			KeptDataPaths: []string{"/home/u/docker/uptime-kuma"},
			RemovedPaths:  []string{"/home/u/.local/bin/wdm"},
		},
	}

	m, _ := dispatchUninstall(t, eng)
	view := m.View()

	assert.Contains(t, view, "Networks removed: 2")
	assert.Contains(t, view, "could not be removed")
	assert.Contains(t, view, "docker network rm wdm_db")
}

func TestModel_Uninstall_AbortDoesNotQuit(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown: []types.TornDownApp{{AppID: "uptime-kuma"}},
			Failed:   []types.TornDownApp{{AppID: "freshrss", Error: "daemon unreachable"}},
		},
	}

	m, cmd := dispatchUninstall(t, eng)

	assert.Equal(t, screenUninstallResult, m.screen, "an abort still advances to the result screen")
	assert.Nil(t, cmd, "an abort must NOT quit; wdm is still installed")

	view := m.View()
	assert.Contains(t, view, "wdm was not removed")
	assert.Contains(t, view, "freshrss")
	assert.Contains(t, view, "daemon unreachable")
}

// A whole-operation error (a declined confirmation, lock contention,
// cancellation) returns no result: the model stays on the uninstall screen,
// surfaces the error, and does not quit.
func TestModel_Uninstall_WholeOperationErrorStaysOnScreen(t *testing.T) {
	t.Parallel()

	eng := &fakeEngine{
		uninstallErr: types.NewError(types.ErrCodeUserCanceled, "uninstall canceled", "re-run to retry"),
	}

	m, cmd := dispatchUninstall(t, eng)

	assert.Equal(t, screenUninstall, m.screen, "a whole-operation error keeps the uninstall screen")
	assert.Nil(t, cmd, "a whole-operation error must not quit")

	view := m.View()
	assert.Contains(t, view, "Could not uninstall wdm")
	assert.Contains(t, view, "uninstall canceled")
}

func TestModel_DashboardListsUninstallAction(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})

	assert.Contains(t, m.View(), "Uninstall wdm", "the dashboard must list the Uninstall wdm action")
}
