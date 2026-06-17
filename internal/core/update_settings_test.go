package core_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// updateSettingsEngine builds a [*core.Engine] whose config.toml path
// is returned so write-through assertions can read the persisted file
// back through the public loader. The config parent is a HOME-backed
// tempdir created at 0o700 (via coreTestTempDir) because
// UpdateSettings writes config.toml at 0o600, and
// state.WriteFileAtomic refuses a group/world-writable parent for any
// 0o600 (secret-mode) write — a /tmp-backed t.TempDir at 1777 would
// fail that guard. The config file itself does NOT exist at
// construction time so core.New takes its PRD §34 defaults path.
func updateSettingsEngine(t *testing.T) (eng *core.Engine, configPath, stateDir string) {
	t.Helper()
	tmp := coreTestTempDir(t)
	stateDir = filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	configPath = filepath.Join(tmp, "config.toml")

	eng, err := core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
		core.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	return eng, configPath, stateDir
}

// validSettings returns a types.Settings that passes every arm of the
// UpdateSettings validation matrix. Tests clone-and-mutate it to build
// single-arm rejection cases. Timezone is empty (the legal
// detect-at-install sentinel) so the happy path never depends on host
// tzdata.
func validSettings() types.Settings {
	return types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "~/docker",
		Timezone:              "",
		DefaultDockerNetwork:  "wdm_default",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "daily-on-launch",
	}
}

// TestUpdateSettings_RejectMatrix drives every binding arm of the
// and asserts each rejection is a typed ErrCodeUsageValidation error
// that leaves config.toml unwritten. The config file does not exist
// before the call, so "not written" is proven by its continued
// absence on disk after the refusal.
func TestUpdateSettings_RejectMatrix(t *testing.T) {
	t.Parallel()

	mutate := func(f func(s *types.Settings)) types.Settings {
		s := validSettings()
		f(&s)
		return s
	}

	tests := []struct {
		name     string
		settings types.Settings
	}{
		{
			name:     "schema version not one",
			settings: mutate(func(s *types.Settings) { s.SchemaVersion = 2 }),
		},
		{
			name:     "schema version zero",
			settings: mutate(func(s *types.Settings) { s.SchemaVersion = 0 }),
		},
		{
			name:     "update check preference unknown",
			settings: mutate(func(s *types.Settings) { s.UpdateCheckPreference = "weekly" }),
		},
		{
			name:     "update check preference empty",
			settings: mutate(func(s *types.Settings) { s.UpdateCheckPreference = "" }),
		},
		{
			name:     "catalog channel not stable",
			settings: mutate(func(s *types.Settings) { s.CatalogChannel = "verified" }),
		},
		{
			name:     "catalog channel empty",
			settings: mutate(func(s *types.Settings) { s.CatalogChannel = "" }),
		},
		{
			name:     "base stack path empty",
			settings: mutate(func(s *types.Settings) { s.BaseStackPath = "" }),
		},
		{
			name:     "base stack path is a reserved system root",
			settings: mutate(func(s *types.Settings) { s.BaseStackPath = "/etc" }),
		},
		{
			name:     "base stack path relative without tilde",
			settings: mutate(func(s *types.Settings) { s.BaseStackPath = "docker" }),
		},
		{
			name:     "default docker network uppercase",
			settings: mutate(func(s *types.Settings) { s.DefaultDockerNetwork = "Wdm_Default" }),
		},
		{
			name:     "default docker network with dot",
			settings: mutate(func(s *types.Settings) { s.DefaultDockerNetwork = "wdm.default" }),
		},
		{
			name:     "default docker network empty",
			settings: mutate(func(s *types.Settings) { s.DefaultDockerNetwork = "" }),
		},
		{
			name:     "default docker network leading digit",
			settings: mutate(func(s *types.Settings) { s.DefaultDockerNetwork = "0net" }),
		},
		{
			name:     "timezone not a valid iana name",
			settings: mutate(func(s *types.Settings) { s.Timezone = "Not/AZone" }),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, configPath, _ := updateSettingsEngine(t)

			err := eng.UpdateSettings(t.Context(), tt.settings)
			require.Error(t, err, "invalid settings must be refused")

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr,
				"refusal must surface as *types.Error for cmd/wdm exit-code mapping")
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code,
				"every matrix rejection must map to usage validation (exit 2)")

			_, statErr := os.Stat(configPath)
			require.True(t, os.IsNotExist(statErr),
				"a rejected UpdateSettings must NOT write config.toml")
		})
	}
}

