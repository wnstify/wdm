package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the ApplySelfUpdate engine method (PRD §14, §31). It is
// state-changing and mirrors
// [Engine.Update] / [Engine.ApplyCatalogUpdate]'s posture: it holds the
// global runtime.lock, emits progress, and gates the consequential step on a
// Confirmer.
// Strict ordering — VERIFY BEFORE ANY REPLACEMENT:
//  1. Closed-flag + ctx.Err.
//  2. Acquire the global runtime.lock attributed "self-update" (released on
//     every path).
//  3. writability gate ([resolveSelfUpdateTarget]): if the running
//     executable is not in a user-writable directory, refuse with the
//     manual-install hint, ErrCodeUsageValidation (exit 2), NEVER sudo,
//     BEFORE any download.
//  4. Resolve the latest release and StageCandidate (download + verify the
//     candidate binary fail-closed) into a staging directory that is a SIBLING
//     of the install target, so the staged binary lands on the target's
//     filesystem and the promotion rename is atomic. A verification fault is
//     exit 3; a transport fault is exit 8. Nothing is replaced
//     yet. A caller-pinned TargetVersion not matching the verified candidate
//     is refused here, before any replacement.
//  5. Confirm via the self_update Confirmer kind (current -> new version, the
//     exec path replaced, that wdm.previous is retained). Nil confirmer ->
//     ErrCodeUsageValidation; decline -> ErrCodeUserCanceled; error ->
//     wrapped.
//  6. Replace atomically + retain previous: COPY the current
//     binary to wdm.previous (a sibling of the target) so the live binary is
//     never absent, then atomically rename the staged verified binary over
//     the target. The executable mode is preserved, and the containing
//     directory is fsync'd so the wdm.previous create and the promotion
//     rename are durable (a power loss cannot silently revert a reported-
//     successful update).
//  7. Smoke check: exec the NEW binary with `--version` (argv
//     only, NO shell), require exit 0 AND the reported version equal to the
//     downloaded release version.
//  8. Rollback: on a failed smoke check restore wdm.previous
//     over the target (atomic rename) and report the rollback honestly. A
//     rolled-back self-update is a failure (generic exit 1).
//  9. Populate SelfUpdateResult and emit step_self_update_* progress.

// selfUpdateLockCommand is the runtime.lock attribution for the apply.
const selfUpdateLockCommand = "self-update"

// previousBinarySuffix is appended to the install-target path to form the
// retained rollback binary (wdm -> wdm.previous), a sibling of the target so
// the rollback restore is a same-directory atomic rename (PRD §14, §31,
// the invariant).
const previousBinarySuffix = ".previous"

// defaultBinaryMode is the executable mode applied to the replacement binary
// when the current binary's mode cannot be observed; owner+group+other read
// and execute, owner write — the conventional installed-binary mode.
const defaultBinaryMode os.FileMode = 0o755

