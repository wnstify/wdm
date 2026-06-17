package core

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the ListBackups and RestoreBackup engine methods
// ListBackups is the read-only
// backup-listing path; RestoreBackup is the state-changing config
// restore.
// ListBackups is read-only (no runtime.lock). It mirrors Engine.Status
// and Engine.ValidateConfig: a managed-only refusal before any
// filesystem walk of backups, a non-blocking shared flock so a busy
// stack refuses fast instead of stalling behind the writer, and zero
// writes — it never acquires, creates, or deletes the runtime.lock
// (PRD §26 read-only clause). It issues no progress events (the frozen
// step set has no step_backup_* constant — see pkg/types/progress.go),
// never reads.env, and constructs no Docker client.
// RestoreBackup is state-changing ("restore" per PRD §26:686): it holds
// the global runtime.lock and the per-stack exclusive flock and restores
// config files ONLY through [restoreConfigSnapshot] — the SAME shared
// internal/core + internal/state path the failed-update auto-restore uses
// — then surfaces the post-restore Status and the recreate
// next-action. It is a config restore, never a rollback:
// every user-facing string says "config restore" and states what is (config
// files) and is not (app data, databases, volumes) restored.
// See restart.go for the callback-type and blank-identifier rationale, and
// the write-path protocol RestoreBackup mirrors (runtime.lock → managed-only
// resolution → exclusive flock → reconfirm → Confirmer before the file
// rewrite → execute → post-restore status fusion).

// ListBackups returns the config-backup snapshots for a managed stack,
// newest first (PRD §7, §21). It resolves the managed stack the same way
// [Engine.Status] does — a directory whose .wdm.lock manifest parses and
// names appID — then projects the [state.ListConfigBackups] snapshots
// onto [types.BackupInfo].
// Managed-only ordering (PRD §10): the stack must resolve before any
// backup directory is walked. An uninstalled app and an unmanaged
// directory both surface [types.ErrCodeUsageValidation] refusals; a
// stack whose flock is held by an in-flight operation refuses fast with
// [types.ErrCodeRuntimeLockHeld] (the non-blocking shared-lock read,
// never stalling behind the writer); a corrupt manifest surfaces a
// wrapped [types.ErrStaleState]. A [state.ListConfigBackups] hard error
// (a symlinked or non-directory backup root, or a stat/read failure) is
// an operational fault and surfaces as [types.ErrCodeGeneric] with the
// cause reachable via [errors.Is]. Context cancellation always
// propagates as an error.
// Ordering lives in [state.ListConfigBackups], which sorts newest-first
// by the tamper-stable unix-nanos creation prefix; this method preserves
// that order and never re-sorts. A managed stack that never backed up
// returns a non-nil empty slice and a nil error so the CLI/JSON layer
// renders [] rather than null.
// Read-only discipline (PRD §26): the manifest is read through
// [state.TryReadStackLock] (non-blocking shared lock), no runtime.lock is
// touched, and the path writes nothing. The returned slice — and the
// Files slice of every element — is freshly allocated, so callers may
// retain or mutate the result without affecting engine state or
// subsequent calls (defensive-copy semantics per golang-safety, matching
// List).
func (e *Engine) ListBackups(ctx context.Context, appID string) ([]types.BackupInfo, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ListBackups: %w", err)
	}
	if appID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}

	stackPath, _, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return nil, err
	}

	snapshots, err := state.ListConfigBackups(stackPath)
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"config backups could not be listed",
			"check stack directory permissions and retry",
			err,
		)
	}

	backups := make([]types.BackupInfo, len(snapshots))
	for i, snapshot := range snapshots {
		backups[i] = types.BackupInfo{
			SnapshotID: snapshot.SnapshotID,
			Operation:  snapshot.Operation,
			CreatedAt:  snapshot.CreatedAt,
			Path:       snapshot.Path,
			// Clone the per-snapshot Files slice so mutating a returned
			// BackupInfo can never reach into the lister's storage. The
			// lister already returns fresh slices per call, so engine state
			// cannot be corrupted regardless; the explicit clone keeps the
			// defensive-copy contract local.
			Files: slices.Clone(snapshot.Files),
		}
	}
	return backups, nil
}

