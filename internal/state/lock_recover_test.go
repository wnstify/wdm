//go:build unix

package state_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// writeStackLockFile writes raw bytes to a fresh <dir>/.wdm.lock and returns
// the path, modeling the on-disk lock left by a prior install.
func writeStackLockFile(t *testing.T, raw []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".wdm.lock")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// validStackLockBytes marshals a minimal current-schema manifest so the
// clear classifies it as a properly managed stack.
func validStackLockBytes(t *testing.T) []byte {
	t.Helper()

	raw, err := json.Marshal(state.StackLock{
		SchemaVersion:  1,
		AppID:          "vaultwarden",
		ComposeProject: "wdm-vaultwarden",
	})
	require.NoError(t, err)
	return raw
}

func TestClearStaleStackLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	outcome, err := state.ClearStaleStackLock(t.Context(), "relative/.wdm.lock")
	require.Error(t, err)
	assert.Equal(t, state.StackLockClearUnknown, outcome)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a relative path must map to usage validation; got %v", err)
}

func TestClearStaleStackLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	path := writeStackLockFile(t, nil) // empty → stale, so a missed check would clear it
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	outcome, err := state.ClearStaleStackLock(ctx, path)
	require.Error(t, err)
	assert.Equal(t, state.StackLockClearUnknown, outcome)
	require.ErrorIs(t, err, context.Canceled)

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "a canceled ctx must not remove the lock file")
}

// TestClearStaleStackLock_AbsentFile proves a missing .wdm.lock reports
// Absent (not an error), so the engine learns the dir is not wdm-managed.
func TestClearStaleStackLock_AbsentFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".wdm.lock")
	outcome, err := state.ClearStaleStackLock(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, state.StackLockClearAbsent, outcome)
}

// TestClearStaleStackLock_EmptyOrphanCleared covers the stale-residue path:
// an empty lock from a hard-killed install is removed and a fresh
// AcquireStackLock then succeeds on the same path.
func TestClearStaleStackLock_EmptyOrphanCleared(t *testing.T) {
	t.Parallel()

	path := writeStackLockFile(t, nil)

	outcome, err := state.ClearStaleStackLock(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, state.StackLockClearCleared, outcome)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the orphan lock file must be gone")

	handle, acqErr := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, acqErr, "a cleared path must be reacquirable")
	require.NoError(t, handle.Release())
}

// TestClearStaleStackLock_CorruptOrphanCleared proves an undecodable lock
// (truncated JSON) classifies as stale residue and is removed.
func TestClearStaleStackLock_CorruptOrphanCleared(t *testing.T) {
	t.Parallel()

	path := writeStackLockFile(t, []byte("{truncated"))

	outcome, err := state.ClearStaleStackLock(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, state.StackLockClearCleared, outcome)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the corrupt orphan lock file must be gone")
}

// TestClearStaleStackLock_ValidManifestRefused proves a valid current-schema
// manifest is refused as managed and the file survives byte-identical.
func TestClearStaleStackLock_ValidManifestRefused(t *testing.T) {
	t.Parallel()

	raw := validStackLockBytes(t)
	path := writeStackLockFile(t, raw)

	outcome, err := state.ClearStaleStackLock(t.Context(), path)
	require.Error(t, err)
	assert.Equal(t, state.StackLockClearUnknown, outcome)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a managed stack must be refused with usage validation; got %v", err)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, raw, after, "refusing a managed lock must not touch the file")
}

// TestClearStaleStackLock_HeldLockRefused proves a live cross-OFD flock
// holder (an install in progress) is refused with ErrStackLockBusy and the
// file is left intact — a held per-stack lock can never be proven stale.
func TestClearStaleStackLock_HeldLockRefused(t *testing.T) {
	t.Parallel()

	// Empty file so the only reason to refuse is the held flock, not the
	// manifest classification.
	path := writeStackLockFile(t, nil)

	// Hold LOCK_EX through a distinct open file description so the clear's
	// own non-blocking LOCK_EX fails.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, state.LockExclusive(f))

	before, readErr := os.ReadFile(path)
	require.NoError(t, readErr)

	outcome, clearErr := state.ClearStaleStackLock(t.Context(), path)
	require.Error(t, clearErr)
	assert.Equal(t, state.StackLockClearUnknown, outcome)
	assert.ErrorIs(t, clearErr, state.ErrStackLockBusy,
		"a held lock must be refused with ErrStackLockBusy; got %v", clearErr)
	assert.True(t, types.IsCode(clearErr, types.ErrCodeRuntimeLockHeld),
		"a held lock refusal must map to ErrCodeRuntimeLockHeld; got %v", clearErr)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "refusing a held lock must not touch the file")
}

// TestStackLockClearOutcome_String pins the stable lowercase tokens.
func TestStackLockClearOutcome_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown", state.StackLockClearUnknown.String())
	assert.Equal(t, "absent", state.StackLockClearAbsent.String())
	assert.Equal(t, "cleared", state.StackLockClearCleared.String())
	assert.Equal(t, "unknown", state.StackLockClearOutcome(99).String())
}
