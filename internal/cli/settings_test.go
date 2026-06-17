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

// These tests pin the `settings set <key> <value>` leaf (PRD §34;
// NewRootCmd through runLeaf with the recording fakeEngine — and lock the
// thin-caller contract: the leaf loads settings via engine.Settings ONCE,
// replaces exactly the named field, persists the merged struct via
// engine.UpdateSettings, and renders that same merged struct (plain
// confirmation or a single wdm.v1 envelope). The leaf does NOT re-read
// after the write: engine.Settings returns the engine's in-memory cache,
// which UpdateSettings does not refresh, so a re-read would render the
// PRE-edit value. The engine owns every value validation; the CLI's only
// own check is the key-name mapping.

// recordingSettingsEngine embeds the shared fakeEngine and overrides only
// the two settings methods so a test can observe the merged struct handed
// to UpdateSettings — the shared fakeEngine.UpdateSettings discards its
// argument, and the shared double lives in fake_engine_test.go. Settings returns the configured baseline and counts its
// calls (the leaf must read exactly once); UpdateSettings records the
// struct it received. The non-overridden methods inherit fakeEngine.
type recordingSettingsEngine struct {
	*fakeEngine

	// settingsReturn is the baseline the single Settings call returns (the
	// pre-edit on-disk settings the leaf loads before merging the edit).
	settingsReturn *types.Settings
	settingsCalls  int

	// updated records the struct passed to UpdateSettings (the merge the
	// leaf built). updateCalled distinguishes "called with the zero struct"
	// from "never called".
	updated      types.Settings
	updateCalled bool
}

// Settings always succeeds with the configured baseline so the
// validation-refusal test exercises the UpdateSettings error path (the
// engine's domain) rather than failing earlier at the load. It counts its
// calls so a test can prove the leaf reads exactly once.
func (e *recordingSettingsEngine) Settings(_ context.Context) (*types.Settings, error) {
	e.settingsCalls++
	return e.settingsReturn, nil
}

func (e *recordingSettingsEngine) UpdateSettings(_ context.Context, s types.Settings) error {
	e.updateCalled = true
	e.updated = s
	if e.err != nil {
		return e.err
	}
	return nil
}

// runSettingsLeaf drives one `settings set` invocation through NewRootCmd
// with the recording engine wired as the lazy factory return, mirroring
// runLeaf. It returns captured stdout, stderr, and the Execute error.
func runSettingsLeaf(t *testing.T, eng *recordingSettingsEngine, args ...string) (stdout, stderr string, err error) {
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

// baselineSettings returns a complete, valid settings struct so a test can
// assert which fields the leaf left untouched after editing one.
func baselineSettings() *types.Settings {
	return &types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "/home/test/docker",
		Timezone:              "Europe/Bratislava",
		DefaultDockerNetwork:  "wdm",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "manual",
	}
}

// TestSettingsSet_HappyPath_MergesAndPersistsPerKey proves, for every
// settable key, that the leaf loads the baseline, replaces exactly the
// named field, and hands the merged struct to UpdateSettings with every
// other field carried over unchanged. The plain-mode stdout confirmation
// echoes the persisted value.
func TestSettingsSet_HappyPath_MergesAndPersistsPerKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		key      string
		value    string
		wantLine string
		// mutate applies the same edit to a copy of the baseline so the test
		// asserts the exact merged struct UpdateSettings must receive.
		mutate func(s *types.Settings)
	}{
		{
			name:     "base_stack_path",
			key:      "base_stack_path",
			value:    "/srv/stacks",
			wantLine: "base_stack_path = /srv/stacks\n",
			mutate:   func(s *types.Settings) { s.BaseStackPath = "/srv/stacks" },
		},
		{
			name:     "timezone",
			key:      "timezone",
			value:    "UTC",
			wantLine: "timezone = UTC\n",
			mutate:   func(s *types.Settings) { s.Timezone = "UTC" },
		},
		{
			name:     "timezone_empty_is_legal",
			key:      "timezone",
			value:    "",
			wantLine: "timezone = \n",
			mutate:   func(s *types.Settings) { s.Timezone = "" },
		},
		{
			name:     "default_docker_network",
			key:      "default_docker_network",
			value:    "edge",
			wantLine: "default_docker_network = edge\n",
			mutate:   func(s *types.Settings) { s.DefaultDockerNetwork = "edge" },
		},
		{
			name:     "catalog_channel",
			key:      "catalog_channel",
			value:    "stable",
			wantLine: "catalog_channel = stable\n",
			mutate:   func(s *types.Settings) { s.CatalogChannel = "stable" },
		},
		{
			name:     "update_check_preference",
			key:      "update_check_preference",
			value:    "disabled",
			wantLine: "update_check_preference = disabled\n",
			mutate:   func(s *types.Settings) { s.UpdateCheckPreference = "disabled" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := &recordingSettingsEngine{
				fakeEngine:     &fakeEngine{},
				settingsReturn: baselineSettings(),
			}

			stdout, _, err := runSettingsLeaf(t, eng, "settings", "set", tc.key, tc.value)
			require.NoError(t, err)

			// Settings is read exactly ONCE: the leaf loads the baseline,
			// merges the edit, and renders that merge — it must NOT re-read the
			// engine's (unrefreshed) cache after the write. This is the
			// regression guard against the stale-re-read bug.
			require.Equal(t, 1, eng.settingsCalls,
				"Settings must be read exactly once (no stale post-write re-read)")

			// The merged struct reached the engine with exactly the one field
			// changed and every other field carried over unchanged.
			require.True(t, eng.updateCalled, "UpdateSettings must be called")
			want := baselineSettings()
			tc.mutate(want)
			assert.Equal(t, *want, eng.updated,
				"UpdateSettings must receive the baseline with exactly the one field replaced")

			// Plain mode prints the key and the merged value (byte-faithful to
			// what UpdateSettings persisted).
			assert.Equal(t, tc.wantLine, stdout, "plain confirmation must name the key and merged value")
		})
	}
}

