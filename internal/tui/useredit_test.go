package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func userEditFake() *fakeEngine {
	return &fakeEngine{
		statuses:           map[string]*types.AppStatus{"alpha": {AppID: "alpha", State: "running"}},
		listStatusApps:     []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha"}, State: "running"}},
		ensureOverridePath: "/home/u/docker/alpha/docker-compose.override.yml",
		ensureEnvPath:      "/home/u/docker/alpha/.env.user",
	}
}

// selectActionByName moves the action cursor to the named action and selects
// it, returning the settled model and the command Update produced.
func selectActionByName(t *testing.T, m model, name string) (model, tea.Cmd) {
	t.Helper()

	for checkAppActions[m.actionCursor] != name {
		m = updateModel(t, m, downKey())
	}
	next, cmd := m.Update(enterKey())
	return assertModel(t, next), cmd
}

func TestCheckAppActions_IncludesEditAndViewActions(t *testing.T) {
	t.Parallel()

	assert.Contains(t, checkAppActions, "Edit compose")
	assert.Contains(t, checkAppActions, "Edit env")
	assert.Contains(t, checkAppActions, "View env (redacted)")
}

func TestModel_EditCompose_SeedsOverrideAndShowsWarning(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	m := loadCheckAppsStatusScreen(t, fake)

	m, cmd := selectActionByName(t, m, "Edit compose")
	require.NotNil(t, cmd)
	assert.Equal(t, overrideEditWarning, m.actionMessage,
		"compose edit must surface the T17 override warning")

	resolved, ok := cmd().(editPathResolvedMsg)
	require.True(t, ok)
	assert.Equal(t, []string{"alpha"}, fake.ensureOverrideCalls)
	assert.True(t, resolved.isCompose)
	assert.Equal(t, fake.ensureOverridePath, resolved.path)
}

func TestModel_EditEnv_OffersRewireThenSeedsEnv(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	m := loadCheckAppsStatusScreen(t, fake)

	// "Edit env" first runs the rewire migration offer (a no-op on an
	// already-wired stack), then chains to the .env.user path resolve.
	m, cmd := selectActionByName(t, m, "Edit env")
	require.NotNil(t, cmd)
	assert.Empty(t, m.actionMessage, "env edit must not show the compose-only warning")

	rewired, ok := cmd().(rewireDoneMsg)
	require.True(t, ok, "Edit env must offer the rewire migration before the editor")
	assert.Equal(t, []string{"alpha"}, fake.rewireStackCalls)

	_, chain, handled := m.updateUserEditMsg(rewired)
	require.True(t, handled)
	require.NotNil(t, chain, "a settled rewire must chain to the env path resolve")

	resolved, ok := chain().(editPathResolvedMsg)
	require.True(t, ok)
	assert.Equal(t, []string{"alpha"}, fake.ensureEnvCalls)
	assert.False(t, resolved.isCompose)
	assert.Equal(t, fake.ensureEnvPath, resolved.path)
}

func TestModel_EditEnv_RewiredShowsStatusAndProceeds(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	fake.rewireStackDone = true
	m := loadCheckAppsStatusScreen(t, fake)

	next, chain, ok := m.updateUserEditMsg(rewireDoneMsg{appID: "alpha", rewired: true})
	require.True(t, ok)
	require.NotNil(t, chain, "a rewired stack still opens the editor")

	settled := assertModel(t, next)
	assert.Contains(t, settled.actionMessage, "Migrated")

	resolved, ok := chain().(editPathResolvedMsg)
	require.True(t, ok)
	assert.Equal(t, fake.ensureEnvPath, resolved.path)
}

func TestModel_EditEnv_DeclinedRewireWarnsAndProceeds(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	m := loadCheckAppsStatusScreen(t, fake)

	next, chain, ok := m.updateUserEditMsg(rewireDoneMsg{appID: "alpha", declined: true})
	require.True(t, ok)
	require.NotNil(t, chain, "a declined rewire is warn-but-allow and still opens the editor")

	settled := assertModel(t, next)
	assert.Contains(t, settled.actionMessage, "not activated")
	assert.Contains(t, settled.actionMessage, "wdm update alpha")

	_, ok = chain().(editPathResolvedMsg)
	require.True(t, ok)
}

func TestModel_EditEnv_RewireErrorAborts(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	m := loadCheckAppsStatusScreen(t, fake)

	next, chain, ok := m.updateUserEditMsg(rewireDoneMsg{appID: "alpha", err: errors.New("rewire blew up")})
	require.True(t, ok)
	assert.Nil(t, chain, "a hard rewire error must not open the editor")

	settled := assertModel(t, next)
	assert.False(t, settled.busy)
	require.Error(t, settled.err)
	assert.Empty(t, fake.ensureEnvCalls, "an aborted rewire must not seed the env overlay")
}

