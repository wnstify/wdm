package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// deletePlan is the outcome of the PRD §19:444-455 destructive-deletion
// planning stage: the managed stack resolved from req.AppID plus the
// consequence data [Engine.executeDelete] needs to confirm the deletion
// (§19:449-451), run `docker compose down` (never -v per §19:453), and
// report what survives (§19:454).
// The plan is assembled read-only — no file write, no.env read, no render
// (delete skips rendering like remove), no Confirmer call (PRD §19:451
// places the typed-name challenge immediately before the deletion), and no
// manifest mutation (the manifest is one of the files the deletion removes,
// so there is no commit point — unlike the safe Remove, which keeps the lock
// as an intent marker per the confirmation rules). It carries the observational fields
// forward so the execution stage consumes them without re-reading the stack,
// the catalog, or Docker — mirroring removePlan.
// deletePaths lists the actual top-level entries of the stack directory
// (§19:449 "clearly list the files and directories that will be deleted"),
// including any foreign files a user dropped in, because os.RemoveAll takes
// the whole directory. backupSnapshotCount records how many.wdm-backups/
// snapshots go with it (destructive delete removes
// .wdm-backups/ along with the stack dir). remainingNamedVolumes describes
// the Docker named volumes that survive the deletion — v1 never deletes them
// (§19:453-455). The planning-time remainingNamedVolumes is
// authoritative: `docker compose down` is free of -v (the wrapper guarantees
// it) and down without -v cannot change a project's named-volume set, so
// [Engine.executeDelete] returns this list directly with no post-down
// re-list — diverging from the safe Remove's post-commit re-list (Remove
// keeps the stack on disk and owes a lasting record; delete removes
// everything and owes no second look). The app's wdm-created networks are NOT
// part of the plan: unlike the safe Remove, destructive delete REMOVES them
// best-effort, discovered at execution time from the still-present rendered
// compose file after `down` and before file deletion.
type deletePlan struct {
	appID                 string
	stackPath             string
	composeProject        string
	deletePaths           []string
	backupSnapshotCount   int
	remainingNamedVolumes []string
}

