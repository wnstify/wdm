//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// StackLockClearOutcome reports which classification [ClearStaleStackLock]
// reached so the engine's orphan-recovery path can phrase distinct
// user-facing copy and decide how aggressively the stack directory may be
// removed: an absent lock is NOT proof of wdm ownership (only an empty dir
// may be removed), while a cleared stale lock proves a wdm-owned orphan
// (the whole dir may be removed).
type StackLockClearOutcome int

const (
	// StackLockClearUnknown is the zero value and never returned by a
	// successful clear. It guards a caller from treating an unset outcome
	// as meaningful, and accompanies every refusal error.
	StackLockClearUnknown StackLockClearOutcome = iota

	// StackLockClearAbsent means no .wdm.lock file exists at the path. The
	// directory is therefore NOT proven wdm-managed — the caller must not
	// treat it as a recoverable wdm orphan beyond an empty-dir removal.
	StackLockClearAbsent

	// StackLockClearCleared means the file existed, classified stale
	// (empty, corrupt, or an unknown schema), and was removed while this
	// writer held the exclusive flock. The surrounding directory is a
	// wdm-owned interrupted-install orphan.
	StackLockClearCleared
)

// String renders the outcome as a stable lowercase token for logs.
func (o StackLockClearOutcome) String() string {
	switch o {
	case StackLockClearAbsent:
		return "absent"
	case StackLockClearCleared:
		return "cleared"
	default:
		return "unknown"
	}
}

// ClearStaleStackLock removes a per-stack .wdm.lock at lockPath ONLY when it
// is provably stale residue from a hard-killed install — never a live or
// validly managed lock. It is the single-arm sibling of
// [ClearStaleRuntimeLock]: the per-stack lock carries no holder PID, so a
// HELD flock can never be proven stale and is always refused. The three
// outcomes:
//   - Absent. No file at lockPath → [StackLockClearAbsent], nil. The caller
//     learns the directory is not a wdm-managed stack and must not remove
//     anything beyond an empty directory.
//   - Held. A non-blocking LOCK_EX fails → a live holder (an install in
//     progress, or a concurrent operation) still pins the flock. Refuse with
//     a [*LockHeldError] wrapping [ErrStackLockBusy]: the staleness of a
//     held per-stack lock cannot be proven, so the safe response is to leave
//     it untouched. Returns [StackLockClearUnknown].
//   - Free. The writer holds the exclusive flock and reads the manifest
//     under it. A valid current-schema manifest → the stack is properly
//     managed; refuse with a typed [types.ErrCodeUsageValidation] error
//     ("uninstall instead") and leave the file intact. An empty file, a
//     decode failure, or an unknown schema (i.e. [decodeStackLock] reports
//     [types.ErrStaleState]) → stale residue; unlink it WHILE the flock is
//     held (the deferred Close releases after), returning
//     [StackLockClearCleared]. A missing file at unlink time folds into
//     success (idempotent).
//
// Every refusal leaves the file's bytes untouched. lockPath MUST be
// absolute; ctx is honored at entry only — the syscalls below are local and
// fast.
func ClearStaleStackLock(ctx context.Context, lockPath string) (StackLockClearOutcome, error) {
	if err := ctx.Err(); err != nil {
		return StackLockClearUnknown, fmt.Errorf("state.ClearStaleStackLock: %w", err)
	}
	if lockPath == "" || !filepath.IsAbs(lockPath) {
		return StackLockClearUnknown, types.NewError(
			types.ErrCodeUsageValidation,
			"stack lock path must be absolute",
			"this is a wdm internal invariant; report it if you see it",
		)
	}

	// G304 is suppressed: lockPath is the engine-controlled
	// "<stack>/.wdm.lock" location, validated absolute above — same
	// rationale as AcquireStackLock / TryReadStackLock. The file is never
	// created: a missing lock is reported absent.
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // G304: engine-controlled stack path, validated absolute
	if errors.Is(err, os.ErrNotExist) {
		return StackLockClearAbsent, nil
	}
	if err != nil {
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be opened for clearing",
			"check the stack directory permissions and retry",
			fmt.Errorf("state.ClearStaleStackLock: opening %q: %w", lockPath, err),
		)
	}
	// The fd is closed on every return path; in the cleared case the close
	// also releases the exclusive flock, but only AFTER the unlink, so the
	// held flock guards the unlink window.
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort fd teardown; unlink already reached goal state, kernel releases flock on close

	got, err := TryLockExclusive(f)
	if err != nil {
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be inspected for clearing",
			"check the stack directory permissions and retry",
			fmt.Errorf("state.ClearStaleStackLock: %w", err),
		)
	}
	if !got {
		// A live holder pins the flock. A per-stack lock has no holder PID,
		// so staleness cannot be proven — refuse, never clear. cmd/wdm maps
		// ErrCodeRuntimeLockHeld to exit 4.
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeRuntimeLockHeld,
			"stack lock is held by another operation",
			"wait for the in-progress operation to finish, then retry",
			fmt.Errorf("state.ClearStaleStackLock %q: %w", lockPath, ErrStackLockBusy),
		)
	}

	raw, err := io.ReadAll(f)
	if err != nil {
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be read for clearing",
			"check the stack directory permissions and retry",
			fmt.Errorf("state.ClearStaleStackLock: reading %q: %w", lockPath, err),
		)
	}

	// Classify with the shared decoder so this clear agrees byte-for-byte
	// with the readers on what counts as corrupt: empty, undecodable, or an
	// unknown schema all wrap types.ErrStaleState → stale residue. A valid
	// current-schema manifest decodes cleanly → a properly managed stack,
	// which must be uninstalled, not force-recovered.
	if _, decErr := decodeStackLock("state.ClearStaleStackLock", lockPath, raw); decErr == nil {
		return StackLockClearUnknown, types.NewError(
			types.ErrCodeUsageValidation,
			"stack is already managed",
			fmt.Sprintf("stack %q has a valid lock manifest; uninstall it instead of forcing a recovery", filepath.Dir(lockPath)),
		)
	} else if !errors.Is(decErr, types.ErrStaleState) {
		// An unexpected decode error shape: fail closed rather than treat an
		// unclassified manifest as stale residue.
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be classified for clearing",
			"inspect the stack directory and remove it manually if it is an interrupted install",
			fmt.Errorf("state.ClearStaleStackLock: classifying %q: %w", lockPath, decErr),
		)
	}

	// Stale residue: unlink while the flock is still held (released by the
	// deferred Close above, AFTER this unlink). A racing removal surfaces as
	// os.ErrNotExist and folds into success.
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return StackLockClearUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"stale stack lock could not be removed",
			"check the stack directory permissions and retry",
			fmt.Errorf("state.ClearStaleStackLock: removing stale lock %q: %w", lockPath, err),
		)
	}
	return StackLockClearCleared, nil
}
