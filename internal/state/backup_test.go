//go:build unix

package state_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

func TestCreateConfigBackup_CopiesStandardFilesAndPreservesModes(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293841).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	composeData := []byte("services:\n  app:\n    image: example:1\n")
	envData := []byte("TOKEN=abc123\n")
	lockData := []byte(`{"schema_version":1}`)

	composePath := filepath.Join(stackDir, "docker-compose.yml")
	envPath := filepath.Join(stackDir, ".env")
	lockPath := filepath.Join(stackDir, ".wdm.lock")
	dataPath := filepath.Join(stackDir, "app-data.sqlite")

	require.NoError(t, os.WriteFile(composePath, composeData, 0o644))
	require.NoError(t, os.WriteFile(envPath, envData, 0o600))
	require.NoError(t, os.WriteFile(lockPath, lockData, 0o640))
	require.NoError(t, os.WriteFile(dataPath, []byte("db-bytes"), 0o600))

	gotSnapshot, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.NoError(t, err)

	wantSnapshot := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	assert.Equal(t, wantSnapshot, gotSnapshot)

	backupRootInfo, err := os.Stat(filepath.Join(stackDir, state.BackupDirName))
	require.NoError(t, err)
	require.True(t, backupRootInfo.IsDir())
	assert.Equal(t, state.GeneratedDirMode, backupRootInfo.Mode().Perm())

	snapshotInfo, err := os.Stat(gotSnapshot)
	require.NoError(t, err)
	require.True(t, snapshotInfo.IsDir())
	assert.Equal(t, state.GeneratedDirMode, snapshotInfo.Mode().Perm())

	assertCopiedFile(t, composePath, filepath.Join(gotSnapshot, "docker-compose.yml"))
	assertCopiedFile(t, envPath, filepath.Join(gotSnapshot, ".env"))
	assertCopiedFile(t, lockPath, filepath.Join(gotSnapshot, ".wdm.lock"))

	_, err = os.Stat(filepath.Join(gotSnapshot, "app-data.sqlite"))
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"unrelated app data must not be copied unless explicitly requested")
}

func TestCreateConfigBackup_SkipsMissingOptionalFilesWhenAtLeastOneExists(t *testing.T) {
	stackDir := t.TempDir()
	state.SwapBackupNowForTest(t, func() time.Time {
		return time.Unix(0, 1747752731487293842).UTC()
	})

	composePath := filepath.Join(stackDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("services: {}\n"), 0o644))

	snapshotPath, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.NoError(t, err)

	assertCopiedFile(t, composePath, filepath.Join(snapshotPath, "docker-compose.yml"))

	_, err = os.Stat(filepath.Join(snapshotPath, ".env"))
	assert.True(t, errors.Is(err, os.ErrNotExist))

	_, err = os.Stat(filepath.Join(snapshotPath, ".wdm.lock"))
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestCreateConfigBackup_CopiesNestedAdditionalRelativePaths(t *testing.T) {
	stackDir := t.TempDir()
	state.SwapBackupNowForTest(t, func() time.Time {
		return time.Unix(0, 1747752731487293843).UTC()
	})

	composePath := filepath.Join(stackDir, "docker-compose.yml")
	additionalOne := filepath.Join(stackDir, "proxy", "guidance.txt")
	additionalTwo := filepath.Join(stackDir, "init", "scripts", "init-data.sh")

	require.NoError(t, os.WriteFile(composePath, []byte("services: {}\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Dir(additionalOne), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(additionalTwo), 0o755))
	require.NoError(t, os.WriteFile(additionalOne, []byte("proxy instructions"), 0o640))
	require.NoError(t, os.WriteFile(additionalTwo, []byte("#!/bin/sh\necho ready\n"), 0o755))

	snapshotPath, err := state.CreateConfigBackup(
		stackDir,
		"update",
		[]string{"proxy/guidance.txt", "init/scripts/init-data.sh", "proxy/guidance.txt"},
	)
	require.NoError(t, err)

	assertCopiedFile(t, additionalOne, filepath.Join(snapshotPath, "proxy", "guidance.txt"))
	assertCopiedFile(t, additionalTwo, filepath.Join(snapshotPath, "init", "scripts", "init-data.sh"))
}

func TestCreateConfigBackup_RejectsInvalidStackPath(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o600))

	tests := []struct {
		name      string
		stackPath string
		assertErr func(t *testing.T, err error)
	}{
		{
			name:      "relative_path",
			stackPath: "relative/stack",
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "absolute")
			},
		},
		{
			name:      "missing_path",
			stackPath: filepath.Join(t.TempDir(), "missing"),
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, errors.Is(err, os.ErrNotExist))
			},
		},
		{
			name:      "file_path",
			stackPath: filePath,
			assertErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "directory")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := state.CreateConfigBackup(tc.stackPath, "update", nil)
			tc.assertErr(t, err)
		})
	}
}

func TestCreateConfigBackup_RejectsUnsafeOperationNames(t *testing.T) {
	stackDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	tests := []string{"", "Update", "update now", "update/v2", ".", "..", "update_1"}
	for _, operation := range tests {
		t.Run(operation, func(t *testing.T) {
			_, err := state.CreateConfigBackup(stackDir, operation, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "operation")
		})
	}
}

func TestCreateConfigBackup_RejectsUnsafeAdditionalPaths(t *testing.T) {
	stackDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	tests := []string{
		"",
		"/abs/path",
		".",
		"..",
		"../escape",
		"nested/../../escape",
		".wdm-backups",
		".wdm-backups/snapshot",
		".wdm-backups-old/snapshot",
	}

	for _, relPath := range tests {
		t.Run(relPath, func(t *testing.T) {
			_, err := state.CreateConfigBackup(stackDir, "update", []string{relPath})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "additional")
		})
	}
}