// DeleteApp permanently deletes a managed stack — the destructive deletion
// flow PRD §19:444-455 mandates be SEPARATE from the safe Remove flow.
// It stops and removes the stack's containers via `docker compose down`
// (NEVER -v per §19:453) and then deletes everything wdm wrote for the app:
// the rendered Compose file, the `.env`, the `.wdm.lock` manifest, the
// `.wdm-backups/` config snapshots, and the stack directory itself. Named
// volumes and on-disk data are NEVER deleted (§19:453-455); the app's
// wdm-created networks ARE removed best-effort after `down` and before file
// deletion (a network that cannot be removed is reported with the manual
// `docker network rm` command and never aborts the deletion; reinstall
// recreates them). The result reports what was removed and what survives.
// Stronger-confirmation gating (§19:451): beyond the
// [types.Confirmer] prompt, the engine re-verifies that req.ConfirmationName
// equals req.AppID and refuses on mismatch BEFORE any lock, Docker call, or
// deletion — a check independent of the UI's own prompt. req.DeleteNamedVolumes
// is reserved and HARD-REFUSED in v1 (§19:453): a true value refuses with a
// usage-validation error rather than silently defaulting to false, so a
// caller that wants volume deletion learns the flow does not exist yet
// Validate-first ordering (the zero-events-on-invalid-request contract):
// isClosed → ctx.Err → empty AppID → DeleteNamedVolumes refusal →
// ConfirmationName mismatch refusal — ALL before the first progress emission
// and before any runtime.lock or Docker contact, proven by zero-events
// assertions.
// Lock posture (PRD §26): DeleteApp is a
// state-changing engine entry, so the global runtime.lock is acquired at
// entry (attributed "delete") and held until return. Planning reads the
// stack manifest through the non-blocking shared-flock path shared with
// Status, Remove, and the update check — a stack mid-operation refuses with
// [types.ErrCodeRuntimeLockHeld] instead of stalling behind the writer —
// while the execution stage takes the exclusive per-stack flock and
// reconfirms managed identity through the held fd before any Docker mutation
// or deletion.
// Managed-only ordering (PRD §9, §10, §19): the stack must
// resolve to a directory whose .wdm.lock parses and names req.AppID before
// any Docker command runs. Unmanaged directories and uninstalled apps refuse
// with [types.ErrCodeUsageValidation]; corrupt manifests surface wrapped
// [types.ErrStaleState]; a manifest missing its compose project refuses with
// [types.ErrCodeUsageValidation]. Resolution is AppID-driven, so a supplied
// req.StackPath is a fail-closed cross-check (it must filepath.Clean-equal
// the resolved managed stack) rather than an alternate resolution path.
// Containment (§19:452 "refuse to delete paths outside the managed stack
// directory"): immediately before the only os.RemoveAll call site in the
// engine, the resolved stack path's symlinks are resolved
// ([filepath.EvalSymlinks]) and the result must sit strictly under the
// engine's stack base via [security.EnsureWithinRoot] — and must NOT be the
// base root itself nor a suspiciously shallow path — so a symlinked or
// escaping stack directory refuses, deleting nothing.
// Teardown order (PRD §19:448-453): Confirmer (immediately before mutation)
// → `docker compose down` (StepDeleteComposeDown; NEVER -v) → best-effort
// removal of the app's wdm-created networks (StepDeleteRemoveNetworks) →
// path-contained file deletion (StepDeleteFiles). A nil confirmer refuses with
// [types.ErrCodeUsageValidation]; a decline maps to
// [types.ErrCodeUserCanceled] with ZERO trace (nothing downed, nothing
// deleted); a confirmer error propagates wrapped. A `down` failure leaves
// the files byte-identical and propagates — nothing was rewritten, so no
// restore is owed. There is no manifest commit point (the manifest is
// deleted) and no post-delete Status verification (DeleteResult carries no
// Status field).
func (e *Engine) DeleteApp(
	ctx context.Context,
	req types.DeleteRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.DeleteResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDeleteRequest(req); err != nil {
		return nil, err
	}
	handle, err := e.acquireRuntimeLock(ctx, "delete")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planDelete(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	return e.executeDelete(ctx, plan, confirmer, onProgress)
}

// validateDeleteRequest enforces the validate-first refusal table that must
// run before any progress emission, runtime.lock acquisition, or Docker
// contact (the install/update/remove zero-events-on-invalid contract). ctx
// is NOT checked here because [Engine.DeleteApp] already rejects a canceled
// context first, so a canceled ctx beats every request-shape refusal
// (matching the remove/restart cross-verb ordering). This keeps the helper a
// pure request-shape validator that runs only on a live context.
//  1. AppID is required (the resolution key).
//  2. DeleteNamedVolumes:true is HARD-REFUSED (§19:453) — v1 has no
//     approved volume-deletion flow, so a true value refuses rather than
//     silently defaulting to false.
//  3. ConfirmationName MUST equal AppID (§19:451) — the
//     engine-side typed-name re-verification, independent of and
//     alongside the Confirmer prompt.
//
// Every rejection is [types.ErrCodeUsageValidation] so cmd/wdm maps it to
// PRD §27 exit code 2.
func validateDeleteRequest(req types.DeleteRequest) error {
	if req.AppID == "" {
		return usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}
	if req.DeleteNamedVolumes {
		return usageValidationError(
			"named-volume deletion is not supported",
			"wdm v1 never deletes named volumes; remove them manually with docker volume rm if you are sure",
			fmt.Errorf("delete_named_volumes is reserved and refused in v1 (§19:453)"),
		)
	}
	if req.ConfirmationName != req.AppID {
		return usageValidationError(
			"confirmation name does not match the app id",
			"type the exact app id to confirm the destructive deletion",
			fmt.Errorf("confirmation_name %q does not equal app id %q", req.ConfirmationName, req.AppID),
		)
	}
	return nil
}

// planDelete runs the non-mutating destructive-deletion planning under the
// held runtime.lock: managed-stack resolution first (PRD §19), the
// fail-closed StackPath cross-check, the corrupt-manifest guard, then the
// §19:449 file/dir list and the opportunistic remaining-volume and
// remaining-network gathering that [types.DeleteResult] surfaces. The emitted
// [types.StepDeletePlanning] event carries the planning outcome; only
// step_delete_* IDs appear on this path (the whole-stream guard).
func (e *Engine) planDelete(
	ctx context.Context,
	req types.DeleteRequest,
	onProgress types.ProgressFn,
) (*deletePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepDeletePlanning, 5, "planning destructive deletion")
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return nil, err
	}

	// Resolution is AppID-driven (mirroring Remove), so a supplied
	// req.StackPath is a fail-closed cross-check, not an alternate resolution
	// path: it must name the stack AppID already resolved to. A mismatch
	// refuses before any Docker call so a stale or wrong --stack-path can
	// never act on a different managed stack.
	if req.StackPath != "" && filepath.Clean(req.StackPath) != stackPath {
		return nil, usageValidationError(
			"stack path does not match the managed stack for this app",
			fmt.Sprintf("the managed stack for %q is at %s", req.AppID, stackPath),
			nil,
		)
	}

	// A managed stack always records its Compose project at install time
	// (PRD §9, §30), so an empty value is a corrupt manifest. Refuse here —
	// before the volume listing and the execution slice's down — so the
	// failure names the corrupt manifest rather than
	// degrading to a generic late refusal from [docker.ComposeDown].
	if lock.ComposeProject == "" {
		return nil, usageValidationError(
			"stack manifest is missing its compose project",
			"the .wdm.lock is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("stack lock for %q records no compose project", req.AppID),
		)
	}

	deletePaths, backupCount := deleteFileList(stackPath)

	volumes, err := e.listDeleteNamedVolumes(ctx, lock.ComposeProject)
	if err != nil {
		return nil, err
	}

	plan := &deletePlan{
		appID:                 req.AppID,
		stackPath:             stackPath,
		composeProject:        lock.ComposeProject,
		deletePaths:           deletePaths,
		backupSnapshotCount:   backupCount,
		remainingNamedVolumes: volumes,
	}
	reportDeletePlan(plan, onProgress)
	return plan, nil
}

