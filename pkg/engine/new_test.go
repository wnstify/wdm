package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// testTmpDirs returns three fresh tmpdirs under t.TempDir for state,
// data, and stack base. Centralized so every test in this file gets
// isolated paths without explicit per-test setup boilerplate.
func testTmpDirs(t *testing.T) (state, data, stackBase string) {
	t.Helper()
	tmp := t.TempDir()
	state = filepath.Join(tmp, "state")
	data = filepath.Join(tmp, "data")
	stackBase = filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	return state, data, stackBase
}

// missingConfigPath returns an absolute path to a non-existent file
// inside a fresh t.TempDir. Used to exercise the "missing
// config.toml → PRD §34 defaults" path through the bridge — every
// happy-path test uses this so the test suite never depends on the
// caller's real ~/.config/wdm/config.toml. The directory is pinned to
// 0o700 because UpdateSettings writes config.toml in secret mode,
// whose parent hardening refuses group- or world-writable directories:
// on systems whose temp dirs inherit group-write bits from the umask,
// an unpinned t.TempDir arrives at 0o775 and the valid-payload
// UpdateSettings probes would refuse with permission denied.
func missingConfigPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	return filepath.Join(dir, "nonexistent.toml")
}

// TestNew_DefaultsWhenConfigMissing verifies the bridge's happy path:
// a missing config.toml is non-fatal (PRD §34 defaults apply
// silently), engine.New returns a usable Engine, and Close is
// idempotent across repeat calls.
func TestNew_DefaultsWhenConfigMissing(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
	)
	require.NoError(t, err)
	require.NotNil(t, eng)
	require.NoError(t, eng.Close())
	require.NoError(t, eng.Close(), "Close must be idempotent")
}

// TestNew_AppliesStackBaseDir verifies WithStackBaseDir is translated
// through the bridge into core.WithStackBaseDir: scanning the
// supplied empty directory returns an empty AppInfo slice rather than
// the engine's default ~/docker scan. This is the strongest available
// integration check for option translation in — the option
// has to reach all the way through the bridge and into the scanner
// for List to see the override.
func TestNew_AppliesStackBaseDir(t *testing.T) {
	t.Parallel()
	state, data, stackBase := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithStackBaseDir(stackBase),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	apps, err := eng.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, apps, "empty stack base must return zero AppInfo")
}

// TestNew_WithLogger exercises the WithLogger setter and the bridge's
// translation to core.WithLogger. Verification is necessarily
// indirect — pkg/engine.Engine exposes no logger accessor — but
// successful construction with an explicit logger walks the bridge's
// `if cfg.logger != nil { coreOpts = append(...) }` branch.
func TestNew_WithLogger(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithLogger(logger),
	)
	require.NoError(t, err)
	require.NoError(t, eng.Close())
}

// TestNew_WithFallbackLogWriter exercises the WithFallbackLogWriter
// setter and the bridge's translation to core.WithFallbackLogWriter.
// With no WithLogger, core opens the PRD §24 file sink under the temp
// state dir, so the fallback writer is not consulted on the happy path;
// the test confirms the option is accepted and the resulting latest.log
// is created with owner-only mode.
func TestNew_WithFallbackLogWriter(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithFallbackLogWriter(io.Discard),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	info, err := os.Stat(filepath.Join(state, "logs", "latest.log"))
	require.NoError(t, err, "default sink must open latest.log under the state dir")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "latest.log must be owner-only")
}

// TestNew_WithCatalog verifies WithCatalog forwards through the
// bridge into core.WithCatalog. does not read the catalog
// from disk, so the test confirms only that construction succeeds
// with a non-default fs.FS supplied — the bridge must accept the option and
// forward it to core.WithCatalog without dropping the value.
func TestNew_WithCatalog(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	var catalogFS fs.FS = fstest.MapFS{
		"catalog.yaml": &fstest.MapFile{Data: []byte("# fixture")},
	}
	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithCatalog(catalogFS),
	)
	require.NoError(t, err)
	require.NoError(t, eng.Close())
}

