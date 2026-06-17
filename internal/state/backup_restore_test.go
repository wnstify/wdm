//go:build unix

package state_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

func TestRestoreConfigBackup_RestoresConfigFilesAndPreservesModes(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	snapshotPath := filepath.Join(backupRoot, "snap-restore")

	writeFileWithMode(t, filepath.Join(snapshotPath, "docker-compose.yml"), []byte("services:\n  app:\n    image: old\n"), 0o644)
	writeFileWithMode(t, filepath.Join(snapshotPath, ".env"), []byte("TOKEN=old\n"), 0o600)
	writeFileWithMode(t, filepath.Join(snapshotPath, ".wdm.lock"), []byte(`{"schema_version":1}`), 0o640)
	writeFileWithMode(t, filepath.Join(snapshotPath, "proxy", "guidance.txt"), []byte("old guidance"), 0o640)
	writeFileWithMode(t, filepath.Join(snapshotPath, "init-data.sh"), []byte("#!/bin/sh\necho old\n"), 0o755)
	writeFileWithMode(t, filepath.Join(snapshotPath, "Caddyfile"), []byte("example.test\n"), 0o644)

	writeFileWithMode(t, filepath.Join(stackDir, "docker-compose.yml"), []byte("new compose"), 0o600)
	writeFileWithMode(t, filepath.Join(stackDir, ".env"), []byte("TOKEN=new\n"), 0o644)
	writeFileWithMode(t, filepath.Join(stackDir, ".wdm.lock"), []byte(`{"schema_version":2}`), 0o600)
	writeFileWithMode(t, filepath.Join(stackDir, "proxy", "guidance.txt"), []byte("new guidance"), 0o600)
	writeFileWithMode(t, filepath.Join(stackDir, "init-data.sh"), []byte("#!/bin/sh\necho new\n"), 0o644)
	writeFileWithMode(t, filepath.Join(stackDir, "Caddyfile"), []byte("new.example.test\n"), 0o600)
	unrelatedPath := filepath.Join(stackDir, "app-data.sqlite")
	writeFileWithMode(t, unrelatedPath, []byte("app data stays"), 0o600)

	err := state.RestoreConfigBackup(stackDir, "snap-restore")
	require.NoError(t, err)

	assertFileBytesAndMode(t, filepath.Join(stackDir, "docker-compose.yml"), []byte("services:\n  app:\n    image: old\n"), 0o644)
	assertFileBytesAndMode(t, filepath.Join(stackDir, ".env"), []byte("TOKEN=old\n"), 0o600)
	assertFileBytesAndMode(t, filepath.Join(stackDir, ".wdm.lock"), []byte(`{"schema_version":1}`), 0o640)
	assertFileBytesAndMode(t, filepath.Join(stackDir, "proxy", "guidance.txt"), []byte("old guidance"), 0o640)
	assertFileBytesAndMode(t, filepath.Join(stackDir, "init-data.sh"), []byte("#!/bin/sh\necho old\n"), 0o755)
	assertFileBytesAndMode(t, filepath.Join(stackDir, "Caddyfile"), []byte("example.test\n"), 0o644)
	assertFileBytesAndMode(t, unrelatedPath, []byte("app data stays"), 0o600)
}