// restoreBackupNextAction is the recreate next-action surfaced by
// [Engine.RestoreBackup]. A config restore rewrites the stack's
// config files but does NOT touch the running containers, and `docker
// compose restart` would NOT help because it does not re-read the Compose
// file. So the next action MUST be the recreate path (the apply pipeline
// behind `wdm apps update`, which runs `docker compose up -d
// --force-recreate`), and the copy states plainly that the running
// containers keep the OLD config until that recreate applies the restored
// files. The word "rollback" never appears.
const restoreBackupNextAction = "config restored to disk; the running containers still use the old config — run `wdm apps update` to recreate them and apply the restored config"

// restorePlan is the outcome of the RestoreBackup planning stage (PRD
// §20:495, §21:539, the invariant): the managed stack resolved from
// req.AppID, the validated snapshot the execution stage restores, and the
// data [Engine.executeRestoreBackup] needs to confirm the restore, rewrite
// the config files through the shared restore path, and report the
// post-restore Status.
// The plan is assembled read-only — no file write, no.env read, no render,
// no Confirmer call (it lands immediately before the file rewrite), no
// runtime mutation. The snapshot is validated against the on-disk backup
// set during planning so an unknown or traversal-shaped SnapshotID refuses
// BEFORE the user is prompted to confirm a restore that could not run.
type restorePlan struct {
	appID          string
	stackPath      string
	composeProject string
	snapshot       types.BackupInfo
	services       []string
	localPorts     []int
}

// RestoreBackup restores a managed stack's config files from a backup
// snapshot (PRD §20:495, §21:539). It
// is a CONFIG restore, never a rollback: it rewrites docker-compose.yml,
// .env, and the stack's managed config files from the snapshot, and does
// NOT touch app data, databases, uploaded files, or Docker volumes. The
// running containers keep the OLD config until the recreate next-action runs
// RestoreBackup never invokes Docker to recreate them.
// The restore goes through [restoreConfigSnapshot], the SAME shared
// internal/core + internal/state code path the failed-update automatic
// restore uses (the invariant, the "ONE restore path, two entry points"
// constraint). The TUI and CLI never grow their own restore logic; they call
// this method through pkg/engine.
// Lock posture (PRD §26:686): RestoreBackup is state-changing, so the global
// runtime.lock is acquired at entry — attributed "restore" — and held until
// return. Planning reads the stack manifest through the non-blocking
// shared-flock path shared with Status, so a stack mid-operation refuses with
// [types.ErrCodeRuntimeLockHeld] instead of stalling behind the writer, while
// the execution stage takes the exclusive per-stack flock and reconfirms
// managed identity through the held fd before the file rewrite.
// Managed-only ordering (PRD §9, §10): the stack must resolve to a directory
// whose .wdm.lock parses and names req.AppID before any restore. Unmanaged
// directories and uninstalled apps refuse with
// [types.ErrCodeUsageValidation]; corrupt manifests surface wrapped
// [types.ErrStaleState]; a manifest missing its Compose project refuses with
// [types.ErrCodeUsageValidation] naming the corrupt lock. Resolution is
// AppID-driven, so a supplied req.StackPath is a fail-closed cross-check
// verified (after filepath.Clean) against the resolved managed stack, not an
// alternate resolution path, mirroring Remove and Restart. The req.SnapshotID
// must name an existing snapshot of THIS stack — an empty, unknown, or
// traversal-shaped id refuses with [types.ErrCodeUsageValidation] during
// planning, before any prompt or write.
// Execution order: exclusive flock → reconfirm managed identity → confirm
// the restore via [types.Confirmer] immediately before the file rewrite
// with a payload naming the app, stack path, snapshot, the
// files that will be rewritten, the config-restore boundary (config files
// only; app data and databases unchanged), and the runtime-keeps-old-config
// consequence → [restoreConfigSnapshot] (the shared config-only restore) →
// post-restore status fusion. A nil confirmer refuses with
// [types.ErrCodeUsageValidation], a decline maps to
// [types.ErrCodeUserCanceled] with ZERO on-disk change (no file rewritten),
// and a confirmer error propagates wrapped. The restore confirmation is SAFE
// — it never destroys data, only rewrites wdm-managed config files — so the
// CLI's --yes auto-accepts it per the confirmation rules gating matrix.
// Progress rides the frozen step_restore_* stream.
// The post-restore status fusion mirrors the install/update/restart verify
// posture: a failed inspection marks the result needs-attention with the
// status_check_failed reason rather than failing the restore (the files are
// already restored), carving out context cancellation only.
func (e *Engine) RestoreBackup(
	ctx context.Context,
	req types.RestoreBackupRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestoreBackupResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "restore")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planRestoreBackup(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	return e.executeRestoreBackup(ctx, plan, confirmer, onProgress)
}

