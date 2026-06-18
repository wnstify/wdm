package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps stop-all` leaf: the wdm.v1 envelope it emits
// under --json, progress suppression, the confirmer it hands the engine,
// the nonzero exit on partial failure, and the clean-success path. They
// drive RunE end-to-end through NewRootCmd using the shared fakeEngine
// double and the runLeaf / decodeEnvelopeData / nonEmptyLines helpers from
// the other cli tests.

func TestAppsStopAll_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		stopAllResult: &types.StopAllResult{
			Stopped: []types.StoppedApp{
				{AppID: "vaultwarden", ComposeProject: "wdm-vaultwarden"},
				{AppID: "freshrss", ComposeProject: "wdm-freshrss"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "stop-all", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "stop-all --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	stopped, ok := data["stopped"].([]any)
	require.True(t, ok, "result must carry the stopped array")
	assert.Len(t, stopped, 2)
}

func TestAppsStopAll_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{stopAllResult: &types.StopAllResult{}}
		_, _, err := runLeaf(t, fake, "apps", "stop-all", "--json")
		require.NoError(t, err)
		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{stopAllResult: &types.StopAllResult{}}
		stdout, _, err := runLeaf(t, fake, "apps", "stop-all", "--yes")
		require.NoError(t, err)
		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

func TestAppsStopAll_PassesConfirmer(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{stopAllResult: &types.StopAllResult{}}
	_, _, err := runLeaf(t, fake, "apps", "stop-all", "--yes", "--json")
	require.NoError(t, err)
	assert.NotNil(t, fake.confirmer, "stop-all must pass a non-nil Confirmer to the engine")
}

// A partial-failure result (the engine returns no error but reports failed
// stacks) must still render the result, then exit nonzero with a typed
// generic error.
func TestAppsStopAll_PartialFailure_RendersResultThenExitsNonzero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		stopAllResult: &types.StopAllResult{
			Stopped: []types.StoppedApp{{AppID: "vaultwarden"}},
			Failed: []types.StoppedApp{
				{AppID: "freshrss", Error: "daemon unreachable"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "stop-all", "--yes")
	require.Error(t, err, "a partial failure must surface a nonzero exit")
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"partial failure maps to the generic exit code")

	// The result was rendered before the error: both stacks appear.
	assert.Contains(t, stdout, "vaultwarden", "the stopped stack must be rendered")
	assert.Contains(t, stdout, "freshrss", "the failed stack must be rendered")
	assert.Contains(t, stdout, "daemon unreachable", "the per-stack failure detail must be rendered")
}

func TestAppsStopAll_CleanSuccess_ExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		stopAllResult: &types.StopAllResult{
			Stopped: []types.StoppedApp{{AppID: "vaultwarden"}},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "stop-all", "--yes")
	require.NoError(t, err, "a clean stop-all must exit zero")
	assert.Contains(t, stdout, "Stopped 1 app(s).")
}

func TestAppsStopAll_EmptyManagedSet_RendersNoApps(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{stopAllResult: &types.StopAllResult{}}
	stdout, _, err := runLeaf(t, fake, "apps", "stop-all", "--yes")
	require.NoError(t, err)
	assert.Contains(t, stdout, "No managed apps to stop.")
}

// stop-all takes no positional args; extra args fail before the engine
// factory runs.
func TestAppsStopAll_NoArgs_RefusesExtraPositional(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{stopAllResult: &types.StopAllResult{}}
	_, _, err := runLeaf(t, fake, "apps", "stop-all", "unexpected", "--yes")
	require.Error(t, err, "a positional arg must be refused by NoArgs validation")
}
