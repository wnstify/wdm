//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnstify/wdm/pkg/types"
)

// ClearStaleRuntimeLockRequest is the caller-supplied input to
// [ClearStaleRuntimeLock]. It carries the staleness POLICY (the engine
// owns the threshold per the invariant — internal/state must not import
// internal/core) plus the fingerprint of the holder the caller observed
// stale, so the writer can refuse to clear a DIFFERENT holder that
// acquired the lock between the caller's probe and this call.
// The expected-holder fields are sourced from the [RuntimeLockProbe]
// (or the engine's RuntimeLockStatus projection of it) the caller took
// immediately before deciding the lock was stale. They bind the clear
// to that specific observation: if the live re-read inside the writer
// drifts from this fingerprint, a new holder rewrote the file in the
// window and the clear is refused (see [ClearStaleRuntimeLock]).
type ClearStaleRuntimeLockRequest struct {
	// MaxHeldAge is the held-duration threshold beyond which a LIVE
	// holder counts as wedged and therefore stale. The engine supplies
	// this from its staleRuntimeLockAge constant (the invariant / PRD
	// §18 condition 8 — "file mtime > 24h"); the state layer never
	// hard-codes the value because the policy lives engine-side.
	// A non-positive MaxHeldAge disables the age arm entirely: a live
	// holder is then clearable ONLY if it is dead/recycled, never on
	// age alone. This is the conservative default for callers that do
	// not want age-based clearing.
	MaxHeldAge time.Duration

	// ExpectedPID is the holder PID the caller observed stale
	// ([RuntimeLockProbe.Holder.PID]). The live re-read inside the
	// writer must match it, or the clear is refused as drifted.
	ExpectedPID int

	// ExpectedStartedAt is the holder's recorded acquisition timestamp
	// ([RuntimeLockProbe.Holder.StartedAt]). Part of the drift
	// fingerprint; compared with [time.Time.Equal] so monotonic-clock
	// stripping across a JSON round-trip does not cause a false drift.
	ExpectedStartedAt time.Time

	// ExpectedStartedTime is the holder's recorded kernel start-time
	// ([RuntimeLockProbe.Holder.StartedTime] — clock ticks since boot).
	// Part of the drift fingerprint. Zero is a legitimate value (old
	// lock files and non-Linux acquisitions carry no start-time), so a
	// zero ExpectedStartedTime matches only a zero live re-read.
	ExpectedStartedTime uint64
}

// ClearOutcome reports which arm of [ClearStaleRuntimeLock] cleared the
// lock so the caller (F3) can phrase distinct user-facing copy: a lock
// already free when the writer re-verified is a benign tidy-up, while a
// lock cleared out from under a wedged stale holder is the the invariant
// stale-lock recovery proper.
type ClearOutcome int

const (
	// ClearOutcomeUnknown is the zero value and never returned by a
	// successful clear. It guards against a caller treating an
	// unset outcome as a meaningful one.
	ClearOutcomeUnknown ClearOutcome = iota

	// ClearOutcomeFreeLeftover means the writer acquired the exclusive
	// flock during re-verification — nobody held the lock, so the file
	// was post-release residue (or the wedge had already exited). The
	// unlink ran WHILE the writer held the flock, so no concurrent
	// opener of the old inode could acquire it.
	ClearOutcomeFreeLeftover

	// ClearOutcomeStaleHolder means the writer could NOT acquire the
	// flock (the wedged holder still pins it) but re-classified the
	// holder as stale — a dead/recycled PID, or a live holder held
	// beyond MaxHeldAge — and the live re-read matched the caller's
	// expected fingerprint. This is the the invariant recovery: the
	// path is unlinked while the orphaned flock survives harmlessly on
	// the now-detached inode.
	ClearOutcomeStaleHolder
)

// String renders the outcome as a stable lowercase token for logs and
// diagnostics.
func (o ClearOutcome) String() string {
	switch o {
	case ClearOutcomeFreeLeftover:
		return "free_leftover"
	case ClearOutcomeStaleHolder:
		return "stale_holder"
	default:
		return "unknown"
	}
}

