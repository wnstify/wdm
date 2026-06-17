//go:build unix

package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigRestoreBoundaryNotice is the user-facing boundary for config
// restore operations.
const ConfigRestoreBoundaryNotice = "config restore restores only wdm config files; app data, databases, uploaded files, media libraries, and Docker volumes are unchanged"

type configRestoreFile struct {
	relativePath string
	sourcePath   string
	mode         os.FileMode
}

// RestoreConfigBackup copies a config backup snapshot back into stackPath.
// It restores config only: regular files from a stack-local backup snapshot,
// written through the same atomic write path as normal config writes. It does
// not touch app data, databases, uploaded files, media libraries, Docker
// volumes, running containers, or files outside stackPath.
func RestoreConfigBackup(stackPath string, snapshotPath string) error {
	if err := validateRestoreStackPath(stackPath); err != nil {
		return err
	}

	backupRoot := filepath.Join(stackPath, BackupDirName)
	if err := validateRestoreBackupRoot(backupRoot); err != nil {
		return err
	}

	snapshotName, err := normalizeRestoreSnapshotName(backupRoot, snapshotPath)
	if err != nil {
		return err
	}

	snapshotDir := filepath.Join(backupRoot, snapshotName)
	if err := validateRestoreSnapshotDirectory(snapshotDir); err != nil {
		return err
	}

	files, err := collectConfigRestoreFiles(snapshotDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("state.RestoreConfigBackup: config restore snapshot %q has no config files", snapshotDir)
	}

	for _, file := range files {
		destinationPath := filepath.Join(stackPath, file.relativePath)
		if err := validateRestoreDestinationAncestors(stackPath, file.relativePath); err != nil {
			return fmt.Errorf(
				"state.RestoreConfigBackup: validating config restore destination %q: %w",
				destinationPath,
				err,
			)
		}

		data, err := os.ReadFile(file.sourcePath)
		if err != nil {
			return fmt.Errorf(
				"state.RestoreConfigBackup: reading config restore source %q: %w",
				file.sourcePath,
				err,
			)
		}
		if err := backupWriteFile(destinationPath, data, file.mode.Perm()); err != nil {
			return fmt.Errorf(
				"state.RestoreConfigBackup: writing config restore destination %q: %w",
				destinationPath,
				err,
			)
		}
	}

	return nil
}

func validateRestoreStackPath(stackPath string) error {
	if stackPath == "" || !filepath.IsAbs(stackPath) {
		return fmt.Errorf("state.RestoreConfigBackup: stackPath must be a non-empty absolute path, got %q", stackPath)
	}

	stackInfo, err := os.Stat(stackPath)
	if err != nil {
		return fmt.Errorf("state.RestoreConfigBackup: stating stackPath %q for config restore: %w", stackPath, err)
	}
	if !stackInfo.IsDir() {
		return fmt.Errorf("state.RestoreConfigBackup: stackPath %q for config restore is not a directory", stackPath)
	}

	return nil
}

func validateRestoreBackupRoot(backupRoot string) error {
	backupRootInfo, err := os.Lstat(backupRoot)
	if err != nil {
		return fmt.Errorf("state.RestoreConfigBackup: stating backup root %q for config restore: %w", backupRoot, err)
	}
	if backupRootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state.RestoreConfigBackup: backup root %q for config restore must not be a symlink", backupRoot)
	}
	if !backupRootInfo.IsDir() {
		return fmt.Errorf("state.RestoreConfigBackup: backup root %q for config restore is not a directory", backupRoot)
	}

	return nil
}

func normalizeRestoreSnapshotName(backupRoot string, snapshotPath string) (string, error) {
	if snapshotPath == "" {
		return "", fmt.Errorf("state.RestoreConfigBackup: config restore snapshot must be non-empty")
	}
	if strings.Contains(snapshotPath, "."+string(filepath.Separator)) ||
		strings.Contains(snapshotPath, string(filepath.Separator)+"..") {
		return "", fmt.Errorf(
			"state.RestoreConfigBackup: config restore snapshot %q must not contain traversal components",
			snapshotPath,
		)
	}

	if !filepath.IsAbs(snapshotPath) {
		if strings.Contains(snapshotPath, string(filepath.Separator)) {
			return "", fmt.Errorf(
				"state.RestoreConfigBackup: config restore snapshot %q must be a direct snapshot basename",
				snapshotPath,
			)
		}
		normalized := filepath.Clean(snapshotPath)
		if normalized == "." || normalized == ".." || normalized == "" {
			return "", fmt.Errorf(
				"state.RestoreConfigBackup: config restore snapshot %q must be a direct snapshot basename",
				snapshotPath,
			)
		}
		return normalized, nil
	}

	relativeToRoot, err := filepath.Rel(backupRoot, filepath.Clean(snapshotPath))
	if err != nil {
		return "", fmt.Errorf(
			"state.RestoreConfigBackup: normalizing config restore snapshot %q relative to backup root %q: %w",
			snapshotPath,
			backupRoot,
			err,
		)
	}
	if relativeToRoot == "." || relativeToRoot == ".." ||
		filepath.IsAbs(relativeToRoot) ||
		strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"state.RestoreConfigBackup: config restore snapshot %q must be inside backup root %q",
			snapshotPath,
			backupRoot,
		)
	}

	normalized := filepath.Clean(relativeToRoot)
	if normalized == "." || normalized == ".." || normalized == "" ||
		strings.Contains(normalized, string(filepath.Separator)) {
		return "", fmt.Errorf(
			"state.RestoreConfigBackup: config restore snapshot %q must resolve to a snapshot basename",
			snapshotPath,
		)
	}

	return normalized, nil
}

