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

func TestModel_ResourcesScreenShowsCurrentValuesAndBands(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	require.Equal(t, screenResources, m.screen)
	require.Equal(t, []string{"alpha"}, fake.resourceSettingsCalls)

	view := m.View()
	assert.Contains(t, view, "Manage resources")
	assert.Contains(t, view, "Service: web")
	assert.Contains(t, view, "Memory: 512m")
	assert.Contains(t, view, "min 256m / recommended 512m / max 2g")
	assert.Contains(t, view, "CPUs: 1.0")
	assert.Contains(t, view, "PIDs: 256")
	assert.Contains(t, view, "default 128 / max 512")
	assert.Contains(t, view, "Esc: back")
	assert.Contains(t, view, "Ctrl+C: quit")
}

func TestModel_ResourcesScreenSaysWhenNoOverrideAllowed(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:       map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps: []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: &types.ResourceSettings{
			AppID:    "alpha",
			Services: []types.ResourceServiceSettings{{Service: "web", Adjustable: false}},
		},
	}
	m := loadResourcesScreen(t, fake)

	assert.Empty(t, m.resourceService)
	assert.Contains(t, m.View(), "does not allow resource overrides")
}

func TestModel_ResourcesAcceptsBAndQAsTypedInput(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)
	require.Equal(t, screenResources, m.screen)

	before := m.resourceFields[m.resourceFieldCursor].value
	m = updateModel(t, m, runeKey('b'))
	m = updateModel(t, m, runeKey('q'))

	assert.Equal(t, before+"bq", m.resourceFields[m.resourceFieldCursor].value)
	assert.Equal(t, screenResources, m.screen)
	assert.False(t, m.exiting)
	assert.Contains(t, m.helpLine(), "Esc: back")
}

func TestModel_ResourcesEscGoesBackToActions(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = assertModel(t, next)
	require.Nil(t, cmd)

	assert.Equal(t, screenAppActions, m.screen)
	assert.False(t, m.exiting)
}

func TestModel_ResourcesSubmitSendsOnlyChangedFieldsAsPointers(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
		reconfigureResult: &types.ReconfigureResult{
			AppID:   "alpha",
			Service: "web",
			Memory:  "1g",
			CPUs:    "1.0",
			PIDs:    256,
			Status:  &types.AppStatus{AppID: "alpha", State: "running"},
		},
	}
	m := loadResourcesScreen(t, fake)

	// Change only Memory (cursor starts on it): clear "512m", type "1g".
	for range "512m" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoResourceField(t, m, "1g")

	// Step past CPUs and PIDs (leave unchanged), then submit on "Apply changes".
	m = updateModel(t, m, enterKey()) // memory -> cpus
	m = updateModel(t, m, enterKey()) // cpus -> pids
	m = updateModel(t, m, enterKey()) // pids -> apply
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.reconfigureRequests, 1)
	req := fake.reconfigureRequests[0]
	assert.Equal(t, "alpha", req.AppID)
	assert.Equal(t, "web", req.Service)
	require.NotNil(t, req.Memory)
	assert.Equal(t, "1g", *req.Memory)
	assert.Nil(t, req.CPUs, "untouched CPU field must send nil")
	assert.Nil(t, req.PIDs, "untouched PID field must send nil")

	view := m.View()
	assert.Contains(t, view, "Reconfigure complete")
	assert.Contains(t, view, "Memory: 1g")
}

func TestModel_ResourcesNoChangeSubmitIsRefusedBeforeEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	// Step straight to "Apply changes" without editing any field.
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.Nil(t, cmd)

	assert.Empty(t, fake.reconfigureRequests, "a no-op submit must not reach the engine")
	require.Error(t, m.err)
	assert.Contains(t, m.View(), "no resource limits changed")
}

func TestModel_ResourcesPidsChangeParsesToIntPointer(t *testing.T) {
	t.Parallel()

	m := model{
		resourceService: "web",
		status:          &types.AppStatus{AppID: "alpha"},
		resourceFields: []resourceField{
			{target: resourceFieldMemory, original: "512m", value: "512m"},
			{target: resourceFieldCPUs, original: "1.0", value: "1.0"},
			{target: resourceFieldPIDs, original: "256", value: "512"},
		},
	}

	req, changed := m.reconfigureRequest()
	assert.True(t, changed)
	assert.Nil(t, req.Memory)
	assert.Nil(t, req.CPUs)
	require.NotNil(t, req.PIDs)
	assert.Equal(t, 512, *req.PIDs)
}