// deleteFileList enumerates the stack directory's actual top-level entries
// for the §19:449 "list files and directories that will be deleted"
// requirement. os.RemoveAll takes the whole directory, so the list is honest
// about what goes — including foreign files a user dropped in, because they
// too will be deleted. Directory entries carry a trailing "/" so the user
// can tell files from directories at a glance.
// The .wdm-backups/ snapshot count is surfaced separately: the
// confirmation must name .wdm-backups/ with its snapshot count so the
// user sees the config backups go before confirming. The count comes from
// [state.ListConfigBackups]; the listing is opportunistic — a lister failure
// degrades to a count of 0 rather than failing the plan, since the backups
// are deleted regardless of how many there are.
// The directory read itself is best-effort: an unreadable stack directory
// (already proven managed by resolution) reports the stack path alone rather
// than failing planning — the deletion still proceeds. The stack path is
// always reported as a deleted path so the result is never empty even when
// the directory listing degrades.
func deleteFileList(stackPath string) (paths []string, backupSnapshotCount int) {
	entries, err := os.ReadDir(stackPath)
	if err != nil {
		// Proven managed by resolution; a transient read failure must not
		// block the deletion. Report the stack dir alone.
		return []string{stackPath}, 0
	}

	paths = make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += string(filepath.Separator)
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	// The stack directory itself is the outermost deleted path.
	paths = append(paths, stackPath)

	backupSnapshotCount = countBackupSnapshots(stackPath)
	return paths, backupSnapshotCount
}

// countBackupSnapshots returns how many config-backup snapshots live under
// the stack's .wdm-backups/ directory (the confirmation
// names .wdm-backups/ with its snapshot count). The read is opportunistic:
// any lister failure degrades to a count of 0 rather than failing the plan,
// because the backups are deleted with the stack directory regardless.
func countBackupSnapshots(stackPath string) int {
	snapshots, err := state.ListConfigBackups(stackPath)
	if err != nil {
		return 0
	}
	return len(snapshots)
}

