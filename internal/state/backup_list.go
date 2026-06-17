//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ConfigBackupSnapshot describes one config-backup snapshot directory under
// a stack's [BackupDirName] root, as enumerated by [ListConfigBackups]. It is
// the read-only counterpart to the snapshots [CreateConfigBackup] writes: a
// directory named "<unix-nanos>-<operation>" holding copied config files.
// ConfigBackupSnapshot stays in internal/state by design. The projection
// onto pkg/types.BackupInfo belongs to internal/core, which alone may import
// pkg/types, keeping this lister free of any cross-package contract.
type ConfigBackupSnapshot struct {
	// SnapshotID is the snapshot directory basename, of the form
	// "<unix-nanos>-<operation>" (for example
	// "1717000000000000000-update"). It matches the name
	// [CreateConfigBackup] generates and [PruneConfigBackups] pins on.
	SnapshotID string

	// Operation is the lifecycle operation that created the snapshot,
	// parsed from the suffix after the first hyphen in SnapshotID
	// ("install", "update", "migration",...).
	Operation string

	// CreatedAt is the snapshot creation time, decoded from the unix-nanos
	// prefix of SnapshotID and reported in UTC. It is the creation instant,
	// not the directory mtime, so a later touch leaves it stable.
	CreatedAt time.Time

	// Path is the absolute snapshot directory path
	// (<stackPath>/<BackupDirName>/<SnapshotID>).
	Path string

	// Files lists the basenames of the regular config files captured
	// directly inside the snapshot directory, sorted lexically. It never
	// includes subdirectories or symlink entries.
	Files []string
}

// ListConfigBackups enumerates the config-backup snapshots under
// <stackPath>/.wdm-backups, newest first. It is a lock-free, read-only view
// for the backups list surface (PRD §7, §21); the projection onto
// pkg/types.BackupInfo happens in internal/core.
// Ordering is newest-first by the unix-nanos creation prefix embedded in each
// snapshot name, with the snapshot basename as a deterministic tie-break. That
// prefix is the same tamper-stable value [CreatedAt] reports, so the sort key
// and the displayed timestamp can never disagree — unlike the directory mtime
// [PruneConfigBackups] evicts by, which a later touch could move.
// A stack that never backed up — an absent or empty backup root — is not an
// error: ListConfigBackups returns an empty slice and a nil error. A
// symlinked backup root is rejected hard, matching the writer, prune, and
// restore posture (the writer never creates one).
// Validation parity with the writer and prune is read-only-relaxed where it
// safely can be. Non-directory entries, snapshot directories whose names do
// not parse as "<unix-nanos>-<operation>", and symlinked snapshot entries are
// skipped rather than surfaced — none can be a snapshot [CreateConfigBackup]
// wrote, and a list must not fail the whole stack over a foreign entry it
// would never restore. (Prune hard-fails on a symlinked snapshot entry
// because it is about to delete it; the lister deletes nothing.) Inside a
// snapshot only regular files are reported; subdirectories and symlink entries
// are skipped for the same read-only reason.
// Because the lister takes no flock, [PruneConfigBackups] (under the per-stack
// exclusive flock) may prune a snapshot directory between the root listing and
// the per-snapshot read. A snapshot that disappears before its directory entry
// is stat'd is skipped; one that disappears after (between the stat and the
// inner file read) is reported with an empty Files list rather than failing —
// never an error either way. Files reflects a single non-atomic read, so a
// snapshot mid-write may list a subset. The lister is deliberately lock-free:
// it is a pure read like [ReadStackEnv] and the stack scanner, and acquiring
// the per-stack flock would stall the read behind any in-flight writer, which
// PRD §26's read-only clause forbids.
func ListConfigBackups(stackPath string) ([]ConfigBackupSnapshot, error) {
	if err := validateListBackupStackPath(stackPath); err != nil {
		return nil, err
	}

	backupRoot := filepath.Join(stackPath, BackupDirName)
	rootInfo, err := os.Lstat(backupRoot)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return []ConfigBackupSnapshot{}, nil
	case err != nil:
		return nil, fmt.Errorf("state.ListConfigBackups: stating backup root %q: %w", backupRoot, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state.ListConfigBackups: backup root %q must not be a symlink", backupRoot)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("state.ListConfigBackups: backup root %q is not a directory", backupRoot)
	}

	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return nil, fmt.Errorf("state.ListConfigBackups: reading backup root %q: %w", backupRoot, err)
	}

	snapshots := make([]ConfigBackupSnapshot, 0, len(entries))
	for _, entry := range entries {
		snapshot, ok, err := collectListSnapshot(backupRoot, entry.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			snapshots = append(snapshots, snapshot)
		}
	}

	sortConfigBackupSnapshotsNewestFirst(snapshots)
	return snapshots, nil
}