// ApplySelfUpdate downloads, verifies, and replaces the wdm binary, keeping a
// rollback binary (PRD §14, §31). See the file-level
// comment for the strict verify-before-replace ordering.
func (e *Engine) ApplySelfUpdate(
	ctx context.Context,
	req types.SelfUpdateRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.SelfUpdateResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ApplySelfUpdate: %w", err)
	}

	// Step 2: hold the global runtime.lock for the whole apply.
	handle, err := e.acquireRuntimeLock(ctx, selfUpdateLockCommand)
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	if onProgress != nil {
		onProgress(types.StepSelfUpdatePlanning, 5, "planning self-update")
	}

	// Step 3: writability gate, BEFORE any download. A non-user-
	// writable install target refuses with the manual-install hint (exit 2)
	// and NEVER escalates privilege (PRD §11, §14).
	target, err := resolveSelfUpdateTarget(e.selfUpdateDeps.executablePath, e.selfUpdateDeps.resolveSymlinks)
	if err != nil {
		return nil, err
	}

	// Step 4: download + verify the candidate fail-closed into a staging dir
	// that is a SIBLING of the install target (so the promotion rename is
	// atomic — same filesystem). The staging dir is removed on every path.
	client, err := e.releaseDeps.newReleaseClient()
	if err != nil {
		return nil, err
	}
	meta, err := client.LatestRelease(ctx)
	if err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(types.StepSelfUpdateDownload, 25, "downloading verified candidate binary")
	}
	stagingDir, err := os.MkdirTemp(target.Dir, ".wdm-selfupdate-stage-*")
	if err != nil {
		return nil, genericError(
			"could not create the self-update staging directory",
			"",
			err,
		)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }() //nolint:errcheck // best-effort removal of the staging dir on every path

	staged, err := e.selfUpdateDeps.stageCandidate(ctx, client, meta, stagingDir)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepSelfUpdateVerify, 50, "candidate binary verified")
	}

	appliedVersion := strings.TrimSpace(staged.Tag)

	// Step 4b: if the caller pinned a target version, the verified candidate
	// must match it (the version surfaced by CheckSelfUpdate). A mismatch is
	// refused BEFORE any replacement, so a TOCTOU between check and apply (the
	// latest release moved) never silently installs a version the user did
	// not authorize.
	if req.TargetVersion != "" && req.TargetVersion != appliedVersion {
		return nil, usageValidationError(
			"the latest verified release does not match the requested target version",
			fmt.Sprintf("the latest verified release is %s; re-run the check and retry", appliedVersion),
			fmt.Errorf("requested target version %q, verified latest %q", req.TargetVersion, appliedVersion),
		)
	}

	if onProgress != nil {
		onProgress(types.StepSelfUpdateStage, 60, "staged verified candidate binary")
	}

	// Step 5: confirm the replace. Nothing has changed on disk
	// at the install target yet.
	if err := confirmSelfUpdate(ctx, confirmer, e.version, appliedVersion, target.Path, onProgress); err != nil {
		return nil, err
	}

	// Steps 6-8: replace + retain previous + smoke + rollback.
	return e.replaceAndSmoke(ctx, target, staged.BinaryPath, e.version, appliedVersion, onProgress)
}

// confirmSelfUpdate gates the replace on the self_update Confirmer kind after
// verification and before any replacement. The consequence
// payload names the version transition, the exec path that will be replaced,
// and that wdm.previous is retained for rollback. A nil confirmer refuses with
// ErrCodeUsageValidation (the install/update posture), a decline maps to
// ErrCodeUserCanceled, and a confirmer error propagates wrapped — none of
// which leaves an on-disk side effect because no replacement has run yet.
func confirmSelfUpdate(
	ctx context.Context,
	confirmer types.Confirmer,
	currentVersion, appliedVersion, execPath string,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required for a self-update",
			"pass a confirmer that can authorize replacing the wdm binary",
		)
	}
	if onProgress != nil {
		onProgress(types.StepSelfUpdateConfirm, 65, "confirming self-update")
	}

	confirmed, err := confirmer.Confirm(ctx, selfUpdateConfirmation(currentVersion, appliedVersion, execPath))
	if err != nil {
		return fmt.Errorf("core.ApplySelfUpdate: confirming self-update: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"self-update canceled before replacing the binary",
			"re-run the self-update and confirm the prompt to replace the verified binary",
		)
	}
	return nil
}

// selfUpdateConfirmation builds the consequence payload surfaced before the
// verified binary is installed (PRD §14; ConfirmationKindSelfUpdate). It names
// the version transition, the exec path being replaced, and the wdm.previous
// retention — never any secret (a binary update carries none).
func selfUpdateConfirmation(currentVersion, appliedVersion, execPath string) types.Confirmation {
	from := currentVersion
	if from == "" {
		from = "(unknown)"
	}
	msg := fmt.Sprintf(
		"Replace the wdm binary %s -> %s?\n"+
			"This installs the verified release binary at %s and keeps the current binary as %s%s for rollback.\n"+
			"A post-replace `wdm --version` smoke check must report %s, or the previous binary is restored automatically.",
		from, appliedVersion, execPath, execPath, previousBinarySuffix, appliedVersion,
	)
	return types.Confirmation{
		Kind:    types.ConfirmationKindSelfUpdate,
		Title:   fmt.Sprintf("update wdm to %s", appliedVersion),
		Message: msg,
	}
}