// listDeleteNamedVolumes lists the Compose-project named volumes the
// destructive deletion leaves in place (§19:454-455 — v1 never deletes named
// volumes). It mirrors the safe-remove planning posture
// ([Engine.listRemoveNamedVolumes]): the listing is opportunistic with the
// same hard carve-outs — context cancellation and an unreachable daemon
// ([types.ErrCodeDockerUnavailable]) propagate unchanged so a canceled
// operation and a dead daemon stay typed errors, while any other inspect
// failure WARN-logs and reports an empty list rather than blocking the
// deletion. The client carries the structural redactor only — there are no
// operation secret literals on the delete path (delete reads no.env,
// renders nothing).
func (e *Engine) listDeleteNamedVolumes(ctx context.Context, composeProject string) ([]string, error) {
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	volumes, err := docker.ListProjectNamedVolumes(ctx, client, composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		if types.IsCode(err, types.ErrCodeDockerUnavailable) {
			return nil, err
		}
		e.logger.WarnContext(ctx, "core: skipping named-volume listing during destructive-delete planning",
			slog.String("compose_project", composeProject),
			slog.Any("error", err),
		)
		return nil, nil
	}
	return volumes, nil
}

// reportDeletePlan emits the planning outcome as a single
// [types.StepDeletePlanning] event naming the stack and the counts of files
// to delete and Docker state preserved. The plan carries no secret values
// (paths, Compose project, volume and network names only), so the message is
// sink-safe by construction.
func reportDeletePlan(plan *deletePlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	onProgress(types.StepDeletePlanning, 15, deletePlanSummaryMessage(plan))
}

func deletePlanSummaryMessage(plan *deletePlan) string {
	return fmt.Sprintf(
		"destructive deletion planned for %s: %d path(s) and %d backup snapshot(s) will be deleted; %d named volume(s) will remain and the app's docker networks will be removed",
		plan.appID,
		len(plan.deletePaths),
		plan.backupSnapshotCount,
		len(plan.remainingNamedVolumes),
	)
}

// executeDelete runs the PRD §19:448-453 destructive-deletion execution
// stage under the runtime.lock already held by [Engine.DeleteApp]
// It takes the per-stack exclusive flock,
// reconfirms managed identity through the held fd, asks the Confirmer to
// authorize the permanent deletion (immediately before any mutation, with
// the §19:449 file list, the §19:450 permanence warning, and the §19:454
// remaining-volume notice in the payload), runs `docker compose down`
// (NEVER -v per §19:453), then deletes the stack directory under
// [security.EnsureWithinRoot] containment, and returns the structured
// [types.DeleteResult].
// Ordering is load-bearing: the confirm precedes BOTH `down` and the file
// deletion, so a decline leaves zero trace (no container stopped, no file
// removed). The `down` precedes the file deletion, so a down failure leaves
// the files byte-identical (no restore is owed because delete rewrites no
// config bytes — it removes them). There is no manifest commit point: the
// manifest is one of the files deleted, so unlike the safe Remove there is no
// last_successful_operation marker. The held.wdm.lock flock fd survives the
// unlink under Unix semantics; Release runs on return regardless.
func (e *Engine) executeDelete(
	ctx context.Context,
	plan *deletePlan,
	confirmer types.Confirmer,
	onProgress types.ProgressFn,
) (*types.DeleteResult, error) {
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

	if err := confirmDelete(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}

	// The delete path reads no.env content and renders nothing, so the
	// Docker client carries the structural redactor only — there are no
	// operation secret literals to register (mirrors the remove client).
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	if err := docker.EnsureBindMountCleanupHelperAvailable(ctx, client); err != nil {
		return nil, err
	}

	if err := runDeleteComposeDown(ctx, client, plan, onProgress); err != nil {
		return nil, err
	}

	// Discover and remove the app's wdm-created networks AFTER `down` (all
	// containers are gone, so no endpoint is attached) and BEFORE the stack
	// files are deleted (the rendered compose must still exist for discovery).
	// Best-effort, mirroring self-uninstall: a network already absent counts as
	// removed, a network that genuinely cannot be removed is recorded retained,
	// and the deletion still proceeds — network removal NEVER aborts the delete.
	removedNetworks, retainedNetworks := e.removeDeleteNetworks(ctx, client, plan, onProgress)

	if err := e.deleteStackFiles(ctx, client, plan, onProgress); err != nil {
		return nil, err
	}

	// The planning-time named-volume list is authoritative for delete, so the
	// result echoes plan.remainingNamedVolumes without a post-down re-list.
	// `docker compose down` is structurally free of -v (the wrapper guarantees
	// it), and down without -v cannot change a project's named-volume set — a
	// re-list would be a no-op Docker call. This diverges from the safe
	// Remove, which re-lists volumes after its commit point because Remove
	// keeps the stack on disk and its result is the user's lasting record;
	// delete removes everything, reports what it left in Docker, and owes no
	// second look.
	return &types.DeleteResult{
		AppID:                 plan.appID,
		DeletedPaths:          plan.deletePaths,
		RemainingNamedVolumes: plan.remainingNamedVolumes,
		RemovedNetworks:       removedNetworks,
		RetainedNetworks:      retainedNetworks,
	}, nil
}

