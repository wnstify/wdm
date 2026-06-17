//go:build unix

package state_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// syntheticV0LockJSON is a hand-written schema_version=0.wdm.lock — a
// version that never existed in the wild (schema versions started at 1), so
// it exercises the migration framework against a synthetic older fixture
// exactly as requires. The shape is otherwise a valid
// manifest so the migrated result round-trips through the normal reader.
const syntheticV0LockJSON = `{
  "schema_version": 0,
  "app_id": "vaultwarden",
  "template_name": "vaultwarden",
  "template_version": "1.2.3",
  "catalog_channel": "stable",
  "catalog_version": "2026.05.01",
  "stack_path": "/home/test/docker/vaultwarden",
  "selected_domain": "vault.example.com",
  "local_ports": [3012, 8080],
  "compose_project": "wdm-vaultwarden",
  "generated_fields": ["DB_PASSWORD", "ADMIN_TOKEN"],
  "last_successful_operation": {
    "kind": "install",
    "at": "2026-05-19T09:14:33Z",
    "wdm_version": "0.1.0"
  }
}`

// writeStackLockAt writes contents to <dir>/.wdm.lock and returns the path.
// Unlike lock_test's writeLockFile it also seeds docker-compose.yml and .env
// so the pre-migration backup has config files to snapshot.
func writeStackLockAt(t *testing.T, contents string) (stackDir, lockPath string) {
	t.Helper()
	stackDir = t.TempDir()
	lockPath = filepath.Join(stackDir, ".wdm.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(contents), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackDir, "docker-compose.yml"),
		[]byte("services: {}\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackDir, ".env"),
		[]byte("FOO=bar\n"),
		0o600,
	))
	return stackDir, lockPath
}

// recordingHandler is a minimal slog.Handler that captures emitted records so
// a test can assert a migration was logged (PRD §30: migrations must be
// logged) and inspect the structured attributes.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := make([]string, len(h.records))
	for i, r := range h.records {
		msgs[i] = r.Message
	}
	return msgs
}

func (h *recordingHandler) attrs(t *testing.T, message string) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != message {
			continue
		}
		got := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			got[a.Key] = a.Value.Any()
			return true
		})
		return got
	}
	t.Fatalf("no log record with message %q (got %v)", message, h.messages())
	return nil
}

func backupSnapshotCount(t *testing.T, stackDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stackDir, state.BackupDirName))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			count++
		}
	}
	return count
}

// TestMigration_OlderVersionIsBackedUpMigratedLogged exercises the happy
// migration path end-to-end against the synthetic v0 fixture: the on-disk
// lock ends at the current schema, a backup snapshot exists under
// .wdm-backups/, the migration was logged with the from/to versions and the
// backup path, and the migrated manifest round-trips through ReadStackLock.
func TestMigration_OlderVersionIsBackedUpMigratedLogged(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, syntheticV0LockJSON)
	handler := &recordingHandler{}
	logger := slog.New(handler)

	handle, err := state.AcquireStackLock(
		t.Context(),
		lockPath,
		state.WithMigrationLogger(logger),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Release() })

	migrated := handle.Lock()
	require.NotNil(t, migrated)
	assert.Equal(t, state.StackLockSchemaVersionForTest(), migrated.SchemaVersion)
	// Field preservation: the identity migration changes only the version.
	assert.Equal(t, "vaultwarden", migrated.AppID)
	assert.Equal(t, []int{3012, 8080}, migrated.LocalPorts)
	assert.Equal(t, []string{"DB_PASSWORD", "ADMIN_TOKEN"}, migrated.GeneratedFields)

	// A backup snapshot was taken before the lock changed.
	assert.Equal(t, 1, backupSnapshotCount(t, stackDir), "exactly one migration backup snapshot")

	// The migration was logged with identifiers only.
	assert.Contains(t, handler.messages(), "migrated .wdm.lock schema")
	attrs := handler.attrs(t, "migrated .wdm.lock schema")
	assert.Equal(t, int64(0), attrs["from_schema_version"])
	assert.Equal(t, int64(state.StackLockSchemaVersionForTest()), attrs["to_schema_version"])
	assert.Equal(t, stackDir, attrs["stack_path"])
	backupPath, _ := attrs["backup_path"].(string)
	assert.NotEmpty(t, backupPath)
	assert.DirExists(t, backupPath)

	require.NoError(t, handle.Release())

	// The migrated manifest is now readable through the normal read path.
	onDisk, err := state.ReadStackLock(t.Context(), lockPath)
	require.NoError(t, err)
	assert.Equal(t, state.StackLockSchemaVersionForTest(), onDisk.SchemaVersion)
	assert.Equal(t, "vaultwarden", onDisk.AppID)
}

