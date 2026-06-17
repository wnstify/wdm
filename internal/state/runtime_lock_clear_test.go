//go:build unix

package state_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// acquireHeldLockForClear acquires the runtime lock in-process and returns
// the path plus the live handle. The handle keeps the flock held (a
// distinct open file description from the one ClearStaleRuntimeLock opens),
// so the writer's own non-blocking LOCK_EX fails and takes the held arm.
// The handle is released on cleanup. The recorded holder is this test
// process (a live PID), with the start-time the swapped reader supplied.
func acquireHeldLockForClear(t *testing.T) (string, *state.RuntimeLockHandle) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Release() })
	return path, handle
}

// fingerprintFromHandle builds the expected-holder fingerprint a caller
// would pass after probing the lock the handle holds.
func fingerprintFromHandle(h *state.RuntimeLockHandle, maxAge time.Duration) state.ClearStaleRuntimeLockRequest {
	info := h.Info()
	return state.ClearStaleRuntimeLockRequest{
		MaxHeldAge:          maxAge,
		ExpectedPID:         info.PID,
		ExpectedStartedAt:   info.StartedAt,
		ExpectedStartedTime: info.StartedTime,
	}
}

func TestClearStaleRuntimeLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	outcome, err := state.ClearStaleRuntimeLock(t.Context(), "relative.lock", state.ClearStaleRuntimeLockRequest{})
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a relative path must map to usage validation; got %v", err)
}

func TestClearStaleRuntimeLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	// A genuinely held lock so a missed ctx check would mutate the file.
	path, _ := acquireHeldLockForClear(t)
	before, readErr := os.ReadFile(path)
	require.NoError(t, readErr)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	outcome, err := state.ClearStaleRuntimeLock(ctx, path, state.ClearStaleRuntimeLockRequest{})
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	require.ErrorIs(t, err, context.Canceled)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "a canceled ctx must not touch the lock file")
}

// TestClearStaleRuntimeLock_MissingFileIsIdempotent proves a clear against
// a path with no lock file succeeds (already cleared) rather than erroring,
// so a second clear or a clear racing another removal never fails.
func TestClearStaleRuntimeLock_MissingFileIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, state.ClearStaleRuntimeLockRequest{})
	require.NoError(t, err)
	assert.Equal(t, state.ClearOutcomeFreeLeftover, outcome)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

// TestClearStaleRuntimeLock_OpenErrorIsTyped proves a non-ENOENT open
// failure maps to a typed ErrCodeGeneric *Error (not a bare error) with
// the underlying cause reachable. The cheapest portable trigger is a path
// whose PARENT is a regular file, so the open fails ENOTDIR rather than
// the ENOENT that the idempotent already-cleared arm folds into success.
func TestClearStaleRuntimeLock_OpenErrorIsTyped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notADir := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))
	// "afile" is a regular file, so "afile/runtime.lock" cannot be opened.
	path := filepath.Join(notADir, "runtime.lock")

	outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, state.ClearStaleRuntimeLockRequest{})
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a non-ENOENT open failure must map to ErrCodeGeneric; got %v", err)
}

// TestClearStaleRuntimeLock_FreeLeftoverIsCleared covers the free arm: a
// released lock leaves its file behind, the writer acquires the flock,
// unlinks the leftover, and a fresh AcquireRuntimeLock then succeeds.
func TestClearStaleRuntimeLock_FreeLeftoverIsCleared(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	require.NoError(t, handle.Release()) // leaves the file on disk, unheld

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "released lock must leave the file behind")

	outcome, clearErr := state.ClearStaleRuntimeLock(t.Context(), path, state.ClearStaleRuntimeLockRequest{})
	require.NoError(t, clearErr)
	assert.Equal(t, state.ClearOutcomeFreeLeftover, outcome,
		"a free leftover must clear via the free arm")

	_, statErr = os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the leftover file must be gone")

	// A fresh acquisition recreates the file and succeeds.
	reacquired, reErr := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, reErr)
	require.NoError(t, reacquired.Release())
}

