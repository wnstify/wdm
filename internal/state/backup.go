//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// BackupDirName is the managed directory under a stack path where
	// pre-operation config snapshots are stored.
	BackupDirName = ".wdm-backups"

	// BackupRetentionLimit is the hard cap for retained snapshots per stack.
	BackupRetentionLimit = 10
)

var (
	backupNowUTC         = func() time.Time { return time.Now().UTC() }
	backupOperationRegex = regexp.MustCompile(`^[a-z0-9-]+$`)
	backupWriteFile      = WriteFileAtomic
	backupSyncDirectory  = SyncDirectory
	backupRemoveAll      = os.RemoveAll
)

type backupCandidate struct {
	relativePath string
	sourcePath   string
	mode         os.FileMode
}

type backupSnapshotDirectory struct {
	name       string
	path       string
	modifiedAt time.Time
}

// CreateConfigBackup snapshots managed config files into a collision-safe
// directory under <stackPath>/.wdm-backups/<unix-nanos>-<operation>.
// It copies only regular files that exist; missing optional files are
// skipped. When no files are copied, the new snapshot directory is removed
// and an error is returned.
func CreateConfigBackup(
	stackPath string,
	operation string,
	additionalRelativePaths []string,
) (snapshotPath string, err error) {
	if err := validateBackupStackPath("state.CreateConfigBackup", stackPath); err != nil {
		return "", err
	}
	if err := validateBackupOperation(operation); err != nil {
		return "", err
	}

	dedupedAdditional, err := normalizeBackupAdditionalPaths(additionalRelativePaths)
	if err != nil {
		return "", err
	}

	backupRoot := filepath.Join(stackPath, BackupDirName)
	if err := ensureBackupRootDirectory(stackPath, backupRoot); err != nil {
		return "", err
	}

	snapshotName := fmt.Sprintf("%d-%s", backupNowUTC().UnixNano(), operation)
	snapshotDir := filepath.Join(backupRoot, snapshotName)
	if err := createBackupSnapshotDirectory(backupRoot, snapshotDir); err != nil {
		return "", err
	}
	snapshotCreated := true
	success := false
	defer func() {
		if !snapshotCreated || success {
			return
		}

		if cleanupErr := os.RemoveAll(snapshotDir); cleanupErr != nil {
			err = errors.Join(
				err,
				fmt.Errorf("state.CreateConfigBackup: removing failed snapshot %q: %w", snapshotDir, cleanupErr),
			)
		}
		if syncErr := backupSyncDirectory(backupRoot); syncErr != nil {
			err = errors.Join(
				err,
				fmt.Errorf("state.CreateConfigBackup: syncing backup root after cleanup %q: %w", backupRoot, syncErr),
			)
		}

		snapshotPath = ""
	}()

	candidates, err := collectBackupCandidates(stackPath, dedupedAdditional)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("state.CreateConfigBackup: no files copied from stack %q", stackPath)
	}

	for _, candidate := range candidates {
		sourceBytes, readErr := os.ReadFile(candidate.sourcePath)
		if readErr != nil {
			return "", fmt.Errorf(
				"state.CreateConfigBackup: reading source file %q: %w",
				candidate.sourcePath,
				readErr,
			)
		}

		destinationPath := filepath.Join(snapshotDir, candidate.relativePath)
		if writeErr := backupWriteFile(destinationPath, sourceBytes, candidate.mode.Perm()); writeErr != nil {
			return "", fmt.Errorf(
				"state.CreateConfigBackup: writing backup file %q: %w",
				destinationPath,
				writeErr,
			)
		}
	}

	if err := backupSyncDirectory(snapshotDir); err != nil {
		return "", fmt.Errorf("state.CreateConfigBackup: syncing snapshot directory %q: %w", snapshotDir, err)
	}

	success = true
	snapshotPath = snapshotDir
	return snapshotPath, nil
}

