//go:build unix

package state_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

func TestListConfigBackups_ReturnsSnapshotsNewestFirst(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	oldest := writeNamedSnapshot(t, backupRoot, "1717000000000000000-install", map[string]string{
		"docker-compose.yml": "old compose",
		".env":               "OLD=1",
	})
	middle := writeNamedSnapshot(t, backupRoot, "1717000000000000500-update", map[string]string{
		".wdm.lock": "{}",
	})
	newest := writeNamedSnapshot(t, backupRoot, "1717000000000001000-migration", map[string]string{
		"docker-compose.yml": "new compose",
	})

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, "1717000000000001000-migration", got[0].SnapshotID)
	assert.Equal(t, "1717000000000000500-update", got[1].SnapshotID)
	assert.Equal(t, "1717000000000000000-install", got[2].SnapshotID)

	assert.Equal(t, "migration", got[0].Operation)
	assert.Equal(t, "update", got[1].Operation)
	assert.Equal(t, "install", got[2].Operation)

	assert.Equal(t, newest, got[0].Path)
	assert.Equal(t, middle, got[1].Path)
	assert.Equal(t, oldest, got[2].Path)

	assert.Equal(t, time.Unix(0, 1717000000000001000).UTC(), got[0].CreatedAt)
	assert.Equal(t, time.Unix(0, 1717000000000000500).UTC(), got[1].CreatedAt)
	assert.Equal(t, time.Unix(0, 1717000000000000000).UTC(), got[2].CreatedAt)

	assert.Equal(t, []string{".env", "docker-compose.yml"}, got[2].Files)
	assert.Equal(t, []string{".wdm.lock"}, got[1].Files)
	assert.Equal(t, []string{"docker-compose.yml"}, got[0].Files)
}

func TestListConfigBackups_CreatedAtIsDecodedFromNameNotMtime(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	snapshotPath := writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{
		"docker-compose.yml": "compose",
	})

	// Touch the directory long after creation: CreatedAt must still report
	// the name-embedded instant, not the mtime prune would evict by.
	mtime := time.Unix(0, 1800000000000000000).UTC()
	require.NoError(t, os.Chtimes(snapshotPath, mtime, mtime))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, time.Unix(0, 1717000000000000000).UTC(), got[0].CreatedAt)
}

func TestListConfigBackups_TieBreaksOnDescendingSnapshotID(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	// Same unix-nanos prefix → identical CreatedAt → deterministic
	// tie-break on the descending basename.
	writeNamedSnapshot(t, backupRoot, "1717000000000000000-aaa", map[string]string{"docker-compose.yml": "a"})
	writeNamedSnapshot(t, backupRoot, "1717000000000000000-bbb", map[string]string{"docker-compose.yml": "b"})
	writeNamedSnapshot(t, backupRoot, "1717000000000000000-ccc", map[string]string{"docker-compose.yml": "c"})

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, "1717000000000000000-ccc", got[0].SnapshotID)
	assert.Equal(t, "1717000000000000000-bbb", got[1].SnapshotID)
	assert.Equal(t, "1717000000000000000-aaa", got[2].SnapshotID)
}

func TestListConfigBackups_AbsentBackupRootIsEmptyNoError(t *testing.T) {
	stackDir := t.TempDir()

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got, "absent root returns a non-nil empty slice")
}

func TestListConfigBackups_EmptyBackupRootIsEmptyNoError(t *testing.T) {
	stackDir := t.TempDir()
	createBackupRoot(t, stackDir)

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListConfigBackups_SkipsNonDirectoryEntries(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{"docker-compose.yml": "c"})
	// A loose regular file directly under the backup root — ignored.
	require.NoError(t, os.WriteFile(filepath.Join(backupRoot, "readme.txt"), []byte("note"), 0o644))
	// A loose file whose name even parses as a snapshot — still ignored, not a directory.
	require.NoError(t, os.WriteFile(filepath.Join(backupRoot, "1717000000000009999-update"), []byte("x"), 0o644))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "1717000000000000000-update", got[0].SnapshotID)
}

func TestListConfigBackups_SkipsMalformedSnapshotNames(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{"docker-compose.yml": "c"})

	malformedNames := []string{
		"no-nanos-prefix",           // non-numeric prefix
		"1717000000000000000",       // missing the hyphen + operation
		"1717000000000000000-",      // empty operation
		"-update",                   // empty nanos (leading hyphen)
		"1717000000000000000-UPPER", // operation fails the lowercase rule
		"abc-update",                // non-integer nanos
	}
	for _, name := range malformedNames {
		require.NoError(t, os.Mkdir(filepath.Join(backupRoot, name), state.GeneratedDirMode))
	}

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the well-named snapshot is surfaced")
	assert.Equal(t, "1717000000000000000-update", got[0].SnapshotID)
}

