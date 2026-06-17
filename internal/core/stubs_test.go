package core_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// noopRegistryResolverForTest is the default registry seam newTestEngine
// wires: it never contacts a registry and always reports "no digest", so
// update planning is a no-op and no test ever makes a real network call by
// accident. Tests that exercise registry
// behavior inject fakeRegistryResolver via WithRegistryClient instead.
type noopRegistryResolverForTest struct{}

func (noopRegistryResolverForTest) ResolveDigest(_ context.Context, _ string) (registry.Manifest, error) {
	return registry.Manifest{}, nil
}

// newTestEngine builds a [*core.Engine] with t.TempDir-backed state /
// data / stack base directories, a missing-on-purpose config.toml
// path, and a silent logger. The stack base directory is pre-created
// to satisfy the absolute-path check in resolveStackBase; the state
// dir is intentionally NOT created so the [stubs.acquireRuntimeLock]
// MkdirAll path gets exercised by the first state-changing stub call.
// Returns the engine plus the absolute path of the state dir so tests
// can stat / read the runtime.lock file directly without poking at
// unexported engine fields.
func newTestEngine(t *testing.T, extra ...core.Option) (eng *core.Engine, stateDir string) {
	t.Helper()
	tmp := coreTestTempDir(t)
	stateDir = filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	configPath := filepath.Join(tmp, "nonexistent.toml")

	opts := []core.Option{
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
		core.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		// Default the registry seam to a no-op so NO test ever reaches a
		// real registry during Update planning. The no-op returns no
		// digest; tests that assert registry visibility override it via
		// WithRegistryClient in extra (a later option wins).
		core.WithRegistryClient(func() core.RegistryResolver { return noopRegistryResolverForTest{} }),
	}
	opts = append(opts, extra...)

	eng, err := core.New(opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	return eng, stateDir
}

func coreTestTempDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	tmp, err := os.MkdirTemp(home, ".wdm-core-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	return tmp
}

// TestStubFlow_StateChangingStubsAcquireAndReleaseRuntimeLock is the
// primary criterion-396 evidence for UpdateSettings: it acquires the
// runtime.lock under the engine's stateDir, writes the metadata JSON,
// releases the lock when the deferred Release fires, and persists the
// supplied settings. The "called twice" check at the end is the
// load-bearing release confirmation — without a clean release, the
// second call would hit ErrRuntimeLockHeld instead. UpdateSettings
// uses a valid types.Settings payload and both calls are expected to
// succeed. Update's
// lock-posture evidence moved to update_test.go alongside the
// remove_test.go alongside the safe-removal planning
// implementation (an empty RemoveRequest now refuses on the required
// app id before any lock dance, so it can no longer prove the bare
// acquire/release dance here).
func TestStubFlow_StateChangingStubsAcquireAndReleaseRuntimeLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		expectedCommand string
		call            func(eng *core.Engine, ctx context.Context) error
	}{
		{
			name:            "UpdateSettings",
			expectedCommand: "update-settings",
			call: func(eng *core.Engine, ctx context.Context) error {
				return eng.UpdateSettings(ctx, validStubSettings())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, stateDir := newTestEngine(t)
			ctx := t.Context()

			err := tt.call(eng, ctx)
			require.NoError(t, err,
				"%s must succeed after the lock acquire/release dance", tt.name)

			lockPath := filepath.Join(stateDir, "runtime.lock")
			require.FileExists(t, lockPath, "%s must create runtime.lock under stateDir", tt.name)

			raw, err := os.ReadFile(lockPath)
			require.NoError(t, err)

			var info state.RuntimeLockInfo
			require.NoError(t, json.Unmarshal(raw, &info))
			assert.Equal(t, 1, info.SchemaVersion, "schema_version must be 1")
			assert.Equal(t, tt.expectedCommand, info.Command,
				"runtime.lock Command must match the method's command name")
			assert.Equal(t, "dev", info.WDMVersion,
				"default wdm_version must be \"dev\" when WithVersion is not supplied")
			assert.Equal(t, os.Getpid(), info.PID,
				"runtime.lock PID must match the current process")

			// Second call confirms the deferred Release fired — without
			// it, this attempt would hit ErrRuntimeLockHeld instead.
			err = tt.call(eng, ctx)
			require.NoError(t, err,
				"%s must succeed on the second call, proving the first call released the lock", tt.name)
		})
	}
}

// validStubSettings returns a types.Settings that passes every arm of
// the UpdateSettings validation matrix, so the lock-dance and
// closed-engine probes exercise the live success/refusal paths without
// each carrying its own payload. Timezone is empty (the legal
// detect-at-install sentinel) so the probe never depends on host
// tzdata.
func validStubSettings() types.Settings {
	return types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "~/docker",
		Timezone:              "",
		DefaultDockerNetwork:  "wdm_default",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "daily-on-launch",
	}
}

