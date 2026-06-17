package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// recordingValidateEngine embeds the shared fakeEngine and overrides only
// ValidateConfig to record the app-id argument the leaf forwards. The
// shared double does not record it (validate has no per-request struct to
// stash, only a bare appID), so this thin wrapper proves the positional
// arg reaches the engine without touching fake_engine_test.go.
type recordingValidateEngine struct {
	*fakeEngine
	gotAppID string
}

func (r *recordingValidateEngine) ValidateConfig(ctx context.Context, appID string) (*types.ValidationResult, error) {
	r.gotAppID = appID
	return r.fakeEngine.ValidateConfig(ctx, appID)
}

// TestAppsValidate_Valid_RendersBlockAndExitsZero pins the happy path: a
// Valid:true result renders the scannable plain block (app + valid=yes
// header, project, file) on stdout and Execute returns nil (exit 0).
func TestAppsValidate_Valid_RendersBlockAndExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		validationResult: &types.ValidationResult{
			AppID:          "vaultwarden",
			ComposeProject: "wdm-vaultwarden",
			ComposeFile:    "/home/u/docker/vaultwarden/docker-compose.yml",
			Valid:          true,
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "validate", "vaultwarden")
	require.NoError(t, err, "a valid config is a successful read and must exit 0")

	assert.Contains(t, stdout, "vaultwarden\tvalid=yes", "the header must carry the app and the yes verdict")
	assert.Contains(t, stdout, "wdm-vaultwarden", "the block must carry the Compose project")
	assert.Contains(t, stdout, "/home/u/docker/vaultwarden/docker-compose.yml", "the block must carry the Compose file path")
	// A valid result has no detail section.
	assert.NotContains(t, stdout, "Detail:", "a valid result must not render a detail section")
}

// TestAppsValidate_Invalid_RendersDetailAndExitsZero is THE pin: an
// invalid-but-readable Compose file is a Valid:false SUCCESS payload, not
// an error. The plain block carries the verdict and the detail, and
// Execute returns nil (exit 0), exactly like apps status on
// needs_attention. The leaf adds no exit-code logic of its own.
func TestAppsValidate_Invalid_RendersDetailAndExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		validationResult: &types.ValidationResult{
			AppID:          "vaultwarden",
			ComposeProject: "wdm-vaultwarden",
			ComposeFile:    "/home/u/docker/vaultwarden/docker-compose.yml",
			Valid:          false,
			Detail:         "services.server: invalid type for port mapping",
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "validate", "vaultwarden")
	assert.NoError(t, err, "an invalid config is a successful read (Valid:false) and must not error")

	assert.Contains(t, stdout, "vaultwarden\tvalid=no", "the header must carry the no verdict")
	assert.Contains(t, stdout, "Detail:", "an invalid result must render the detail section")
	assert.Contains(t, stdout, "services.server: invalid type for port mapping", "the scrubbed detail must reach stdout")
}

// TestAppsValidate_JSON_WrapsResultDirectly pins the --json contract: a
// single wdm.v1 envelope on stdout wrapping the ValidationResult DIRECTLY
// as data (:347, no nesting under a "result" or "validation"
// key), with raw-stdout byte-equality discipline (exactly the envelope
// line, nothing else).
func TestAppsValidate_JSON_WrapsResultDirectly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		validationResult: &types.ValidationResult{
			AppID:          "vaultwarden",
			ComposeProject: "wdm-vaultwarden",
			ComposeFile:    "/home/u/docker/vaultwarden/docker-compose.yml",
			Valid:          true,
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "validate", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "validate --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	// The ValidationResult fields sit at the top of data — app_id and valid
	// are direct keys, NOT nested under a wrapper. The ValidationResult IS the
	// envelope.data object.
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, true, data["valid"])
	assert.NotContains(t, data, "validation", "ValidationResult must be data directly, not nested under a validation key")
	assert.NotContains(t, data, "result", "ValidationResult must be data directly, not nested under a result key")
}

// TestAppsValidate_JSON_InvalidExitsZero pins that the invalid-result
// envelope path also exits 0: --json on a Valid:false result emits the
// envelope and returns nil, the JSON-mode mirror of the plain-mode
// pin.
func TestAppsValidate_JSON_InvalidExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		validationResult: &types.ValidationResult{
			AppID:  "vaultwarden",
			Valid:  false,
			Detail: "services.server: invalid type for port mapping",
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "validate", "vaultwarden", "--json")
	assert.NoError(t, err, "an invalid config under --json is still a successful read and must exit 0")

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "validate --json must emit exactly one envelope even when invalid")
	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, false, data["valid"], "the envelope must carry the false verdict")
	assert.Equal(t, "services.server: invalid type for port mapping", data["detail"], "the scrubbed detail must ride the envelope")
}

// TestAppsValidate_ErrorPath_EmptyStdout pins the typed-error contract in
// both output modes: a typed engine error propagates unchanged out of
// Execute and stdout stays empty (no partial block, no envelope).
func TestAppsValidate_ErrorPath_EmptyStdout(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "stack is not managed", "run wdm apps list")

	cases := []struct {
		name string
		args []string
	}{
		{"plain", []string{"apps", "validate", "vaultwarden"}},
		{"json", []string{"apps", "validate", "vaultwarden", "--json"}},
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

// TestAppsValidate_AppIDReachesEngine proves the positional arg is
// forwarded verbatim to engine.ValidateConfig, using a wrapper that
// records the received app-id (the shared fakeEngine does not).
func TestAppsValidate_AppIDReachesEngine(t *testing.T) {
	t.Parallel()

	rec := &recordingValidateEngine{
		fakeEngine: &fakeEngine{
			validationResult: &types.ValidationResult{AppID: "n8n", Valid: true},
		},
	}

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return rec, nil
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"apps", "validate", "n8n"})
	root.SetContext(t.Context())

	require.NoError(t, root.Execute())
	assert.Equal(t, "n8n", rec.gotAppID, "the positional app-id must reach engine.ValidateConfig verbatim")
}

// TestAppsValidate_ExactArgs pins the ExactArgs(1) contract: zero or two
// positional args refuse with cobra's standard arg-count error BEFORE the
// engine factory runs. A sentinel factory proves no engine is constructed:
// if arg validation ran after construction, Execute would surface the
// sentinel instead of the arg-count error.
func TestAppsValidate_ExactArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"zero_args", []string{"apps", "validate"}},
		{"two_args", []string{"apps", "validate", "vaultwarden", "extra"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			factoryErr := errors.New("engine factory must not be consulted")
			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, factoryErr
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "a wrong arg count must refuse")
			assert.NotErrorIs(t, err, factoryErr,
				"arg validation must refuse before the engine factory runs")
			assert.Empty(t, outBuf.String(), "no output may be written on an arg-count refusal")
		})
	}
}