// planRestoreBackup runs the non-mutating RestoreBackup planning under the
// held runtime.lock: the validate-first contract (ctx.Err → empty-AppID →
// empty-SnapshotID, all before the first progress event), managed-stack
// resolution (PRD §9, §10), the fail-closed StackPath cross-check, the
// corrupt-manifest Compose-project guard, and snapshot validation against the
// on-disk backup set. The emitted [types.StepRestorePlanning] events carry
// the planning outcome so callers never parse prose for step identity; only
// step_restore_* IDs ever appear on this path. Planning makes no Docker call
// and writes nothing.
// Snapshot validation uses [state.ListConfigBackups] — the same read-only
// lister ListBackups projects — so the SnapshotID must match a snapshot the
// listing surfaces for this stack. This refuses an unknown or traversal-shaped
// id (the lister never surfaces a traversal-shaped basename, and a
// non-matching id finds no entry) with [types.ErrCodeUsageValidation] BEFORE
// the user is prompted, and yields the snapshot's metadata (creation time,
// operation, captured files) for the confirmation payload and the result's
// RestoredFiles. It is defense-in-depth, not the only gate: the shared
// [restoreConfigSnapshot] / [state.RestoreConfigBackup] path re-validates the
// snapshot id (traversal, symlink, managed-config allowlist) authoritatively
// at write time.
func (e *Engine) planRestoreBackup(
	ctx context.Context,
	req types.RestoreBackupRequest,
	onProgress types.ProgressFn,
) (*restorePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.AppID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}
	if req.SnapshotID == "" {
		return nil, usageValidationError(
			"snapshot id is required",
			"pass the snapshot id to restore (see wdm apps backups list)",
			nil,
		)
	}
	if onProgress != nil {
		onProgress(types.StepRestorePlanning, 5, "planning config restore")
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return nil, err
	}

	// Resolution is AppID-driven (mirroring Remove and Restart), so a
	// supplied req.StackPath is a fail-closed cross-check, not an alternate
	// resolution path: it must name the stack AppID already resolved to. A
	// mismatch refuses before any prompt or write, so a stale or wrong
	// --stack-path can never restore into a different managed stack.
	if req.StackPath != "" && filepath.Clean(req.StackPath) != stackPath {
		return nil, usageValidationError(
			"stack path does not match the managed stack for this app",
			fmt.Sprintf("the managed stack for %q is at %s", req.AppID, stackPath),
			nil,
		)
	}

	// A managed stack always records its Compose project at install time
	// (PRD §9, §30), so an empty value is a corrupt manifest. Refuse here so
	// the post-restore status fusion has a project to inspect.
	if lock.ComposeProject == "" {
		return nil, usageValidationError(
			"stack manifest is missing its compose project",
			"the .wdm.lock is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("stack lock for %q records no compose project", req.AppID),
		)
	}

	snapshot, err := resolveRestoreSnapshot(stackPath, req.AppID, req.SnapshotID)
	if err != nil {
		return nil, err
	}

	plan := &restorePlan{
		appID:          req.AppID,
		stackPath:      stackPath,
		composeProject: lock.ComposeProject,
		snapshot:       snapshot,
		services:       expectedStatusServices(lock),
		localPorts:     lock.LocalPorts,
	}
	reportRestorePlan(plan, onProgress)
	return plan, nil
}