func validateListBackupStackPath(stackPath string) error {
	if stackPath == "" || !filepath.IsAbs(stackPath) {
		return fmt.Errorf(
			"state.ListConfigBackups: stackPath must be a non-empty absolute path, got %q",
			stackPath,
		)
	}

	stackInfo, err := os.Stat(stackPath)
	if err != nil {
		return fmt.Errorf("state.ListConfigBackups: stating stackPath %q: %w", stackPath, err)
	}
	if !stackInfo.IsDir() {
		return fmt.Errorf("state.ListConfigBackups: stackPath %q is not a directory", stackPath)
	}

	return nil
}

// collectListSnapshot inspects one backup-root entry. It reports ok=false
// (with a nil error) for any entry that is not a valid wdm snapshot the list
// should surface — a non-directory, a symlink, a non-parsing name, or a
// directory that vanished before its stat. A snapshot that vanishes after
// the stat (before the inner file read) is still reported, with an empty
// Files list. It returns a non-nil error only for an unexpected stat or read
// failure on a real snapshot directory.
func collectListSnapshot(backupRoot, name string) (ConfigBackupSnapshot, bool, error) {
	entryPath := filepath.Join(backupRoot, name)

	entryInfo, err := os.Lstat(entryPath)
	if errors.Is(err, os.ErrNotExist) {
		return ConfigBackupSnapshot{}, false, nil
	}
	if err != nil {
		return ConfigBackupSnapshot{}, false, fmt.Errorf(
			"state.ListConfigBackups: stating backup entry %q: %w",
			entryPath,
			err,
		)
	}
	if entryInfo.Mode()&os.ModeSymlink != 0 {
		return ConfigBackupSnapshot{}, false, nil
	}
	if !entryInfo.IsDir() {
		return ConfigBackupSnapshot{}, false, nil
	}

	createdAt, operation, ok := parseSnapshotName(name)
	if !ok {
		return ConfigBackupSnapshot{}, false, nil
	}

	files, err := listSnapshotFiles(entryPath)
	if err != nil {
		return ConfigBackupSnapshot{}, false, err
	}

	return ConfigBackupSnapshot{
		SnapshotID: name,
		Operation:  operation,
		CreatedAt:  createdAt,
		Path:       entryPath,
		Files:      files,
	}, true, nil
}

// parseSnapshotName decodes a "<unix-nanos>-<operation>" snapshot basename. It
// reports ok=false for any name that does not match the shape
// [CreateConfigBackup] writes: a non-negative unix-nanos integer, a single
// separating hyphen, and a non-empty operation matching the writer's
// lowercase-alphanumeric-and-hyphen rule.
func parseSnapshotName(name string) (createdAt time.Time, operation string, ok bool) {
	hyphen := strings.IndexByte(name, '-')
	if hyphen <= 0 || hyphen == len(name)-1 {
		return time.Time{}, "", false
	}

	nanosField := name[:hyphen]
	operationField := name[hyphen+1:]

	nanos, err := strconv.ParseInt(nanosField, 10, 64)
	if err != nil || nanos < 0 {
		return time.Time{}, "", false
	}
	if !backupOperationRegex.MatchString(operationField) {
		return time.Time{}, "", false
	}

	return time.Unix(0, nanos).UTC(), operationField, true
}

// listSnapshotFiles returns the sorted basenames of the regular files directly
// inside a snapshot directory, skipping subdirectories and symlink entries. A
// snapshot that vanishes mid-walk yields an empty list and a nil error.
func listSnapshotFiles(snapshotDir string) ([]string, error) {
	entries, err := os.ReadDir(snapshotDir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state.ListConfigBackups: reading snapshot directory %q: %w", snapshotDir, err)
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryType := entry.Type()
		if entryType&os.ModeSymlink != 0 {
			continue
		}
		if !entryType.IsRegular() {
			continue
		}
		files = append(files, entry.Name())
	}

	slices.Sort(files)
	return files, nil
}

// sortConfigBackupSnapshotsNewestFirst orders snapshots by descending creation
// time, breaking ties on the descending basename so the result is fully
// deterministic for snapshots that share a nanosecond.
func sortConfigBackupSnapshotsNewestFirst(snapshots []ConfigBackupSnapshot) {
	slices.SortFunc(snapshots, func(a, b ConfigBackupSnapshot) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return b.CreatedAt.Compare(a.CreatedAt)
		}
		return strings.Compare(b.SnapshotID, a.SnapshotID)
	})
}
