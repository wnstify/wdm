package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// runtimeLockPath is the runtime.lock under the engine's state dir.
func runtimeLockPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime.lock")
}

// writeRuntimeLockFile writes holder metadata to the engine's runtime.lock
// without holding the flock, so the lock reads as Exists-but-not-Held (a
// free leftover with recorded metadata). It creates the state dir first.
func writeRuntimeLockFile(t *testing.T, stateDir string, info state.RuntimeLockInfo) string {
	t.Helper()

	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	raw, err := json.MarshalIndent(info, "", "  ")
	require.NoError(t, err)
	path := runtimeLockPath(stateDir)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// holdRuntimeLock writes holder metadata and then holds the exclusive
// flock through a SECOND, independent open file description until the test
// ends. Because flock is per-OFD, this conflicts with the engine's own
// fresh open — exactly modeling another holder pinning the lock. It
// returns the held *os.File so a test can release it early if needed.
func holdRuntimeLock(t *testing.T, stateDir string, info state.RuntimeLockInfo) (string, *os.File) {
	t.Helper()

	path := writeRuntimeLockFile(t, stateDir, info)
	return holdRuntimeLockPath(t, path)
}

// holdRuntimeLockPath holds the exclusive flock on an already-written lock
// file through a SECOND, independent open file description until the test
// ends, modeling another holder pinning the lock. It is the content-agnostic
// core of holdRuntimeLock, used directly when a test needs to hold a lock
// whose on-disk metadata is corrupt rather than valid JSON.
func holdRuntimeLockPath(t *testing.T, path string) (string, *os.File) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, state.LockExclusive(f))
	return path, f
}

// noSuchPID is a PID above the system's pid_max on Linux and macOS, so
// kill(pid, 0) always reports the process gone and the holder classifies
// NOT alive. This mirrors status_test.go's stale-lock fixture and avoids
// the PID-recycling flakiness a reaped child PID would carry — the state
// package's /proc start-time seam is not reachable from core tests, so a
// genuinely-impossible PID is the portable dead-holder model here.
const noSuchPID = 1 << 30

// liveHolderInfo builds holder metadata recording THIS test process (a
// genuinely live PID) with no kernel start-time, so the liveness probe
// trusts signal-0 and classifies the holder alive.
func liveHolderInfo(command string, startedAt time.Time) state.RuntimeLockInfo {
	return state.RuntimeLockInfo{
		SchemaVersion: 1,
		PID:           os.Getpid(),
		Command:       command,
		StartedAt:     startedAt,
		WDMVersion:    "0.1.0",
	}
}

// TestRuntimeLockStatus_Projection covers the read-only projection across
// the absent / free-leftover / held arms, including holder-field
// propagation and the holder_alive value.
func TestRuntimeLockStatus_Projection(t *testing.T) {
	t.Parallel()

	t.Run("absent lock reports nothing", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		require.NotNil(t, status)

		assert.False(t, status.Exists)
		assert.False(t, status.Held)
		assert.False(t, status.Stale)
		assert.Zero(t, status.HolderPID)
		assert.Empty(t, status.HolderCommand)
		assert.False(t, status.HolderAlive)
		assert.Nil(t, status.StartedAt)

		_, statErr := os.Stat(runtimeLockPath(stateDir))
		assert.True(t, os.IsNotExist(statErr),
			"RuntimeLockStatus must not create the runtime.lock")
	})

	t.Run("free leftover exists but is not held", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		writeRuntimeLockFile(t, stateDir, liveHolderInfo("install", time.Now().UTC()))

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		require.NotNil(t, status)

		assert.True(t, status.Exists)
		assert.False(t, status.Held, "an unheld leftover must not read as held")
		assert.False(t, status.Stale, "a free leftover is never stale")
		// The probe only reads holder metadata when held, so an unheld
		// leftover projects zero holder fields.
		assert.Zero(t, status.HolderPID)
		assert.Empty(t, status.HolderCommand)
		assert.False(t, status.HolderAlive)
	})

	t.Run("held live lock reports holder fields", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		started := time.Now().UTC().Add(-2 * time.Minute)
		holdRuntimeLock(t, stateDir, liveHolderInfo("update", started))

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		require.NotNil(t, status)

		assert.True(t, status.Exists)
		assert.True(t, status.Held)
		assert.False(t, status.Stale, "a live, recent holder is not stale")
		assert.Equal(t, os.Getpid(), status.HolderPID)
		assert.Equal(t, "update", status.HolderCommand)
		assert.True(t, status.HolderAlive)
		assert.Equal(t, "0.1.0", status.WDMVersion)
		require.NotNil(t, status.StartedAt)
		assert.True(t, started.Equal(*status.StartedAt))
	})
}