// resolveRestoreSnapshot finds the requested snapshot among the stack's
// on-disk backups and projects it onto [types.BackupInfo]. It uses the same
// read-only [state.ListConfigBackups] lister as ListBackups so the snapshot
// surface stays single-sourced: a SnapshotID the lister does not surface —
// unknown, mistyped, or traversal-shaped (the lister never returns a
// traversal-shaped basename) — refuses with [types.ErrCodeUsageValidation],
// naming the app, before the restore prompts or writes. A
// [state.ListConfigBackups] hard error (a symlinked or non-directory backup
// root, or a stat/read failure) is an operational fault surfaced as
// [types.ErrCodeGeneric] with the cause reachable via errors.Is.
func resolveRestoreSnapshot(stackPath, appID, snapshotID string) (types.BackupInfo, error) {
	snapshots, err := state.ListConfigBackups(stackPath)
	if err != nil {
		return types.BackupInfo{}, types.WrapError(
			types.ErrCodeGeneric,
			"config backups could not be listed",
			"check stack directory permissions and retry",
			err,
		)
	}

	for _, snapshot := range snapshots {
		if snapshot.SnapshotID != snapshotID {
			continue
		}
		return types.BackupInfo{
			SnapshotID: snapshot.SnapshotID,
			Operation:  snapshot.Operation,
			CreatedAt:  snapshot.CreatedAt,
			Path:       snapshot.Path,
			Files:      slices.Clone(snapshot.Files),
		}, nil
	}

	return types.BackupInfo{}, usageValidationError(
		"backup snapshot was not found for this app",
		fmt.Sprintf("run wdm apps backups list %s to see available snapshots", appID),
		fmt.Errorf("no backup snapshot %q under %q", snapshotID, stackPath),
	)
}

// reportRestorePlan emits the planning outcome as a single
// [types.StepRestorePlanning] event naming the stack, the snapshot, and the
// count of config files the restore will rewrite. The plan carries no secret
// values (stack path, Compose project, snapshot id, and config filenames
// only), so the message is sink-safe by construction.
func reportRestorePlan(plan *restorePlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	onProgress(types.StepRestorePlanning, 15, fmt.Sprintf(
		"config restore planned for %s from snapshot %s: %d config file(s) will be rewritten",
		plan.appID,
		plan.snapshot.SnapshotID,
		len(plan.snapshot.Files),
	))
}

// executeRestoreBackup runs the RestoreBackup execution stage under the
// runtime.lock already held by [Engine.RestoreBackup]. It takes the per-stack
// exclusive flock, reconfirms managed identity through the held fd, asks the
// Confirmer to authorize the restore (immediately before the file rewrite,
// [restoreConfigSnapshot] path, verifies the post-restore status, and returns
// the [types.RestoreBackupResult] with the config-restore boundary notice and
// the recreate next-action.
// A nil confirmer refuses with [types.ErrCodeUsageValidation], a decline maps
// to [types.ErrCodeUserCanceled] with zero on-disk change (the confirm runs
// before the rewrite), and a confirmer error propagates wrapped. The restore
// is config-files-only; it never invokes Docker to recreate containers —
// that is the user's next action.
func (e *Engine) executeRestoreBackup(
	ctx context.Context,
	plan *restorePlan,
	confirmer types.Confirmer,
	onProgress types.ProgressFn,
) (*types.RestoreBackupResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	handle, err := acquireInstallStackLock(ctx, plan.stackPath)
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	if _, err := reconfirmManagedStack(handle, plan.appID); err != nil {
		return nil, err
	}

	if err := confirmRestoreBackup(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(types.StepRestoreExecute, 60, "restoring config files from snapshot")
	}
	// The shared config-restore path. It is contextless by
	// design — a config restore is fail-closed cleanup that must complete
	// even under a canceled operation context — and rewrites managed config
	// files only, never app data or Docker-side state.
	if err := restoreConfigSnapshot(plan.stackPath, plan.snapshot.SnapshotID); err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"config restore failed",
			fmt.Sprintf("inspect the snapshot at %s and retry", plan.snapshot.Path),
			err,
		)
	}

	status, err := e.verifyRestoreStatus(ctx, plan, onProgress)
	if err != nil {
		return nil, err
	}

	return &types.RestoreBackupResult{
		AppID:      plan.appID,
		SnapshotID: plan.snapshot.SnapshotID,
		// RestoredFiles mirrors the snapshot's top-level file listing, the
		// set the restore writes back for every curated layout. The restore
		// walks the snapshot recursively, so a nested additional file would
		// be written back without appearing here (see the
		// types.RestoreBackupResult godoc).
		RestoredFiles:  slices.Clone(plan.snapshot.Files),
		BoundaryNotice: state.ConfigRestoreBoundaryNotice,
		NextAction:     restoreBackupNextAction,
		Status:         status,
	}, nil
}

