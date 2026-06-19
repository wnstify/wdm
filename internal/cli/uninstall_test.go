package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the top-level `uninstall` command (PRD §39, issue #29):
// the wdm.v1 envelope it emits under --json, progress suppression, the
// confirmer it hands the engine, the clean-success exit-0 path, and the
// fail-closed abort that renders the failed stacks then exits nonzero.
// They drive RunE end-to-end through NewRootCmd using the shared fakeEngine
// double and the runLeaf / decodeEnvelopeData / nonEmptyLines helpers.

func TestUninstall_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown: []types.TornDownApp{
				{AppID: "vaultwarden", ComposeProject: "wdm-vaultwarden"},
				{AppID: "freshrss", ComposeProject: "wdm-freshrss"},
			},
			KeptDataPaths: []string{"/home/u/docker/vaultwarden", "/home/u/docker/freshrss"},
			RemovedPaths:  []string{"/home/u/.config/wdm", "/home/u/.local/bin/wdm"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "uninstall", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "uninstall --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	tornDown, ok := data["torn_down"].([]any)
	require.True(t, ok, "result must carry the torn_down array")
	assert.Len(t, tornDown, 2)

	kept, ok := data["kept_data_paths"].([]any)
	require.True(t, ok, "result must carry the kept_data_paths array")
	assert.Len(t, kept, 2)

	removed, ok := data["removed_paths"].([]any)
	require.True(t, ok, "result must carry the removed_paths array")
	assert.Len(t, removed, 2)
}

func TestUninstall_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{uninstallResult: &types.UninstallResult{}}
		_, _, err := runLeaf(t, fake, "uninstall", "--json")
		require.NoError(t, err)
		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{uninstallResult: &types.UninstallResult{}}
		stdout, _, err := runLeaf(t, fake, "uninstall", "--yes")
		require.NoError(t, err)
		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

func TestUninstall_PassesConfirmer(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{uninstallResult: &types.UninstallResult{}}
	_, _, err := runLeaf(t, fake, "uninstall", "--yes", "--json")
	require.NoError(t, err)
	assert.NotNil(t, fake.confirmer, "uninstall must pass a non-nil Confirmer to the engine")
}

// A clean uninstall (no error, no failed stacks) exits zero and reports the
// preservation guarantee on the plain finish screen.
func TestUninstall_CleanSuccess_ExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown:      []types.TornDownApp{{AppID: "vaultwarden"}},
			KeptDataPaths: []string{"/home/u/docker/vaultwarden"},
			RemovedPaths:  []string{"/home/u/.local/bin/wdm"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "uninstall", "--yes")
	require.NoError(t, err, "a clean uninstall must exit zero")
	assert.Contains(t, stdout, "wdm was uninstalled.")
	assert.Contains(t, stdout, "vaultwarden")
	assert.Contains(t, stdout, "named volumes and per-app stack data")
}

// A fail-closed abort (the engine returns no error but reports failed
// stacks) must render the result, name the failed stacks, state wdm was NOT
// removed, then exit nonzero with a typed generic error.
func TestUninstall_Abort_RendersResultThenExitsNonzero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		uninstallResult: &types.UninstallResult{
			TornDown: []types.TornDownApp{{AppID: "vaultwarden"}},
			Failed: []types.TornDownApp{
				{AppID: "freshrss", Error: "daemon unreachable"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "uninstall", "--yes")
	require.Error(t, err, "an abort must surface a nonzero exit")
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a fail-closed abort maps to the generic exit code")

	assert.Contains(t, stdout, "wdm was NOT removed")
	assert.Contains(t, stdout, "freshrss", "the failed stack must be rendered")
	assert.Contains(t, stdout, "daemon unreachable", "the per-stack failure detail must be rendered")
}

// A declined confirmation surfaces as the engine's user-canceled error; the
// CLI propagates it unchanged. The fake never invokes Confirm, so this test
// drives the error path through the engine's err field instead.
func TestUninstall_EngineUserCanceled_Propagates(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		err: types.NewError(types.ErrCodeUserCanceled, "uninstall canceled", "re-run to retry"),
	}

	_, _, err := runLeaf(t, fake, "uninstall")
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled),
		"a declined confirmation maps to the user-canceled exit code")
}

// uninstall takes no positional args; extra args fail before the engine
// factory runs.
func TestUninstall_NoArgs_RefusesExtraPositional(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{uninstallResult: &types.UninstallResult{}}
	_, _, err := runLeaf(t, fake, "uninstall", "unexpected", "--yes")
	require.Error(t, err, "a positional arg must be refused by NoArgs validation")
}

// uninstall is a top-level command, not under `apps`.
func TestUninstall_RegisteredAtTopLevel(t *testing.T) {
	t.Parallel()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return &fakeEngine{}, nil
	})

	cmd := findCommandPath(root, "uninstall")
	require.NotNil(t, cmd, "uninstall must be registered at the top level")
	require.NotNil(t, cmd.RunE, "uninstall must have a runnable RunE")

	notUnderApps := findCommandPath(root, "apps", "uninstall")
	assert.Nil(t, notUnderApps, "uninstall must NOT be registered under apps")
}