func TestModel_ResourcesConfirmationFlowSurfacesEngineConfirmation(t *testing.T) {
	t.Parallel()

	const message = "service: web\nrecreates the container (brief downtime)"
	sender := newRecordingSender()
	fake := &confirmingReconfigureEngine{
		fakeEngine: &fakeEngine{
			statuses:          map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
			listStatusApps:    []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
			resourceSettings:  resourceSettingsFixture(),
			reconfigureResult: &types.ReconfigureResult{AppID: "alpha", Service: "web", Memory: "1g"},
		},
		confirmation: types.Confirmation{
			Kind:    "reconfigure_deploy",
			Title:   "reconfigure alpha",
			Message: message,
		},
	}
	m := loadResourcesScreenWithSender(t, fake, sender.Send)

	for range "512m" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoResourceField(t, m, "1g")
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	request := sender.waitConfirmation(t)
	m = updateModel(t, m, request)
	view := m.View()
	assert.Contains(t, view, "reconfigure alpha")
	assert.Contains(t, view, "recreates the container (brief downtime)")
	assert.Contains(t, view, "Yes: y")

	m = updateModel(t, m, runeKey('y'))
	select {
	case result := <-resultC:
		m = updateModel(t, m, result)
	case <-time.After(time.Second):
		t.Fatal("reconfigure command did not finish after confirmation")
	}

	require.Len(t, fake.reconfigureRequests, 1)
	assert.Contains(t, m.View(), "Reconfigure complete")
}

func TestModel_ResourcesSurfacesEngineError(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
		reconfigureErr:   assertErr("memory 8g exceeds band max 2g"),
	}
	m := loadResourcesScreen(t, fake)

	for range "512m" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoResourceField(t, m, "8g")
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())
	m = updateModel(t, m, enterKey())
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	require.Len(t, fake.reconfigureRequests, 1)
	view := m.View()
	assert.Contains(t, view, "Reconfigure failed")
	assert.Contains(t, view, "memory 8g exceeds band max 2g")
	assert.Equal(t, screenResources, m.screen, "error keeps the user on the screen to correct and retry")
}

func TestModel_ResourcesSurfacesLoadErrorDistinctly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:            map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:      []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettingsErr: assertErr("alpha is not installed"),
	}
	m := loadResourcesScreen(t, fake)

	require.Equal(t, screenResources, m.screen)
	require.Equal(t, []string{"alpha"}, fake.resourceSettingsCalls)

	view := m.View()
	assert.Contains(t, view, "Could not load resource settings: alpha is not installed")
	assert.NotContains(t, view, "Reconfigure failed")
	assert.NotContains(t, view, "does not allow resource overrides")

	// Screen stays usable: Esc returns to the app-actions menu, no crash.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = assertModel(t, next)
	require.Nil(t, cmd)
	assert.Equal(t, screenAppActions, m.screen)
	assert.False(t, m.exiting)
}

type confirmingReconfigureEngine struct {
	*fakeEngine
	confirmation types.Confirmation
}

func (e *confirmingReconfigureEngine) Reconfigure(
	ctx context.Context,
	req types.ReconfigureRequest,
	_ engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.ReconfigureResult, error) {
	e.reconfigureRequests = append(e.reconfigureRequests, req)
	if confirmer != nil {
		accepted, err := confirmer.Confirm(ctx, e.confirmation)
		if err != nil {
			return nil, err
		}
		if !accepted {
			return nil, assertErr("reconfigure declined")
		}
	}
	return e.reconfigureResult, e.reconfigureErr
}

func loadResourcesScreen(t *testing.T, eng *fakeEngine) model {
	t.Helper()

	m := loadCheckAppsStatusScreen(t, eng)
	return openResources(t, m)
}

func loadResourcesScreenWithSender(t *testing.T, eng engine.Engine, send func(tea.Msg)) model {
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

	return openResources(t, m)
}

func openResources(t *testing.T, m model) model {
	t.Helper()

	for checkAppActions[m.actionCursor] != "Manage resources" {
		m = updateModel(t, m, downKey())
	}
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	return updateModel(t, m, cmd())
}

func typeIntoResourceField(t *testing.T, m model, value string) model {
	t.Helper()

	for _, r := range value {
		m = updateModel(t, m, runeKey(r))
	}
	return m
}

func resourceSettingsFixture() *types.ResourceSettings {
	return &types.ResourceSettings{
		AppID: "alpha",
		Services: []types.ResourceServiceSettings{
			{
				Service:           "web",
				Adjustable:        true,
				CurrentMemory:     "512m",
				CurrentCPUs:       "1.0",
				CurrentPIDs:       256,
				MemoryMin:         "256m",
				MemoryRecommended: "512m",
				MemoryMax:         "2g",
				CPUsMin:           "0.5",
				CPUsRecommended:   "1.0",
				CPUsMax:           "2.0",
				PIDsDefault:       128,
				PIDsMax:           512,
			},
		},
	}
}

func assertErr(msg string) error {
	return &stringError{msg: msg}
}

type stringError struct {
	msg string
}

func (e *stringError) Error() string {
	return e.msg
}