func TestModel_EditPathResolved_LaunchesEditor(t *testing.T) {
	// No t.Parallel: t.Setenv requires a non-parallel test.
	//
	// "true" is a real no-op binary, so ResolveEditorArgv yields a valid argv
	// and ExecProcess builds a non-nil command. The command is NOT executed —
	// only the branch is asserted.
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")

	m := loadCheckAppsStatusScreen(t, userEditFake())
	next, cmd, ok := m.updateUserEditMsg(editPathResolvedMsg{
		appID:     "alpha",
		path:      "/home/u/docker/alpha/.env.user",
		isCompose: false,
	})
	require.True(t, ok)
	require.NotNil(t, cmd, "a resolved path must launch the editor")
	assert.IsType(t, model{}, next)
}

func TestModel_EditPathResolved_SurfacesSeedError(t *testing.T) {
	t.Parallel()

	m := loadCheckAppsStatusScreen(t, userEditFake())
	next, cmd, ok := m.updateUserEditMsg(editPathResolvedMsg{
		appID: "alpha",
		err:   errors.New("seed failed"),
	})
	require.True(t, ok)
	assert.Nil(t, cmd)
	settled := next.(model)
	assert.False(t, settled.busy)
	require.Error(t, settled.err)
}

func TestModel_Edited_ValidatesWarnButAllow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		isCompose bool
	}{
		{name: "compose edit", isCompose: true},
		{name: "env edit", isCompose: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := userEditFake()
			fake.validateStackWarn = []string{"service web: port 0.0.0.0:80 exposed"}
			m := loadCheckAppsStatusScreen(t, fake)

			// editedMsg (editor exited cleanly) must trigger ValidateStack for
			// BOTH compose and env edits.
			next, cmd, ok := m.updateUserEditMsg(editedMsg{appID: "alpha", isCompose: tt.isCompose})
			require.True(t, ok)
			require.NotNil(t, cmd)

			settled := assertModel(t, next)
			validated := assertModel(t, updateModel(t, settled, cmd()))

			assert.Equal(t, []string{"alpha"}, fake.validateStackCalls)
			assert.False(t, validated.busy)
			assert.Nil(t, validated.err, "warnings are warn-but-allow, not a block")
			assert.Contains(t, validated.actionMessage, "0.0.0.0:80 exposed")
		})
	}
}

func TestModel_Edited_ValidationErrorStaysAllowed(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	fake.validateStackErr = errors.New("compose config rejected the override")
	m := loadCheckAppsStatusScreen(t, fake)

	next, cmd, ok := m.updateUserEditMsg(editedMsg{appID: "alpha", isCompose: true})
	require.True(t, ok)
	require.NotNil(t, cmd)

	validated := assertModel(t, updateModel(t, assertModel(t, next), cmd()))
	assert.Nil(t, validated.err, "a validation error after the edit is warn-but-allow")
	assert.Contains(t, validated.actionMessage, "compose config rejected the override")
}

func TestModel_ViewEnv_RendersRedactedAndLeaksNoSecret(t *testing.T) {
	t.Parallel()

	const rawSecret = "S3CR3T-PLAINTEXT-VALUE"
	fake := userEditFake()
	fake.viewEnvResult = &types.ViewEnvResult{
		AppID: "alpha",
		Entries: []types.EnvEntry{
			{Key: "TZ", Value: "UTC", Secret: false},
			{Key: "ADMIN_TOKEN", Value: "********", Secret: true},
		},
	}

	m := loadCheckAppsStatusScreen(t, fake)
	m, cmd := selectActionByName(t, m, "View env (redacted)")
	require.Equal(t, screenViewEnv, m.screen)
	require.NotNil(t, cmd)

	m = updateModel(t, m, cmd())
	require.Equal(t, []string{"alpha"}, fake.viewEnvCalls)
	assert.Empty(t, fake.rewireStackCalls, "the read-only view must never rewire/restart the stack")
	require.NotNil(t, m.viewEnv)

	view := m.View()
	assert.Contains(t, view, "View env (redacted)")
	assert.Contains(t, view, "TZ=UTC")
	assert.Contains(t, view, "ADMIN_TOKEN=********")
	assert.Contains(t, view, "[secret]")
	assert.NotContains(t, view, rawSecret, "the redacted view must never render a raw secret")
}

func TestModel_ViewEnv_SurfacesLoadError(t *testing.T) {
	t.Parallel()

	fake := userEditFake()
	fake.viewEnvErr = errors.New("stack not found")
	m := loadCheckAppsStatusScreen(t, fake)

	m, cmd := selectActionByName(t, m, "View env (redacted)")
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	assert.Equal(t, screenViewEnv, m.screen)
	assert.Contains(t, m.View(), "Could not load environment")
}