// replaceAndSmoke performs the brick-resistant replacement, the post-replace
// smoke check, and the rollback on failure. The ordering keeps
// the install target NEVER absent across any single step, and directory
// entries are fsync'd so a reported-successful update survives a power loss
// (see the per-step durability notes):
//  1. Capture the current binary's mode (best-effort; defaults to 0o755).
//  2. COPY the current binary to wdm.previous (a sibling) and fsync the
//     directory so wdm.previous is durable BEFORE the swap. The target stays
//     in place; a crash here leaves the working old binary and a harmless
//     copy. A fsync failure at this PRE-swap step fails closed (nothing is
//     replaced).
//  3. chmod the staged binary to the captured mode, then atomically rename it
//     over the target. The rename is atomic (same filesystem — the staging
//     dir is a subdirectory of the target's directory), so the target is
//     either the old or the new binary, never a partial. The directory is
//     fsync'd after the swap; because this is POST-swap (the new binary is
//     already live), a fsync failure here is WARN-logged and tolerated rather
//     than rolled back.
//  4. Smoke the NEW binary: `wdm --version` must exit 0 and report
//     appliedVersion.
//  5. On smoke failure, atomically rename wdm.previous back over the target
//     (restore), fsync the directory (POST-swap — WARN-and-continue), and
//     report the rollback. The copy-then-atomic-rename ordering means a crash
//     mid-replacement cannot brick the binary.
func (e *Engine) replaceAndSmoke(
	ctx context.Context,
	target selfUpdateTarget,
	stagedBinaryPath, currentVersion, appliedVersion string,
	onProgress types.ProgressFn,
) (*types.SelfUpdateResult, error) {
	previousPath := target.Path + previousBinarySuffix

	mode := currentBinaryMode(target.Path)

	// Step 6a: retain the current binary as wdm.previous via a COPY (not a
	// rename), so the live target file is never momentarily absent.
	if err := copyFilePreservingContents(target.Path, previousPath, mode); err != nil {
		return nil, genericError(
			"could not retain the current binary for rollback",
			"the binary was not replaced; the current binary is unchanged",
			err,
		)
	}

	// Step 6b: install the verified binary atomically (same-filesystem
	// rename). chmod first so the atomic swap installs the correct mode.
	if err := os.Chmod(stagedBinaryPath, mode); err != nil {
		_ = os.Remove(previousPath) //nolint:errcheck // best-effort cleanup of the just-written rollback copy
		return nil, genericError(
			"could not set the executable mode on the verified binary",
			"the binary was not replaced; the current binary is unchanged",
			err,
		)
	}
	if onProgress != nil {
		onProgress(types.StepSelfUpdateReplace, 80, "installing verified binary")
	}
	if err := os.Rename(stagedBinaryPath, target.Path); err != nil {
		_ = os.Remove(previousPath) //nolint:errcheck // best-effort cleanup; the target is unchanged because the rename failed
		return nil, genericError(
			"could not install the verified binary",
			"the binary was not replaced; the current binary is unchanged",
			err,
		)
	}
	// Fsync the directory so the promotion is durable. This is POST-swap: the
	// new binary is already live, so a fsync hiccup must NOT undo a good swap —
	// reverting a verified, installed binary over a directory-flush failure is
	// worse than the small durability window. Log WARN and continue.
	if err := syncDir(target.Dir); err != nil {
		e.logger.WarnContext(ctx,
			"core: self-update install directory was not flushed; the replacement is live but may not be durable until the next sync",
			slog.String("dir", target.Dir),
			slog.String("cause", err.Error()),
		)
	}

	// Step 7: smoke the new binary.
	if onProgress != nil {
		onProgress(types.StepSelfUpdateSmoke, 90, "running `wdm --version` smoke check")
	}
	smokeErr := smokeCheck(ctx, e.selfUpdateDeps.runVersionSmoke, target.Path, appliedVersion)
	if smokeErr == nil {
		return &types.SelfUpdateResult{
			PreviousVersion:    currentVersion,
			AppliedVersion:     appliedVersion,
			Replaced:           true,
			SmokeOK:            true,
			RolledBack:         false,
			PreviousBinaryPath: previousPath,
			Message: fmt.Sprintf(
				"updated wdm %s -> %s; the previous binary is kept at %s",
				displayVersion(currentVersion), appliedVersion, previousPath,
			),
		}, nil
	}

	// Step 8: rollback — restore wdm.previous over the target atomically.
	if onProgress != nil {
		onProgress(types.StepSelfUpdateRollback, 95, "smoke check failed; restoring the previous binary")
	}
	return rollbackSelfUpdate(ctx, e.logger, target, previousPath, currentVersion, appliedVersion, smokeErr)
}