// TestMigration_BackupExistsBeforeLockBytesChange proves the fail-closed
// ordering: the persist step is induced to fail AFTER the backup is taken;
// the test asserts the backup snapshot exists while the on-disk lock is
// byte-identical to the v0 fixture and the error is ErrCodeMigrationFailure.
func TestMigration_BackupExistsBeforeLockBytesChange(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, syntheticV0LockJSON)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	// Take the REAL backup snapshot, then fail the backup stage immediately
	// afterwards. The in-memory chain has already succeeded by this point
	// (backup runs after the chain), so this proves the ordering invariant:
	// the snapshot is on disk while the lock-write step never runs, leaving
	// the on-disk lock byte-identical.
	var sawBackup string
	state.SwapBackupBeforeMigrationForTest(t, func(sp string) (string, error) {
		path, e := state.CreateConfigBackup(sp, "migration", nil)
		require.NoError(t, e)
		sawBackup = path
		return path, fmt.Errorf("induced backup-stage failure after snapshot")
	})

	_, err = state.AcquireStackLock(t.Context(), lockPath, state.WithMigrationLogger(nil))
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeMigrationFailure),
		"backup-stage failure must surface ErrCodeMigrationFailure; got %v", err)

	// The backup snapshot exists (it ran first)...
	require.NotEmpty(t, sawBackup)
	assert.DirExists(t, sawBackup)
	assert.Equal(t, 1, backupSnapshotCount(t, stackDir))

	// ...but the on-disk lock is byte-identical: nothing was written.
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "lock bytes must be unchanged on a failed migration")
}

// TestMigration_InducedFailureLeavesStackUntouched injects a migration step
// that returns an error. The in-memory chain fails BEFORE the backup, so the
// stack is entirely untouched and the error is ErrCodeMigrationFailure with
// the injected cause reachable.
func TestMigration_InducedFailureLeavesStackUntouched(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, syntheticV0LockJSON)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	sentinel := errors.New("induced migration step failure")
	state.SwapStackLockMigrationsForTest(t, state.StackLockMigrationForTest{
		FromVersion: 0,
		Migrate: func(*state.StackLock) error {
			return sentinel
		},
	})

	_, err = state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeMigrationFailure))
	assert.ErrorIs(t, err, sentinel, "the injected cause must be errors.Is-reachable")

	// Failed in memory before the backup: nothing on disk changed.
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 0, backupSnapshotCount(t, stackDir), "no backup on a pre-backup failure")
}

// TestMigration_ChainGapFailsClosed installs an empty registry so the v0
// lock has no migration to version 1. The framework must fail closed with
// ErrCodeMigrationFailure and leave the stack untouched.
func TestMigration_ChainGapFailsClosed(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, syntheticV0LockJSON)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	state.SwapStackLockMigrationsForTest(t) // empty registry → chain gap

	_, err = state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeMigrationFailure))
	assert.Contains(t, err.Error(), "no migration registered")

	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 0, backupSnapshotCount(t, stackDir))
}

// TestMigration_StepThatDoesNotAdvanceFailsClosed registers a step whose
// Migrate forgets to bump the version. The chain runner must reject it (it
// would otherwise loop or silently corrupt the chain) and fail closed.
func TestMigration_StepThatDoesNotAdvanceFailsClosed(t *testing.T) {
	_, lockPath := writeStackLockAt(t, syntheticV0LockJSON)

	state.SwapStackLockMigrationsForTest(t, state.StackLockMigrationForTest{
		FromVersion: 0,
		Migrate: func(*state.StackLock) error {
			// Deliberately does NOT set SchemaVersion to 1.
			return nil
		},
	})

	_, err := state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeMigrationFailure))
	assert.Contains(t, err.Error(), "expected 1")
}