// TestClearStaleRuntimeLock_LiveHolderRefused proves the the invariant
// invariant against a REAL concurrent live holder: a subprocess holds the
// runtime lock, this process attempts to clear it, and the clear must
// refuse with ErrRuntimeLockHeld while the file survives byte-identical.
// flock(2) is per-OFD, so the only honest test of a live cross-process
// holder is to hold it from another process; a same-process flock would
// be acquired by the writer's own fresh open and silently pass.
func TestClearStaleRuntimeLock_LiveHolderRefused(t *testing.T) {
	t.Parallel()

	lockPath, _ := startRuntimeLockHelper(t)
	before, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)

	// Read the live holder's fingerprint off disk so the request carries
	// the genuine holder identity — the refusal must come from liveness,
	// not from a fingerprint mismatch.
	probe, probeErr := state.ProbeRuntimeLock(t.Context(), lockPath)
	require.NoError(t, probeErr)
	require.True(t, probe.Held)
	require.True(t, probe.HolderAlive)

	req := state.ClearStaleRuntimeLockRequest{
		MaxHeldAge:          24 * time.Hour,
		ExpectedPID:         probe.Holder.PID,
		ExpectedStartedAt:   probe.Holder.StartedAt,
		ExpectedStartedTime: probe.Holder.StartedTime,
	}
	outcome, err := state.ClearStaleRuntimeLock(t.Context(), lockPath, req)
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"a live holder must be refused with ErrRuntimeLockHeld; got %v", err)

	var heldErr *state.LockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, lockPath, heldErr.Path)

	after, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "refusing a live holder must not touch the file")
}

// TestClearStaleRuntimeLock_DeadHolderCleared covers the held arm's stale
// path via the F1 liveness seam: the lock is held in-process (so the
// writer's own flock fails), but the swapped start-time reader makes the
// recorded holder classify NOT alive (a recycled PID). With the
// fingerprint matching, the clear succeeds, the file is gone, and a fresh
// acquisition then works.
func TestClearStaleRuntimeLock_DeadHolderCleared(t *testing.T) {
	// Record one start-time at acquisition.
	const recordedStart = uint64(111000)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart, true
	})

	path, handle := acquireHeldLockForClear(t)
	req := fingerprintFromHandle(handle, 24*time.Hour)

	// The live reader now returns a DIFFERENT start-time, so the live PID
	// classifies recycled → holderStillAlive is false → stale.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart + 1, true
	})

	outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, req)
	require.NoError(t, err)
	assert.Equal(t, state.ClearOutcomeStaleHolder, outcome,
		"a stale (recycled-PID) holder clears via the held arm")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the stale lock file must be gone")

	// The in-process flock survives on the now-detached inode (harmless);
	// a fresh acquisition opens the new path and succeeds.
	reacquired, reErr := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, reErr)
	require.NoError(t, reacquired.Release())
}

// TestClearStaleRuntimeLock_LiveHeldWithinAgeRefused covers the held arm's
// live-and-young refusal in-process: the holder is alive (start-times
// match) and the hold is well within MaxHeldAge, so the clear must refuse
// and leave the file intact.
func TestClearStaleRuntimeLock_LiveHeldWithinAgeRefused(t *testing.T) {
	const start = uint64(222000)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return start, true
	})

	path, handle := acquireHeldLockForClear(t)
	before, readErr := os.ReadFile(path)
	require.NoError(t, readErr)

	req := fingerprintFromHandle(handle, 24*time.Hour)
	outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, req)
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"a live, within-age holder must be refused; got %v", err)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "refusing a live holder must not touch the file")
}

// TestClearStaleRuntimeLock_AgeArm proves the age policy: a holder that is
// alive (start-times match) but has held the lock longer than MaxHeldAge
// is clearable, while the same holder within MaxHeldAge is refused. The
// hold age is driven by the recorded StartedAt, so the test rewrites the
// on-disk holder with an old StartedAt to model a wedged-but-live hold.
func TestClearStaleRuntimeLock_AgeArm(t *testing.T) {
	const start = uint64(333000)

	cases := []struct {
		name      string
		maxAge    time.Duration
		startedAt time.Time
		wantClear bool
	}{
		{
			name:      "beyond_max_age_clears",
			maxAge:    1 * time.Hour,
			startedAt: time.Now().UTC().Add(-48 * time.Hour),
			wantClear: true,
		},
		{
			name:      "within_max_age_refused",
			maxAge:    72 * time.Hour,
			startedAt: time.Now().UTC().Add(-1 * time.Hour),
			wantClear: false,
		},
		{
			name:      "zero_max_age_disables_age_arm",
			maxAge:    0,
			startedAt: time.Now().UTC().Add(-48 * time.Hour),
			wantClear: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Holder classifies ALIVE (start-times match) for the whole
			// test so only the age arm decides the outcome.
			state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
				return start, true
			})

			path, _ := acquireHeldLockForClear(t)

			// Re-read the on-disk holder, rewrite its StartedAt to model an
			// old (or recent) hold, then re-derive the fingerprint from the
			// rewritten bytes so the request still matches.
			holder := rewriteHolderStartedAt(t, path, tc.startedAt, start)
			req := state.ClearStaleRuntimeLockRequest{
				MaxHeldAge:          tc.maxAge,
				ExpectedPID:         holder.PID,
				ExpectedStartedAt:   holder.StartedAt,
				ExpectedStartedTime: holder.StartedTime,
			}

			before, readErr := os.ReadFile(path)
			require.NoError(t, readErr)

			outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, req)
			if tc.wantClear {
				require.NoError(t, err)
				assert.Equal(t, state.ClearOutcomeStaleHolder, outcome)
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "an over-age hold must clear")
				return
			}

			require.Error(t, err)
			assert.Equal(t, state.ClearOutcomeUnknown, outcome)
			assert.ErrorIs(t, err, state.ErrRuntimeLockHeld)
			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, before, after, "a within-age hold must not touch the file")
		})
	}
}

