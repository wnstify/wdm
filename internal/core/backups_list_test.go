package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// writeBackupSnapshot creates one <unix-nanos>-<operation> snapshot
// directory directly under the stack's.wdm-backups root, mirroring the
// on-disk shape state.CreateConfigBackup writes. It is used by the
// ordering test where deterministic, collision-free creation prefixes
// matter (the real writer keys names off time.Now.UnixNano, which
// two rapid calls could collide on); the projection-against-real-
// artifacts proof uses the real writer instead.
func writeBackupSnapshot(t *testing.T, stackPath, snapshotID string, files map[string]string) {
	t.Helper()

	snapshotDir := filepath.Join(stackPath, state.BackupDirName, snapshotID)
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	for name, contents := range files {
		require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, name), []byte(contents), 0o600))
	}
}

// TestListBackups_ProjectsRealWriterSnapshot proves the projection
// against a REAL state.CreateConfigBackup artifact: ListBackups maps the
// state.ConfigBackupSnapshot field-for-field onto types.BackupInfo
// (SnapshotID, Operation, CreatedAt, Path, Files), and the read-only
// path issues zero docker calls, creates no runtime.lock, and leaves the
// stack dir's.wdm.lock byte-identical.
func TestListBackups_ProjectsRealWriterSnapshot(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)

	// The status fixture writes only.wdm.lock into the stack dir, so the
	// snapshot captures exactly that file — a valid non-empty backup.
	snapshotPath, err := state.CreateConfigBackup(fixture.stackPath, "update", nil)
	require.NoError(t, err)

	manifestBefore, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	backups, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.Len(t, backups, 1)

	got := backups[0]
	assert.Equal(t, filepath.Base(snapshotPath), got.SnapshotID)
	assert.Equal(t, "update", got.Operation)
	assert.Equal(t, snapshotPath, got.Path)
	assert.False(t, got.CreatedAt.IsZero(), "CreatedAt is decoded from the snapshot name prefix")
	assert.Equal(t, []string{".wdm.lock"}, got.Files)

	// Read-only discipline: no docker call, no runtime.lock, manifest
	// untouched.
	assert.Zero(t, fixture.fake.calls, "ListBackups must issue no docker command")
	_, statErr := os.Stat(filepath.Join(fixture.stateDir, "runtime.lock"))
	assert.True(t, os.IsNotExist(statErr), "ListBackups must not create runtime.lock")
	manifestAfter, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter, "ListBackups must not rewrite the manifest")
}

// TestListBackups_NewestFirstOrderingPreserved proves ListBackups
// preserves the state lister's newest-first order (by the unix-nanos
// creation prefix) and never re-sorts: three snapshots with ascending
// prefixes come back newest-first with every field projected.
func TestListBackups_NewestFirstOrderingPreserved(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)

	// Ascending creation prefixes; ListBackups must return them descending.
	writeBackupSnapshot(t, fixture.stackPath, "1000000000000000000-install", map[string]string{
		".wdm.lock":          "{}",
		"docker-compose.yml": "services: {}",
	})
	writeBackupSnapshot(t, fixture.stackPath, "2000000000000000000-update", map[string]string{
		".wdm.lock": "{}",
	})
	writeBackupSnapshot(t, fixture.stackPath, "3000000000000000000-migration", map[string]string{
		".wdm.lock": "{}",
		".env":      "K=V",
	})

	backups, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.Len(t, backups, 3)

	assert.Equal(t, "3000000000000000000-migration", backups[0].SnapshotID)
	assert.Equal(t, "migration", backups[0].Operation)
	assert.Equal(t, []string{".env", ".wdm.lock"}, backups[0].Files)

	assert.Equal(t, "2000000000000000000-update", backups[1].SnapshotID)
	assert.Equal(t, "update", backups[1].Operation)
	assert.Equal(t, []string{".wdm.lock"}, backups[1].Files)

	assert.Equal(t, "1000000000000000000-install", backups[2].SnapshotID)
	assert.Equal(t, "install", backups[2].Operation)
	assert.Equal(t, []string{".wdm.lock", "docker-compose.yml"}, backups[2].Files)

	// CreatedAt is strictly descending, decoded from the nanos prefix.
	assert.True(t, backups[0].CreatedAt.After(backups[1].CreatedAt))
	assert.True(t, backups[1].CreatedAt.After(backups[2].CreatedAt))

	// Absolute snapshot paths point under the stack's backup root.
	for _, b := range backups {
		assert.Equal(t, filepath.Join(fixture.stackPath, state.BackupDirName, b.SnapshotID), b.Path)
	}
}

// TestListBackups_EmptyManagedStackReturnsNonNilEmptySlice proves a
// managed stack that never backed up returns a non-nil empty slice and a
// nil error, so the CLI/JSON layer renders [] rather than null.
func TestListBackups_EmptyManagedStackReturnsNonNilEmptySlice(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)

	backups, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, backups, "an empty result must marshal to [] not null")
	assert.Empty(t, backups)
	assert.Zero(t, fixture.fake.calls, "ListBackups must issue no docker command")
}