// TestUpdateSettings_AcceptsValidPayloads proves the accept side of the
// matrix: each individually-valid arm value is persisted. Timezone is
// the one field with two legal shapes (empty sentinel vs a real IANA
// name), and every UpdateCheckPreference enum member is accepted. The
// written file is read back through the public loader so the assertion
// rides the same parse + schema validation production uses.
func TestUpdateSettings_AcceptsValidPayloads(t *testing.T) {
	t.Parallel()

	mutate := func(f func(s *types.Settings)) types.Settings {
		s := validSettings()
		f(&s)
		return s
	}

	tests := []struct {
		name     string
		settings types.Settings
	}{
		{
			name:     "baseline valid settings",
			settings: validSettings(),
		},
		{
			name:     "timezone empty sentinel",
			settings: mutate(func(s *types.Settings) { s.Timezone = "" }),
		},
		{
			name:     "timezone valid iana name",
			settings: mutate(func(s *types.Settings) { s.Timezone = "UTC" }),
		},
		{
			name:     "update check preference manual",
			settings: mutate(func(s *types.Settings) { s.UpdateCheckPreference = "manual" }),
		},
		{
			name:     "update check preference disabled",
			settings: mutate(func(s *types.Settings) { s.UpdateCheckPreference = "disabled" }),
		},
		{
			name:     "base stack path under home with tilde",
			settings: mutate(func(s *types.Settings) { s.BaseStackPath = "~/stacks" }),
		},
		{
			name:     "default docker network with hyphen",
			settings: mutate(func(s *types.Settings) { s.DefaultDockerNetwork = "wdm-net-1" }),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, configPath, _ := updateSettingsEngine(t)

			err := eng.UpdateSettings(t.Context(), tt.settings)
			require.NoError(t, err, "valid settings must be persisted")

			loaded, err := state.LoadConfig(context.Background(), configPath)
			require.NoError(t, err, "the written config.toml must parse and schema-validate")
			assert.Equal(t, tt.settings, *loaded,
				"the loaded settings must round-trip the persisted payload verbatim")
		})
	}
}

// TestUpdateSettings_WritesConfigAtomicallyWithSecretMode proves the
// success path's on-disk contract: config.toml is created at the
// engine's configPath, the public loader round-trips the exact
// Settings, and the file mode is 0o600 (the conservative per-user
// mode UpdateSettings selects).
func TestUpdateSettings_WritesConfigAtomicallyWithSecretMode(t *testing.T) {
	t.Parallel()

	eng, configPath, _ := updateSettingsEngine(t)

	settings := validSettings()
	settings.BaseStackPath = "~/docker-data"
	settings.DefaultDockerNetwork = "wdm-shared"
	settings.UpdateCheckPreference = "manual"
	settings.Timezone = "Europe/Bratislava"

	require.NoError(t, eng.UpdateSettings(t.Context(), settings))

	loaded, err := state.LoadConfig(context.Background(), configPath)
	require.NoError(t, err)
	assert.Equal(t, settings, *loaded,
		"state.LoadConfig must return the exact Settings UpdateSettings wrote")

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"config.toml must be written at 0o600")
}

// TestUpdateSettings_OverwriteReplacesValues proves UpdateSettings
// overwrites an existing config.toml with the new values, while a
// later REJECTED call leaves the previously-written bytes byte-intact
// (fail-closed: a bad save never corrupts a good config).
func TestUpdateSettings_OverwriteReplacesValues(t *testing.T) {
	t.Parallel()

	eng, configPath, _ := updateSettingsEngine(t)

	first := validSettings()
	first.UpdateCheckPreference = "daily-on-launch"
	require.NoError(t, eng.UpdateSettings(t.Context(), first))

	second := validSettings()
	second.UpdateCheckPreference = "disabled"
	second.DefaultDockerNetwork = "wdm-other"
	require.NoError(t, eng.UpdateSettings(t.Context(), second))

	loaded, err := state.LoadConfig(context.Background(), configPath)
	require.NoError(t, err)
	assert.Equal(t, second, *loaded,
		"a second UpdateSettings must replace the first config.toml's values")

	// A rejected save must leave the last-good bytes untouched.
	beforeReject, err := os.ReadFile(configPath)
	require.NoError(t, err)

	bad := validSettings()
	bad.CatalogChannel = "verified"
	err = eng.UpdateSettings(t.Context(), bad)
	require.Error(t, err)

	afterReject, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, beforeReject, afterReject,
		"a rejected UpdateSettings must leave the existing config.toml byte-identical")
}