func validateRestoreSnapshotDirectory(snapshotDir string) error {
	snapshotInfo, err := os.Lstat(snapshotDir)
	if err != nil {
		return fmt.Errorf("state.RestoreConfigBackup: stating config restore snapshot %q: %w", snapshotDir, err)
	}
	if snapshotInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state.RestoreConfigBackup: config restore snapshot %q must not be a symlink", snapshotDir)
	}
	if !snapshotInfo.IsDir() {
		return fmt.Errorf("state.RestoreConfigBackup: config restore snapshot %q is not a directory", snapshotDir)
	}

	return nil
}

func collectConfigRestoreFiles(snapshotDir string) ([]configRestoreFile, error) {
	files := make([]configRestoreFile, 0, 4)
	err := filepath.WalkDir(snapshotDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking config restore snapshot %q: %w", path, err)
		}
		if path == snapshotDir {
			return nil
		}

		relativePath, err := filepath.Rel(snapshotDir, path)
		if err != nil {
			return fmt.Errorf(
				"normalizing config restore snapshot entry %q relative to %q: %w",
				path,
				snapshotDir,
				err,
			)
		}
		if err := validateConfigRestoreRelativePath(relativePath); err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stating config restore snapshot entry %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("config restore snapshot entry %q must not be a symlink", path)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config restore snapshot entry %q must be a regular file", path)
		}
		if !isManagedConfigRestorePath(filepath.Clean(relativePath)) {
			return fmt.Errorf("config restore snapshot entry %q is not a managed config path", relativePath)
		}

		files = append(files, configRestoreFile{
			relativePath: filepath.Clean(relativePath),
			sourcePath:   path,
			mode:         info.Mode(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("state.RestoreConfigBackup: collecting config restore files from %q: %w", snapshotDir, err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].relativePath < files[j].relativePath
	})
	return files, nil
}

func validateConfigRestoreRelativePath(relativePath string) error {
	cleaned := filepath.Clean(relativePath)
	if relativePath == "" || cleaned == "." || cleaned == ".." ||
		filepath.IsAbs(relativePath) ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("config restore snapshot entry %q must stay inside the snapshot", relativePath)
	}
	if cleaned == BackupDirName || strings.HasPrefix(cleaned, BackupDirName+string(filepath.Separator)) {
		return fmt.Errorf("config restore snapshot entry %q must not target %q", relativePath, BackupDirName)
	}

	return nil
}

func isManagedConfigRestorePath(relativePath string) bool {
	if hasConfigRestoreDataComponent(relativePath) {
		return false
	}

	baseName := filepath.Base(relativePath)
	switch relativePath {
	case "docker-compose.yml", ".env", stackLockFilename, "Caddyfile", "init-data.sh", "nginx.conf":
		return true
	}
	switch baseName {
	case "Caddyfile", "docker-compose.yml", "guidance.txt", "init-data.sh", "nginx.conf":
		return true
	}
	extension := filepath.Ext(baseName)
	switch extension {
	case ".conf", ".json", ".sh", ".toml", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func hasConfigRestoreDataComponent(relativePath string) bool {
	for _, component := range strings.Split(relativePath, string(filepath.Separator)) {
		switch strings.ToLower(component) {
		case "app", "app-data", "apps",
			"cache",
			"data", "database", "databases", "db",
			"file", "files",
			"log", "logs",
			"mariadb", "media", "mysql",
			"postgres", "postgresql",
			"redis",
			"storage",
			"upload", "uploads",
			"volume", "volumes":
			return true
		}
	}
	return false
}

func validateRestoreDestinationAncestors(stackPath string, relativePath string) error {
	parentPath := filepath.Dir(filepath.Clean(relativePath))
	if parentPath == "." {
		return nil
	}

	currentPath := stackPath
	for _, component := range strings.Split(parentPath, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}

		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stating config restore destination path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("config restore destination path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("config restore destination path component %q is not a directory", currentPath)
		}
	}

	return nil
}