// ClearStaleRuntimeLock removes the global runtime.lock at path — but
// ONLY when the lock is provably clearable, never a live one
// #45, forbidden to weaken). It is the state-layer MECHANISM behind the
// engine's ClearStaleRuntimeLock; the engine owns the staleness policy
// (MaxHeldAge), the Confirmer gate, and the user-facing refusal copy.
// The writer re-verifies through its OWN freshly opened descriptor
// immediately before acting, because the world can change between the
// caller's probe and this call: the wedged holder may have exited, or a
// NEW live operation may have acquired and rewritten the file. Two arms,
// each with its own safe shape:
//   - Free arm. A non-blocking LOCK_EX succeeds → nobody holds the lock,
//     so the file is post-release residue (or the wedge already exited).
//     The unlink runs WHILE the writer still holds the flock, which
//     fences only the interleavings that ATTEMPT a flock during the hold:
//     a concurrent opener that already has the old inode open and reaches
//     its own non-blocking flock before this writer's Close fails against
//     the held lock and retries the fresh path. It does NOT fence the
//     open-before-unlink / flock-after-close interleaving — an acquirer
//     that opened the path before the unlink can flock the now-orphaned
//     inode after this writer's Close releases the lock. That interleaving
//     is closed instead by [AcquireRuntimeLock]'s post-flock
//     inode-identity verification, which makes such an acquirer detect
//     the detached inode and retry the fresh path. Returns
//     [ClearOutcomeFreeLeftover]. A free lock is treated as a sanctioned
//     tidy-up rather than a "nothing to clear" refusal: a leftover file
//     confuses the operator ("ls shows runtime.lock — is something
//     running?") and removing it is safe because nobody holds it.
//   - Held arm. The non-blocking LOCK_EX fails (EWOULDBLOCK) → the wedge
//     still pins the flock. The writer re-reads the holder metadata and
//     re-classifies with [holderStillAlive] fused with MaxHeldAge:
//   - holder ALIVE and within MaxHeldAge → REFUSE with a
//     [*LockHeldError] wrapping [ErrRuntimeLockHeld] (cmd/wdm maps
//     this to ErrCodeRuntimeLockHeld → exit 4). This is the
//     the invariant live-lock refusal.
//   - holder classified stale (dead/recycled PID, OR live but held
//     beyond MaxHeldAge) AND the live re-read matches the caller's
//     expected fingerprint → unlink the path. The writer CANNOT hold
//     the flock here (the wedge holds it), so the re-verify→unlink
//     window cannot be closed by a held lock; it is closed instead by
//     the fingerprint match, which binds the unlink to the specific
//     holder the caller observed stale. Returns
//     [ClearOutcomeStaleHolder].
//   - the live re-read DRIFTS from the caller's expected fingerprint
//     → REFUSE (a different holder acquired in the window; the
//     caller's staleness observation no longer applies). The drift
//     refusal also wraps [ErrRuntimeLockHeld] because the safe
//     response is identical to a live lock: do not touch it.
//
// Held-lock race: between the fingerprint-confirming re-read
// and the os.Remove, a stale holder could in theory exit AND a new
// LIVE acquirer win the flock AND rewrite the file. If that new holder
// also evaded the fingerprint, this writer would unlink a lock a live
// operation holds — SPLIT BRAIN: the surviving holder keeps its flock on
// the detached inode while a later acquirer opens the freshly recreated
// path, and two processes would each believe they hold THE global lock
// (the PRD §26 violation). Evading the fingerprint requires the rewritten
// file to carry the SAME PID, the SAME nanosecond StartedAt, and the SAME
// kernel start-time as the holder this writer observed stale, which is
// not achievable in the microseconds the window spans — so the
// fingerprint renders the race astronomically improbable, not harmless.
// This race is physically unfixable from the clear side (the writer
// cannot hold the wedge's flock to fence the unlink) but is also closed
// from the acquirer side: [AcquireRuntimeLock]'s post-flock
// inode-identity verification makes any fresh acquirer that locked the
// detached inode detect the swap and retry the fresh path, so a
// would-be second holder cannot proceed even in the impossible-fingerprint
// case.
// A missing file is reported as already-cleared (nil error,
// [ClearOutcomeFreeLeftover]) so the operation is idempotent: a second
// clear, or a clear racing another tool's removal, never fails. Every
// refusal path leaves the lock file's bytes untouched. path MUST be
// absolute; ctx is honored at entry only — the syscalls below are local
// and fast.
func ClearStaleRuntimeLock(
	ctx context.Context,
	path string,
	req ClearStaleRuntimeLockRequest,
) (ClearOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ClearOutcomeUnknown, fmt.Errorf("state.ClearStaleRuntimeLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return ClearOutcomeUnknown, types.NewError(
			types.ErrCodeUsageValidation,
			"runtime lock path must be absolute",
			"this is a wdm internal invariant; report it if you see it",
		)
	}

	// G304 is suppressed: path is the engine-controlled XDG runtime.lock
	// location, validated absolute above. Same rationale as
	// AcquireRuntimeLock / ProbeRuntimeLock. The file is never created —
	// a missing lock is already-cleared.
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: engine-controlled XDG path, validated absolute
	if errors.Is(err, os.ErrNotExist) {
		// No file means no lock to clear. Idempotent success.
		return ClearOutcomeFreeLeftover, nil
	}
	if err != nil {
		return ClearOutcomeUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be opened for clearing",
			"check the wdm state directory permissions and retry",
			fmt.Errorf("state.ClearStaleRuntimeLock: opening %q: %w", path, err),
		)
	}
	// The fd is closed on every return path. In the free arm the close
	// also releases the writer's exclusive flock — but only AFTER the
	// unlink, so the held flock guards the unlink window. Close is
	// best-effort: the unlink (when it ran) already reached the goal
	// state, and the kernel releases the flock with the fd regardless.
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort fd teardown; unlink already reached goal state, kernel releases flock on close

	got, err := TryLockExclusive(f)
	if err != nil {
		return ClearOutcomeUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be inspected for clearing",
			"check the wdm state directory permissions and retry",
			fmt.Errorf("state.ClearStaleRuntimeLock: %w", err),
		)
	}

	if got {
		// Free arm: the writer holds the exclusive flock (released only
		// by the deferred Close above, AFTER this unlink), so the unlink
		// runs under the held lock and any concurrent opener of the old
		// inode fails its own non-blocking flock and retries the fresh
		// path on the next AcquireRuntimeLock.
		return clearFreeRuntimeLock(path)
	}
	return clearHeldRuntimeLock(path, f, req)
}