// TestUpdateSettings_ReleasesLockOnSuccessAndReject proves the lock
// discipline holds on BOTH outcomes: calling UpdateSettings twice in
// succession (once accepting, once rejecting, then accepting again)
// never trips ErrRuntimeLockHeld, which it would if any path failed to
// release the global runtime.lock through the deferred Release.
func TestUpdateSettings_ReleasesLockOnSuccessAndReject(t *testing.T) {
	t.Parallel()

	eng, _, _ := updateSettingsEngine(t)

	require.NoError(t, eng.UpdateSettings(t.Context(), validSettings()),
		"first valid save must succeed")

	bad := validSettings()
	bad.SchemaVersion = 9
	err := eng.UpdateSettings(t.Context(), bad)
	require.Error(t, err)
	require.NotErrorIs(t, err, state.ErrRuntimeLockHeld,
		"a rejected save must not be a lock-contention error — the prior call released the lock")

	require.NoError(t, eng.UpdateSettings(t.Context(), validSettings()),
		"a save after a rejected one must succeed, proving the rejected path released the lock")
}

// TestUpdateSettings_BusyLockReturnsLockHeld verifies the contention
// path: when the global runtime.lock is already held (here, acquired
// externally by the test), UpdateSettings refuses with a *types.Error
// carrying ErrCodeRuntimeLockHeld (PRD §27 exit code 4) before any
// validation or write, and the underlying state.ErrRuntimeLockHeld
// sentinel stays errors.Is-detectable. config.toml is never written.
func TestUpdateSettings_BusyLockReturnsLockHeld(t *testing.T) {
	t.Parallel()

	eng, configPath, stateDir := updateSettingsEngine(t)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))

	preLock, err := state.AcquireRuntimeLock(
		t.Context(),
		filepath.Join(stateDir, "runtime.lock"),
		state.RuntimeLockMetadata{Command: "test-preacquire", WDMVersion: "test"},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = preLock.Release() })

	err = eng.UpdateSettings(t.Context(), validSettings())
	require.Error(t, err)
	require.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"contention must remain errors.Is-detectable as state.ErrRuntimeLockHeld")

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typedErr.Code,
		"a busy runtime.lock must map to exit code 4")

	_, statErr := os.Stat(configPath)
	require.True(t, os.IsNotExist(statErr),
		"a contended UpdateSettings must not write config.toml")
}

// TestUpdateSettings_ClosedEngineReturnsErrClosed proves ErrClosed
// takes precedence over everything else: a closed engine refuses
// UpdateSettings (even with a valid payload) before the lock acquire,
// leaving no runtime.lock and no config.toml on disk.
func TestUpdateSettings_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, configPath, stateDir := updateSettingsEngine(t)
	require.NoError(t, eng.Close())

	err := eng.UpdateSettings(t.Context(), validSettings())
	require.ErrorIs(t, err, core.ErrClosed,
		"a closed engine must refuse UpdateSettings with ErrClosed")

	_, statErr := os.Stat(filepath.Join(stateDir, "runtime.lock"))
	require.True(t, os.IsNotExist(statErr),
		"a closed-engine refusal must not create runtime.lock")
	_, statErr = os.Stat(configPath)
	require.True(t, os.IsNotExist(statErr),
		"a closed-engine refusal must not write config.toml")
}

