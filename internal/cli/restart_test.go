package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps restart` leaf: the wdm.v1
// envelope it emits under --json, progress suppression, the confirmer it
// hands the engine, the --stack-path request mapping, the empty-stdout
// error path, the needs-attention exit-0 contract, and the ExactArgs(1)
// refusals. They drive RunE end-to-end through NewRootCmd (the honest path
// since the persistent --json flag only resolves through the root) using
// the shared fakeEngine double and the runLeaf helper from
// fake_engine_test.go, reusing decodeEnvelopeData / nonEmptyLines from
// envelope_contract_test.go.

// --- Point 1: apps restart --json -> exactly one envelope wrapping RestartResult.

func TestAppsRestart_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		restartResult: &types.RestartResult{
			AppID:             "vaultwarden",
			ComposeProject:    "wdm-vaultwarden",
			RestartedServices: []string{"server", "db"},
			Status:            &types.AppStatus{AppID: "vaultwarden", State: "running"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "restart --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "wdm-vaultwarden", data["compose_project"])
	// RestartedServices rides under its snake_case key as a JSON array.
	assert.Equal(t, []any{"server", "db"}, data["restarted_services"])
	// The post-restart status is nested under "status" as an object.
	status, ok := data["status"].(map[string]any)
	require.True(t, ok, "result must nest the status object")
	assert.Equal(t, "running", status["state"])
}

// --- Point 2: progress suppression and confirmer wiring. Under --json the
// engine receives a nil ProgressFn (PRD §32); in plain mode it receives a
// non-nil one. Both modes hand the engine a non-nil Confirmer.

func TestAppsRestart_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{restartResult: &types.RestartResult{AppID: "vaultwarden"}}
		stdout, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--json")
		require.NoError(t, err)

		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
		lines := nonEmptyLines(stdout)
		require.Len(t, lines, 1, "--json stdout must carry only the envelope")
		assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
		assert.Equal(t, "vaultwarden", decodeEnvelopeData(t, lines[0])["app_id"])
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{restartResult: &types.RestartResult{AppID: "vaultwarden"}}
		stdout, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--yes")
		require.NoError(t, err)

		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		// Plain mode writes a human finish screen, not an envelope.
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

func TestAppsRestart_PassesConfirmer(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{restartResult: &types.RestartResult{AppID: "vaultwarden"}}
	_, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--yes", "--json")
	require.NoError(t, err)
	assert.NotNil(t, fake.confirmer, "restart must pass a non-nil Confirmer to the engine")
}

// --- Point 3: --stack-path maps onto the request. The shared fakeEngine
// does not record the RestartRequest, so a local wrapper embeds it and
// overrides only Restart to capture the request before delegating — the
// shared fake_engine_test.go stays untouched.

// recordingRestartEngine embeds *fakeEngine and overrides Restart to
// record the request it received. Every other method (List, Status,
// Install,...) is inherited from the embedded fakeEngine, so the
// engine.Engine interface stays satisfied.
type recordingRestartEngine struct {
	*fakeEngine
	gotReq types.RestartRequest
}

func (r *recordingRestartEngine) Restart(
	ctx context.Context,
	req types.RestartRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestartResult, error) {
	r.gotReq = req
	return r.fakeEngine.Restart(ctx, req, onProgress, confirmer)
}

// runRestartLeaf drives one `apps restart` invocation through NewRootCmd
// with the recording engine wired as the lazy factory result, mirroring
// runLeaf but typed to the local wrapper (runLeaf returns *fakeEngine).
func runRestartLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return eng, nil
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(args)
	root.SetContext(t.Context())

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestAppsRestart_MapsAppIDAndStackPathOntoRequest(t *testing.T) {
	t.Parallel()

	rec := &recordingRestartEngine{
		fakeEngine: &fakeEngine{restartResult: &types.RestartResult{AppID: "vaultwarden"}},
	}

	_, _, err := runRestartLeaf(t, rec, "apps", "restart", "vaultwarden",
		"--stack-path", "/home/test/docker/vaultwarden", "--json")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", rec.gotReq.AppID, "the positional app-id must map onto RestartRequest.AppID")
	assert.Equal(t, "/home/test/docker/vaultwarden", rec.gotReq.StackPath,
		"--stack-path must map onto RestartRequest.StackPath")
}

func TestAppsRestart_OmittedStackPathIsEmpty(t *testing.T) {
	t.Parallel()

	rec := &recordingRestartEngine{
		fakeEngine: &fakeEngine{restartResult: &types.RestartResult{AppID: "vaultwarden"}},
	}

	_, _, err := runRestartLeaf(t, rec, "apps", "restart", "vaultwarden", "--json")
	require.NoError(t, err)

	assert.Empty(t, rec.gotReq.StackPath, "an omitted --stack-path must leave RestartRequest.StackPath empty")
}

// --- Point 4: error path. A typed *types.Error propagates out of Execute,
// and stdout stays empty in BOTH the --json and plain modes.

func TestAppsRestart_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "stack is not managed", "run wdm apps list")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"apps", "restart", "vaultwarden", "--json"}},
		{"plain", []string{"apps", "restart", "vaultwarden", "--yes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// --- Point 5: a needs_attention result is a successful restart (exit 0),
// and plain mode renders the neutral headline rather than asserting health.

func TestAppsRestart_NeedsAttention_ExitsZeroAndRendersNeutralHeadline(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		restartResult: &types.RestartResult{
			AppID:             "vaultwarden",
			RestartedServices: []string{"server"},
			Status: &types.AppStatus{
				AppID:          "vaultwarden",
				State:          "needs_attention",
				NeedsAttention: true,
				Message:        "post-restart verification found issues; run apps status for details",
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--yes")
	require.NoError(t, err, "a needs_attention restart is a successful operation and must not error")

	// The neutral headline must NOT assert the stack is running; it must
	// defer to the status block (gate mirrors writeRemoveFinish).
	assert.Contains(t, stdout, "see the status below", "needs-attention headline must defer to the status block")
	assert.NotContains(t, stdout, "is running", "needs-attention headline must not assert the stack is running")
	assert.Contains(t, stdout, "needs_attention", "the status state must be shown")
}

// TestAppsRestart_HealthyResult_RendersRunningHeadline pins the happy-path
// finish screen: a healthy result asserts the stack is running and lists
// the restarted services.
func TestAppsRestart_HealthyResult_RendersRunningHeadline(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		restartResult: &types.RestartResult{
			AppID:             "vaultwarden",
			RestartedServices: []string{"server", "db"},
			Status:            &types.AppStatus{AppID: "vaultwarden", State: "running"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "restart", "vaultwarden", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "is running", "a healthy result must assert the stack is running")
	assert.Contains(t, stdout, "Restarted services:", "the finish screen must list the restarted services")
	assert.Contains(t, stdout, "- server", "each restarted service must be listed")
	assert.Contains(t, stdout, "- db", "each restarted service must be listed")
}

// --- Point 6: ExactArgs(1) refusals. Zero or two positional args fail
// before the engine factory ever runs, proven by a sentinel factory that
// fails the test if invoked.

func TestAppsRestart_ExactArgs_RefusesWithoutConstructingEngine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"zero args", []string{"apps", "restart"}},
		{"two args", []string{"apps", "restart", "a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The factory must never be reached: ExactArgs validation runs
			// before RunE, so a bad arg count cannot construct the engine.
			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an arg-count refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "an arg-count violation must surface an error")
			assert.Empty(t, outBuf.String(), "an arg-count refusal must write nothing to stdout")
		})
	}
}