// TestRuntimeLockStatus_StaleDerivation pins that the engine-side Stale
// flag mirrors staleRuntimeLockCondition's policy across every arm: dead
// holder, live-within-age, live-beyond-age, and the ModTime fallback when
// StartedAt is zero.
func TestRuntimeLockStatus_StaleDerivation(t *testing.T) {
	t.Parallel()

	t.Run("dead holder is stale", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		info := liveHolderInfo("install", time.Now().UTC())
		info.PID = noSuchPID
		holdRuntimeLock(t, stateDir, info)

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Held)
		assert.False(t, status.HolderAlive, "an impossible PID is not alive")
		assert.True(t, status.Stale, "a dead holder is stale")
	})

	t.Run("live holder within age is not stale", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		holdRuntimeLock(t, stateDir, liveHolderInfo("update", time.Now().UTC().Add(-1*time.Hour)))

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Held)
		assert.True(t, status.HolderAlive)
		assert.False(t, status.Stale)
	})

	t.Run("live holder beyond age is stale", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		holdRuntimeLock(t, stateDir, liveHolderInfo("update", time.Now().UTC().Add(-48*time.Hour)))

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Held)
		assert.True(t, status.HolderAlive, "the holder is the live test process")
		assert.True(t, status.Stale, "held longer than 24h is stale")
	})

	t.Run("modtime fallback when startedat is zero", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		// Live PID, but no recorded StartedAt: staleness falls back to the
		// file's ModTime.
		info := state.RuntimeLockInfo{
			SchemaVersion: 1,
			PID:           os.Getpid(),
			Command:       "update",
			WDMVersion:    "0.1.0",
		}
		path, _ := holdRuntimeLock(t, stateDir, info)

		// Age the file's ModTime well beyond the staleness window.
		old := time.Now().Add(-72 * time.Hour)
		require.NoError(t, os.Chtimes(path, old, old))

		status, err := eng.RuntimeLockStatus(t.Context())
		require.NoError(t, err)
		assert.True(t, status.Held)
		assert.True(t, status.HolderAlive)
		assert.Nil(t, status.StartedAt, "no StartedAt was recorded")
		assert.True(t, status.Stale, "an old ModTime drives staleness when StartedAt is zero")
	})
}

// TestRuntimeLockStatus_NoWritesAndNoDeadlock proves the read-only path
// performs zero writes and never blocks behind another holder: it runs to
// completion while a distinct OFD holds the exclusive flock, and the lock
// file is byte-identical before and after.
func TestRuntimeLockStatus_NoWritesAndNoDeadlock(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	path, _ := holdRuntimeLock(t, stateDir, liveHolderInfo("update", time.Now().UTC()))

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// A short deadline would expire if the probe blocked on the held lock.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	status, err := eng.RuntimeLockStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.True(t, status.Held, "the probe sees the lock held but does not block")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "RuntimeLockStatus must not modify the lock file")
}

func TestRuntimeLockStatus_ClosedEngine(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	status, err := eng.RuntimeLockStatus(t.Context())
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, status)
}

func TestRuntimeLockStatus_CanceledContext(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	status, err := eng.RuntimeLockStatus(ctx)
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context must surface as context.Canceled, not a buried generic error")
	assert.Nil(t, status)
}