// TestClearStaleRuntimeLock_FingerprintDriftRefused proves the
// observed-stale binding: the holder is classified stale (recycled PID),
// but the caller's expected fingerprint names a DIFFERENT holder than the
// one on disk, so the clear refuses and the file survives. This is the
// "a new live holder acquired in the window" hole the fingerprint closes.
func TestClearStaleRuntimeLock_FingerprintDriftRefused(t *testing.T) {
	const recordedStart = uint64(444000)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart, true
	})

	path, handle := acquireHeldLockForClear(t)
	before, readErr := os.ReadFile(path)
	require.NoError(t, readErr)

	// The holder is stale (recycled PID): live reader drifts.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart + 99, true
	})

	// Build a request whose expected PID does NOT match the on-disk holder,
	// modeling the caller having observed a different (now-replaced) holder.
	req := fingerprintFromHandle(handle, 24*time.Hour)
	req.ExpectedPID = handle.Info().PID + 1 // drift

	outcome, err := state.ClearStaleRuntimeLock(t.Context(), path, req)
	require.Error(t, err)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"a fingerprint drift must refuse the clear; got %v", err)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "a drift refusal must not touch the file")
}

// TestClearStaleRuntimeLock_UnreadableHolderRefused proves a held lock
// whose on-disk metadata is unparseable (zero Holder, PID 0) is refused
// even though a PID-0 holder classifies not-alive: the fingerprint can
// never match a zero PID, so a corrupt-but-held file is never cleared on
// a degenerate all-zero match. The file survives.
func TestClearStaleRuntimeLock_UnreadableHolderRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	require.NoError(t, os.WriteFile(path, []byte("{truncated"), 0o600))

	// Hold the flock through a distinct OFD so the writer takes the held
	// arm. The handle is a raw second open; closing it on cleanup releases.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	got, lockErr := state.TryLockExclusive(f)
	require.NoError(t, lockErr)
	require.True(t, got)

	before, readErr := os.ReadFile(path)
	require.NoError(t, readErr)

	req := state.ClearStaleRuntimeLockRequest{
		MaxHeldAge:  24 * time.Hour,
		ExpectedPID: 4242, // a non-zero expected PID; on-disk holder is PID 0
	}
	outcome, clearErr := state.ClearStaleRuntimeLock(t.Context(), path, req)
	require.Error(t, clearErr)
	assert.Equal(t, state.ClearOutcomeUnknown, outcome)
	assert.ErrorIs(t, clearErr, state.ErrRuntimeLockHeld)

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "a corrupt held file must not be cleared")
}