// removeDeleteNetworks discovers the app's wdm-created networks from the
// still-present rendered compose file and removes them best-effort, between
// `docker compose down` and the stack-file deletion. The wdm-created networks
// are declared external in the rendered compose (install pre-creates them), so
// `down` never owns or removes them; without this they would linger after a
// delete. Discovery reuses [readExternalNetworkNames] (the same compose
// projection self-uninstall uses) and removal reuses
// [docker.RemoveNetworkIfPresent]: a network already absent counts as removed
// (idempotent), and one that genuinely cannot be removed (for example still
// holding an endpoint from an unrelated stack) is recorded retained with its
// redacted reason. This sub-phase is best-effort: it NEVER returns an error and
// NEVER blocks the file deletion that follows.
func (e *Engine) removeDeleteNetworks(
	ctx context.Context,
	client docker.Client,
	plan *deletePlan,
	onProgress types.ProgressFn,
) (removed []string, retained []types.RetainedNetwork) {
	composePath, err := security.SafeJoin(plan.stackPath, installComposeFilename)
	if err != nil {
		return nil, nil
	}

	names := readExternalNetworkNames(composePath)
	if len(names) == 0 {
		return nil, nil
	}
	if onProgress != nil {
		onProgress(types.StepDeleteRemoveNetworks, 70, fmt.Sprintf(
			"removing %d wdm-created network(s)",
			len(names),
		))
	}

	for _, name := range names {
		if removeErr := docker.RemoveNetworkIfPresent(ctx, client, name); removeErr != nil {
			retained = append(retained, types.RetainedNetwork{
				Name:   name,
				Reason: removeErr.Error(),
			})
			continue
		}
		removed = append(removed, name)
	}
	return removed, retained
}

// confirmDelete asks the Confirmer to authorize the permanent deletion
// immediately before any mutation (PRD §19:448-451). A nil confirmer refuses
// with [types.ErrCodeUsageValidation] per the pkg/engine contract, a decline
// maps to [types.ErrCodeUserCanceled] (zero trace — the confirm precedes both
// the down and the file deletion), and a confirmer error propagates wrapped.
// The engine-side typed-name re-verification already ran in
// [validateDeleteRequest]; this Confirmer is the second,
// independent gate (§19:451).
func confirmDelete(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *deletePlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before destructive deletion",
			"pass a confirmer that can authorize the permanent deletion",
		)
	}
	if onProgress != nil {
		onProgress(types.StepDeleteConfirm, 30, "confirming destructive deletion")
	}

	confirmed, err := confirmer.Confirm(ctx, deleteConfirmation(plan))
	if err != nil {
		return fmt.Errorf("core.delete: confirming deletion: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"deletion canceled before any files were removed",
			"re-run the deletion and confirm the prompt",
		)
	}
	return nil
}