// clearFreeRuntimeLock handles the free arm: the caller holds the
// exclusive flock (its deferred Close has not run yet), so nobody else
// does. It unlinks the path; because the flock is still held, a
// concurrent opener that reaches its own non-blocking flock during the
// hold fails and falls through to the fresh path. An opener that flocks
// only AFTER this writer's Close lands on the orphaned inode instead and
// is caught by [AcquireRuntimeLock]'s post-flock inode-identity
// verification, not by the held flock here. A racing unlink by another
// tool surfaces as os.ErrNotExist and is folded into success — the goal
// state (file gone) is reached either way.
func clearFreeRuntimeLock(path string) (ClearOutcome, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ClearOutcomeUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock file could not be removed",
			"check the wdm state directory permissions and retry",
			fmt.Errorf("state.ClearStaleRuntimeLock: removing free lock %q: %w", path, err),
		)
	}
	return ClearOutcomeFreeLeftover, nil
}

// clearHeldRuntimeLock handles the held arm: the writer could NOT
// acquire the flock, so a holder (wedged or live) still pins it. It
// re-reads the holder, re-classifies staleness, refuses a live or
// drifted holder, and otherwise unlinks the path. The writer cannot hold
// the flock here, so the re-verify→unlink window is closed by the
// fingerprint match rather than by lock holding.
func clearHeldRuntimeLock(
	path string,
	f *os.File,
	req ClearStaleRuntimeLockRequest,
) (ClearOutcome, error) {
	holder := readHolderBestEffort(f)

	// Re-classify staleness through F1's liveness fusion plus the
	// caller's age policy. A live, within-age holder is NEVER clearable.
	if !runtimeLockHolderStale(holder, req.MaxHeldAge) {
		return ClearOutcomeUnknown, &LockHeldError{Path: path, Holder: holder}
	}

	// Bind the clear to the specific holder the caller observed stale.
	// If the live re-read drifted, a different holder acquired the lock
	// in the window between the caller's probe and this re-read — refuse
	// rather than clear a holder the caller never classified stale.
	if !holderFingerprintMatches(holder, req) {
		return ClearOutcomeUnknown, &LockHeldError{Path: path, Holder: holder}
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ClearOutcomeUnknown, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock file could not be removed",
			"check the wdm state directory permissions and retry",
			fmt.Errorf("state.ClearStaleRuntimeLock: removing stale lock %q: %w", path, err),
		)
	}
	return ClearOutcomeStaleHolder, nil
}

// runtimeLockHolderStale reports whether the held holder counts as stale
// under the same two-condition policy [Engine.staleRuntimeLockCondition]
// (internal/core) evaluates: the recorded holder is gone (dead or
// recycled PID, via [holderStillAlive]) OR a still-live holder has held
// the lock longer than maxHeldAge. A non-positive maxHeldAge disables
// the age arm so only a dead/recycled holder is stale.
// Held-duration derivation mirrors the engine: it uses the recorded
// StartedAt. The writer has no ModTime in scope, so a live holder with a
// zero StartedAt is treated as NOT-stale-on-age (fail toward refusing
// the clear). The age path is therefore reachable only when StartedAt is
// present, which acquisition always populates; a live holder with a zero
// StartedAt could only come from a corrupt file, where refusing the
// age-based clear is the safe bias (the dead-holder arm still clears it
// if the PID is gone).
func runtimeLockHolderStale(holder RuntimeLockInfo, maxHeldAge time.Duration) bool {
	if !holderStillAlive(holder) {
		return true
	}
	if maxHeldAge <= 0 {
		return false
	}
	if holder.StartedAt.IsZero() {
		return false
	}
	return time.Since(holder.StartedAt) > maxHeldAge
}

// holderFingerprintMatches reports whether the live re-read holder is the
// same one the caller observed stale, comparing PID, acquisition
// timestamp, and kernel start-time. StartedAt is compared with
// [time.Time.Equal] so a JSON round-trip (which strips the monotonic
// reading and may shift the location) does not register as a drift.
// A zero ExpectedPID never matches: the caller must pass a real observed
// holder, and an unreadable on-disk holder (PID 0) cannot be
// fingerprinted, so a corrupt-but-held file is refused rather than
// cleared on a degenerate all-zero match.
func holderFingerprintMatches(holder RuntimeLockInfo, req ClearStaleRuntimeLockRequest) bool {
	if req.ExpectedPID == 0 || holder.PID == 0 {
		return false
	}
	if holder.PID != req.ExpectedPID {
		return false
	}
	if !holder.StartedAt.Equal(req.ExpectedStartedAt) {
		return false
	}
	return holder.StartedTime == req.ExpectedStartedTime
}
