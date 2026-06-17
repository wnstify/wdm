//go:build unix

package state_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

func TestPruneConfigBackups_KeepsNewestTenByMtime(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748890000000000000).UTC()

	pathsByName := make(map[string]string, 12)
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.NoError(t, err)
	require.Equal(t, []string{
		pathsByName["snap-01"],
		pathsByName["snap-02"],
	}, removed)

	assertSnapshotMissing(t, pathsByName["snap-01"])
	assertSnapshotMissing(t, pathsByName["snap-02"])
	for i := 3; i <= 12; i++ {
		assertSnapshotExists(t, pathsByName[fmt.Sprintf("snap-%02d", i)])
	}
}

func TestPruneConfigBackups_KeepsPinnedOldSnapshot(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748891000000000000).UTC()

	pathsByName := make(map[string]string, 12)
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	removed, err := state.PruneConfigBackups(stackDir, "snap-01")
	require.NoError(t, err)
	require.Equal(t, []string{
		pathsByName["snap-02"],
		pathsByName["snap-03"],
	}, removed)

	assertSnapshotExists(t, pathsByName["snap-01"])
	assertSnapshotMissing(t, pathsByName["snap-02"])
	assertSnapshotMissing(t, pathsByName["snap-03"])
}

func TestPruneConfigBackups_AcceptsAbsolutePinnedPathInsideBackupRoot(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748892000000000000).UTC()

	pathsByName := make(map[string]string, 11)
	for i := 1; i <= 11; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	removed, err := state.PruneConfigBackups(stackDir, pathsByName["snap-01"])
	require.NoError(t, err)
	require.Equal(t, []string{pathsByName["snap-02"]}, removed)

	assertSnapshotExists(t, pathsByName["snap-01"])
	assertSnapshotMissing(t, pathsByName["snap-02"])
}

func TestPruneConfigBackups_MissingBackupRootIsNoOp(t *testing.T) {
	stackDir := t.TempDir()

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestPruneConfigBackups_RejectsSymlinkedBackupRoot(t *testing.T) {
	stackDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideRoot := filepath.Join(outsideDir, "outside-backups")
	require.NoError(t, os.Mkdir(outsideRoot, 0o755))
	require.NoError(t, os.Symlink(outsideRoot, filepath.Join(stackDir, state.BackupDirName)))

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup root")
	assert.Contains(t, err.Error(), "symlink")
	assert.Empty(t, removed)
}

func TestPruneConfigBackups_RejectsSymlinkedSnapshotEntry(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748893000000000000).UTC()

	for i := 1; i <= 11; i++ {
		createSnapshotDir(t, backupRoot, fmt.Sprintf("snap-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	outsideTarget := filepath.Join(t.TempDir(), "outside-snapshot")
	require.NoError(t, os.Mkdir(outsideTarget, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outsideTarget, "sentinel.txt"), []byte("sentinel"), 0o600))
	require.NoError(t, os.Symlink(outsideTarget, filepath.Join(backupRoot, "snap-link")))

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	assert.Empty(t, removed)

	_, statErr := os.Stat(filepath.Join(outsideTarget, "sentinel.txt"))
	require.NoError(t, statErr, "symlink target must remain untouched")
}

func TestPruneConfigBackups_IgnoresNonDirectoryEntries(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748894000000000000).UTC()

	pathsByName := make(map[string]string, 11)
	for i := 1; i <= 11; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	looseFile := filepath.Join(backupRoot, "readme.txt")
	require.NoError(t, os.WriteFile(looseFile, []byte("do not touch"), 0o644))

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.NoError(t, err)
	require.Equal(t, []string{pathsByName["snap-01"]}, removed)

	assertSnapshotMissing(t, pathsByName["snap-01"])
	_, statErr := os.Stat(looseFile)
	require.NoError(t, statErr, "regular files under backup root are intentionally ignored")
}

func TestPruneConfigBackups_SurfacesSyncFailureAfterRemoval(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748895000000000000).UTC()

	pathsByName := make(map[string]string, 11)
	for i := 1; i <= 11; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	state.SwapBackupSyncDirectoryForTest(t, func(path string) error {
		if path == backupRoot {
			return fmt.Errorf("forced backup-root sync failure")
		}
		return state.SyncDirectory(path)
	})

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced backup-root sync failure")
	require.Equal(t, []string{pathsByName["snap-01"]}, removed)
	assertSnapshotMissing(t, pathsByName["snap-01"])
}

