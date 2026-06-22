package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the RuntimeLockStatus and ClearStaleRuntimeLock engine
// methods (PRD §26, §18 condition 8).
// Both methods read the global runtime.lock at the engine's
// stateDir-joined [runtimeLockFilename] — the same path the read-only
// status path probes via [Engine.staleRuntimeLockCondition]. The engine
// owns the staleness POLICY ([runtimeLockProbeStale] in status.go); the
// state layer owns the clear MECHANISM ([state.ClearStaleRuntimeLock]).
// RuntimeLockStatus is strictly read-only: it projects a
// [state.RuntimeLockProbe] into [types.RuntimeLockStatus] without
// acquiring, creating, or deleting the lock. ClearStaleRuntimeLock
// operates ON the runtime.lock itself, so — unlike every other
// state-changing engine method — it deliberately does NOT acquire the
// global runtime.lock: the lock it must clear may be the very lock a
// wedged operation is starving everyone behind, and acquiring it would
// block forever. Cross-process safety for the clear is the state writer's
// concern (its non-blocking re-verify plus holder fingerprint), not a
// held runtime.lock.
// See restart.go for the callback-type and blank-identifier rationale.

// RuntimeLockStatus reports the current global runtime.lock state (PRD
// §26, §18 condition 8) without mutating it. It probes the lock through
// the read-only [state.ProbeRuntimeLock] and projects the result into a
// [types.RuntimeLockStatus], deriving the Stale flag from the engine's
// own staleness policy ([runtimeLockProbeStale]) so a UI layer never has
// to compute it — and so a lock this method reports Stale is exactly a
// lock [Engine.ClearStaleRuntimeLock] would clear.
// The path writes nothing and never acquires, creates, or deletes the
// lock, so it is safe to call while another process holds the lock — the
// probe's own shared flock is non-blocking and released immediately. When
// no lock file exists, the returned status is the zero value (Exists
// false, every other field zero).
func (e *Engine) RuntimeLockStatus(ctx context.Context) (*types.RuntimeLockStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.RuntimeLockStatus: %w", err)
	}
	probe, err := state.ProbeRuntimeLock(ctx, e.runtimeLockPath())
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be inspected",
			"check the wdm state directory permissions and retry",
			err,
		)
	}
	return projectRuntimeLockStatus(probe), nil
}

// ClearStaleRuntimeLock recovers a wedged global runtime.lock by removing
// it — but ONLY when it is provably stale (PRD §26:689, the invariant,
// forbidden to weaken). It probes the lock fresh, classifies staleness
// with the same engine-side policy [Engine.RuntimeLockStatus] reports
// ([runtimeLockProbeStale]), and:
//   - A lock held by a LIVE, within-age holder is NEVER clearable: the
//     method refuses with [types.ErrCodeRuntimeLockHeld] (cmd/wdm exit 4)
//     naming the holder PID, the held duration, and the kill-and-retry
//     remediation. The Confirmer is never consulted on this path.
//   - A STALE lock (a dead/recycled holder, or a live holder held beyond
//     [staleRuntimeLockAge]) requires explicit authorization. The
//     Confirmer is consulted BEFORE any mutation with a
//     [confirmationKindClearStaleLock] payload naming the lock path,
//     holder, held age, and why the lock is classified stale. A nil
//     confirmer refuses with [types.ErrCodeUsageValidation]; a decline
//     maps to [types.ErrCodeUserCanceled] with the file untouched; a
//     confirmer backend error propagates wrapped. On authorization the
//     clear runs through [state.ClearStaleRuntimeLock], bound to the
//     holder fingerprint this method observed so a different holder that
//     acquired the lock in the window is refused rather than cleared.
//   - A free leftover lock (the file present but unheld — post-release
//     residue) is tidied without a recovery prompt: nothing is wedged, so
//     removing the file is a benign cleanup. The result reports the lock
//     was already free rather than asserting a stale recovery. A missing
//     lock file is the same benign no-op.
//
// As the recovery flow for the global lock, ClearStaleRuntimeLock does NOT
// acquire the runtime.lock (see the file-level comment). It makes no
// Docker contact. The returned [types.RuntimeLockStatus] is a fresh
// post-clear re-probe so the caller sees the honest cleared state.
func (e *Engine) ClearStaleRuntimeLock(ctx context.Context, confirmer types.Confirmer) (*types.RuntimeLockStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ClearStaleRuntimeLock: %w", err)
	}

	path := e.runtimeLockPath()
	probe, err := state.ProbeRuntimeLock(ctx, path)
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be inspected",
			"check the wdm state directory permissions and retry",
			err,
		)
	}

	// A held-but-not-stale lock is a live operation in flight: never
	// clearable. Refuse before consulting the Confirmer.
	if probe.Held && !runtimeLockProbeStale(probe) {
		return nil, liveRuntimeLockRefusal(probe)
	}

	// A stale lock is the §26:689 recovery: gate it on the safe-recovery
	// prompt before any mutation. A free leftover (unheld file) is a
	// benign tidy-up and skips the prompt entirely.
	if probe.Held {
		if err := confirmClearStaleRuntimeLock(ctx, confirmer, path, probe); err != nil {
			return nil, err
		}
	}

	if err := e.runStaleRuntimeLockClear(ctx, path, probe); err != nil {
		return nil, err
	}

	cleared, err := state.ProbeRuntimeLock(ctx, path)
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be inspected after clearing",
			"check the wdm state directory permissions and retry",
			err,
		)
	}
	return projectRuntimeLockStatus(cleared), nil
}