// TestMigration_CurrentVersionDoesNotMigrate proves behavior preservation: a
// schema_version=1 lock opens with NO backup and NO migration log — the
// framework is invisible to current-version stacks.
func TestMigration_CurrentVersionDoesNotMigrate(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, currentSchemaLockJSON())
	handler := &recordingHandler{}

	handle, err := state.AcquireStackLock(
		t.Context(),
		lockPath,
		state.WithMigrationLogger(slog.New(handler)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Release() })

	lock := handle.Lock()
	require.NotNil(t, lock)
	assert.Equal(t, state.StackLockSchemaVersionForTest(), lock.SchemaVersion)

	assert.Equal(t, 0, backupSnapshotCount(t, stackDir), "current-version open must not back up")
	assert.NotContains(t, handler.messages(), "migrated .wdm.lock schema",
		"current-version open must not log a migration")
}

// TestMigration_FutureVersionStaysStale pins that a schema_version newer than
// the current build is still ErrStaleState (not a migration), matching the
// pre-framework behavior.
func TestMigration_FutureVersionStaysStale(t *testing.T) {
	_, lockPath := writeStackLockAt(t, `{
  "schema_version": 999,
  "app_id": "test",
  "compose_project": "wdm-test"
}`)

	_, err := state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState),
		"a future schema_version must stay stale, never migrate; got %v", err)
}

// TestMigration_CorruptJSONStaysStale pins that unparseable bytes still map
// to ErrStaleState (the version peek fails, so it falls through to the
// pre-framework decode), never to a migration.
func TestMigration_CorruptJSONStaysStale(t *testing.T) {
	_, lockPath := writeStackLockAt(t, "{ not valid json")

	_, err := state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
}

// TestMigration_MissingSchemaVersionStaysStale pins that a valid JSON object
// that OMITS schema_version is refused as ErrStaleState, never migrated: the
// version peek reports not-ok (a field-less object is not schema 0), so it
// falls through to the pre-framework decode. A fail-closed framework must not
// adopt a field-less object as an older version and "migrate" it.
func TestMigration_MissingSchemaVersionStaysStale(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, `{
  "app_id": "vaultwarden",
  "compose_project": "wdm-vaultwarden"
}`)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	_, err = state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState),
		"an object without schema_version must stay stale, never migrate; got %v", err)

	// Nothing was written: no backup snapshot, lock bytes unchanged.
	assert.Equal(t, 0, backupSnapshotCount(t, stackDir),
		"a field-less object must not be backed up by the migration framework")
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "lock bytes must be unchanged for a field-less object")
}

// TestMigration_NullSchemaVersionStaysStale pins that an explicit
// "schema_version": null is refused as ErrStaleState, never migrated — the
// pointer peek decodes null to nil exactly like an absent field, so it falls
// through to the pre-framework decode rather than being adopted as schema 0.
func TestMigration_NullSchemaVersionStaysStale(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, `{
  "schema_version": null,
  "app_id": "vaultwarden",
  "compose_project": "wdm-vaultwarden"
}`)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	_, err = state.AcquireStackLock(t.Context(), lockPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState),
		"a null schema_version must stay stale, never migrate; got %v", err)

	// Nothing was written: no backup snapshot, lock bytes unchanged.
	assert.Equal(t, 0, backupSnapshotCount(t, stackDir),
		"a null schema_version must not be backed up by the migration framework")
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "lock bytes must be unchanged for a null schema_version")
}