// smokeCheck runs the post-replacement `wdm --version` smoke check and
// verifies BOTH that it succeeded (exit 0, no exec error) AND that the
// reported version equals the downloaded release version. A
// non-zero exit, an exec failure, or a version mismatch all return a non-nil
// error so the caller rolls back. The error message names the mismatch
// without leaking any unexpected output verbatim beyond the trimmed version
// token (the smoke seam already discards stderr).
func smokeCheck(
	ctx context.Context,
	run func(ctx context.Context, binaryPath string) (string, error),
	binaryPath, expectedVersion string,
) error {
	reported, err := run(ctx, binaryPath)
	if err != nil {
		return fmt.Errorf("the new binary failed the version smoke check: %w", err)
	}
	if reported != expectedVersion {
		return fmt.Errorf(
			"the new binary reported version %q but %q was expected",
			reported, expectedVersion,
		)
	}
	return nil
}

// rollbackSelfUpdate restores wdm.previous over the install target (an atomic
// same-directory rename) after a failed smoke check and reports the rollback
// honestly. A successful rollback is still a failure: the
// self-update did not take effect, so it returns ErrCodeGeneric (exit 1)
// naming the rollback, with the original smoke fault reachable via errors.Is.
// A rollback that ITSELF fails leaves the (verified but smoke-failing) new
// binary in place and joins both faults so neither cause is lost.
func rollbackSelfUpdate(
	ctx context.Context,
	logger *slog.Logger,
	target selfUpdateTarget,
	previousPath, currentVersion, appliedVersion string,
	smokeErr error,
) (*types.SelfUpdateResult, error) {
	targetPath := target.Path
	if err := os.Rename(previousPath, targetPath); err != nil {
		// The restore failed: the new binary is still installed (verified but
		// smoke-failing) and wdm.previous is still on disk for manual
		// recovery. Report Replaced=true, RolledBack=false honestly, and join
		// both faults.
		joined := errors.Join(smokeErr, err)
		result := &types.SelfUpdateResult{
			PreviousVersion:    currentVersion,
			AppliedVersion:     appliedVersion,
			Replaced:           true,
			SmokeOK:            false,
			RolledBack:         false,
			PreviousBinaryPath: previousPath,
			Message: fmt.Sprintf(
				"self-update smoke check failed AND the automatic rollback failed; "+
					"the new binary is still installed and the previous binary is kept at %s — restore it manually",
				previousPath,
			),
		}
		return result, genericError(
			"self-update smoke check failed and the previous binary could not be restored",
			fmt.Sprintf("restore the previous binary manually: mv %s %s", previousPath, targetPath),
			joined,
		)
	}

	// The previous binary is restored over the target. Fsync the directory so
	// the restore is durable. This is POST-swap: the old binary is already
	// live again, so a fsync hiccup must NOT undo the good restore — log WARN
	// and continue.
	if err := syncDir(target.Dir); err != nil {
		logger.WarnContext(ctx,
			"core: self-update rollback directory was not flushed; the previous binary is restored but may not be durable until the next sync",
			slog.String("dir", target.Dir),
			slog.String("cause", err.Error()),
		)
	}

	// The new binary is gone (renamed away by the restore), so Replaced is
	// false and RolledBack is true. No retained wdm.previous remains — the
	// restore rename consumed it — so PreviousBinaryPath is empty (omitempty)
	// rather than naming the now-live target, which would break the field's
	// "retained wdm.previous" contract.
	result := &types.SelfUpdateResult{
		PreviousVersion:    currentVersion,
		AppliedVersion:     appliedVersion,
		Replaced:           false,
		SmokeOK:            false,
		RolledBack:         true,
		PreviousBinaryPath: "",
		Message: fmt.Sprintf(
			"self-update to %s failed its smoke check; the previous binary (%s) was restored",
			appliedVersion, displayVersion(currentVersion),
		),
	}
	return result, genericError(
		"self-update failed its smoke check and was rolled back",
		"the previous binary was restored; no action is needed",
		smokeErr,
	)
}