// TestUpdateSettings_CanceledContextWritesNothing proves a pre-canceled
// context short-circuits in acquireRuntimeLock before any side effect:
// the call propagates context.Canceled, creates no stateDir, and
// writes no config.toml.
func TestUpdateSettings_CanceledContextWritesNothing(t *testing.T) {
	t.Parallel()

	eng, configPath, stateDir := updateSettingsEngine(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := eng.UpdateSettings(ctx, validSettings())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context must propagate as context.Canceled")

	_, statErr := os.Stat(stateDir)
	require.True(t, os.IsNotExist(statErr),
		"a canceled UpdateSettings must not create stateDir")
	_, statErr = os.Stat(configPath)
	require.True(t, os.IsNotExist(statErr),
		"a canceled UpdateSettings must not write config.toml")
}

// updateSettingsEngineWithConfigPath builds a [*core.Engine] whose
// config.toml lives at the caller-supplied path (which may sit under a
// parent chain the caller has shaped deliberately). It mirrors
// updateSettingsEngine but does not own configPath placement, so the
// secret-mode parent-hardening behavior of the 0o600 config write can
// be exercised against a fresh (missing) parent chain or a deliberately
// group-writable parent. The state / data / stack-base dirs hang off
// the same HOME-backed 0o700 tempdir so the runtime.lock acquire path
// stays clean.
func updateSettingsEngineWithConfigPath(t *testing.T, tmp, configPath string) *core.Engine {
	t.Helper()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	eng, err := core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
		core.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// TestUpdateSettings_FreshBoxCreatesNestedParents proves the fresh-box
// first-write path: the config parent chain does NOT exist when
// UpdateSettings runs, so state.WriteFileAtomic creates the nested
// parents at GeneratedDirMode (0o755 — not group/world-writable) before
// the secret-mode RejectInsecureParent check, the file lands at 0o600,
// and the public loader round-trips the persisted payload. This is the
// accept arm of the 0o600 secret-mode parent hardening: a freshly
// created config parent is clean, so the save succeeds.
func TestUpdateSettings_FreshBoxCreatesNestedParents(t *testing.T) {
	t.Parallel()

	tmp := coreTestTempDir(t)
	// Only <tmp>/home exists (at 0o700); the.config/wdm chain below it
	// is created on demand by the first UpdateSettings write.
	homeDir := filepath.Join(tmp, "home")
	require.NoError(t, os.Mkdir(homeDir, 0o700))
	configParent := filepath.Join(homeDir, ".config", "wdm")
	configPath := filepath.Join(configParent, "config.toml")

	_, statErr := os.Stat(configParent)
	require.True(t, os.IsNotExist(statErr),
		"the config parent chain must not exist before the first write")

	eng := updateSettingsEngineWithConfigPath(t, tmp, configPath)

	require.NoError(t, eng.UpdateSettings(t.Context(), validSettings()),
		"a fresh-box first write must create nested parents and succeed")

	info, err := os.Stat(configParent)
	require.NoError(t, err, "the nested config parent must have been created")
	require.True(t, info.IsDir())

	fileInfo, err := os.Stat(configPath)
	require.NoError(t, err, "config.toml must have been written")
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm(),
		"the fresh-box config.toml must land at 0o600")

	loaded, err := state.LoadConfig(context.Background(), configPath)
	require.NoError(t, err, "the written config.toml must parse and schema-validate")
	assert.Equal(t, validSettings(), *loaded,
		"the loaded settings must round-trip the persisted payload verbatim")
}

// TestUpdateSettings_GroupWritableParentRefuses proves the refusal arm
// of the 0o600 secret-mode parent hardening: a config parent at 0o775
// (group-writable) makes state.WriteFileAtomic's secret-mode
// RejectInsecureParent check refuse the save with a typed
// ErrCodePermissionDenied error (PRD §27 exit 6), and config.toml is
// never written. The parent is pre-created group-writable (so
// ensureParentDirectories finds it existing and never re-modes it to
// the clean 0o755) and chmod'd back to 0o700 in cleanup so t.TempDir /
// HOME-backed removal still works.
func TestUpdateSettings_GroupWritableParentRefuses(t *testing.T) {
	t.Parallel()

	tmp := coreTestTempDir(t)
	configParent := filepath.Join(tmp, "config")
	require.NoError(t, os.Mkdir(configParent, 0o775))
	// os.Mkdir's mode is masked by the process umask (typically 0o022,
	// which strips the group-write bit), so chmod the directory back to
	// 0o775 explicitly to make the parent genuinely group-writable —
	// otherwise it lands at 0o755 (clean) and RejectInsecureParent
	// passes.
	require.NoError(t, os.Chmod(configParent, 0o775))
	// Restore a removable mode so the HOME-backed tempdir cleanup
	// (os.RemoveAll) is not impeded by the deliberately loose perms.
	t.Cleanup(func() { _ = os.Chmod(configParent, 0o700) })
	configPath := filepath.Join(configParent, "config.toml")

	eng := updateSettingsEngineWithConfigPath(t, tmp, configPath)

	err := eng.UpdateSettings(t.Context(), validSettings())
	require.Error(t, err, "a group-writable config parent must be refused")

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr,
		"refusal must surface as *types.Error for cmd/wdm exit-code mapping")
	assert.Equal(t, types.ErrCodePermissionDenied, typedErr.Code,
		"a group-writable config parent must map to permission denied (exit 6)")

	_, statErr := os.Stat(configPath)
	require.True(t, os.IsNotExist(statErr),
		"a refused UpdateSettings must NOT write config.toml")
}
