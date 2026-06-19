package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the top-level `resources` command: the read-only
// current-values view (no limit flags), the reconfigure path (with limit
// flags), the wdm.v1 envelope under --json, progress suppression, the
// confirmer wiring, the flag-to-pointer request mapping, the error path,
// and the ExactArgs(1) refusal. They drive RunE end-to-end through
// NewRootCmd using the shared fakeEngine double.

// --- Read-only view: no limit flags calls ResourceSettings, not Reconfigure.

func TestResources_NoFlags_RendersCurrentValuesView(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		resourceSettings: &types.ResourceSettings{
			AppID: "vaultwarden",
			Services: []types.ResourceServiceSettings{
				{
					Service:           "server",
					Adjustable:        true,
					CurrentMemory:     "512m",
					CurrentCPUs:       "1.0",
					CurrentPIDs:       200,
					MemoryMin:         "256m",
					MemoryRecommended: "512m",
					MemoryMax:         "1g",
					CPUsMax:           "2.0",
					PIDsMax:           500,
				},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", fake.resourcesAppID, "the no-flags view must call ResourceSettings with the app id")
	assert.Contains(t, stdout, "server")
	assert.Contains(t, stdout, "current=512m")
	assert.Contains(t, stdout, "max=1g")
}

func TestResources_NoFlags_JSON_EmitsSettingsEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		resourceSettings: &types.ResourceSettings{
			AppID: "vaultwarden",
			Services: []types.ResourceServiceSettings{
				{Service: "server", Adjustable: true, CurrentMemory: "512m", MemoryMax: "1g"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "resources --json must emit exactly one envelope")
	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	services, ok := data["services"].([]any)
	require.True(t, ok, "the settings payload must carry a services array")
	require.Len(t, services, 1)
}

// --- Reconfigure path: limit flags call Reconfigure with a single envelope.

func TestResources_WithFlags_JSON_EmitsReconfigureEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		reconfigureResult: &types.ReconfigureResult{
			AppID:          "vaultwarden",
			Service:        "server",
			ComposeProject: "wdm-vaultwarden",
			Memory:         "1g",
			CPUs:           "2.0",
			PIDs:           300,
			Status:         &types.AppStatus{AppID: "vaultwarden", State: "running"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--memory", "1g", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "resources --json must emit exactly one envelope")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "server", data["service"])
	assert.Equal(t, "1g", data["memory"])
	status, ok := data["status"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "running", status["state"])
}

func TestResources_WithFlags_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "app"}}
		_, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--cpus", "1.5", "--json")
		require.NoError(t, err)
		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "app"}}
		stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--cpus", "1.5", "--yes")
		require.NoError(t, err)
		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

func TestResources_WithFlags_PassesConfirmer(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "app"}}
	_, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--pids", "300", "--yes", "--json")
	require.NoError(t, err)
	assert.NotNil(t, fake.confirmer, "reconfigure must pass a non-nil Confirmer to the engine")
}

// --- Flag-to-pointer mapping: only the changed flags become non-nil
// request fields; an omitted flag stays nil ("leave unchanged").

func TestResources_FlagToPointerMapping(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "db"}}
	_, _, err := runLeaf(t, fake,
		"resources", "vaultwarden", "--service", "db", "--memory", "768m", "--pids", "400", "--json")
	require.NoError(t, err)

	req := fake.reconfigureReq
	assert.Equal(t, "vaultwarden", req.AppID)
	assert.Equal(t, "db", req.Service)
	require.NotNil(t, req.Memory, "a supplied --memory must map to a non-nil pointer")
	assert.Equal(t, "768m", *req.Memory)
	assert.Nil(t, req.CPUs, "an omitted --cpus must stay nil (leave unchanged)")
	require.NotNil(t, req.PIDs, "a supplied --pids must map to a non-nil pointer")
	assert.Equal(t, 400, *req.PIDs)
}

func TestResources_StackPathMapsOntoRequest(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "app"}}
	_, _, err := runLeaf(t, fake,
		"resources", "vaultwarden", "--memory", "1g", "--stack-path", "/home/test/docker/vaultwarden", "--json")
	require.NoError(t, err)
	assert.Equal(t, "/home/test/docker/vaultwarden", fake.reconfigureReq.StackPath,
		"--stack-path must map onto ReconfigureRequest.StackPath")
}

// --- Plain-text finish screen: the human-readable reconfigure output
// covers both the running headline and the needs-attention headline, the
// applied-limits block, the backup path, and the status message.