// runtimeLockPath is the absolute path of the global runtime.lock under
// the engine's state dir, shared by the read-only probe and the clear.
func (e *Engine) runtimeLockPath() string {
	return filepath.Join(e.stateDir, runtimeLockFilename)
}

// projectRuntimeLockStatus maps a [state.RuntimeLockProbe] onto the
// UI-facing [types.RuntimeLockStatus], deriving Stale from the engine's
// staleness policy. Holder metadata is best-effort: the probe populates
// it only when the lock is held, so an unheld or absent lock yields zero
// holder fields. StartedAt is a pointer so an unrecorded acquisition
// timestamp serializes as absent rather than the zero time.
func projectRuntimeLockStatus(probe state.RuntimeLockProbe) *types.RuntimeLockStatus {
	status := &types.RuntimeLockStatus{
		Exists:        probe.Exists,
		Held:          probe.Held,
		Stale:         runtimeLockProbeStale(probe),
		HolderPID:     probe.Holder.PID,
		HolderCommand: probe.Holder.Command,
		HolderAlive:   probe.HolderAlive,
		WDMVersion:    probe.Holder.WDMVersion,
	}
	if !probe.Holder.StartedAt.IsZero() {
		startedAt := probe.Holder.StartedAt
		status.StartedAt = &startedAt
	}
	return status
}

// confirmationKindClearStaleLock is the [types.Confirmation] Kind for the
// stale-runtime-lock recovery prompt (PRD §26:689 "safe recovery
// prompt"). It follows the inline-kind precedent — confirmation kinds are
// unexported string literals at their consequence site (only
// [types.ConfirmationKindDeleteDestructive] is exported, for the
// double-confirm delete flow). Clearing a stale lock is a SAFE recovery,
// SAFE matrix) may accept it.
const confirmationKindClearStaleLock = "clear_stale_lock"

// confirmClearStaleRuntimeLock consults the Confirmer before clearing a
// stale lock (PRD §26:689). A nil confirmer refuses with
// [types.ErrCodeUsageValidation] per the pkg/engine contract; a decline
// maps to [types.ErrCodeUserCanceled] with the lock file untouched; a
// confirmer backend error propagates wrapped.
func confirmClearStaleRuntimeLock(
	ctx context.Context,
	confirmer types.Confirmer,
	path string,
	probe state.RuntimeLockProbe,
) error {
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before clearing the runtime lock",
			"pass a confirmer that can authorize the stale-lock recovery",
		)
	}
	confirmed, err := confirmer.Confirm(ctx, clearStaleRuntimeLockConfirmation(path, probe))
	if err != nil {
		return fmt.Errorf("core.clearStaleRuntimeLock: confirming recovery: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"stale runtime lock recovery canceled",
			"re-run the recovery and confirm the prompt to clear the stale lock",
		)
	}
	return nil
}

// clearStaleRuntimeLockConfirmation assembles the safe-recovery
// consequence payload (PRD §26:689): the lock path, the holder identity
// (command and PID), how long it has been held, why it is classified
// stale (a dead/recycled holder versus a live hold beyond the staleness
// window), and the consequence of clearing it (a new operation may
// proceed). The payload carries no secret values — holder command, PID,
// and timestamps only — so it is sink-safe.
func clearStaleRuntimeLockConfirmation(path string, probe state.RuntimeLockProbe) types.Confirmation {
	lines := []string{
		"runtime lock: " + path,
		fmt.Sprintf("held by: %s (pid %d)", runtimeLockHolderLabel(probe.Holder.Command), probe.Holder.PID),
		"held for: " + runtimeLockHeldDescription(probe),
		"why stale: " + runtimeLockStaleReason(probe),
		"clearing lets a new wdm operation proceed",
	}
	return types.Confirmation{
		Kind:    confirmationKindClearStaleLock,
		Title:   "clear stale runtime lock",
		Message: strings.Join(lines, "\n"),
	}
}

// runtimeLockStaleReason describes which arm of the staleness policy
// classified the lock stale, for the recovery prompt.
func runtimeLockStaleReason(probe state.RuntimeLockProbe) string {
	if !probe.HolderAlive {
		return "the holder process is no longer running (dead or recycled pid)"
	}
	return fmt.Sprintf("the lock has been held longer than %s", staleRuntimeLockAge)
}

// runtimeLockHolderLabel renders the holder's recorded command, falling
// back to a stable placeholder when the lock file recorded none (an
// unparseable or empty holder).
func runtimeLockHolderLabel(command string) string {
	if command == "" {
		return "an unknown operation"
	}
	return command
}

