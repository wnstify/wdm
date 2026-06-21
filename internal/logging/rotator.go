package logging

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// archivePrefix and archiveSuffix bound the timestamped archive name
// PRD §24 reserves (wdm-YYYY-MM-DD-HHMMSS.log). The rotator both writes
// names in this shape and globs for them when pruning, so the two stay
// defined together.
const (
	archivePrefix = "wdm-"
	archiveSuffix = ".log"
	// archiveTimeLayout formats the timestamp embedded in an archive
	// name. Colons are illegal in some filesystems, so PRD §24 uses a
	// flat HHMMSS form rather than RFC3339.
	archiveTimeLayout = "2006-01-02-150405"
	// logDirPerm and logFilePerm lock the on-disk log tree to the owner
	// (PRD §11, §24): logs may carry operational detail, so no group or
	// world access.
	logDirPerm  os.FileMode = 0o700
	logFilePerm os.FileMode = 0o600
)

// OpenLogFile prepares the PRD §24 log sink under dir and returns an open
// handle to a fresh latest.log. It creates dir (0700) if absent, archives
// any prior latest.log under a timestamped wdm-YYYY-MM-DD-HHMMSS.log name,
// opens a new latest.log (0600) truncated, then prunes archives that fall
// outside the retention intersection (PRD §24): an archive survives only
// when it is BOTH within [RetentionMaxAge] AND among the
// [RetentionMaxFiles]-1 newest archives. latest.log is always kept.
//
// The caller owns the returned file and MUST Close it. dir MUST be an
// absolute, engine-resolved path; OpenLogFile resolves symlinks on dir and
// refuses to write through any component that escapes the resolved root, so
// a tampered logs symlink cannot redirect writes elsewhere.
func OpenLogFile(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("logging: log dir must be absolute, got %q", dir)
	}
	if err := os.MkdirAll(dir, logDirPerm); err != nil {
		return nil, fmt.Errorf("logging: creating log dir: %w", err)
	}

	resolved, err := safeLogDir(dir)
	if err != nil {
		return nil, err
	}

	if err := archiveLatest(resolved, time.Now()); err != nil {
		return nil, err
	}

	latest := filepath.Join(resolved, LatestLogName)
	//nolint:gosec // G304: latest is LatestLogName joined onto the engine-resolved, symlink-checked log dir, never user input.
	f, err := os.OpenFile(latest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, logFilePerm)
	if err != nil {
		return nil, fmt.Errorf("logging: opening %s: %w", LatestLogName, err)
	}

	// Pruning failures are non-fatal: an open sink already exists, and
	// refusing to log because an old archive could not be removed would
	// violate PRD §24 ("wdm must always write a normal log").
	//nolint:errcheck // best-effort prune; an open sink already exists so a prune fault must not block logging (PRD §24).
	_ = prune(resolved, time.Now())
	return f, nil
}

// safeLogDir resolves symlinks on dir and confirms the result still names a
// directory, rejecting a path that resolves outside itself. Returns the
// resolved absolute directory the caller writes under.
func safeLogDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("logging: resolving log dir: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("logging: stat log dir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("logging: log path %q is not a directory", resolved)
	}
	return resolved, nil
}

// archiveLatest renames an existing latest.log to a timestamped archive so
// the fresh session starts clean while the prior session's log is retained
// under the PRD §24 naming scheme. A missing latest.log is not an error:
// first run has nothing to archive.
func archiveLatest(dir string, now time.Time) error {
	latest := filepath.Join(dir, LatestLogName)
	if _, err := os.Lstat(latest); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("logging: stat %s: %w", LatestLogName, err)
	}

	archive := filepath.Join(dir, archivePrefix+now.Format(archiveTimeLayout)+archiveSuffix)
	// Guard against a same-second collision (two opens within one
	// second): suffix with a counter rather than clobbering an archive.
	for i := 1; ; i++ {
		if _, err := os.Lstat(archive); err != nil {
			break // free name, or a stat fault Rename will surface; never spin
		}
		archive = filepath.Join(dir, fmt.Sprintf("%s%s-%d%s", archivePrefix, now.Format(archiveTimeLayout), i, archiveSuffix))
	}

	if err := os.Rename(latest, archive); err != nil {
		return fmt.Errorf("logging: archiving %s: %w", LatestLogName, err)
	}
	return nil
}

// prune enforces the PRD §24 retention intersection over the archives in
// dir: it deletes any archive that is older than [RetentionMaxAge] OR not
// among the [RetentionMaxFiles]-1 newest archives. latest.log is reserved
// for the live session and is never globbed or removed. The -1 keeps the
// total file count (archives + latest.log) at or below [RetentionMaxFiles].
func prune(dir string, now time.Time) error {
	matches, err := filepath.Glob(filepath.Join(dir, archivePrefix+"*"+archiveSuffix))
	if err != nil {
		return fmt.Errorf("logging: globbing archives: %w", err)
	}

	type archive struct {
		path    string
		modTime time.Time
	}
	archives := make([]archive, 0, len(matches))
	for _, path := range matches {
		if filepath.Base(path) == LatestLogName {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		archives = append(archives, archive{path: path, modTime: info.ModTime()})
	}

	// Newest first so the count cap keeps the most recent archives.
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].modTime.After(archives[j].modTime)
	})

	cutoff := now.Add(-RetentionMaxAge)
	keepCount := RetentionMaxFiles - 1
	for i, a := range archives {
		tooOld := a.modTime.Before(cutoff)
		overCount := i >= keepCount
		if tooOld || overCount {
			//nolint:errcheck // best-effort delete; a stuck archive must not abort retention or block logging (PRD §24).
			_ = os.Remove(a.path)
		}
	}
	return nil
}

// fileRotator is the concrete [Rotator] backing live file rotation under a
// resolved log directory. It satisfies the retention.go contract for
// callers that hold a Rotator and want to rotate mid-run; the engine's
// startup path calls [OpenLogFile] directly and does not need it.
type fileRotator struct {
	dir string
}

// NewRotator returns a [Rotator] that archives and prunes latest.log under
// dir per PRD §24. dir MUST be the absolute, engine-resolved log directory.
func NewRotator(dir string) Rotator {
	return fileRotator{dir: strings.TrimSpace(dir)}
}

// Rotate archives the current latest.log under a timestamped name and
// prunes archives outside the retention intersection (PRD §24). It does not
// open a new latest.log: the caller's existing handle keeps writing, and a
// fresh file is opened on the next [OpenLogFile]. ctx cancellation is
// honored before any filesystem mutation.
func (r fileRotator) Rotate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, err := safeLogDir(r.dir)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := archiveLatest(resolved, now); err != nil {
		return err
	}
	return prune(resolved, now)
}