// swapPathInodeOnce recreates path as a fresh, unlocked regular file,
// detaching the inode the caller's locked fd already references. It is the
// body of the afterRuntimeLockFlock seam: invoked after the acquirer's
// flock succeeds, it makes the locked inode no longer the file at path so
// the inode-identity verification trips.
func swapPathInodeOnce(t *testing.T, path string) {
	t.Helper()

	require.NoError(t, os.Remove(path))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// TestAcquireRuntimeLock_InodeSwapFirstAttemptRetriesToFreshInode proves
// the split-brain guard's happy retry path: a clear unlinks the lock in
// the open→flock window on the FIRST attempt, so the acquirer locks a
// detached inode, detects the mismatch, drops the orphaned flock, and
// RETRIES — the second attempt locks the fresh file at the path and
// succeeds. The returned handle's fd must be os.SameFile with the live
// path, proving the acquirer holds THE lock on the current inode, not the
// detached one.
func TestAcquireRuntimeLock_InodeSwapFirstAttemptRetriesToFreshInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")

	var calls int
	state.SwapAfterRuntimeLockFlockForTest(t, func() {
		calls++
		if calls == 1 {
			// Swap the inode out from under the first attempt's locked fd.
			swapPathInodeOnce(t, path)
		}
	})

	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err, "the retry must acquire the fresh inode")
	require.NotNil(t, handle)
	t.Cleanup(func() { _ = handle.Release() })

	assert.Equal(t, 2, calls, "the first attempt's detached inode must force exactly one retry")

	// The handle's fd must reference the file currently at path — proof the
	// acquirer holds the live inode, not the detached one.
	liveInfo, statErr := os.Stat(path)
	require.NoError(t, statErr)
	heldInfo, statErr := os.Stat(handle.Path())
	require.NoError(t, statErr)
	assert.True(t, os.SameFile(liveInfo, heldInfo),
		"the acquired handle must be on the live path inode after the retry")

	// The holder metadata was written to the live inode.
	holder, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Contains(t, string(holder), "\"command\": \"install\"",
		"the holder JSON must land on the fresh inode")
}

// TestAcquireRuntimeLock_InodeSwapBothAttemptsRefusesBusy proves the
// bounded-retry ceiling: when the inode is swapped on BOTH attempts (an
// unlink storm), the acquirer never proceeds on a detached inode. It
// refuses with the *LockHeldError busy report and writes NO holder
// metadata to either inode.
func TestAcquireRuntimeLock_InodeSwapBothAttemptsRefusesBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")

	var calls int
	state.SwapAfterRuntimeLockFlockForTest(t, func() {
		calls++
		swapPathInodeOnce(t, path) // detach on every attempt
	})

	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.Error(t, err)
	assert.Nil(t, handle)
	assert.Equal(t, 2, calls, "the bounded retry must make exactly two attempts")
	assert.ErrorIs(t, err, state.ErrRuntimeLockHeld,
		"a persistent inode swap must refuse busy; got %v", err)

	var heldErr *state.LockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, path, heldErr.Path)

	// Neither inode received holder metadata: the last-created file at path
	// is the fresh empty one from the seam, never an acquisition write.
	final, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Empty(t, final, "a refused acquisition must not write holder JSON")
}

// TestAcquireRuntimeLock_NoSwapTakesSingleAttempt pins that the
// split-brain guard is invisible on the normal no-clear path: with the
// seam a no-op, acquisition succeeds on the first attempt with no retry,
// so the verification adds no behavior change to every existing caller.
func TestAcquireRuntimeLock_NoSwapTakesSingleAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")

	var calls int
	state.SwapAfterRuntimeLockFlockForTest(t, func() { calls++ })

	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	require.NotNil(t, handle)
	t.Cleanup(func() { _ = handle.Release() })

	assert.Equal(t, 1, calls, "the no-clear path must acquire on the first attempt")
}

// TestClearOutcome_String pins the stable lowercase tokens used in logs.
func TestClearOutcome_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown", state.ClearOutcomeUnknown.String())
	assert.Equal(t, "free_leftover", state.ClearOutcomeFreeLeftover.String())
	assert.Equal(t, "stale_holder", state.ClearOutcomeStaleHolder.String())
	assert.Equal(t, "unknown", state.ClearOutcome(99).String())
}

// rewriteHolderStartedAt rewrites the on-disk runtime.lock holder's
// StartedAt (and re-asserts PID/StartedTime) so age-arm tests can model an
// arbitrarily old hold without waiting wall-clock time. It returns the
// holder as it now reads from disk so the test can derive a matching
// fingerprint. The flock held by the test's handle is on a distinct OFD,
// so this independent write does not contend.
func rewriteHolderStartedAt(
	t *testing.T,
	path string,
	startedAt time.Time,
	startedTime uint64,
) state.RuntimeLockInfo {
	t.Helper()

	probe, err := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, err)
	require.True(t, probe.Held)

	info := probe.Holder
	info.StartedAt = startedAt
	info.StartedTime = startedTime

	// Marshal with the exported field tags exactly as production does, so
	// readHolderBestEffort parses the rewrite back to the same value.
	raw, marshalErr := json.MarshalIndent(info, "", "  ")
	require.NoError(t, marshalErr)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	// Read it back so the returned value reflects the on-disk JSON exactly
	// (RFC3339 second/nanosecond fidelity through the marshal round-trip).
	reread, rereadErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, rereadErr)
	return reread.Holder
}