// recordingConfirmer captures the Confirmation it was asked to authorize
// and returns a fixed verdict / error.
type recordingConfirmer struct {
	approve bool
	err     error
	calls   int
	last    types.Confirmation
}

func (c *recordingConfirmer) Confirm(_ context.Context, conf types.Confirmation) (bool, error) {
	c.calls++
	c.last = conf
	return c.approve, c.err
}

// TestClearStaleRuntimeLock_LiveHolderRefused proves the the invariant
// invariant: a live, within-age holder is NEVER clearable. The refusal is
// ErrCodeRuntimeLockHeld and the hint names the PID and the kill-and-retry
// remediation; the Confirmer is never consulted and the file survives.
func TestClearStaleRuntimeLock_LiveHolderRefused(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	path, _ := holdRuntimeLock(t, stateDir, liveHolderInfo("update", time.Now().UTC().Add(-30*time.Minute)))

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	confirmer := &recordingConfirmer{approve: true}
	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(clearErr, types.ErrCodeRuntimeLockHeld),
		"a live holder must refuse with ErrCodeRuntimeLockHeld; got %v", clearErr)

	var typedErr *types.Error
	require.ErrorAs(t, clearErr, &typedErr)
	assert.Contains(t, typedErr.Hint, "pid "+strconv.Itoa(os.Getpid()),
		"the refusal must name the holder PID")
	assert.Contains(t, typedErr.Hint, "if the operation is wedged, kill that process and retry",
		"the refusal must carry the kill-and-retry remediation without asserting wedgedness")

	assert.Zero(t, confirmer.calls, "a live lock must never reach the Confirmer")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "refusing a live holder must not touch the file")
}

// TestClearStaleRuntimeLock_StaleHolderCleared covers the genuine recovery:
// a held lock whose recorded holder is dead is classified stale, confirmed,
// and cleared. The file is gone, the result reflects the cleared state, and
// the Confirmation carried the holder PID, age, reason, and SAFE kind.
func TestClearStaleRuntimeLock_StaleHolderCleared(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)

	confirmer := &recordingConfirmer{approve: true}
	status, err := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.NoError(t, err)
	require.NotNil(t, status)

	// The post-clear re-probe is honest: the file is gone.
	assert.False(t, status.Exists, "the cleared lock must report not-exists")
	assert.False(t, status.Held)
	assert.False(t, status.Stale)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the stale lock file must be removed")

	// The recovery prompt carried the holder details and the SAFE kind.
	require.Equal(t, 1, confirmer.calls)
	assert.Equal(t, "clear_stale_lock", confirmer.last.Kind,
		"the recovery confirmation must use the SAFE clear_stale_lock kind")
	assert.Contains(t, confirmer.last.Message, "pid "+strconv.Itoa(noSuchPID),
		"the prompt must name the holder PID")
	assert.Contains(t, confirmer.last.Message, "no longer running",
		"the prompt must explain the dead-holder reason")
	assert.Contains(t, confirmer.last.Message, "held for:",
		"the prompt must report the held age")
	assert.Contains(t, confirmer.last.Message, path,
		"the prompt must name the lock path")
}

// TestClearStaleRuntimeLock_WedgedLiveHolderCleared covers the age arm of
// the recovery: a LIVE holder (this test process) that has held the lock
// longer than the staleness window is classified stale, confirmed, and
// cleared. The recovery prompt's reason cites the held-too-long policy
// rather than a dead holder, proving the age-arm reason text.
func TestClearStaleRuntimeLock_WedgedLiveHolderCleared(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	// Live PID, held well beyond 24h: stale on age, not on liveness.
	path, _ := holdRuntimeLock(t, stateDir, liveHolderInfo("update", time.Now().UTC().Add(-48*time.Hour)))

	confirmer := &recordingConfirmer{approve: true}
	status, err := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.False(t, status.Exists, "the wedged lock must clear")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the wedged lock file must be removed")

	require.Equal(t, 1, confirmer.calls)
	assert.Contains(t, confirmer.last.Message, "held longer than 24h0m0s",
		"the age-arm reason must cite the staleness window")
}