// TestReconfigure_RoutesThroughFacadeToCore proves the Reconfigure
// method is wired through the bridge to core: with no managed stack on
// disk, the call reaches core's managed-stack resolution and returns the
// typed usage-validation refusal (exit 2), confirming the facade routes
// the request rather than dropping it. It carries a non-nil confirmer so
// the refusal is the not-installed path, not the nil-confirmer one.
func TestReconfigure_RoutesThroughFacadeToCore(t *testing.T) {
	t.Parallel()
	state, data, stackBase := testTmpDirs(t)

	var catalogFS fs.FS = fstest.MapFS{
		"stable/catalog.yaml": &fstest.MapFile{Data: []byte("schema_version: 1\nchannel: stable\ngenerated_at: 2026-05-29T00:00:00Z\napps: []\n")},
	}
	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithStackBaseDir(stackBase),
		engine.WithCatalog(catalogFS),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	memory := "1g"
	res, err := eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   "ghost",
		Service: "app",
		Memory:  &memory,
	}, nil, stubConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)

	var typed *types.Error
	require.True(t, errors.As(err, &typed), "the refusal must be a typed *types.Error")
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code,
		"an uninstalled app must refuse with usage-validation through the facade")
}

// TestResourceSettings_RoutesThroughFacadeToCore proves the read-only
// ResourceSettings method routes through the bridge to core: an
// uninstalled app refuses with the typed usage-validation code.
func TestResourceSettings_RoutesThroughFacadeToCore(t *testing.T) {
	t.Parallel()
	state, data, stackBase := testTmpDirs(t)

	var catalogFS fs.FS = fstest.MapFS{
		"stable/catalog.yaml": &fstest.MapFile{Data: []byte("schema_version: 1\nchannel: stable\ngenerated_at: 2026-05-29T00:00:00Z\napps: []\n")},
	}
	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithStackBaseDir(stackBase),
		engine.WithCatalog(catalogFS),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	res, err := eng.ResourceSettings(t.Context(), "ghost")
	require.Error(t, err)
	assert.Nil(t, res)

	var typed *types.Error
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
}

// stubConfirmer authorizes every prompt; used where a non-nil confirmer
// must be supplied but no prompt is reached.
type stubConfirmer struct{}

func (stubConfirmer) Confirm(context.Context, types.Confirmation) (bool, error) { return true, nil }

// TestNew_MalformedConfigWrapsErrConfigInvalid is the load-bearing
// error-path test: a schema-invalid config.toml must propagate
// types.ErrConfigInvalid through both the engine.New: and core.New:
// wrap layers so cmd/wdm can route to PRD §27 exit code 2 via
// errors.Is. The PRD §14 self-update smoke check depends on this
// chain staying intact end-to-end.
func TestNew_MalformedConfigWrapsErrConfigInvalid(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// schema_version = 999 fails the const constraint in
	// config/schema.json (the schema pins schema_version = 1).
	require.NoError(t, os.WriteFile(cfgPath, []byte("schema_version = 999\n"), 0o644))

	eng, err := engine.New(
		engine.WithConfigPath(cfgPath),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
	)
	require.Error(t, err)
	assert.Nil(t, eng)
	assert.True(t, errors.Is(err, types.ErrConfigInvalid),
		"engine.New must propagate types.ErrConfigInvalid through both wrap layers")
	assert.Contains(t, err.Error(), "engine.New:",
		"outer wrap layer must preserve engine.New: prefix")
}

// TestNew_RelativeStackBaseDirRejected verifies the absolute-path
// validation in core.resolveDirs propagates through the bridge with
// the engine.New: outer wrap intact. The error originates in
// core.resolveDirs (not in the Option setter, which is a pure
// assignment), so this test exercises the "core.New returned an
// error" branch of the bridge specifically.
func TestNew_RelativeStackBaseDirRejected(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithStackBaseDir("relative/path"),
	)
	require.Error(t, err)
	assert.Nil(t, eng)
	assert.Contains(t, err.Error(), "WithStackBaseDir requires absolute path",
		"core.resolveDirs absolute-path error must reach the caller")
	assert.Contains(t, err.Error(), "engine.New:",
		"outer wrap layer must preserve engine.New: prefix")
}

