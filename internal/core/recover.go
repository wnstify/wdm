package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// recoverOrphanedStack opt-in recovers the stack directory left behind by a
// hard-killed (SIGKILL) install so a fresh install can proceed. A clean
// install failure self-cleans through failFreshInstall + the deferred lock
// release; only an uncatchable kill leaves the directory and its empty or
// half-written .wdm.lock behind. Recovery is fail-closed and NEVER deletes
// named volumes — it removes only the on-disk orphan directory and then
// best-effort sweeps the networks the rendered compose declared.
//
// The whole flow runs under the engine's global runtime lock (acquired in
// [Engine.Install] before this call), so no concurrent wdm operation can
// race the probe-then-remove window. Order:
//   - Nothing on disk → no-op (force is harmless when there is nothing to
//     recover).
//   - Any container of the project is Running or Restarting → REFUSE: this
//     is a live stack, not an orphan; the user wants uninstall, not --force.
//   - Classify the .wdm.lock via [state.ClearStaleStackLock]:
//   - Cleared → the directory is a wdm-owned interrupted-install orphan;
//     remove the whole directory under the uninstall path guards.
//   - Absent → no lock means the directory is NOT proven wdm-managed, so
//     only an EMPTY directory may be removed; a non-empty foreign
//     directory is refused and left untouched.
//   - Held / managed → propagate the typed refusal; remove nothing.
//   - Best-effort sweep the captured networks; faults are logged, not fatal.
func (e *Engine) recoverOrphanedStack(
	ctx context.Context,
	client docker.Client,
	stackPath string,
	composeProject string,
) error {
	lg := e.newOpLogger(e.installLogger(nil), "install")

	exists, err := installStackPathExists(stackPath)
	if err != nil {
		return err
	}
	if !exists {
		lg.step(ctx, "recover: no orphan stack directory present")
		return nil
	}

	containers, err := docker.InspectProjectContainers(ctx, client, composeProject)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.State.Running || c.State.Restarting {
			return types.NewError(
				types.ErrCodeUsageValidation,
				"stack appears to be running",
				"run `wdm apps uninstall` to remove a running stack instead of forcing a recovery",
			)
		}
	}
	lg.step(ctx, "recover: no running containers for project")

	// Capture the wdm-created networks from the rendered compose BEFORE
	// removing the directory; a missing/unparseable file yields no names.
	composePath := filepath.Join(stackPath, installComposeFilename)
	networks := readExternalNetworkNames(composePath)

	outcome, err := state.ClearStaleStackLock(ctx, filepath.Join(stackPath, ".wdm.lock"))
	if err != nil {
		return err
	}

	switch outcome {
	case state.StackLockClearCleared:
		if err := removeOrphanStackDir(stackPath); err != nil {
			return err
		}
		lg.step(ctx, "recover: removed wdm-owned orphan stack directory")
	case state.StackLockClearAbsent:
		// No .wdm.lock: NOT proven wdm-managed. Only an empty directory may
		// be removed; os.Remove refuses a non-empty one.
		if err := os.Remove(stackPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return types.NewError(
				types.ErrCodeUsageValidation,
				"stack path exists and is not a recoverable wdm stack",
				"this directory has no wdm lock and is not empty; remove it manually if it is safe to delete",
			)
		}
		lg.step(ctx, "recover: removed empty non-wdm stack directory")
	default:
		// StackLockClearUnknown without an error is unreachable, but fail
		// closed rather than fall through to a network sweep on bad state.
		return types.NewError(
			types.ErrCodeGeneric,
			"stack recovery reached an unexpected state",
			"report this; it is a wdm internal invariant",
		)
	}

	e.sweepRecoveredNetworks(ctx, client, networks, lg)
	return nil
}

// removeOrphanStackDir removes a proven wdm-owned orphan stack directory
// under the SAME containment guards uninstall uses for footprint removal:
// reject an unsafe root, reject symlinked ancestors, reject an out-of-home
// or suspiciously shallow resolved path, then RemoveAll. Named volumes are
// Docker objects and are never touched here.
func removeOrphanStackDir(stackPath string) error {
	if err := security.RejectUnsafeRoot(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"could not resolve the home directory for orphan recovery",
			"",
			err,
		)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"the home directory could not be resolved",
			"",
			err,
		)
	}

	cleaned := filepath.Clean(stackPath)
	if err := security.EnsureWithinRoot(filepath.Clean(resolvedHome), cleaned); err != nil {
		return usageValidationError(
			"the orphan stack path resolves outside the home directory",
			"wdm refuses to remove paths outside the home directory (PRD §39)",
			err,
		)
	}
	if isSuspiciouslyShallowPath(cleaned) {
		return usageValidationError(
			"the orphan stack path resolves to a suspiciously shallow location",
			"wdm refuses to remove a near-root directory (PRD §39)",
			fmt.Errorf("resolved orphan stack path %q is too shallow to remove", cleaned),
		)
	}

	if err := os.RemoveAll(cleaned); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"the orphan stack directory could not be removed",
			"inspect the stack directory and remove it manually",
			err,
		)
	}
	return nil
}

// sweepRecoveredNetworks best-effort removes the wdm-created networks the
// orphaned stack declared. It runs AFTER the directory is gone (reinstall is
// already unblocked), so every fault is logged and swallowed: EnsureNetwork
// reuses these networks idempotently on the next install regardless.
func (e *Engine) sweepRecoveredNetworks(
	ctx context.Context,
	client docker.Client,
	names []string,
	lg opLogger,
) {
	for _, name := range names {
		ok, skipped, err := docker.RemoveNetworkIfManaged(ctx, client, name)
		switch {
		case err != nil:
			lg.step(ctx, "recover: network sweep skipped on error",
				slog.String("network", name),
				slog.String("error", err.Error()),
			)
		case skipped:
			lg.step(ctx, "recover: network left in place (not wdm-managed)",
				slog.String("network", name),
			)
		case ok:
			lg.step(ctx, "recover: removed wdm-created network",
				slog.String("network", name),
			)
		}
	}
}