func TestPruneConfigBackups_RemovedPathsAreDeterministic(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	fixed := time.Unix(0, 1748896000000000000).UTC()

	pathsByName := map[string]string{
		"snap-a": createSnapshotDir(t, backupRoot, "snap-a", fixed),
		"snap-b": createSnapshotDir(t, backupRoot, "snap-b", fixed),
		"snap-c": createSnapshotDir(t, backupRoot, "snap-c", fixed),
		"snap-d": createSnapshotDir(t, backupRoot, "snap-d", fixed),
		"snap-e": createSnapshotDir(t, backupRoot, "snap-e", fixed),
		"snap-f": createSnapshotDir(t, backupRoot, "snap-f", fixed),
		"snap-g": createSnapshotDir(t, backupRoot, "snap-g", fixed),
		"snap-h": createSnapshotDir(t, backupRoot, "snap-h", fixed),
		"snap-i": createSnapshotDir(t, backupRoot, "snap-i", fixed),
		"snap-j": createSnapshotDir(t, backupRoot, "snap-j", fixed),
		"snap-k": createSnapshotDir(t, backupRoot, "snap-k", fixed),
		"snap-l": createSnapshotDir(t, backupRoot, "snap-l", fixed),
	}

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.NoError(t, err)
	require.Equal(t, []string{
		pathsByName["snap-a"],
		pathsByName["snap-b"],
	}, removed)
}

func TestPruneConfigBackups_SyncsBackupRootOnPartialRemovalFailure(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748897000000000000).UTC()

	pathsByName := make(map[string]string, 12)
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("snap-%02d", i)
		pathsByName[name] = createSnapshotDir(t, backupRoot, name, base.Add(time.Duration(i)*time.Minute))
	}

	state.SwapBackupRemoveAllForTest(t, func(path string) error {
		if path == pathsByName["snap-02"] {
			return fmt.Errorf("forced remove failure")
		}
		return os.RemoveAll(path)
	})

	syncCalls := 0
	state.SwapBackupSyncDirectoryForTest(t, func(path string) error {
		if path == backupRoot {
			syncCalls++
		}
		return state.SyncDirectory(path)
	})

	removed, err := state.PruneConfigBackups(stackDir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced remove failure")
	assert.Equal(t, 1, syncCalls, "backup root must be synced after at least one deletion on partial failure")
	require.Equal(t, []string{pathsByName["snap-01"]}, removed)

	assertSnapshotMissing(t, pathsByName["snap-01"])
	assertSnapshotExists(t, pathsByName["snap-02"])
}

func TestPruneConfigBackups_RejectsAbsolutePinnedPathOutsideBackupRoot(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748898000000000000).UTC()
	for i := 1; i <= 11; i++ {
		createSnapshotDir(t, backupRoot, fmt.Sprintf("snap-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	outsidePinnedPath := filepath.Join(t.TempDir(), "snap-outside")
	removed, err := state.PruneConfigBackups(stackDir, outsidePinnedPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be inside backup root")
	assert.Empty(t, removed)
}

func TestPruneConfigBackups_RejectsRelativePinnedPathWithTraversalOrSeparator(t *testing.T) {
	stackDir := t.TempDir()
	backupRoot := createBackupRoot(t, stackDir)
	base := time.Unix(0, 1748899000000000000).UTC()
	for i := 1; i <= 11; i++ {
		createSnapshotDir(t, backupRoot, fmt.Sprintf("snap-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	testInputs := []string{
		"nested/snap-01",
		"nested/../snap-01",
		"./snap-01",
		"../snap-01",
	}
	for _, pinned := range testInputs {
		t.Run(strings.ReplaceAll(pinned, "/", "_"), func(t *testing.T) {
			removed, err := state.PruneConfigBackups(stackDir, pinned)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "basename")
			assert.Empty(t, removed)
		})
	}
}

func createBackupRoot(t *testing.T, stackDir string) string {
	t.Helper()

	backupRoot := filepath.Join(stackDir, state.BackupDirName)
	require.NoError(t, os.Mkdir(backupRoot, 0o755))
	return backupRoot
}

func createSnapshotDir(t *testing.T, backupRoot, snapshotName string, modifiedAt time.Time) string {
	t.Helper()

	snapshotPath := filepath.Join(backupRoot, snapshotName)
	require.NoError(t, os.Mkdir(snapshotPath, state.GeneratedDirMode))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotPath, "docker-compose.yml"), []byte(snapshotName), 0o644))
	require.NoError(t, os.Chtimes(snapshotPath, modifiedAt, modifiedAt))
	return snapshotPath
}

func assertSnapshotExists(t *testing.T, snapshotPath string) {
	t.Helper()

	info, err := os.Stat(snapshotPath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func assertSnapshotMissing(t *testing.T, snapshotPath string) {
	t.Helper()

	_, err := os.Stat(snapshotPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
