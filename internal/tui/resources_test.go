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

	req, changed, err := m.reconfigureRequest()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Nil(t, req.Memory)
	assert.Nil(t, req.CPUs)
	require.NotNil(t, req.PIDs)
	assert.Equal(t, 512, *req.PIDs)
}

// TestModel_ResourcesInvalidPidsSurfacesInlineErrorAndDoesNotSubmit is the
// Task 1 / issue #28 regression: a changed-but-unparseable PIDs field
// combined with another changed field must surface an inline error and NOT
// submit — instead of silently dropping the PIDs input. Without the fix the
// reconfigure would run reporting success while ignoring the bad PIDs value.
func TestModel_ResourcesInvalidPidsSurfacesInlineErrorAndDoesNotSubmit(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:          map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:    []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings:  resourceSettingsFixture(),
		reconfigureResult: &types.ReconfigureResult{AppID: "alpha", Service: "web"},
	}
	m := loadResourcesScreen(t, fake)

	// Change Memory (valid) AND PIDs (invalid) so `changed` is true but the
	// PIDs value cannot parse.
	for range "512m" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoResourceField(t, m, "1g")
	m = updateModel(t, m, enterKey()) // memory -> cpus
	m = updateModel(t, m, enterKey()) // cpus -> pids
	for range "256" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeIntoResourceField(t, m, "1.5") // not a whole number
	m = updateModel(t, m, enterKey())      // pids -> apply
	next, cmd := m.Update(enterKey())      // submit
	m = assertModel(t, next)

	require.Nil(t, cmd, "an invalid PIDs value must not dispatch a reconfigure command")
	assert.Empty(t, fake.reconfigureRequests, "an invalid PIDs value must not reach the engine")
	require.Error(t, m.err)
	assert.Contains(t, m.View(), "PIDs limit must be a whole number")
	assert.Equal(t, screenResources, m.screen, "the user stays on the screen to correct the value")
}

func TestModel_ResourcesNoAdjustableServiceSubmitRefused(t *testing.T) {
	t.Parallel()

	m := model{resourceService: ""}
	next, cmd := m.submitReconfigure()
	m = assertModel(t, next)
	require.Nil(t, cmd)
	require.Error(t, m.err)
	assert.Contains(t, m.err.Error(), "no adjustable service")
}

// TestModel_ResourcesCPUsChangeMapsToPointer covers the resourceFieldCPUs
// arm of reconfigureRequest: a changed CPUs field maps to a non-nil CPUs
// pointer while untouched memory/pids stay nil ("leave unchanged").
func TestModel_ResourcesCPUsChangeMapsToPointer(t *testing.T) {
	t.Parallel()

	m := model{
		resourceService: "web",
		status:          &types.AppStatus{AppID: "alpha"},
		resourceFields: []resourceField{
			{target: resourceFieldMemory, original: "512m", value: "512m"},
			{target: resourceFieldCPUs, original: "1.0", value: "2.0"},
			{target: resourceFieldPIDs, original: "256", value: "256"},
		},
	}

	req, changed, err := m.reconfigureRequest()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Nil(t, req.Memory)
	assert.Nil(t, req.PIDs)
	require.NotNil(t, req.CPUs)
	assert.Equal(t, "2.0", *req.CPUs)
}

// TestModel_ResourcesViewLoadingState covers the busy && settings==nil arm
// of resourcesView: while the read-only settings load is in flight the
// screen shows a loading line for the active app, not the field editor.
func TestModel_ResourcesViewLoadingState(t *testing.T) {
	t.Parallel()

	m := model{
		screen: screenResources,
		busy:   true,
		status: &types.AppStatus{AppID: "alpha"},
	}

	view := m.resourcesView()
	assert.Contains(t, view, "Loading resource limits for alpha...")
	assert.NotContains(t, view, "Apply changes")
}

// TestModel_ResourcesViewReconfiguringState covers the busy (settings
// loaded) arm of resourcesView: while the reconfigure runs the screen
// shows the reconfiguring line plus the streamed progress message.
func TestModel_ResourcesViewReconfiguringState(t *testing.T) {
	t.Parallel()

	m := model{
		screen:           screenResources,
		busy:             true,
		status:           &types.AppStatus{AppID: "alpha"},
		resourceSettings: resourceSettingsFixture(),
		progress:         progressMsg{message: "recreating container"},
	}

	view := m.resourcesView()
	assert.Contains(t, view, "Reconfiguring alpha...")
	assert.Contains(t, view, "recreating container")
}