// confirmRestoreBackup asks the Confirmer to authorize the config restore
// immediately before the file rewrite, mirroring restart's [confirmRestart]
// posture: a nil confirmer refuses with [types.ErrCodeUsageValidation] per
// the pkg/engine contract, a decline maps to [types.ErrCodeUserCanceled], and
// a confirmer error propagates wrapped. The confirm runs before any byte is
// rewritten, so a decline leaves the stack's config unchanged.
func confirmRestoreBackup(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *restorePlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before config restore",
			"pass a confirmer that can authorize the config restore",
		)
	}
	if onProgress != nil {
		onProgress(types.StepRestoreConfirm, 30, "confirming config restore")
	}

	confirmed, err := confirmer.Confirm(ctx, restoreConfirmation(plan))
	if err != nil {
		return fmt.Errorf("core.restore: confirming config restore: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"config restore canceled before any file was rewritten",
			"re-run the config restore and confirm the prompt",
		)
	}
	return nil
}

// restoreConfirmation assembles the config-restore consequence payload: the
// app name, stack path, Compose project, the snapshot identity (id, creation
// time, originating operation), the config files that will be rewritten, the
// config-restore boundary (what IS restored — config files — and what is NOT
// — app data, databases, uploaded files, and Docker volumes), and the
// runtime-keeps-old-config consequence. The Kind is the SAFE
// "restore_config" literal (mirroring remove's "remove_safe" / restart's
// "restart_safe" inline-literal precedent): a config restore destroys no data,
// so --yes auto-accepts it per the confirmation rules gating matrix the CLI
// implements. The payload carries no secret values
// — stack path, Compose project, snapshot id, and config filenames only — so
// it is sink-safe by construction. The word "rollback" never appears
// #42).
func restoreConfirmation(plan *restorePlan) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		fmt.Sprintf(
			"snapshot: %s (%s, taken %s)",
			plan.snapshot.SnapshotID,
			plan.snapshot.Operation,
			plan.snapshot.CreatedAt.UTC().Format(time.RFC3339),
		),
		"this is a config restore: it rewrites wdm config files only",
		"it does NOT restore app data, databases, uploaded files, or Docker volumes",
		"the running containers keep the old config until you recreate them (wdm apps update)",
	}
	for _, file := range plan.snapshot.Files {
		lines = append(lines, "rewrites config file "+file)
	}
	return types.Confirmation{
		Kind:    "restore_config",
		Title:   "config restore " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// verifyRestoreStatus inspects the stack's containers by Compose project and
// wdm labels and fuses the PRD §18 condition subset (missing container,
// unexpected exit, restart loop, unhealthy, port mismatch) into a
// [types.AppStatus] through the shared [fuseManagedServiceStatuses] /
// [finalizeStatus] helpers (PRD §18). The pass runs AFTER the config files are
// restored, so a failed inspection never fails the operation: it marks the
// result needs-attention with the status_check_failed reason instead
// (mirroring [verifyRestartStatus] / [verifyUpdateStatus] / [verifyInstallStatus],
// which carve out ctx.Err ONLY). A daemon-down inspect failure likewise fuses
// as needs-attention rather than propagating.
// The status reflects the still-running containers, which keep the OLD config
// until the recreate next-action runs: the restore rewrote the
// config files on disk but recreated nothing. Expected services and the
// port-mismatch check come from the manifest read during planning, since a
// restore reuses the installed identity. The read-only Docker client is built
// over the structural redactor only — the restore path generates no secrets
// and reads no.env content (mirroring restart's client).
func (e *Engine) verifyRestoreStatus(
	ctx context.Context,
	plan *restorePlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRestoreStatus, 90, "verifying stack status after config restore")
	}

	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	status := &types.AppStatus{
		AppID:          plan.appID,
		ComposeProject: plan.composeProject,
		StackPath:      plan.stackPath,
		UpdatedAt:      &now,
	}

	containers, err := docker.InspectProjectContainers(ctx, client, plan.composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status.State = statusStateNeedsAttention
		status.NeedsAttention = true
		status.AttentionReasons = []string{statusReasonStatusCheckFailed}
		status.Message = "post-restore status verification failed; run apps status for details"
		return status, nil
	}

	managed, reasons := fuseManagedServiceStatuses(plan.appID, plan.services, nil, containers, status)
	status.LocalPorts = observedLocalPorts(managed)
	if lockPortsMismatch(plan.localPorts, managed) {
		reasons[statusReasonPortMismatch] = struct{}{}
	}

	finalizeStatus(
		status,
		reasons,
		"config restored; managed services are running (recreate to apply the restored config)",
		"config restored; status verification found issues — run apps status for details",
	)
	return status, nil
}
