package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps redeploy` leaf: the wdm.v1 envelope it emits
// under --json, progress suppression, the confirmer it hands the engine, the
// app-id/--stack-path request mapping, and the empty-stdout error path. They
// drive RunE end-to-end through NewRootCmd using the shared fakeEngine double,
// which records the RedeployStack request on redeployReq.

func TestAppsRedeploy_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		redeployResult: &types.RestartResult{
			AppID:             "vaultwarden",
			ComposeProject:    "wdm-vaultwarden",
			RestartedServices: []string{"server", "db"},
			Status:            &types.AppStatus{AppID: "vaultwarden", State: "running"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "redeploy", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "redeploy --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "wdm-vaultwarden", data["compose_project"])
	assert.Equal(t, []any{"server", "db"}, data["restarted_services"])
	status, ok := data["status"].(map[string]any)
	require.True(t, ok, "result must nest the status object")
	assert.Equal(t, "running", status["state"])
}

func TestAppsRedeploy_InvokesEngineAndMapsRequest(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{redeployResult: &types.RestartResult{AppID: "vaultwarden"}}

	_, _, err := runLeaf(t, fake, "apps", "redeploy", "vaultwarden",
		"--stack-path", "/home/test/docker/vaultwarden", "--yes", "--json")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", fake.redeployReq.AppID,
		"the positional app-id must map onto the redeploy request")
	assert.Equal(t, "/home/test/docker/vaultwarden", fake.redeployReq.StackPath,
		"--stack-path must map onto the redeploy request")
	assert.NotNil(t, fake.confirmer, "redeploy must pass a non-nil Confirmer to the engine")
}

func TestAppsRedeploy_JSON_SuppressesProgress(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{redeployResult: &types.RestartResult{AppID: "vaultwarden"}}
	_, _, err := runLeaf(t, fake, "apps", "redeploy", "vaultwarden", "--json")
	require.NoError(t, err)

	assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
}

func TestAppsRedeploy_PlainRendersFinishScreen(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		redeployResult: &types.RestartResult{
			AppID:             "vaultwarden",
			RestartedServices: []string{"server"},
			Status:            &types.AppStatus{AppID: "vaultwarden", State: "running"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "redeploy", "vaultwarden", "--yes")
	require.NoError(t, err)

	assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
	assert.Contains(t, stdout, "was redeployed and is running", "the finish screen must headline success")
	assert.Contains(t, stdout, "Recreated services:", "the finish screen must list the recreated services")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the finish screen, not a JSON envelope")
}

func TestAppsRedeploy_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "stack is not managed", "run wdm apps list")
	fake := &fakeEngine{err: engineErr}

	stdout, _, err := runLeaf(t, fake, "apps", "redeploy", "vaultwarden", "--json")
	require.Error(t, err, "a typed engine error must propagate out of Execute")
	assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
	assert.Empty(t, stdout, "no output may be written to stdout on the error path")
}