// TestSettingsSet_JSON_EmitsSingleSettingsEnvelope pins the --json
// contract: exactly one wdm.v1 envelope on stdout wrapping the MERGED
// settings (the baseline with the one field replaced, byte-faithful to
// what UpdateSettings persisted), with the Settings object as
// envelope.data directly (no nesting key) and raw-stdout byte discipline.
func TestSettingsSet_JSON_EmitsSingleSettingsEnvelope(t *testing.T) {
	t.Parallel()

	eng := &recordingSettingsEngine{
		fakeEngine:     &fakeEngine{},
		settingsReturn: baselineSettings(),
	}

	stdout, _, err := runSettingsLeaf(t, eng, "settings", "set", "update_check_preference", "disabled", "--json")
	require.NoError(t, err)

	// The envelope must reflect the merge the leaf built, not a second
	// (stale) cache read: Settings is consulted exactly once.
	require.Equal(t, 1, eng.settingsCalls,
		"Settings must be read exactly once (no stale post-write re-read)")

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "settings set --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	// Settings wraps DIRECTLY as envelope data (mirroring status), so its
	// snake_case keys are at the top of data, not nested under a key.
	assert.Equal(t, "disabled", data["update_check_preference"],
		"the envelope must carry the merged edited value")
	assert.Equal(t, "stable", data["catalog_channel"],
		"untouched fields must round-trip into the envelope")
	assert.NotContains(t, lines[0], `"settings":`,
		"Settings must be the envelope.data object directly, not nested under a settings key")
}

// TestSettingsSet_RendersMergedStruct_NotStaleCache is the direct
// regression guard for the stale-re-read bug. engine.Settings returns the
// engine's in-memory cache (loaded once at New), which UpdateSettings does
// NOT refresh — so a leaf that re-read after the write would render the
// PRE-edit value. The fake here is configured so a hypothetical re-read
// would yield the unedited baseline; the leaf must instead render the
// MERGED edit it built and persisted (the input value, byte-faithful to
// config.toml). Proven in both plain and --json output, with Settings
// consulted exactly once.
func TestSettingsSet_RendersMergedStruct_NotStaleCache(t *testing.T) {
	t.Parallel()

	t.Run("plain", func(t *testing.T) {
		t.Parallel()

		eng := &recordingSettingsEngine{
			fakeEngine:     &fakeEngine{},
			settingsReturn: baselineSettings(), // a re-read would return this UNEDITED struct
		}

		stdout, _, err := runSettingsLeaf(t, eng, "settings", "set", "base_stack_path", "/srv/stacks")
		require.NoError(t, err)

		require.Equal(t, 1, eng.settingsCalls,
			"Settings must be read exactly once (no stale post-write re-read)")
		// The baseline's pre-edit base_stack_path is /home/test/docker; the
		// edit is /srv/stacks. The confirmation must show the EDIT.
		assert.Equal(t, "base_stack_path = /srv/stacks\n", stdout,
			"plain confirmation must render the merged edit, not the stale cache value")
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		eng := &recordingSettingsEngine{
			fakeEngine:     &fakeEngine{},
			settingsReturn: baselineSettings(), // a re-read would return this UNEDITED struct
		}

		stdout, _, err := runSettingsLeaf(t, eng, "settings", "set", "base_stack_path", "/srv/stacks", "--json")
		require.NoError(t, err)

		require.Equal(t, 1, eng.settingsCalls,
			"Settings must be read exactly once (no stale post-write re-read)")
		data := decodeEnvelopeData(t, nonEmptyLines(stdout)[0])
		assert.Equal(t, "/srv/stacks", data["base_stack_path"],
			"the envelope must carry the merged edit, not the stale cache value")
		assert.Equal(t, "stable", data["catalog_channel"],
			"untouched fields must carry over from the loaded baseline")
	})
}