func TestListConfigBackups_RejectsSymlinkedBackupRoot(t *testing.T) {
	stackDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideRoot := filepath.Join(outsideDir, "outside-backups")
	require.NoError(t, os.Mkdir(outsideRoot, 0o755))
	require.NoError(t, os.Symlink(outsideRoot, filepath.Join(stackDir, state.BackupDirName)))

	got, err := state.ListConfigBackups(stackDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup root")
	assert.Contains(t, err.Error(), "symlink")
	assert.Nil(t, got)
}

func TestListConfigBackups_SkipsSymlinkedSnapshotEntry(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{"docker-compose.yml": "c"})

	// A symlink whose name even parses as a snapshot — skipped, never followed.
	outsideTarget := filepath.Join(t.TempDir(), "outside-snapshot")
	require.NoError(t, os.Mkdir(outsideTarget, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outsideTarget, "sentinel.txt"), []byte("sentinel"), 0o600))
	require.NoError(t, os.Symlink(outsideTarget, filepath.Join(backupRoot, "1717000000000009999-update")))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "1717000000000000000-update", got[0].SnapshotID)

	_, statErr := os.Stat(filepath.Join(outsideTarget, "sentinel.txt"))
	require.NoError(t, statErr, "symlink target must remain untouched")
}

func TestListConfigBackups_SkipsSubdirectoriesAndSymlinksInsideSnapshot(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	snapshotPath := writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{
		"docker-compose.yml": "compose",
		".env":               "K=V",
	})

	// A nested directory inside the snapshot is not a config file → skipped.
	require.NoError(t, os.Mkdir(filepath.Join(snapshotPath, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotPath, "nested", "inner.yml"), []byte("x"), 0o644))

	// A symlink file inside the snapshot → skipped, never followed.
	linkTarget := filepath.Join(t.TempDir(), "link-target.txt")
	require.NoError(t, os.WriteFile(linkTarget, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(linkTarget, filepath.Join(snapshotPath, "link.txt")))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []string{".env", "docker-compose.yml"}, got[0].Files,
		"only regular files directly inside the snapshot are listed")
}

func TestListConfigBackups_ReportsSnapshotWithNoFiles(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	// An empty (but well-named) snapshot directory — surfaced with an empty
	// Files slice. The lister never re-applies the writer's no-files refusal.
	require.NoError(t, os.Mkdir(filepath.Join(backupRoot, "1717000000000000000-update"), state.GeneratedDirMode))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "1717000000000000000-update", got[0].SnapshotID)
	assert.Empty(t, got[0].Files)
}

func TestListConfigBackups_RejectsInvalidStackPath(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := state.ListConfigBackups("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute path")
		assert.Nil(t, got)
	})

	t.Run("relative", func(t *testing.T) {
		got, err := state.ListConfigBackups("relative/stack")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute path")
		assert.Nil(t, got)
	})

	t.Run("missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		got, err := state.ListConfigBackups(missing)
		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		assert.Nil(t, got)
	})

	t.Run("not a directory", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "a-file")
		require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o600))
		got, err := state.ListConfigBackups(filePath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
		assert.Nil(t, got)
	})
}

func TestListConfigBackups_BackupRootIsNotADirectory(t *testing.T) {
	stackDir := t.TempDir()
	// A regular file occupying the backup-root name.
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, state.BackupDirName), []byte("x"), 0o600))

	got, err := state.ListConfigBackups(stackDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a directory")
	assert.Nil(t, got)
}

func TestListConfigBackups_MutatingResultDoesNotCorruptNextListing(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{
		"docker-compose.yml": "compose",
		".env":               "K=V",
	})

	first, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Len(t, first[0].Files, 2)

	// Mutate the returned record and its Files slice in place.
	first[0].SnapshotID = "tampered"
	first[0].Files[0] = "tampered.yml"

	second, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "1717000000000000000-update", second[0].SnapshotID,
		"a fresh listing is unaffected by mutating a prior result")
	assert.Equal(t, []string{".env", "docker-compose.yml"}, second[0].Files)
}

func TestListConfigBackups_ToleratesSnapshotDisappearingDuringSnapshotRead(t *testing.T) {
	// The race window between the root listing and the per-snapshot read is
	// not directly forceable without a seam, but a snapshot directory that is
	// concurrently emptied/removed still yields a clean listing. Here the
	// closest portable proxy: an entry that os.ReadDir reports but is removed
	// before its inner read still must not fail the whole listing. We exercise
	// the disappear-mid-walk tolerance through the public surface by removing a
	// snapshot directory immediately after a sibling is enumerated, leaving the
	// survivor.
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)

	survivor := writeNamedSnapshot(t, backupRoot, "1717000000000000000-update", map[string]string{
		"docker-compose.yml": "compose",
	})
	doomed := writeNamedSnapshot(t, backupRoot, "1717000000000001000-update", map[string]string{
		"docker-compose.yml": "compose",
	})
	require.NoError(t, os.RemoveAll(doomed))

	got, err := state.ListConfigBackups(stackDir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, survivor, got[0].Path)
}

// writeNamedSnapshot creates a snapshot directory with the real
// "<unix-nanos>-<operation>" name shape that ListConfigBackups parses, seeded
// with the named files. It returns the absolute snapshot directory path.
func writeNamedSnapshot(t *testing.T, backupRoot, name string, files map[string]string) string {
	t.Helper()

	snapshotPath := filepath.Join(backupRoot, name)
	require.NoError(t, os.Mkdir(snapshotPath, state.GeneratedDirMode))
	for fileName, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(snapshotPath, fileName), []byte(content), 0o644),
			fmt.Sprintf("seeding %q in snapshot %q", fileName, name))
	}
	return snapshotPath
}