// deleteConfirmation assembles the destructive-deletion consequence payload
// (PRD §19:449-455): the app/stack/project identity, the explicit list of
// files and directories that will be deleted (§19:449) — with the
// .wdm-backups/ snapshot count called out — the permanence
// warning that this is NOT the safe remove and cannot be undone (§19:450),
// the named Docker volumes that survive the deletion (§19:454-455 — v1 never
// deletes them), and a note that the app's wdm-created networks are removed
// (PRD §10, §19; reinstall recreates them). The payload carries no secret
// values — paths, Compose project, and volume/network names only, never.env
// content — so it is sink-safe by construction.
func deleteConfirmation(plan *deletePlan) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		"WARNING: this PERMANENTLY deletes the files below — it is NOT the safe remove and cannot be undone",
		"this stops and removes the stack's containers, then deletes:",
	}
	for _, path := range plan.deletePaths {
		lines = append(lines, "deletes "+path)
	}
	lines = append(lines, fmt.Sprintf(
		"deletes %d backup snapshot(s) under %s/",
		plan.backupSnapshotCount,
		state.BackupDirName,
	))
	lines = append(lines, "named volumes and app data are NOT deleted (v1 never deletes them)")
	for _, volume := range plan.remainingNamedVolumes {
		lines = append(lines, "keeps named volume "+volume)
	}
	lines = append(lines, "the app's wdm-created docker networks are removed (reinstall recreates them)")
	return types.Confirmation{
		Kind:    types.ConfirmationKindDeleteDestructive,
		Title:   "delete " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// runDeleteComposeDown runs `docker compose down` for the managed stack
// before any file is removed (PRD §19:448, §19:453). [docker.ComposeDown] is
// structurally free of -v: it stops and removes
// the stack's containers and the default network Compose created for the
// project, but never touches named volumes. The down step itself does not
// remove the app's external wdm-created networks — a separate
// network-removal step (after this `down`, before file deletion) handles
// those best-effort. Client errors propagate unchanged so
// internal/docker's typed error-code mapping stays authoritative; a down
// failure leaves the files byte-identical because the file deletion is the
// later step.
func runDeleteComposeDown(
	ctx context.Context,
	client docker.Client,
	plan *deletePlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepDeleteComposeDown, 55, "stopping and removing containers")
	}

	project, err := deleteComposeProject(plan)
	if err != nil {
		return err
	}
	return docker.ComposeDown(ctx, client, project)
}

// deleteComposeProject builds the validated [docker.ComposeProject] for
// the down invocation from the managed stack path and the manifest's
// Compose project name. The compose and env file paths are resolved under
// the stack path via [security.SafeJoin] (PRD §12, §13), and the project
// name is the manifest's recorded `wdm-<app>` value (already proven
// non-empty by planDelete's corrupt-manifest guard).
func deleteComposeProject(plan *deletePlan) (docker.ComposeProject, error) {
	composePath, err := security.SafeJoin(plan.stackPath, installComposeFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(plan.stackPath, installEnvFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	return docker.ComposeProject{
		ComposeFile: composePath,
		EnvFile:     envPath,
		ProjectName: plan.composeProject,
	}, nil
}

// deleteStackFiles is the §19:452 containment heart of the destructive
// deletion: it resolves the stack path's symlinks and proves the resolved
// directory sits strictly under the engine's stack base before issuing the
// single os.RemoveAll call site in the engine, then removes the whole stack
// directory — the rendered files, the .env, the .wdm.lock manifest, the
// .wdm-backups/ snapshots, and the directory itself.
// The containment proof (§19:452 "refuse to delete paths outside the managed
// stack directory") is symlink-aware: [filepath.EvalSymlinks] resolves every
// component so a stack path that is itself a symlink to /etc, or that
// traverses one, is rejected on its resolved target. The resolved path must
// (a) sit under the engine's stack base via [security.EnsureWithinRoot],
// (b) NOT be the stack base itself (deleting the base would wipe every
// managed stack), and (c) NOT be a shallow path (a defense-in-depth backstop
// in case the base ever resolves to a near-root location). Only after all
// three pass does os.RemoveAll run — the production-source scan pins this as
// the engine's sole RemoveAll site behind the containment guard.
func (e *Engine) deleteStackFiles(
	ctx context.Context,
	client docker.Client,
	plan *deletePlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepDeleteFiles, 80, "deleting stack files")
	}

	resolved, err := resolveDeleteTarget(e.stackBase, plan.stackPath)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(resolved); err != nil {
		if errors.Is(err, os.ErrPermission) {
			// Containers can leave root-owned bind files under the managed
			// stack. The fallback mounts only the already containment-proven
			// stack directory and never touches Compose named volumes.
			if cleanupErr := docker.RemoveBindMountContents(ctx, client, resolved); cleanupErr != nil {
				return wrapDeleteStackFilesError(plan.stackPath, errors.Join(err, cleanupErr))
			}
			if retryErr := os.RemoveAll(resolved); retryErr != nil {
				return wrapDeleteStackFilesError(plan.stackPath, errors.Join(err, retryErr))
			}
			return nil
		}
		return wrapDeleteStackFilesError(plan.stackPath, err)
	}
	return nil
}