// currentBinaryMode returns the permission bits of the binary at path, or
// [defaultBinaryMode] when they cannot be observed. The mode is preserved
// onto the replacement binary so an operator's chosen permissions survive the
// update; a stat failure is non-fatal because 0o755 is a safe fallback for an
// executable.
func currentBinaryMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return defaultBinaryMode
	}
	return info.Mode().Perm()
}

// copyFilePreservingContents copies src to dst byte-for-byte at mode, used to
// retain the current binary as wdm.previous WITHOUT moving it, so the live
// install target is never momentarily absent during the replace. dst is
// written via a same-directory temp file + atomic rename so a partially-
// written wdm.previous is never observable; the temp is created O_EXCL so a
// stale or planted file is refused rather than reused. The containing
// directory is fsync'd after the rename so the wdm.previous directory entry is
// durable BEFORE the promotion swap — a power loss after a reported-successful
// update must never silently revert to a binary with no rollback sibling on
// disk.
func copyFilePreservingContents(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is the symlink-resolved install target, not user input.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() //nolint:errcheck // read-only handle; close error is not actionable after a successful copy

	tmp := dst + ".tmp"
	// Best-effort remove a stale wdm.previous.tmp left by a prior crash mid-
	// copy: it lives in the persistent install directory, so without this the
	// O_EXCL create below would fail every future self-update until an operator
	// deleted it by hand. Removing it unconditionally is safe — it is this
	// tool's own crash leftover, the held runtime.lock excludes concurrent
	// self-updates, and the gate already proved the operator owns
	// target.Dir. O_EXCL below stays as defense-in-depth.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode) //nolint:gosec // G304: dst is the install-target sibling path, not user input.
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()    //nolint:errcheck // primary error is the failed copy; best-effort cleanup follows
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()    //nolint:errcheck // primary error is the failed fsync; best-effort cleanup follows
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file
		return err
	}
	// Fsync the directory so the wdm.previous rename is durable BEFORE the
	// promotion swap. This is pre-swap, so a failure here fails closed: the
	// caller has not replaced the live binary, so reporting the error leaves
	// the working old binary in place.
	if err := syncDir(filepath.Dir(dst)); err != nil {
		_ = os.Remove(dst) //nolint:errcheck // best-effort removal after a non-durable rename so no half-durable rollback copy is left behind
		return err
	}
	return nil
}

// syncDir fsyncs a directory so a preceding rename into it is durable. It
// mirrors internal/release's syncDir but is kept local so internal/core stays
// free of any sigstore-adjacent import from that package.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the install-target directory, not user input.
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() //nolint:errcheck // primary error is the failed sync; best-effort close follows
		return err
	}
	return d.Close()
}

// displayVersion renders a version for user-facing messages, mapping the
// empty string to a readable placeholder so a message never shows a blank
// "from" version.
func displayVersion(version string) string {
	if version == "" {
		return "(unknown)"
	}
	return version
}