func TestResources_Plain_FinishScreen_RunningHeadline(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		reconfigureResult: &types.ReconfigureResult{
			AppID:      "vaultwarden",
			Service:    "server",
			Memory:     "1g",
			CPUs:       "2.0",
			PIDs:       300,
			BackupPath: "/home/test/docker/vaultwarden/.wdm-backups/123-reconfigure",
			Status:     &types.AppStatus{AppID: "vaultwarden", State: "running", Message: "all healthy"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--memory", "1g", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "vaultwarden service server was reconfigured and is running.")
	assert.Contains(t, stdout, "Applied limits:")
	assert.Contains(t, stdout, "memory: 1g")
	assert.Contains(t, stdout, "cpus:   2.0")
	assert.Contains(t, stdout, "pids:   300")
	assert.Contains(t, stdout, "Config backup: /home/test/docker/vaultwarden/.wdm-backups/123-reconfigure")
	assert.Contains(t, stdout, "Status: running")
	assert.Contains(t, stdout, "all healthy")
}

func TestResources_Plain_FinishScreen_NeedsAttentionHeadline(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		reconfigureResult: &types.ReconfigureResult{
			AppID:   "vaultwarden",
			Service: "server",
			Memory:  "1g",
			Status:  &types.AppStatus{AppID: "vaultwarden", State: "degraded", NeedsAttention: true},
		},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--memory", "1g", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "see the status below for services that need attention.")
	assert.Contains(t, stdout, "Status: degraded")
	assert.NotContains(t, stdout, "and is running.")
}

func TestResources_Plain_FinishScreen_OmitsBackupAndStatusWhenAbsent(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		reconfigureResult: &types.ReconfigureResult{AppID: "vaultwarden", Service: "server", Memory: "1g"},
	}

	stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden", "--memory", "1g", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "was reconfigured and is running.")
	assert.NotContains(t, stdout, "Config backup:", "an empty backup path is omitted")
	assert.NotContains(t, stdout, "Status:", "an absent status block is omitted")
}

// --- Plain-text read-only view: covers the empty-services line and the
// dash-rendering of absent band values.

func TestResources_Plain_View_RendersDashesAndEmptyServices(t *testing.T) {
	t.Parallel()

	t.Run("dashes_for_absent_values", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{
			resourceSettings: &types.ResourceSettings{
				AppID: "vaultwarden",
				Services: []types.ResourceServiceSettings{
					{Service: "server", Adjustable: false},
				},
			},
		}

		stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden")
		require.NoError(t, err)
		assert.Contains(t, stdout, "Service: server (not adjustable)")
		assert.Contains(t, stdout, "current=- allowed min=- recommended=- max=-",
			"absent string band values render as dashes")
	})

	t.Run("empty_services_line", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{
			resourceSettings: &types.ResourceSettings{AppID: "vaultwarden"},
		}

		stdout, _, err := runLeaf(t, fake, "resources", "vaultwarden")
		require.NoError(t, err)
		assert.Contains(t, stdout, "this app declares no adjustable resource limits")
	})
}

// --- Error path: a typed engine error propagates and stdout stays empty
// on both the read-only and reconfigure paths.

func TestResources_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "app is not installed", "run wdm apps list")

	cases := []struct {
		name string
		args []string
	}{
		{"view", []string{"resources", "ghost", "--json"}},
		{"reconfigure", []string{"resources", "ghost", "--memory", "1g", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)
			require.Error(t, err)
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// --- Engine-factory failure: a newEngine that errors propagates before
// any ResourceSettings/Reconfigure call and writes nothing to stdout.

func TestResources_EngineFactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine: lock held by another wdm process")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"resources", "vaultwarden"})
	root.SetContext(context.Background())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "the leaf must return the engine-factory error unchanged")
	assert.Empty(t, outBuf.String(), "no output may be written when the engine cannot be constructed")
}

// --- Writer faults: the two finish/view renderers wrap a failing writer
// with their own context rather than dropping it.

func TestWriteResourceSettings_WriteErrorWrapped(t *testing.T) {
	t.Parallel()

	w := &errorWriter{err: errors.New("disk full")}
	err := writeResourceSettings(w, &types.ResourceSettings{
		AppID:    "vaultwarden",
		Services: []types.ResourceServiceSettings{{Service: "server", Adjustable: true}},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "resources: writing output")
	assert.ErrorContains(t, err, "disk full")
}

func TestWriteReconfigureFinish_WriteErrorWrapped(t *testing.T) {
	t.Parallel()

	w := &errorWriter{err: errors.New("disk full")}
	err := writeReconfigureFinish(w, &types.ReconfigureResult{
		AppID:   "vaultwarden",
		Service: "server",
		Memory:  "1g",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "resources: writing finish screen")
	assert.ErrorContains(t, err, "disk full")
}

// --- ExactArgs(1) refusal: zero or two positional args fail before the
// engine factory runs.

func TestResources_ExactArgs_RefusesWithoutConstructingEngine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"zero args", []string{"resources"}},
		{"two args", []string{"resources", "a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an arg-count refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(context.Background())

			err := root.Execute()
			require.Error(t, err)
			assert.Empty(t, outBuf.String())
		})
	}
}