// runtimeLockHeldDescription renders the lock's held duration for the
// prompt, using the recorded StartedAt and falling back to the file's
// ModTime (mirroring [runtimeLockProbeStale]'s held-since derivation).
// When neither is available it reports an unknown duration.
func runtimeLockHeldDescription(probe state.RuntimeLockProbe) string {
	heldSince := probe.Holder.StartedAt
	if heldSince.IsZero() {
		heldSince = probe.ModTime
	}
	if heldSince.IsZero() {
		return "an unknown duration"
	}
	return time.Since(heldSince).Round(time.Second).String()
}

// liveRuntimeLockRefusal builds the the invariant live-lock refusal: a
// typed [types.ErrCodeRuntimeLockHeld] error whose hint names the holder
// PID, the held duration, and the kill-and-retry remediation. The clear
// never touches a live within-age lock; the operator must stop the
// holding process and retry.
func liveRuntimeLockRefusal(probe state.RuntimeLockProbe) error {
	// The holder may be a healthy concurrent operation rather than a wedge,
	// so the hint carries the the invariant elements (PID, held duration,
	// kill-and-retry remediation) without asserting the hold is wedged.
	hint := fmt.Sprintf(
		"the runtime lock is held by pid %d for %s; if the operation is wedged, kill that process and retry",
		probe.Holder.PID,
		runtimeLockHeldDescription(probe),
	)
	return types.NewError(
		types.ErrCodeRuntimeLockHeld,
		"the runtime lock is held by a live operation and cannot be cleared",
		hint,
	)
}

// runStaleRuntimeLockClear invokes the state-layer clear mechanism for a
// stale or free lock, binding the request to the holder fingerprint this
// engine observed (so a holder that drifted in the window is refused) and
// supplying the engine's [staleRuntimeLockAge] policy as MaxHeldAge.
//   - A [*state.LockHeldError] means the writer could not re-verify the
//     lock as stale. This is reachable three ways, none of which is "the
//     lock became active" specifically: the lock genuinely went live or
//     its fingerprint drifted between this engine's probe and the writer's
//     re-verify; the held lock's recorded metadata is corrupt (a zero PID
//     the writer refuses to fingerprint); or the engine classified the
//     lock stale via its ModTime fallback, which the writer's
//     StartedAt-only age policy does not share. All three MUST route
//     through the same errors.As → [types.WrapError] with
//     [types.ErrCodeRuntimeLockHeld] translation [acquireRuntimeLock]
//     applies, never bubbling raw to a generic exit-1 error, and the
//     user-facing copy MUST stay honest across all three.
//   - The two clear outcomes are distinguished by [state.ClearStaleRuntimeLock]
//     ([state.ClearOutcomeFreeLeftover] versus [state.ClearOutcomeStaleHolder]);
//     the post-clear re-probe in ClearStaleRuntimeLock reflects the true
//     cleared state regardless, so a benign tidy-up is never presented as
//     a wedge recovery.
func (e *Engine) runStaleRuntimeLockClear(
	ctx context.Context,
	path string,
	probe state.RuntimeLockProbe,
) error {
	req := state.ClearStaleRuntimeLockRequest{
		MaxHeldAge:          staleRuntimeLockAge,
		ExpectedPID:         probe.Holder.PID,
		ExpectedStartedAt:   probe.Holder.StartedAt,
		ExpectedStartedTime: probe.Holder.StartedTime,
	}
	outcome, err := state.ClearStaleRuntimeLock(ctx, path, req)
	if err == nil {
		// The frozen return type cannot carry the outcome, so the logger
		// reports which arm cleared the lock (a benign free-leftover tidy-up
		// versus the the invariant stale-holder recovery). One record per
		// successful clear.
		e.logger.InfoContext(ctx, "core: cleared runtime lock",
			slog.String("outcome", outcome.String()),
			slog.String("path", path),
		)
		return nil
	}

	// The writer refused the clear: surface it as a held-lock refusal
	// (exit 4), mirroring acquireRuntimeLock's translation, rather than
	// letting the raw *LockHeldError reach the generic exit-1 mapping. The
	// copy stays honest across all three reachable sub-cases (genuine
	// live/drift, corrupt metadata, ModTime policy divergence): the holder
	// could not be re-verified as stale — it may have become active, or its
	// metadata may be unreadable.
	var lockHeld *state.LockHeldError
	if errors.As(err, &lockHeld) {
		hint := "the lock may have become active or its metadata may be unreadable; re-run `wdm lock status`, and kill the holding process and retry if it is wedged"
		if lockHeld.Holder.Command != "" {
			hint = fmt.Sprintf(
				"in-progress: %q (pid %d); wait for it to finish, or kill that process and retry if it is wedged",
				lockHeld.Holder.Command,
				lockHeld.Holder.PID,
			)
		}
		return types.WrapError(
			types.ErrCodeRuntimeLockHeld,
			"the runtime lock could not be cleared: the holder could not be re-verified as stale",
			hint,
			err,
		)
	}
	return err
}