func TestRestoreConfigBackup_AcceptsAbsoluteSnapshotPathInsideBackupRoot(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	snapshotPath := filepath.Join(backupRoot, "snap-absolute")
	writeFileWithMode(t, filepath.Join(snapshotPath, "docker-compose.yml"), []byte("services: {}\n"), 0o644)

	err := state.RestoreConfigBackup(stackDir, snapshotPath)
	require.NoError(t, err)

	assertFileBytesAndMode(t, filepath.Join(stackDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644)
}

func TestRestoreConfigBackup_RejectsMissingBackupRootAndSnapshot(t *testing.T) {
	stackDir := secureTempStackDir(t)

	err := state.RestoreConfigBackup(stackDir, "missing-snapshot")
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.Contains(t, err.Error(), "config restore")

	backupRoot := createBackupRoot(t, stackDir)
	require.NoError(t, os.WriteFile(filepath.Join(backupRoot, "loose-file"), []byte("not a dir"), 0o644))

	err = state.RestoreConfigBackup(stackDir, "loose-file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot")
	assert.Contains(t, err.Error(), "directory")
}

func TestRestoreConfigBackup_RejectsSymlinkedBackupRootAndSnapshot(t *testing.T) {
	stackDir := secureTempStackDir(t)
	outsideRoot := filepath.Join(t.TempDir(), "outside-backups")
	require.NoError(t, os.Mkdir(outsideRoot, 0o755))
	require.NoError(t, os.Symlink(outsideRoot, filepath.Join(stackDir, state.BackupDirName)))

	err := state.RestoreConfigBackup(stackDir, "snap")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup root")
	assert.Contains(t, err.Error(), "symlink")

	stackDir = secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	outsideSnapshot := filepath.Join(t.TempDir(), "outside-snapshot")
	require.NoError(t, os.Mkdir(outsideSnapshot, 0o755))
	require.NoError(t, os.Symlink(outsideSnapshot, filepath.Join(backupRoot, "snap-link")))

	err = state.RestoreConfigBackup(stackDir, "snap-link")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot")
	assert.Contains(t, err.Error(), "symlink")
}

func TestRestoreConfigBackup_RejectsSymlinkSnapshotEntry(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	snapshotPath := filepath.Join(backupRoot, "snap-symlink-entry")
	require.NoError(t, os.Mkdir(snapshotPath, 0o755))

	outsideTarget := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outsideTarget, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(outsideTarget, filepath.Join(snapshotPath, "docker-compose.yml")))

	err := state.RestoreConfigBackup(stackDir, "snap-symlink-entry")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	_, statErr := os.Stat(filepath.Join(stackDir, "docker-compose.yml"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestRestoreConfigBackup_RejectsUnsafeSnapshotPathInputs(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	writeFileWithMode(t, filepath.Join(backupRoot, "snap-good", "docker-compose.yml"), []byte("services: {}\n"), 0o644)

	tests := []string{
		"nested/snap",
		"nested/../snap-good",
		"./snap-good",
		"../snap-good",
		filepath.Join(t.TempDir(), "snap-outside"),
		backupRoot + string(filepath.Separator) + "nested" + string(filepath.Separator) + ".." + string(filepath.Separator) + "snap-good",
		filepath.Join(backupRoot, "snap-good", "nested"),
	}

	for _, snapshotPath := range tests {
		t.Run(strings.ReplaceAll(snapshotPath, string(filepath.Separator), "_"), func(t *testing.T) {
			err := state.RestoreConfigBackup(stackDir, snapshotPath)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "snapshot")
		})
	}
}

func TestRestoreConfigBackup_RejectsUnsafeSnapshotEntries(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)

	tests := []struct {
		name          string
		buildSnapshot func(t *testing.T, snapshotPath string)
		wantContains  string
	}{
		{
			name: "backup_root_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, state.BackupDirName, "nested"), []byte("bad"), 0o644)
			},
			wantContains: state.BackupDirName,
		},
		{
			name: "app_data_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "app-data.sqlite"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_data_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "data", "db.sqlite"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_app_data_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "app", "data.sqlite"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_upload_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "service", "uploads", "file.jpg"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_upload_guidance_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "service", "uploads", "guidance.txt"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_app_compose_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "app", "docker-compose.yml"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_app_config_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "app", "config.yaml"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "nested_data_json_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "data", "settings.json"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "case_variant_data_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "Data", "settings.json"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "case_variant_app_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "App", "docker-compose.yml"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "case_variant_upload_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				writeFileWithMode(t, filepath.Join(snapshotPath, "Service", "Uploads", "guidance.txt"), []byte("bad"), 0o644)
			},
			wantContains: "not a managed config path",
		},
		{
			name: "fifo_entry",
			buildSnapshot: func(t *testing.T, snapshotPath string) {
				require.NoError(t, os.Mkdir(snapshotPath, 0o755))
				require.NoError(t, syscall.Mkfifo(filepath.Join(snapshotPath, "docker-compose.yml"), 0o600))
			},
			wantContains: "regular file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshotPath := filepath.Join(backupRoot, tc.name)
			tc.buildSnapshot(t, snapshotPath)

			err := state.RestoreConfigBackup(stackDir, tc.name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantContains)
		})
	}
}