// TestMigration_MultiStepChainReachesCurrent proves the chain runs stepwise:
// a synthetic v(-1) lock with two registered steps (-1→0, 0→1) lands at the
// current version through both steps in order.
func TestMigration_MultiStepChainReachesCurrent(t *testing.T) {
	_, lockPath := writeStackLockAt(t, `{
  "schema_version": -1,
  "app_id": "vaultwarden",
  "template_name": "vaultwarden",
  "template_version": "1.0.0",
  "catalog_channel": "stable",
  "catalog_version": "v1",
  "stack_path": "/tmp/x",
  "compose_project": "wdm-vaultwarden",
  "last_successful_operation": null
}`)

	var order []int
	state.SwapStackLockMigrationsForTest(t,
		state.StackLockMigrationForTest{
			FromVersion: -1,
			Migrate: func(l *state.StackLock) error {
				order = append(order, -1)
				l.SchemaVersion = 0
				return nil
			},
		},
		state.StackLockMigrationForTest{
			FromVersion: 0,
			Migrate: func(l *state.StackLock) error {
				order = append(order, 0)
				l.SchemaVersion = 1
				return nil
			},
		},
	)

	handle, err := state.AcquireStackLock(t.Context(), lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Release() })

	assert.Equal(t, []int{-1, 0}, order, "steps run in ascending version order")
	lock := handle.Lock()
	require.NotNil(t, lock)
	assert.Equal(t, state.StackLockSchemaVersionForTest(), lock.SchemaVersion)
}

// TestMigration_MigratedHandleIsUsable proves the migrated open returns a
// usable handle: a subsequent Write persists through the held fd.
func TestMigration_MigratedHandleIsUsable(t *testing.T) {
	_, lockPath := writeStackLockAt(t, syntheticV0LockJSON)

	handle, err := state.AcquireStackLock(t.Context(), lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Release() })

	lock := handle.Lock()
	require.NotNil(t, lock)
	lock.TemplateVersion = "9.9.9"
	require.NoError(t, handle.Write(*lock))
	require.NoError(t, handle.Release())

	onDisk, err := state.ReadStackLock(t.Context(), lockPath)
	require.NoError(t, err)
	assert.Equal(t, "9.9.9", onDisk.TemplateVersion)
	assert.Equal(t, state.StackLockSchemaVersionForTest(), onDisk.SchemaVersion)
}

// TestMigration_ProductionRegistryCoversChainToCurrent asserts the shipped
// registry is non-empty and forms a complete chain from its lowest
// FromVersion up to the current schema, so the framework is provable
// end-to-end (the honesty-note guard).
func TestMigration_ProductionRegistryCoversChainToCurrent(t *testing.T) {
	versions := state.RegisteredMigrationFromVersionsForTest()
	require.NotEmpty(t, versions, "the production registry must ship at least one migration")

	current := state.StackLockSchemaVersionForTest()
	registered := map[int]struct{}{}
	for _, v := range versions {
		registered[v] = struct{}{}
	}
	for v := versions[0]; v < current; v++ {
		_, ok := registered[v]
		assert.Truef(t, ok, "missing migration step for schema_version %d in the production chain", v)
	}
}

// TestMigration_ReadOnlyPathsNeverMigrate pins that the read-only readers
// refuse an older-schema lock with ErrStaleState rather than migrating — a
// read-only command must never write (PRD §26 read-only clause).
func TestMigration_ReadOnlyPathsNeverMigrate(t *testing.T) {
	stackDir, lockPath := writeStackLockAt(t, syntheticV0LockJSON)

	_, readErr := state.ReadStackLock(t.Context(), lockPath)
	require.Error(t, readErr)
	assert.True(t, errors.Is(readErr, types.ErrStaleState),
		"ReadStackLock must refuse an older version, not migrate; got %v", readErr)

	_, tryErr := state.TryReadStackLock(t.Context(), lockPath)
	require.Error(t, tryErr)
	assert.True(t, errors.Is(tryErr, types.ErrStaleState),
		"TryReadStackLock must refuse an older version, not migrate; got %v", tryErr)

	// Neither read path wrote anything: no backup, lock unchanged.
	assert.Equal(t, 0, backupSnapshotCount(t, stackDir))
}

// currentSchemaLockJSON builds a minimal valid manifest at the current schema
// version so the behavior-preservation test does not hardcode the literal.
func currentSchemaLockJSON() string {
	return fmt.Sprintf(`{
  "schema_version": %d,
  "app_id": "vaultwarden",
  "template_name": "vaultwarden",
  "template_version": "1.2.3",
  "catalog_channel": "stable",
  "catalog_version": "2026.05.01",
  "stack_path": "/home/test/docker/vaultwarden",
  "compose_project": "wdm-vaultwarden",
  "last_successful_operation": null
}`, state.StackLockSchemaVersionForTest())
}