func TestCreateConfigBackup_RejectsNonRegularSources(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293846).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	require.NoError(t, os.Mkdir(filepath.Join(stackDir, "not-a-file"), 0o755))
	_, err := state.CreateConfigBackup(stackDir, "update", []string{"not-a-file"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "regular file")

	snapshotPath := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	_, statErr := os.Stat(snapshotPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"snapshot directory must be removed after non-regular source validation failure")
}

func TestCreateConfigBackup_RejectsSymlinkSources(t *testing.T) {
	stackDir := t.TempDir()
	target := filepath.Join(stackDir, "compose-real.yml")
	link := filepath.Join(stackDir, "docker-compose.yml")
	require.NoError(t, os.WriteFile(target, []byte("services: {}\n"), 0o644))
	require.NoError(t, os.Symlink(target, link))

	_, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "regular file")
}

func TestCreateConfigBackup_ReturnsCollisionErrorWithoutOverwriting(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293844).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	snapshotPath := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	require.NoError(t, os.MkdirAll(snapshotPath, 0o755))
	sentinelPath := filepath.Join(snapshotPath, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinelPath, []byte("keep-me"), 0o600))

	_, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot")

	got, readErr := os.ReadFile(sentinelPath)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("keep-me"), got)
}

func TestCreateConfigBackup_ErrorsWhenNoConfigFilesCopiedAndRemovesEmptySnapshot(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293845).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	_, err := state.CreateConfigBackup(stackDir, "update", []string{"missing-guidance.txt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no files")

	snapshotPath := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	_, statErr := os.Stat(snapshotPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"empty snapshot directory must be removed on zero-file backup")
}

func TestCreateConfigBackup_RemovesSnapshotOnWriteFailure(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293847).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	state.SwapBackupWriteFileForTest(
		t,
		func(_ string, _ []byte, _ os.FileMode) error {
			return fmt.Errorf("forced write failure")
		},
	)

	_, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced write failure")

	snapshotPath := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	_, statErr := os.Stat(snapshotPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"snapshot directory must be removed after backup file write failure")
}

func TestCreateConfigBackup_RemovesSnapshotWhenPostMkdirStepFails(t *testing.T) {
	stackDir := t.TempDir()
	fixed := time.Unix(0, 1747752731487293848).UTC()
	state.SwapBackupNowForTest(t, func() time.Time { return fixed })

	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(stackDir, state.BackupDirName), 0o755))

	backupRootPath := filepath.Join(stackDir, state.BackupDirName)
	state.SwapBackupSyncDirectoryForTest(
		t,
		func(path string) error {
			if path == backupRootPath {
				return fmt.Errorf("forced backup-root sync failure")
			}
			return state.SyncDirectory(path)
		},
	)

	_, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced backup-root sync failure")

	snapshotPath := filepath.Join(stackDir, state.BackupDirName, fmt.Sprintf("%d-update", fixed.UnixNano()))
	_, statErr := os.Stat(snapshotPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"snapshot directory must be removed when a post-mkdir helper step fails")
}

func TestCreateConfigBackup_RejectsSymlinkedBackupRoot(t *testing.T) {
	stackDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	outsideDir := t.TempDir()
	outsideBackupRoot := filepath.Join(outsideDir, "outside-backups")
	require.NoError(t, os.Mkdir(outsideBackupRoot, 0o755))
	sentinelPath := filepath.Join(outsideBackupRoot, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinelPath, []byte("sentinel"), 0o600))

	backupRootPath := filepath.Join(stackDir, state.BackupDirName)
	require.NoError(t, os.Symlink(outsideBackupRoot, backupRootPath))

	_, err := state.CreateConfigBackup(stackDir, "update", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup root")

	entries, readErr := os.ReadDir(outsideBackupRoot)
	require.NoError(t, readErr)
	require.Len(t, entries, 1, "outside backup root must remain untouched")
	assert.Equal(t, "sentinel.txt", entries[0].Name())
}

func TestCreateConfigBackup_RejectsSymlinkedAncestorInAdditionalPath(t *testing.T) {
	stackDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))

	outsideDir := t.TempDir()
	outsideFilePath := filepath.Join(outsideDir, "guidance.txt")
	require.NoError(t, os.WriteFile(outsideFilePath, []byte("outside-guidance"), 0o600))

	require.NoError(t, os.Symlink(outsideDir, filepath.Join(stackDir, "proxy")))

	_, err := state.CreateConfigBackup(stackDir, "update", []string{"proxy/guidance.txt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	backupRoot := filepath.Join(stackDir, state.BackupDirName)
	if _, statErr := os.Stat(backupRoot); statErr == nil {
		entries, readErr := os.ReadDir(backupRoot)
		require.NoError(t, readErr)
		require.Len(t, entries, 0, "no snapshots should remain after additional-path ancestor rejection")
	}
}

func assertCopiedFile(t *testing.T, sourcePath, backupPath string) {
	t.Helper()

	sourceData, err := os.ReadFile(sourcePath)
	require.NoError(t, err)

	backupData, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, sourceData, backupData)

	sourceInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)
	backupInfo, err := os.Stat(backupPath)
	require.NoError(t, err)
	assert.Equal(t, sourceInfo.Mode().Perm(), backupInfo.Mode().Perm())
}