// PruneConfigBackups enforces [BackupRetentionLimit] under
// <stackPath>/.wdm-backups by evicting the oldest snapshot directories first.
// When pinnedSnapshot matches a direct child snapshot name, that snapshot is
// never evicted.
func PruneConfigBackups(stackPath string, pinnedSnapshot string) (removed []string, err error) {
	if err := validateBackupStackPath("state.PruneConfigBackups", stackPath); err != nil {
		return nil, err
	}

	backupRoot := filepath.Join(stackPath, BackupDirName)
	exists, err := validatePruneBackupRoot(backupRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	defer func() {
		err = joinPruneBackupRootSyncError(backupRoot, removed, err)
	}()

	pinnedName, err := normalizePinnedSnapshotName(backupRoot, pinnedSnapshot)
	if err != nil {
		return nil, err
	}

	snapshots, err := collectBackupSnapshotDirectories(backupRoot)
	if err != nil {
		return nil, err
	}
	if len(snapshots) <= BackupRetentionLimit {
		return nil, nil
	}

	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].modifiedAt.Equal(snapshots[j].modifiedAt) {
			return snapshots[i].name < snapshots[j].name
		}
		return snapshots[i].modifiedAt.Before(snapshots[j].modifiedAt)
	})

	toRemove := len(snapshots) - BackupRetentionLimit
	removed = make([]string, 0, toRemove)
	for _, snapshot := range snapshots {
		if toRemove == 0 {
			break
		}
		if pinnedName != "" && snapshot.name == pinnedName {
			continue
		}

		if err := removeBackupSnapshot(snapshot.path); err != nil {
			return removed, err
		}
		removed = append(removed, snapshot.path)
		toRemove--
	}

	if toRemove > 0 {
		return removed, fmt.Errorf(
			"state.PruneConfigBackups: unable to satisfy retention limit %d under %q",
			BackupRetentionLimit,
			backupRoot,
		)
	}
	return removed, nil
}

func validatePruneBackupRoot(backupRoot string) (bool, error) {
	backupRootInfo, err := os.Lstat(backupRoot)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("state.PruneConfigBackups: stating backup root %q: %w", backupRoot, err)
	}
	if backupRootInfo.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("state.PruneConfigBackups: backup root %q must not be a symlink", backupRoot)
	}
	if !backupRootInfo.IsDir() {
		return false, fmt.Errorf("state.PruneConfigBackups: backup root %q is not a directory", backupRoot)
	}
	return true, nil
}

func joinPruneBackupRootSyncError(backupRoot string, removed []string, err error) error {
	if len(removed) == 0 {
		return err
	}
	syncErr := backupSyncDirectory(backupRoot)
	if syncErr == nil {
		return err
	}
	wrapped := fmt.Errorf(
		"state.PruneConfigBackups: syncing backup root %q after pruning: %w",
		backupRoot,
		syncErr,
	)
	if err != nil {
		return errors.Join(err, wrapped)
	}
	return wrapped
}

func removeBackupSnapshot(path string) error {
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("state.PruneConfigBackups: stating snapshot %q before removal: %w", path, err)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state.PruneConfigBackups: snapshot entry %q must not be a symlink", path)
	}
	if !currentInfo.IsDir() {
		return fmt.Errorf("state.PruneConfigBackups: snapshot entry %q is no longer a directory", path)
	}
	if err := backupRemoveAll(path); err != nil {
		return fmt.Errorf("state.PruneConfigBackups: removing snapshot %q: %w", path, err)
	}
	return nil
}

// validateBackupStackPath enforces the no-write-outside-stack invariant
// (PRD §29) for the stack-directory argument: it must be a non-empty
// absolute path resolving to an existing directory. op is the caller's
// error prefix (e.g. "state.CreateConfigBackup") so messages stay identical
// to the per-caller checks they replace.
func validateBackupStackPath(op, stackPath string) error {
	if stackPath == "" || !filepath.IsAbs(stackPath) {
		return fmt.Errorf("%s: stackPath must be a non-empty absolute path, got %q", op, stackPath)
	}

	stackInfo, err := os.Stat(stackPath)
	if err != nil {
		return fmt.Errorf("%s: stating stackPath %q: %w", op, stackPath, err)
	}
	if !stackInfo.IsDir() {
		return fmt.Errorf("%s: stackPath %q is not a directory", op, stackPath)
	}

	return nil
}