func TestRestoreConfigBackup_RejectsDestinationSymlinkAncestor(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	writeFileWithMode(t, filepath.Join(backupRoot, "snap-symlink-parent", "proxy", "guidance.txt"), []byte("restored"), 0o644)

	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "guidance.txt")
	require.NoError(t, os.WriteFile(outsidePath, []byte("outside unchanged"), 0o600))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(stackDir, "proxy")))

	err := state.RestoreConfigBackup(stackDir, "snap-symlink-parent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	got, readErr := os.ReadFile(outsidePath)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("outside unchanged"), got)
}

func TestRestoreConfigBackup_SurfacesWriteFailureAndStopsBeforeLaterFiles(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	writeFileWithMode(t, filepath.Join(backupRoot, "snap-write-failure", "docker-compose.yml"), []byte("new compose"), 0o644)
	writeFileWithMode(t, filepath.Join(backupRoot, "snap-write-failure", "proxy", "guidance.txt"), []byte("new guidance"), 0o644)
	writeFileWithMode(t, filepath.Join(stackDir, "docker-compose.yml"), []byte("old compose"), 0o644)
	writeFileWithMode(t, filepath.Join(stackDir, "proxy", "guidance.txt"), []byte("old guidance"), 0o644)

	state.SwapBackupWriteFileForTest(t, func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == "docker-compose.yml" {
			return fmt.Errorf("forced restore write failure")
		}
		return state.WriteFileAtomic(path, data, mode)
	})

	err := state.RestoreConfigBackup(stackDir, "snap-write-failure")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced restore write failure")

	assertFileBytesAndMode(t, filepath.Join(stackDir, "docker-compose.yml"), []byte("old compose"), 0o644)
	assertFileBytesAndMode(t, filepath.Join(stackDir, "proxy", "guidance.txt"), []byte("old guidance"), 0o644)
}

func TestRestoreConfigBackup_EmptySnapshotFails(t *testing.T) {
	stackDir := secureTempStackDir(t)
	backupRoot := createBackupRoot(t, stackDir)
	require.NoError(t, os.Mkdir(filepath.Join(backupRoot, "snap-empty"), 0o755))

	err := state.RestoreConfigBackup(stackDir, "snap-empty")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no config files")
}

func TestRestoreConfigBackup_ProductionStringsAvoidForbiddenRestoreTerms(t *testing.T) {
	t.Parallel()

	forbidden := []string{"rollback", "data rollback", "database rollback", "volume rollback"}
	productionFiles := productionGoFiles(t, ".")

	for _, path := range productionFiles {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			fileSet := token.NewFileSet()
			parsed, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
			require.NoError(t, err)

			ast.Inspect(parsed, func(node ast.Node) bool {
				lit, ok := node.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}

				value, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				lowerValue := strings.ToLower(value)
				for _, term := range forbidden {
					assert.NotContains(t, lowerValue, term, "%s contains forbidden restore wording", path)
				}
				return true
			})
		})
	}
}

func productionGoFiles(t *testing.T, root string) []string {
	t.Helper()

	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	require.NoError(t, err)

	sort.Strings(paths)
	return paths
}

func secureTempStackDir(t *testing.T) string {
	t.Helper()

	stackDir := t.TempDir()
	require.NoError(t, os.Chmod(stackDir, 0o700))
	return stackDir
}

func writeFileWithMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, mode))
	require.NoError(t, os.Chmod(path, mode))
}

func assertFileBytesAndMode(t *testing.T, path string, wantBytes []byte, wantMode os.FileMode) {
	t.Helper()

	gotBytes, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, wantBytes, gotBytes)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, wantMode, info.Mode().Perm())
}