// TestNew_WithVersionRejectsEmpty is the load-bearing API-validation
// test for the [engine.WithVersion] option: an
// empty version string must fail at engine.New so a typo in the
// release pipeline cannot produce a runtime.lock with an empty
// wdm_version downstream. The engine-side check fires before the
// bridge forwards to core, so the error chain carries the engine.New:
// outer wrap intact.
func TestNew_WithVersionRejectsEmpty(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithVersion(""),
	)
	require.Error(t, err)
	assert.Nil(t, eng)
	assert.Contains(t, err.Error(), "WithVersion requires non-empty version string",
		"the option-setter rejection message must reach the caller")
	assert.Contains(t, err.Error(), "engine.New:",
		"outer wrap layer must preserve engine.New: prefix")
}

// TestNew_WithVersionPropagatesToRuntimeLock is the end-to-end
// criterion 396 propagation test: engine.WithVersion("X") at
// construction time must flow through the bridge (engine.New →
// core.WithVersion → core.Engine.version) and end up in the
// wdm_version field of the on-disk runtime.lock JSON when a
// state-changing method acquires the lock. UpdateSettings uses a valid
// types.Settings payload and succeeds; the runtime.lock JSON written during
// the dance is still the load-bearing assertion. Reading the file back and asserting on
// the JSON content is the only honest end-to-end check available
// without exposing private engine fields.
func TestNew_WithVersionPropagatesToRuntimeLock(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
		engine.WithVersion("0.1.0"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	err = eng.UpdateSettings(t.Context(), validEngineSettings())
	require.NoError(t, err,
		"UpdateSettings on a healthy engine must succeed after the lock dance")

	raw, err := os.ReadFile(filepath.Join(state, "runtime.lock"))
	require.NoError(t, err, "UpdateSettings must have created runtime.lock under stateDir")

	var info struct {
		WDMVersion string `json:"wdm_version"`
		Command    string `json:"command"`
	}
	require.NoError(t, json.Unmarshal(raw, &info))
	assert.Equal(t, "0.1.0", info.WDMVersion,
		"WithVersion must propagate through the bridge to runtime.lock.wdm_version")
	assert.Equal(t, "update-settings", info.Command,
		"UpdateSettings's runtime.lock Command field must read \"update-settings\"")
}

// TestNew_DefaultVersionIsDev verifies the no-WithVersion default
// path: when cmd/wdm (or a test) does not supply WithVersion at
// all, the underlying core.New defaults the version to "dev" — the
// same default cmd/wdm carries for unstamped local Makefile
// builds. Verified by reading the runtime.lock JSON after a
// state-changing UpdateSettings call (live config-write since
// payload is a VALID types.Settings so the call succeeds.
func TestNew_DefaultVersionIsDev(t *testing.T) {
	t.Parallel()
	state, data, _ := testTmpDirs(t)

	eng, err := engine.New(
		engine.WithConfigPath(missingConfigPath(t)),
		engine.WithStateDir(state),
		engine.WithDataDir(data),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	err = eng.UpdateSettings(t.Context(), validEngineSettings())
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(state, "runtime.lock"))
	require.NoError(t, err)

	var info struct {
		WDMVersion string `json:"wdm_version"`
	}
	require.NoError(t, json.Unmarshal(raw, &info))
	assert.Equal(t, "dev", info.WDMVersion,
		"no WithVersion supplied → core.New must default wdm_version to \"dev\"")
}

// validEngineSettings returns a types.Settings that passes the
// engine's UpdateSettings validation matrix so the runtime.lock
// propagation probes drive the live success path. Timezone is empty
// (the legal detect-at-install sentinel) so the probes never depend on
// host tzdata.
func validEngineSettings() types.Settings {
	return types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "~/docker",
		Timezone:              "",
		DefaultDockerNetwork:  "wdm_default",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "daily-on-launch",
	}
}