// TestSettingsSet_KeyRefusals_BeforeEngineConstruction pins that
// schema_version and an unknown key refuse with a usage error BEFORE the
// engine factory runs (mirroring install --set): the factory returns a
// sentinel error, and if the key check ran after construction Execute
// would surface that sentinel instead of the key refusal. Stdout stays
// empty (no partial envelope), even under --json.
func TestSettingsSet_KeyRefusals_BeforeEngineConstruction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
		// wantContains is a fragment unique to this refusal's message.
		wantContains string
	}{
		{name: "schema_version", key: "schema_version", wantContains: "schema_version is managed by wdm"},
		{name: "unknown_key", key: "bogus_key", wantContains: "unknown setting"},
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
			root.SetArgs([]string{"settings", "set", tc.key, "value", "--json"})
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "a non-settable key must refuse")
			assert.NotErrorIs(t, err, factoryErr,
				"a key refusal must happen before the engine factory runs")
			assert.ErrorContains(t, err, tc.wantContains, "the surfaced error must be the key refusal")
			assert.Contains(t, err.Error(), "settable keys:",
				"the refusal must list the settable keys to guide the user")
			assert.Empty(t, outBuf.String(), "no envelope may be written on a key refusal")
		})
	}
}

// TestSettingsSet_EngineValidationRefusal_Propagates pins that a typed
// usage-validation error from engine.UpdateSettings (an invalid VALUE, the
// engine's domain) propagates out of Execute unchanged, with empty stdout.
// The error is errors.As-reachable as a *types.Error so cmd/wdm's
// exitCodeFor maps it to exit 2.
func TestSettingsSet_EngineValidationRefusal_Propagates(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(
		types.ErrCodeUsageValidation,
		"update_check_preference is invalid",
		"set update_check_preference to one of: manual, daily-on-launch, disabled",
	)

	eng := &recordingSettingsEngine{
		fakeEngine:     &fakeEngine{err: engineErr},
		settingsReturn: baselineSettings(),
	}

	stdout, _, err := runSettingsLeaf(t, eng,
		"settings", "set", "update_check_preference", "weekly", "--json")

	require.Error(t, err, "an invalid value must surface the engine's typed error")
	assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")

	var typed *types.Error
	require.ErrorAs(t, err, &typed, "the error must be a *types.Error for cmd/wdm exit mapping")
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code, "an invalid value maps to exit 2")

	// The leaf reached UpdateSettings (the merge succeeded) and surfaced its
	// rejection: Settings was read once, UpdateSettings was called.
	require.Equal(t, 1, eng.settingsCalls, "Settings must be read once before the UpdateSettings call")
	assert.True(t, eng.updateCalled, "the leaf must reach UpdateSettings before surfacing its error")

	assert.Empty(t, stdout, "no output may be written on the engine error path")
}

// TestSettingsSet_FactoryError_Propagates pins that a failed engine
// factory surfaces out of Execute with empty stdout — the leaf builds the
// engine inside RunE, after the key check, so a construction failure on a
// valid key is the next thing it can hit.
func TestSettingsSet_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"settings", "set", "timezone", "UTC"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no output may be written when the engine cannot be built")
}

// TestSettingsSet_ArgCount_RefusedByCobra pins the ExactArgs(2) contract:
// zero, one, and three positional args all refuse via cobra before RunE
// runs (so the engine is never constructed). cobra's standard arg-count
// message is surfaced; the leaf writes nothing to stdout.
func TestSettingsSet_ArgCount_RefusedByCobra(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{name: "zero_args", args: []string{"settings", "set"}},
		{name: "one_arg", args: []string{"settings", "set", "timezone"}},
		{name: "three_args", args: []string{"settings", "set", "timezone", "UTC", "extra"}},
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
				"an arg-count refusal happens in cobra, before RunE constructs the engine")
			assert.Empty(t, outBuf.String(), "no output may be written on an arg-count refusal")
		})
	}
}

// TestSettingsSet_Help_DocumentsSettableKeys pins the doc-truth surface:
// the help text must list the settable keys and state that schema_version
// is not settable. Help output is the primary CLI documentation
// (golang-cli), so a regression that drops or misstates these is caught
// here. --help exits 0 and never constructs the engine.
func TestSettingsSet_Help_DocumentsSettableKeys(t *testing.T) {
	t.Parallel()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, errors.New("factory must not be consulted for --help")
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"settings", "set", "--help"})
	root.SetContext(t.Context())

	require.NoError(t, root.Execute())

	stdout := outBuf.String()
	for _, key := range []string{
		"base_stack_path", "timezone", "default_docker_network",
		"catalog_channel", "update_check_preference",
	} {
		assert.Contains(t, stdout, key, "help must document the settable key %q", key)
	}
	assert.Contains(t, stdout, "schema_version is managed by wdm",
		"help must state schema_version is not settable")
}