// TestStubFlow_ContentionReturnsLockHeldError verifies the contention
// path: when another process (here, the test itself) holds the
// runtime.lock, every state-changing stub returns a *types.Error
// carrying ErrCodeRuntimeLockHeld. The underlying state.ErrRuntimeLockHeld
// sentinel remains detectable via errors.Is through the wrap chain.
// Same-process simulation is sufficient for this contract: flock(2) on
// Linux is per-OFD, so two distinct file descriptors held by the same
// process compete the same way two processes would. The
// internal/state package's own runtime_lock_test.go uses a subprocess
// for the cross-process variant; this test exercises the engine's
// contention-mapping layer specifically.
func TestStubFlow_ContentionReturnsLockHeldError(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))

	// Pre-acquire the runtime.lock externally; release at test cleanup.
	preLock, err := state.AcquireRuntimeLock(
		t.Context(),
		filepath.Join(stateDir, "runtime.lock"),
		state.RuntimeLockMetadata{Command: "test-preacquire", WDMVersion: "test"},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = preLock.Release() })

	_, err = eng.Install(t.Context(), types.InstallRequest{}, nil, nil)
	require.Error(t, err)

	// The wrap chain must preserve errors.Is detection of the sentinel.
	require.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"contention error must remain errors.Is-detectable as state.ErrRuntimeLockHeld")

	// And it must be mappable to PRD §27 exit code 4 via the
	// *types.Error path that cmd/wdm's exitCodeFor uses.
	var typeErr *types.Error
	require.ErrorAs(t, err, &typeErr,
		"contention error must surface as *types.Error so cmd/wdm maps it to exit code 4")
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typeErr.Code)
	assert.Contains(t, typeErr.Message, "in progress",
		"user-visible message must indicate contention")
	assert.Contains(t, typeErr.Hint, "test-preacquire",
		"hint must surface the holder's command when readable")
}

// TestStubFlow_ClosedEngineReturnsErrClosedBeforeAcquire verifies the
// isClosed precedence rule: closing the engine must short-circuit
// every state-changing stub BEFORE the mkdir + lock acquire path runs,
// leaving no runtime.lock artifact on disk. Without this guard, the
// stub would still write a runtime.lock file even though the engine
// is marked closed — confusing on a stale-state inspection later.
func TestStubFlow_ClosedEngineReturnsErrClosedBeforeAcquire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(eng *core.Engine, ctx context.Context) error
	}{
		{"Install", func(eng *core.Engine, ctx context.Context) error {
			_, err := eng.Install(ctx, types.InstallRequest{}, nil, nil)
			return err
		}},
		{"Update", func(eng *core.Engine, ctx context.Context) error {
			_, err := eng.Update(ctx, types.UpdateRequest{}, nil, nil)
			return err
		}},
		{"Remove", func(eng *core.Engine, ctx context.Context) error {
			_, err := eng.Remove(ctx, types.RemoveRequest{}, nil, nil)
			return err
		}},
		{"UpdateSettings", func(eng *core.Engine, ctx context.Context) error {
			// A valid payload proves ErrClosed wins over a settings
			// value that would otherwise be persisted successfully.
			return eng.UpdateSettings(ctx, validStubSettings())
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, stateDir := newTestEngine(t)
			require.NoError(t, eng.Close())

			err := tt.call(eng, t.Context())
			require.ErrorIs(t, err, core.ErrClosed,
				"%s on a closed engine must return ErrClosed before any I/O", tt.name)

			lockPath := filepath.Join(stateDir, "runtime.lock")
			_, statErr := os.Stat(lockPath)
			require.True(t, os.IsNotExist(statErr),
				"%s must not create runtime.lock when the engine is closed", tt.name)
		})
	}
}

// TestStubFlow_CanceledContextDoesNotCreateStateDir is the
// regression test for the P3 review catch on the criterion 396
// commit: a pre-canceled context must short-circuit
// [Engine.acquireRuntimeLock] BEFORE the [os.MkdirAll] side effect
// runs, so no stateDir directory is created as a residue of the
// canceled call.
// [newTestEngine] intentionally does NOT pre-create stateDir — the
// helper's MkdirAll is normally what creates it on first
// state-changing call. With the ctx.Err guard in place, a canceled
// context must leave stateDir non-existent after the call returns.
// The os.IsNotExist(os.Stat(...)) assertion is the exact side-effect
// check the original review specified.
// The error must propagate as context.Canceled through the
// "core.acquireRuntimeLock:" wrap so callers (and cmd/wdm's
// exitCodeFor) can still detect cancellation via errors.Is.
func TestStubFlow_CanceledContextDoesNotCreateStateDir(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel before the call so ctx.Err() is non-nil at entry.

	_, err := eng.Install(ctx, types.InstallRequest{}, nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"canceled context must propagate as context.Canceled through the wrap chain")

	_, statErr := os.Stat(stateDir)
	require.True(t, os.IsNotExist(statErr),
		"canceled context must NOT create stateDir as an MkdirAll side effect")
}

// TestStubFlow_ReadStubsDoNotAcquireLock verifies the PRD §26
// scope-of-lock rule: read-only paths (Status, Logs) MUST NOT
// acquire the runtime.lock. The check is concrete: after calling
// Status and Logs (refusing the uninstalled app and the nil callback
// respectively) on
// an engine with a fresh empty stateDir, no runtime.lock file should
// exist on disk.
func TestStubFlow_ReadStubsDoNotAcquireLock(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	lockPath := filepath.Join(stateDir, "runtime.lock")

	_, err := eng.Status(t.Context(), "any-app")
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code,
		"Status on an uninstalled app must refuse with usage validation")
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr),
		"Status must not create runtime.lock")

	err = eng.Logs(t.Context(), types.LogsRequest{}, nil)
	require.Error(t, err)
	var logsTypedErr *types.Error
	require.ErrorAs(t, err, &logsTypedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, logsTypedErr.Code,
		"Logs without a callback must refuse with usage validation")
	_, statErr = os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr),
		"Logs must not create runtime.lock")
}