func normalizePinnedSnapshotName(backupRoot string, pinnedSnapshot string) (string, error) {
	if pinnedSnapshot == "" {
		return "", nil
	}

	normalized := pinnedSnapshot
	if !filepath.IsAbs(pinnedSnapshot) {
		if strings.Contains(pinnedSnapshot, string(filepath.Separator)) {
			return "", fmt.Errorf(
				"state.PruneConfigBackups: pinned snapshot %q must be a direct snapshot basename",
				pinnedSnapshot,
			)
		}
		normalized = filepath.Clean(pinnedSnapshot)
		if normalized == "." || normalized == ".." || normalized == "" {
			return "", fmt.Errorf(
				"state.PruneConfigBackups: pinned snapshot %q must be a direct snapshot basename",
				pinnedSnapshot,
			)
		}
		return normalized, nil
	}

	if filepath.IsAbs(pinnedSnapshot) {
		relativeToRoot, err := filepath.Rel(backupRoot, filepath.Clean(pinnedSnapshot))
		if err != nil {
			return "", fmt.Errorf(
				"state.PruneConfigBackups: normalizing pinned snapshot %q relative to backup root %q: %w",
				pinnedSnapshot,
				backupRoot,
				err,
			)
		}
		if relativeToRoot == "." || relativeToRoot == ".." ||
			filepath.IsAbs(relativeToRoot) ||
			strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf(
				"state.PruneConfigBackups: pinned snapshot %q must be inside backup root %q",
				pinnedSnapshot,
				backupRoot,
			)
		}
		normalized = relativeToRoot
	}

	normalized = filepath.Clean(normalized)
	if normalized == "." || normalized == ".." || normalized == "" ||
		strings.Contains(normalized, string(filepath.Separator)) {
		return "", fmt.Errorf(
			"state.PruneConfigBackups: pinned snapshot %q must resolve to a snapshot basename",
			pinnedSnapshot,
		)
	}

	return normalized, nil
}

func collectBackupSnapshotDirectories(backupRoot string) ([]backupSnapshotDirectory, error) {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return nil, fmt.Errorf("state.PruneConfigBackups: reading backup root %q: %w", backupRoot, err)
	}

	snapshots := make([]backupSnapshotDirectory, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(backupRoot, entry.Name())
		entryInfo, statErr := os.Lstat(entryPath)
		if statErr != nil {
			return nil, fmt.Errorf("state.PruneConfigBackups: stating backup entry %q: %w", entryPath, statErr)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("state.PruneConfigBackups: snapshot entry %q must not be a symlink", entryPath)
		}
		if !entryInfo.IsDir() {
			continue
		}

		snapshots = append(snapshots, backupSnapshotDirectory{
			name:       entry.Name(),
			path:       entryPath,
			modifiedAt: entryInfo.ModTime(),
		})
	}

	return snapshots, nil
}

func validateBackupOperation(operation string) error {
	if operation == "" {
		return fmt.Errorf("state.CreateConfigBackup: operation must be non-empty")
	}
	if !backupOperationRegex.MatchString(operation) {
		return fmt.Errorf(
			"state.CreateConfigBackup: operation %q must contain only lowercase letters, digits, and hyphen",
			operation,
		)
	}

	return nil
}

func normalizeBackupAdditionalPaths(paths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(paths))
	deduped := make([]string, 0, len(paths))

	for _, rawPath := range paths {
		if rawPath == "" {
			return nil, fmt.Errorf("state.CreateConfigBackup: additional path must be non-empty")
		}
		if filepath.IsAbs(rawPath) {
			return nil, fmt.Errorf("state.CreateConfigBackup: additional path %q must be relative", rawPath)
		}

		cleaned := filepath.Clean(rawPath)
		if cleaned == "." || cleaned == ".." {
			return nil, fmt.Errorf("state.CreateConfigBackup: additional path %q is unsafe", rawPath)
		}
		if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("state.CreateConfigBackup: additional path %q escapes stack root", rawPath)
		}
		if strings.HasPrefix(cleaned, BackupDirName) {
			return nil, fmt.Errorf(
				"state.CreateConfigBackup: additional path %q must not target %q",
				rawPath,
				BackupDirName,
			)
		}

		if _, alreadySeen := seen[cleaned]; alreadySeen {
			continue
		}
		seen[cleaned] = struct{}{}
		deduped = append(deduped, cleaned)
	}

	return deduped, nil
}