// TestClearStaleRuntimeLock_DeclineLeavesLockUntouched proves a declined
// recovery maps to ErrCodeUserCanceled and leaves the lock file
// byte-identical.
func TestClearStaleRuntimeLock_DeclineLeavesLockUntouched(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	confirmer := &recordingConfirmer{approve: false}
	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(clearErr, types.ErrCodeUserCanceled),
		"a declined recovery must map to ErrCodeUserCanceled; got %v", clearErr)
	assert.Equal(t, 1, confirmer.calls)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a decline must not touch the lock file")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the lock file must survive a decline")
}

// TestClearStaleRuntimeLock_NilConfirmerRefused proves a stale lock with no
// confirmer refuses with ErrCodeUsageValidation and leaves the file intact.
func TestClearStaleRuntimeLock_NilConfirmerRefused(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), nil)
	require.Error(t, clearErr)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(clearErr, types.ErrCodeUsageValidation),
		"a nil confirmer on a stale lock must map to usage validation; got %v", clearErr)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a nil-confirmer refusal must not touch the file")
}

// TestClearStaleRuntimeLock_ConfirmerErrorPropagates proves a confirmer
// backend error propagates (wrapped) without clearing the lock.
func TestClearStaleRuntimeLock_ConfirmerErrorPropagates(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)

	sentinel := assertSentinel{}
	confirmer := &recordingConfirmer{err: sentinel}
	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)
	assert.ErrorIs(t, clearErr, sentinel, "the confirmer backend error must remain reachable")

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "a confirmer error must not clear the lock")
}

// TestClearStaleRuntimeLock_FreeLeftoverTidiedWithoutPrompt proves an
// unheld leftover lock is tidied as a benign cleanup: the file is removed,
// the Confirmer is never consulted, and the result honestly reports the
// cleared (free) state — never a wedge recovery. A nil confirmer is
// accepted because nothing is wedged.
func TestClearStaleRuntimeLock_FreeLeftoverTidiedWithoutPrompt(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	path := writeRuntimeLockFile(t, stateDir, liveHolderInfo("install", time.Now().UTC()))

	confirmer := &recordingConfirmer{approve: true}
	status, err := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.False(t, status.Exists, "the tidied leftover must report not-exists")
	assert.False(t, status.Held)
	assert.False(t, status.Stale)

	assert.Zero(t, confirmer.calls,
		"a free leftover is a tidy-up, not a recovery: the Confirmer must not be consulted")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the leftover file must be removed")

	// A fresh acquisition then succeeds, proving the path is reusable.
	handle, acqErr := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, acqErr)
	require.NoError(t, handle.Release())
}

// TestClearStaleRuntimeLock_MissingFileIsNoOp proves a clear against an
// absent lock succeeds as a benign no-op (idempotent), reporting the
// not-exists state with no confirmer consulted.
func TestClearStaleRuntimeLock_MissingFileIsNoOp(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	require.NoError(t, os.MkdirAll(stateDir, 0o700))

	confirmer := &recordingConfirmer{approve: true}
	status, err := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.False(t, status.Exists)
	assert.Zero(t, confirmer.calls, "no lock means nothing to confirm")
}

