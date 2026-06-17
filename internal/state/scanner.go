//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// ScanResult is the structured outcome of [ScanStacks]: the parsed
// applications and any per-directory warnings collected during the
// walk. callers route Apps into Engine.List output (Commit
// 11) and feed Warnings into slog so cmd/wdm can render user-
// visible hints from the [types.ErrStaleState]-wrapped causes.
type ScanResult struct {
	// Apps is one [types.AppInfo] per subdirectory of the stack
	// base whose .wdm.lock parsed cleanly. The slice is freshly
	// allocated on every call and may be retained or mutated by the
	// caller without affecting subsequent scans (defensive-copy
	// posture per the golang-safety guidance applied across the
	// engine surface).
	// Order matches [os.ReadDir]: lexically by directory entry name
	// on Linux.
	Apps []types.AppInfo

	// Warnings is one [ScanWarning] per subdirectory that contained
	// a .wdm.lock but failed to read, parse, or pass the
	// schema_version check. Subdirectories WITHOUT a .wdm.lock
	// never appear here — they belong to the user and are silently
	// ignored.
	Warnings []ScanWarning
}

// ScanWarning describes a single non-fatal scanner observation: a
// directory that LOOKED like a managed stack (its.wdm.lock was
// present) but failed to read or parse. Surfacing these as warnings
// rather than fatal errors lets the user see the rest of their
// stacks; PRD §26's "Detect stale locks where practical" clause is
// honored by the user-facing hint cmd/wdm composes from Cause.
type ScanWarning struct {
	// Path is the absolute path of the offending.wdm.lock.
	Path string

	// Cause is the underlying error from [ReadStackLock]. Typically
	// wraps [types.ErrStaleState] (corrupt JSON, schema_version
	// mismatch); may also wrap I/O errors (EACCES, EIO) for stacks
	// the user cannot read. Detect class with [errors.Is].
	Cause error
}

// ScanStacks walks baseStackPath one level deep and returns one
// [types.AppInfo] per subdirectory containing a parseable
// .wdm.lock. Behavior follows:
//   - subdirectories without a .wdm.lock are silently ignored
//     (they belong to the user)
//   - subdirectories with a corrupt.wdm.lock are surfaced as
//     ScanResult.Warnings, NOT returned as a fatal error
//   - a missing baseStackPath returns an empty result with nil error
//     (first-launch users have no managed stacks yet)
//   - all per-stack I/O failures (open, flock, read, parse) are
//     folded into Warnings, so a single unreadable stack does not
//     blank the whole list
//
// baseStackPath MUST be absolute; relative paths are rejected up
// front. Path expansion (~/ → $HOME) happens upstream in the engine.
// The scan is non-recursive: nested stacks are not detected here.
// Symlinks at the immediate child level are NOT followed —
// [os.DirEntry.IsDir] returns false for symlink entries on Linux,
// so symlinked directories are treated as non-stack entries. This
// matches PRD §12-13's path-safety posture.
// ctx is honored at entry and once per directory iteration; the
// per-stack I/O inside [ReadStackLock] is local and fast, so
// finer-grained cancellation buys nothing.
func ScanStacks(ctx context.Context, baseStackPath string) (ScanResult, error) {
	var result ScanResult

	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("state.ScanStacks: %w", err)
	}
	if baseStackPath == "" || !filepath.IsAbs(baseStackPath) {
		return result, fmt.Errorf("state.ScanStacks: baseStackPath must be absolute, got %q", baseStackPath)
	}

	entries, err := os.ReadDir(baseStackPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// First-launch user with no ~/docker yet; not an error.
			return result, nil
		}
		return result, fmt.Errorf("state.ScanStacks: reading %q: %w", baseStackPath, err)
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("state.ScanStacks: %w", err)
		}
		if !entry.IsDir() {
			continue
		}
		stackDir := filepath.Join(baseStackPath, entry.Name())
		lockPath := filepath.Join(stackDir, stackLockFilename)

		lock, err := ReadStackLock(ctx, lockPath)
		if err != nil {
			// No .wdm.lock = user-owned directory; silently skip.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// Corrupt JSON, schema_version mismatch, EACCES, … →
			// non-fatal warning. PRD §26 wants the rest of the list
			// to come through even when one stack is unreadable.
			result.Warnings = append(result.Warnings, ScanWarning{
				Path:  lockPath,
				Cause: err,
			})
			continue
		}
		result.Apps = append(result.Apps, appInfoFromLock(lock))
	}

	return result, nil
}

// appInfoFromLock projects a [StackLock] into a [types.AppInfo] for
// the Engine.List contract. NeedsAttention (PRD §18, derived from
// runtime container introspection) is left at the zero value here;
// List stays a cheap lock-file scan and Engine.Status is the
// canonical runtime signal that runs docker-inspect per
// "List semantics".
// The function is unexported because the projection is an
// implementation detail of [ScanStacks]: callers consume
// [types.AppInfo] from ScanResult.Apps and never construct one
// directly from a [StackLock].
func appInfoFromLock(lock *StackLock) types.AppInfo {
	return types.AppInfo{
		AppID:                   lock.AppID,
		TemplateName:            lock.TemplateName,
		StackPath:               lock.StackPath,
		CatalogChannel:          lock.CatalogChannel,
		CatalogVersion:          lock.CatalogVersion,
		LastSuccessfulOperation: lock.LastSuccessfulOperation,
		NeedsAttention:          false,
	}
}
