package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// portConflict builds a typed engine conflict for the remap loop tests.
func portConflict(service string, conflicting, suggested int) *types.PortConflictError {
	return types.NewPortConflictError(service, 80, conflicting, suggested,
		types.WrapError(types.ErrCodeUsageValidation, "local port is already in use",
			"remap it with --port", nil))
}

// submitInstallForm walks the loaded install form to the Install action and
// submits it, returning the model after the resulting installFinishedMsg is
// folded in.
func submitInstallForm(t *testing.T, m model) model {
	t.Helper()

	m = typeIntoInstallField(t, m, "alpha.example.com")
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func TestModel_PortConflictShowsRemapScreenWithSuggestion(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installErr = portConflict("web", 8080, 8081)
	m := submitInstallForm(t, loadInstallForm(t, fake))

	require.Equal(t, screenPortRemap, m.screen)
	view := m.View()
	assert.Contains(t, view, "127.0.0.1:8080")
	assert.Contains(t, view, "web")
	assert.Contains(t, view, "8081", "the suggested port must be prefilled")
	assert.Nil(t, m.err, "a remappable conflict is not surfaced as a failure")
}

func TestModel_PortRemapAcceptSuggestionReinvokesWithOverride(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installOutcomes = []installOutcome{
		{err: portConflict("web", 8080, 8081)},
		{result: &types.InstallResult{AppID: "alpha", StackPath: "/srv/alpha"}},
	}
	m := submitInstallForm(t, loadInstallForm(t, fake))
	require.Equal(t, screenPortRemap, m.screen)

	// Enter accepts the prefilled suggestion and re-invokes install.
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 2)
	assert.Equal(t, map[int]int{8080: 8081}, fake.installRequests[1].PortOverrides)
	assert.Equal(t, screenInstallResult, m.screen)
	assert.Contains(t, m.View(), "Install complete")
}

func TestModel_PortRemapTypedPortOverridesSuggestion(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installOutcomes = []installOutcome{
		{err: portConflict("web", 8080, 8081)},
		{result: &types.InstallResult{AppID: "alpha", StackPath: "/srv/alpha"}},
	}
	m := submitInstallForm(t, loadInstallForm(t, fake))
	require.Equal(t, screenPortRemap, m.screen)

	// Clear the "8081" suggestion and type a different port.
	for range "8081" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoInstallField(t, m, "9999")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 2)
	assert.Equal(t, map[int]int{8080: 9999}, fake.installRequests[1].PortOverrides)
}

func TestModel_PortRemapCancelAbortsFailClosed(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installErr = portConflict("web", 8080, 8081)
	m := submitInstallForm(t, loadInstallForm(t, fake))
	require.Equal(t, screenPortRemap, m.screen)

	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	assert.NotEqual(t, screenPortRemap, m.screen, "Esc must leave the remap screen")
	assert.Len(t, fake.installRequests, 1, "cancel must not re-invoke install")
	assert.Nil(t, m.portConflict, "cancel must clear the conflict")
}

// TestModel_PortRemapRepeatConflictRekeysCatalogPort is the correctness lock:
// a chosen port that is itself busy reports the effective (already-remapped)
// host port, but PortOverrides must stay keyed on the original catalog port.
func TestModel_PortRemapRepeatConflictRekeysCatalogPort(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installOutcomes = []installOutcome{
		{err: portConflict("web", 8080, 8081)}, // catalog 8080 busy -> suggest 8081
		{err: portConflict("web", 8081, 8082)}, // chosen 8081 busy -> suggest 8082
		{result: &types.InstallResult{AppID: "alpha", StackPath: "/srv/alpha"}},
	}
	m := submitInstallForm(t, loadInstallForm(t, fake))
	require.Equal(t, screenPortRemap, m.screen)

	// Accept 8081; it comes back busy with a fresh suggestion.
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())
	require.Equal(t, screenPortRemap, m.screen)
	assert.Contains(t, m.View(), "127.0.0.1:8081")
	assert.Contains(t, m.View(), "8082")

	// Accept 8082; install succeeds.
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 3)
	assert.Equal(t, map[int]int{8080: 8082}, fake.installRequests[2].PortOverrides,
		"the override must stay keyed on the catalog port, not the intermediate remap")
	assert.Equal(t, screenInstallResult, m.screen)
}

func TestModel_PortRemapMultipleConflictsResolvedOneAtATime(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installOutcomes = []installOutcome{
		{err: portConflict("web", 8080, 8081)},
		{err: portConflict("db", 5432, 5433)},
		{result: &types.InstallResult{AppID: "alpha", StackPath: "/srv/alpha"}},
	}
	m := submitInstallForm(t, loadInstallForm(t, fake))

	// Resolve the web conflict.
	require.Equal(t, screenPortRemap, m.screen)
	assert.Contains(t, m.View(), "web")
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	// Then the db conflict, on its own screen.
	require.Equal(t, screenPortRemap, m.screen)
	assert.Contains(t, m.View(), "db")
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 3)
	assert.Equal(t, map[int]int{8080: 8081, 5432: 5433}, fake.installRequests[2].PortOverrides)
	assert.Equal(t, screenInstallResult, m.screen)
}

// TestModel_PortRemapEngineErrorKeepsScreenUsable locks the UX recovery path:
// when the re-invoke returns a plain engine error (not a PortConflictError),
// the remap screen must keep rendering the conflict plus the new error and stay
// editable so the user can retype and retry, rather than dead-ending (ADR 0004).
func TestModel_PortRemapEngineErrorKeepsScreenUsable(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installOutcomes = []installOutcome{
		{err: portConflict("web", 8080, 8081)},
		{err: types.WrapError(types.ErrCodeUsageValidation,
			"port override target is out of range", "choose 1025..65535", nil)},
	}
	m := submitInstallForm(t, loadInstallForm(t, fake))
	require.Equal(t, screenPortRemap, m.screen)

	// Replace the "8081" suggestion with an engine-invalid port and submit.
	for range "8081" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoInstallField(t, m, "70000")
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Equal(t, screenPortRemap, m.screen, "a plain engine error must not leave the remap screen")
	require.Error(t, m.err)
	require.NotNil(t, m.portConflict, "the conflict must stay so the screen keeps rendering")
	assert.Contains(t, m.View(), "port override target is out of range")

	// Screen is still usable: retype and Enter re-invokes install again.
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = typeIntoInstallField(t, m, "5")
	next, cmd = m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.installRequests, 3, "the second Enter must re-invoke install")
}

func TestModel_PortConflictNoSuggestionStaysFailClosed(t *testing.T) {
	t.Parallel()

	fake := installFormFake()
	fake.installErr = portConflict("web", 8080, 0) // 0 == no free port found
	m := submitInstallForm(t, loadInstallForm(t, fake))

	assert.NotEqual(t, screenPortRemap, m.screen, "a fail-closed conflict offers no remap screen")
	assert.Equal(t, screenInstallForm, m.screen)
	require.Error(t, m.err)
	assert.Contains(t, m.View(), "Install failed")
}