// TestClearStaleRuntimeLock_StateLockHeldErrorTranslated proves
// #1: when the lock drifts to a LIVE holder between the engine's stale
// classification and the state writer's re-verify, the *state.LockHeldError
// the writer returns is translated to ErrCodeRuntimeLockHeld (exit 4), not
// a raw exit-1 generic.
// The drift is induced under a still-held flock: the lock is held through a
// distinct OFD with a DEAD recorded holder (so the engine's probe
// classifies it stale and proceeds), and the confirmer — invoked after the
// stale classification, before the state writer runs — rewrites the on-disk
// holder to a LIVE process WITHOUT dropping the flock. The state writer's
// held arm then re-reads the live holder, classifies it NOT stale, and
// returns *LockHeldError, which the engine must surface as
// ErrCodeRuntimeLockHeld.
func TestClearStaleRuntimeLock_StateLockHeldErrorTranslated(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)

	// Held through a distinct OFD with a dead recorded holder: the engine's
	// probe sees Held + stale and proceeds to the clear.
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)

	// At confirm time, rewrite the file content to a LIVE holder. The flock
	// stays held by the test fd (rewriting content does not drop it), so the
	// state writer's held arm re-reads a live holder and refuses.
	confirmer := &rewriteHolderConfirmer{
		t:        t,
		path:     path,
		liveInfo: liveHolderInfo("update", time.Now().UTC()),
	}

	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(clearErr, types.ErrCodeRuntimeLockHeld),
		"a state-layer LockHeldError must translate to ErrCodeRuntimeLockHeld, not exit 1; got %v", clearErr)
	assert.ErrorIs(t, clearErr, state.ErrRuntimeLockHeld,
		"the underlying state sentinel must remain reachable")

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "a refused clear must not remove the live lock")
}

// rewriteHolderConfirmer rewrites the lock file's holder content to a live
// process at confirm time, modeling a fresh operation re-acquiring the lock
// in the classify→clear window while the flock stays held.
type rewriteHolderConfirmer struct {
	t        *testing.T
	path     string
	liveInfo state.RuntimeLockInfo
}

func (c *rewriteHolderConfirmer) Confirm(_ context.Context, _ types.Confirmation) (bool, error) {
	c.t.Helper()
	raw, err := json.MarshalIndent(c.liveInfo, "", "  ")
	require.NoError(c.t, err)
	require.NoError(c.t, os.WriteFile(c.path, raw, 0o600))
	return true, nil
}

// TestClearStaleRuntimeLock_CorruptHeldMetadataRefused covers a held lock
// whose on-disk metadata is corrupt (empty content → PID 0). The engine's
// probe classifies it stale via the dead-holder arm (a zero PID is never
// alive), so the recovery prompt fires naming "an unknown operation (pid 0)";
// the confirmer accepts, but the state writer refuses on its zero-PID
// fingerprint guard (a corrupt holder cannot be fingerprinted), returning
// *LockHeldError. The engine MUST translate that to ErrCodeRuntimeLockHeld
// with the honest "could not be re-verified as stale" wording — never the
// false "became active" copy — and leave the lock file untouched.
func TestClearStaleRuntimeLock_CorruptHeldMetadataRefused(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)

	// Create the lock file with corrupt (empty) content, then pin the
	// exclusive flock through a distinct OFD so the probe reads Held + a
	// zero-PID holder and classifies it stale on the dead-holder arm.
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	path := runtimeLockPath(stateDir)
	require.NoError(t, os.WriteFile(path, []byte{}, 0o600))
	holdRuntimeLockPath(t, path)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	confirmer := &recordingConfirmer{approve: true}
	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)

	// The engine classified the corrupt holder stale and prompted: the
	// payload names the placeholder holder and the dead-holder reason.
	require.Equal(t, 1, confirmer.calls,
		"a corrupt-but-held lock classifies stale and reaches the recovery prompt")
	assert.Contains(t, confirmer.last.Message, "an unknown operation (pid 0)",
		"the prompt must name the unreadable holder as pid 0")

	assert.True(t, types.IsCode(clearErr, types.ErrCodeRuntimeLockHeld),
		"the writer's zero-PID refusal must translate to ErrCodeRuntimeLockHeld; got %v", clearErr)
	assert.ErrorIs(t, clearErr, state.ErrRuntimeLockHeld,
		"the underlying state sentinel must remain reachable")

	var typedErr *types.Error
	require.ErrorAs(t, clearErr, &typedErr)
	assert.Equal(t, "the runtime lock could not be cleared: the holder could not be re-verified as stale", typedErr.Message,
		"the refusal copy must be honest for corrupt metadata, not assert the lock became active")
	assert.NotContains(t, typedErr.Message, "became active",
		"a corrupt held lock did not become active")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused clear must not touch the lock file")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the corrupt lock file must survive the refusal")
}