// TestModel_ResourcesViewResultStateRendersBackupPath covers the
// reconfigureResult arm of resourcesView and the BackupPath arm of
// writeReconfigureResult: the post-reconfigure summary renders the applied
// limits, the config backup path, and the runtime state.
func TestModel_ResourcesViewResultStateRendersBackupPath(t *testing.T) {
	t.Parallel()

	m := model{
		screen: screenResources,
		status: &types.AppStatus{AppID: "alpha"},
		reconfigureResult: &types.ReconfigureResult{
			AppID:      "alpha",
			Service:    "web",
			Memory:     "1g",
			CPUs:       "2.0",
			PIDs:       300,
			BackupPath: "/home/test/docker/alpha/.wdm-backups/123-reconfigure",
			Status:     &types.AppStatus{AppID: "alpha", State: "running"},
		},
	}

	view := m.resourcesView()
	assert.Contains(t, view, "Reconfigure complete.")
	assert.Contains(t, view, "Service: web")
	assert.Contains(t, view, "Memory: 1g")
	assert.Contains(t, view, "CPUs: 2.0")
	assert.Contains(t, view, "PIDs: 300")
	assert.Contains(t, view, "Config backup: /home/test/docker/alpha/.wdm-backups/123-reconfigure")
	assert.Contains(t, view, "State: running")
}

// TestResourceBandHint_AllUnsetReturnsEmpty covers the empty-band arm of
// resourceBandHint: when the catalog declares no min/recommended/max the
// hint is empty, not a dangling "()".
func TestResourceBandHint_AllUnsetReturnsEmpty(t *testing.T) {
	t.Parallel()

	assert.Empty(t, resourceBandHint("", "", ""))
}

// TestResourcePIDsHint_AllUnsetReturnsEmpty covers the empty-band arm of
// resourcePIDsHint: a profile with no default and no max yields an empty
// hint.
func TestResourcePIDsHint_AllUnsetReturnsEmpty(t *testing.T) {
	t.Parallel()

	assert.Empty(t, resourcePIDsHint(0, 0))
}

// TestResourceServiceSettings_NilReturnsNil covers the settings==nil arm of
// resourceServiceSettings.
func TestResourceServiceSettings_NilReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, resourceServiceSettings(nil))
}

// TestModel_ResourcesLoadNilSettingsSurfacesError covers the
// settings==nil && err==nil arm of loadResourceSettingsCmd: a fake engine
// that returns (nil, nil) is treated as "settings unavailable", surfacing
// a load error rather than rendering an empty editor.
func TestModel_ResourcesLoadNilSettingsSurfacesError(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: nil,
	}
	m := loadResourcesScreen(t, fake)

	require.Error(t, m.resourceLoadErr)
	view := m.View()
	assert.Contains(t, view, "Could not load resource settings")
	assert.Contains(t, view, "resource settings unavailable")
}

func TestModel_ResourcesBackspaceOnEmptyFieldIsNoOp(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	// Clear the memory field fully, then an extra backspace must be a no-op.
	for range "512m" {
		m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	assert.Empty(t, m.resourceFields[m.resourceFieldCursor].value)
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Empty(t, m.resourceFields[m.resourceFieldCursor].value, "backspace on an empty field is a no-op")
}

func TestModel_ResourcesEditingIgnoredOnApplyRow(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	originals := make([]string, len(m.resourceFields))
	for i := range m.resourceFields {
		originals[i] = m.resourceFields[i].value
	}

	// Step the cursor onto the "Apply changes" row (past every field).
	for range m.resourceFields {
		m = updateModel(t, m, enterKey())
	}
	require.Equal(t, len(m.resourceFields), m.resourceFieldCursor)

	// Typing and backspace on the apply row must not mutate any field.
	m = updateModel(t, m, runeKey('x'))
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeySpace})

	for i := range m.resourceFields {
		assert.Equal(t, originals[i], m.resourceFields[i].value,
			"input on the apply row must not edit a field value")
	}
}

func TestModel_ResourcesUpDownNavigationStaysInBounds(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	// Up at the top row is clamped to 0.
	require.Zero(t, m.resourceFieldCursor)
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyUp})
	assert.Zero(t, m.resourceFieldCursor, "up at the top row stays at 0")

	// Down past the apply row is clamped to len(fields).
	for range len(m.resourceFields) + 3 {
		m = updateModel(t, m, downKey())
	}
	assert.Equal(t, len(m.resourceFields), m.resourceFieldCursor,
		"down is clamped to the apply row")

	// Up from the apply row moves back into the fields.
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, len(m.resourceFields)-1, m.resourceFieldCursor,
		"up from the apply row moves to the last field")
}

func TestModel_ResourcesSpaceTypedIntoField(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statuses:         map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:   []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		resourceSettings: resourceSettingsFixture(),
	}
	m := loadResourcesScreen(t, fake)

	before := m.resourceFields[m.resourceFieldCursor].value
	m = updateModel(t, m, tea.KeyMsg{Type: tea.KeySpace})
	assert.Equal(t, before+" ", m.resourceFields[m.resourceFieldCursor].value,
		"space appends to the focused field")
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