func ensureBackupRootDirectory(stackPath, backupRoot string) error {
	backupInfo, err := os.Lstat(backupRoot)
	switch {
	case err == nil:
		if backupInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("state.CreateConfigBackup: backup root %q must not be a symlink", backupRoot)
		}
		if !backupInfo.IsDir() {
			return fmt.Errorf("state.CreateConfigBackup: backup root %q is not a directory", backupRoot)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("state.CreateConfigBackup: stating backup root %q: %w", backupRoot, err)
	}

	if err := os.Mkdir(backupRoot, GeneratedDirMode); err != nil {
		return fmt.Errorf("state.CreateConfigBackup: creating backup root %q: %w", backupRoot, err)
	}
	if err := os.Chmod(backupRoot, GeneratedDirMode); err != nil {
		return fmt.Errorf("state.CreateConfigBackup: setting mode on backup root %q: %w", backupRoot, err)
	}
	if err := backupSyncDirectory(stackPath); err != nil {
		return fmt.Errorf(
			"state.CreateConfigBackup: syncing stack directory entry for backup root %q: %w",
			backupRoot,
			err,
		)
	}

	return nil
}

func createBackupSnapshotDirectory(backupRoot, snapshotPath string) error {
	if err := os.Mkdir(snapshotPath, GeneratedDirMode); err != nil {
		return fmt.Errorf("state.CreateConfigBackup: creating snapshot directory %q: %w", snapshotPath, err)
	}

	if err := os.Chmod(snapshotPath, GeneratedDirMode); err != nil {
		return cleanupFailedSnapshotDirectory(
			backupRoot,
			snapshotPath,
			fmt.Errorf("state.CreateConfigBackup: setting mode on snapshot directory %q: %w", snapshotPath, err),
		)
	}
	if err := backupSyncDirectory(backupRoot); err != nil {
		return cleanupFailedSnapshotDirectory(
			backupRoot,
			snapshotPath,
			fmt.Errorf(
				"state.CreateConfigBackup: syncing backup root directory entry for snapshot %q: %w",
				snapshotPath,
				err,
			),
		)
	}

	return nil
}

func cleanupFailedSnapshotDirectory(backupRoot, snapshotPath string, cause error) error {
	errs := []error{cause}
	if err := os.RemoveAll(snapshotPath); err != nil {
		errs = append(errs, fmt.Errorf("state.CreateConfigBackup: removing failed snapshot %q: %w", snapshotPath, err))
	}
	if err := backupSyncDirectory(backupRoot); err != nil {
		errs = append(errs, fmt.Errorf("state.CreateConfigBackup: syncing backup root after failed snapshot %q: %w", snapshotPath, err))
	}

	return errors.Join(errs...)
}

func collectBackupCandidates(stackPath string, additionalRelativePaths []string) ([]backupCandidate, error) {
	standardPaths := []string{
		"docker-compose.yml",
		"docker-compose.override.yml",
		".env",
		".env.user",
		stackLockFilename,
	}

	candidates := make([]backupCandidate, 0, len(standardPaths)+len(additionalRelativePaths))
	for _, standardPath := range standardPaths {
		candidate, exists, err := collectBackupCandidate(stackPath, standardPath)
		if err != nil {
			return nil, err
		}
		if exists {
			candidates = append(candidates, candidate)
		}
	}

	for _, additionalPath := range additionalRelativePaths {
		if err := validateAdditionalPathAncestors(stackPath, additionalPath); err != nil {
			return nil, fmt.Errorf(
				"state.CreateConfigBackup: validating additional path %q: %w",
				additionalPath,
				err,
			)
		}

		candidate, exists, err := collectBackupCandidate(stackPath, additionalPath)
		if err != nil {
			return nil, err
		}
		if exists {
			candidates = append(candidates, candidate)
		}
	}

	return candidates, nil
}

func collectBackupCandidate(stackPath, relativePath string) (backupCandidate, bool, error) {
	sourcePath := filepath.Join(stackPath, relativePath)

	sourceInfo, err := os.Lstat(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return backupCandidate{}, false, nil
	}
	if err != nil {
		return backupCandidate{}, false, fmt.Errorf(
			"state.CreateConfigBackup: stating source file %q: %w",
			sourcePath,
			err,
		)
	}
	if !sourceInfo.Mode().IsRegular() {
		return backupCandidate{}, false, fmt.Errorf(
			"state.CreateConfigBackup: source %q must be a regular file",
			sourcePath,
		)
	}

	return backupCandidate{
		relativePath: relativePath,
		sourcePath:   sourcePath,
		mode:         sourceInfo.Mode(),
	}, true, nil
}

func validateAdditionalPathAncestors(stackPath, relativePath string) error {
	parentPath := filepath.Dir(relativePath)
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
			return fmt.Errorf("stating path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("path component %q is not a directory", currentPath)
		}
	}

	return nil
}