func wrapDeleteStackFilesError(stackPath string, err error) error {
	return types.WrapError(
		types.ErrCodeGeneric,
		"stack files could not be deleted",
		fmt.Sprintf("inspect %s and remove leftover files manually", stackPath),
		err,
	)
}

// resolveDeleteTarget performs the §19:452 containment check and returns the
// symlink-resolved absolute path that may be deleted, or a typed
// usage-validation error refusing the deletion. It NEVER deletes anything —
// it only decides whether deletion is permitted and what the safe target is.
// Callers MUST delete the returned path, not the raw stack path, so
// os.RemoveAll operates on the containment-proven resolution.
// The stack base is resolved through [filepath.EvalSymlinks] too, so the
// containment comparison is symlink-consistent on both sides: comparing a
// symlink-resolved candidate against an unresolved base could spuriously pass
// or fail when either crosses a symlink (e.g. a /var → /private/var
// indirection on the test host).
func resolveDeleteTarget(stackBase, stackPath string) (string, error) {
	resolvedBase, err := filepath.EvalSymlinks(stackBase)
	if err != nil {
		return "", usageValidationError(
			"stack base could not be resolved",
			"check that the configured stack base directory exists",
			err,
		)
	}

	resolved, err := filepath.EvalSymlinks(stackPath)
	if err != nil {
		return "", usageValidationError(
			"stack path could not be resolved",
			"the stack directory may have moved; re-run apps list",
			err,
		)
	}

	cleanedBase := filepath.Clean(resolvedBase)
	cleaned := filepath.Clean(resolved)

	if err := security.EnsureWithinRoot(cleanedBase, cleaned); err != nil {
		return "", usageValidationError(
			"stack path resolves outside the managed stack base",
			"wdm refuses to delete paths outside its stack base (PRD §19)",
			err,
		)
	}
	if cleaned == cleanedBase {
		return "", usageValidationError(
			"stack path resolves to the stack base itself",
			"wdm refuses to delete the entire stack base (PRD §19)",
			fmt.Errorf("resolved stack path %q is the stack base", cleaned),
		)
	}
	if isSuspiciouslyShallowPath(cleaned) {
		return "", usageValidationError(
			"stack path resolves to a suspiciously shallow location",
			"wdm refuses to delete a near-root directory (PRD §19)",
			fmt.Errorf("resolved stack path %q is too shallow to delete", cleaned),
		)
	}
	return cleaned, nil
}

// isSuspiciouslyShallowPath reports whether an absolute, cleaned path sits at
// the filesystem root or a single top-level component such as "/etc" or
// "/home" — and ONLY those. The floor is deliberately shallow: a two-segment
// path such as "/data/<app>" is NOT shallow and so remains deletable,
// because a single-segment custom stack base (e.g. BaseStackPath="/data",
// resolving stacks to "/data/<app>") is legitimate and a stricter
// >=3-segment floor would refuse it, creating a delete-only asymmetry no
// other verb shares. /home-style two-segment bases are likewise allowed —
// the unsafeRoots design keeps /home and /Users off the deny-list
// (internal/security/paths.go:34-36).
// This is therefore a defense-in-depth backstop, NOT the primary guard
// against a near-root misconfiguration. What actually protects the deletion
// is the layered managed-stack resolution that runs first: a parsing
// .wdm.lock recording the exact app id (resolveManagedStack),
// [security.EnsureWithinRoot] containment under the stack base, the separate
// base-itself refusal (cleaned == cleanedBase), the engine-side typed-name
// re-verification, and the §19:449
// file-list confirmation. A one-segment target survives all of those only
// under a pathological base, so this check refuses it as a last line rather
// than the load-bearing one.
func isSuspiciouslyShallowPath(cleaned string) bool {
	trimmed := strings.Trim(cleaned, string(filepath.Separator))
	if trimmed == "" {
		// The filesystem root.
		return true
	}
	// A single top-level component (no separator after trimming) is shallow.
	return !strings.Contains(trimmed, string(filepath.Separator))
}