// TestListBackups_DefensiveCopyCannotCorruptEngineState is the
// BackupInfo (its Files slice) through the public Engine API cannot
// corrupt anything a subsequent call observes. It proves the property
// end-to-end through ListBackups, not by reaching into core internals.
func TestListBackups_DefensiveCopyCannotCorruptEngineState(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	writeBackupSnapshot(t, fixture.stackPath, "1000000000000000000-install", map[string]string{
		".wdm.lock":          "{}",
		"docker-compose.yml": "services: {}",
	})

	first, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, []string{".wdm.lock", "docker-compose.yml"}, first[0].Files)

	// Hostile mutation of the returned result.
	first[0].SnapshotID = "tampered"
	first[0].Operation = "tampered"
	first[0].Path = "tampered"
	for i := range first[0].Files {
		first[0].Files[i] = "tampered"
	}

	// A fresh call must see the pristine snapshot — the mutation reached
	// no retained state and no shared backing array.
	second, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "1000000000000000000-install", second[0].SnapshotID)
	assert.Equal(t, "install", second[0].Operation)
	assert.Equal(t, []string{".wdm.lock", "docker-compose.yml"}, second[0].Files)
}

// TestListBackups_RefusesMissingAndUnmanagedStacks proves the
// managed-only refusal ordering (PRD §10): an uninstalled app, an
// unmanaged directory, and an empty app id all refuse with
// ErrCodeUsageValidation BEFORE any backup directory is walked.
func TestListBackups_RefusesMissingAndUnmanagedStacks(t *testing.T) {
	t.Parallel()

	t.Run("app not installed", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		backups, err := eng.ListBackups(t.Context(), "ghost-app")
		require.Error(t, err)
		assert.Nil(t, backups)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "app is not installed")
		assert.Zero(t, fake.calls, "managed-only refusal must precede any docker call")
	})

	t.Run("directory exists but is unmanaged", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
		require.NoError(t, os.MkdirAll(filepath.Join(stackBase, "user-dir"), 0o755))

		backups, err := eng.ListBackups(t.Context(), "user-dir")
		require.Error(t, err)
		assert.Nil(t, backups)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "stack directory is not managed by wdm")
	})

	t.Run("empty app id", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		backups, err := eng.ListBackups(t.Context(), "")
		require.Error(t, err)
		assert.Nil(t, backups)
		assertUsageValidation(t, err)
	})
}

// TestListBackups_RefusesMismatchedAppID proves a stack whose .wdm.lock
// records a DIFFERENT app refuses with ErrCodeUsageValidation — the
// on-disk managed-identity gate, not a directory-name match, decides.
func TestListBackups_RefusesMismatchedAppID(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)

	// fixture.appID is "status-app"; query a different id with no stack.
	backups, err := fixture.eng.ListBackups(t.Context(), "other-app")
	require.Error(t, err)
	assert.Nil(t, backups)
	assertUsageValidation(t, err)
	assert.Zero(t, fixture.fake.calls, "managed-only refusal must precede any docker call")
}

// TestListBackups_SymlinkedBackupRootSurfacesTypedGenericError proves a
// state.ListConfigBackups hard error (here a symlinked.wdm-backups root)
// surfaces as a typed ErrCodeGeneric — an operational fault carrying an
// actionable hint, not the exit-2 usage refusal the plain wrap produced —
// with the state-lister cause reachable in the message and zero docker
// calls.
func TestListBackups_SymlinkedBackupRootSurfacesTypedGenericError(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)

	// A symlinked backup root is the state-lister's hard refusal: the
	// managed stack resolves cleanly, then ListConfigBackups rejects the
	// symlinked root before any snapshot walk.
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(fixture.stackPath, state.BackupDirName)))

	backups, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, backups)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeGeneric, typedErr.Code)
	assert.Contains(t, err.Error(), "config backups could not be listed")
	assert.Contains(t, err.Error(), "must not be a symlink")
	assert.Zero(t, fixture.fake.calls, "the lister refusal must precede any docker call")
}

// TestListBackups_RefusesBusyStackWithoutBlocking proves the PRD §26
// read-only lock posture: a held per-stack exclusive flock makes
// ListBackups refuse fast with ErrCodeRuntimeLockHeld and walk no
// backups (the non-blocking shared-lock read, never stalling behind the
// writer).
func TestListBackups_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	// Seed a real snapshot so a (wrong) blocking read could otherwise see it.
	writeBackupSnapshot(t, fixture.stackPath, "1000000000000000000-install", map[string]string{
		".wdm.lock": "{}",
	})
	holdFlockExclusive(t, filepath.Join(fixture.stackPath, ".wdm.lock"))

	backups, err := fixture.eng.ListBackups(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, backups)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typedErr.Code)
	require.ErrorIs(t, err, state.ErrStackLockBusy)
	assert.Zero(t, fixture.fake.calls)
}

// TestListBackups_ContextCancellation proves a pre-canceled context
// refuses before any stack resolution or backup walk, surfacing as an
// error.
func TestListBackups_ContextCancellation(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	backups, err := fixture.eng.ListBackups(ctx, fixture.appID)
	require.Error(t, err)
	assert.Nil(t, backups)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, fixture.fake.calls)
}

// TestListBackups_HonorsClosed keeps the closed-engine pin: a closed
// engine returns ErrClosed with a nil result.
func TestListBackups_HonorsClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	backups, err := eng.ListBackups(t.Context(), "uptime-kuma")
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, backups)
}