// TestClearStaleRuntimeLock_ModTimeDivergenceRefused covers the ModTime
// fallback divergence: a held lock with a LIVE recorded holder, a zero
// StartedAt, and an old file mtime. The engine classifies it stale via its
// ModTime fallback (StartedAt is zero, so it falls back to the file mtime,
// which is older than the staleness window) and prompts with the age reason;
// the confirmer accepts, but the state writer's age policy has no ModTime in
// scope — a live holder with a zero StartedAt is NOT-stale-on-age there — so
// it refuses with *LockHeldError. The engine MUST translate that to the same
// honest ErrCodeRuntimeLockHeld refusal (nothing became active here either).
func TestClearStaleRuntimeLock_ModTimeDivergenceRefused(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)

	// Live PID, no recorded StartedAt: the engine's staleness falls back to
	// the file ModTime, which the writer's age policy does not share.
	info := state.RuntimeLockInfo{
		SchemaVersion: 1,
		PID:           os.Getpid(),
		Command:       "update",
		WDMVersion:    "0.1.0",
	}
	path, _ := holdRuntimeLock(t, stateDir, info)

	// Age the file's ModTime well beyond the staleness window so the engine
	// classifies it stale via the ModTime fallback.
	old := time.Now().Add(-72 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	confirmer := &recordingConfirmer{approve: true}
	status, clearErr := eng.ClearStaleRuntimeLock(t.Context(), confirmer)
	require.Error(t, clearErr)
	assert.Nil(t, status)

	// The engine classified the lock stale (ModTime fallback) and prompted.
	require.Equal(t, 1, confirmer.calls,
		"the ModTime fallback classifies the lock stale and reaches the recovery prompt")
	assert.Contains(t, confirmer.last.Message, "held longer than 24h0m0s",
		"the age-arm reason must cite the staleness window")

	assert.True(t, types.IsCode(clearErr, types.ErrCodeRuntimeLockHeld),
		"the writer's age-policy refusal must translate to ErrCodeRuntimeLockHeld; got %v", clearErr)
	assert.ErrorIs(t, clearErr, state.ErrRuntimeLockHeld,
		"the underlying state sentinel must remain reachable")

	var typedErr *types.Error
	require.ErrorAs(t, clearErr, &typedErr)
	assert.Equal(t, "the runtime lock could not be cleared: the holder could not be re-verified as stale", typedErr.Message,
		"the refusal copy must be honest for a policy divergence, not assert the lock became active")
	assert.NotContains(t, typedErr.Message, "became active",
		"a ModTime-policy divergence did not make the lock active")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused clear must not touch the lock file")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the lock file must survive the refusal")
}

func TestClearStaleRuntimeLock_ClosedEngine(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	status, err := eng.ClearStaleRuntimeLock(t.Context(), &recordingConfirmer{approve: true})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, status)
}

func TestClearStaleRuntimeLock_CanceledContext(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	// A genuinely stale lock so a missed ctx check would mutate it.
	info := liveHolderInfo("install", time.Now().UTC().Add(-10*time.Minute))
	info.PID = noSuchPID
	path, _ := holdRuntimeLock(t, stateDir, info)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	status, clearErr := eng.ClearStaleRuntimeLock(ctx, &recordingConfirmer{approve: true})
	require.ErrorIs(t, clearErr, context.Canceled)
	assert.Nil(t, status)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a canceled ctx must not touch the lock file")
}

// assertSentinel is a distinct error type so confirmer-error propagation
// can be asserted with errors.Is without colliding with package sentinels.
type assertSentinel struct{}

func (assertSentinel) Error() string { return "confirmer backend failure" }
