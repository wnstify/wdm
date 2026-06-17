package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// runUpdateDeployment runs the post-write, pre-commit span of the apply
// path — PRD §20 step 9 validate, the recreate confirmation,
// catalog network pre-creation, pull, and `up -d --force-recreate` (step
// 10) — returning the first fault. Every step runs AFTER [writeUpdateFiles]
// exposed the new bytes and BEFORE the manifest commit point, so
// [Engine.applyUpdate] routes any error through [Engine.restoreUpdateOnFailure]
// (the step 7 sad path). The span is a single restore-on-failure fan-in
// point rather than five separate restore call sites.
// The network leg is passed nil progress on purpose: ensureInstallNetworks
// emits types.StepInstallNetworkCreate (install.go) and pkg/types defines
// no StepUpdateNetworkCreate, so an install-prefixed step ID on the row-37
// frozen update progress API would mislabel the event — a mismatched frozen
// step ID is worse than silence (mirrors pruneUpdateBackups). A future
// pkg/types slice can add the update constant and thread it here.
func runUpdateDeployment(
	ctx context.Context,
	client docker.Client,
	plan *updateCheckPlan,
	rewrite *installPlan,
	existing *state.StackLock,
	confirmer types.Confirmer,
	backupPath string,
	onProgress types.ProgressFn,
) error {
	if err := validateUpdateComposeConfig(ctx, client, rewrite, onProgress); err != nil {
		return err
	}
	if err := confirmUpdateDeployment(ctx, confirmer, plan, rewrite, existing.LocalPorts, backupPath, onProgress); err != nil {
		return err
	}
	// The update path has no fresh-install Docker rollback (it restores
	// the step-3 snapshot on failure), so it passes no cleanup tracker.
	if err := ensureInstallNetworks(ctx, client, rewrite, nil, nil); err != nil {
		return err
	}
	if err := pullUpdateStack(ctx, client, rewrite, onProgress); err != nil {
		return err
	}
	return deployUpdateStack(ctx, client, rewrite, onProgress)
}

// validateUpdateComposeConfig runs `docker compose config --quiet`
// against a private tempdir copy of the complete rewritten artifact set
// (PRD §20 step 9). Unlike
// install, which validates BEFORE exposing bytes, the update rewrite has
// already replaced the live stack files, so PRD §20 orders validation
// (step 9) after the tag rewrite (step 8) and before pull + recreate
// (step 10). The hermetic workspace keeps the secret-bearing copies from
// outliving the call. Client errors propagate unchanged so
// internal/docker's typed error-code mapping stays authoritative
func validateUpdateComposeConfig(
	ctx context.Context,
	client docker.Client,
	rewrite *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepUpdateComposeValidate, 40, "validating updated compose config")
	}
	return validateRenderedComposeConfig(ctx, client, &rewrite.rendered)
}

// confirmUpdateDeployment gates the recreate on the [types.Confirmer] after
// the tag rewrite and before pull, mirroring install's [confirmInstallDeployment]:
// a nil confirmer refuses with [types.ErrCodeUsageValidation] per the
// pkg/engine contract, a decline maps to [types.ErrCodeUserCanceled], and
// a confirmer error propagates wrapped.
// A decline runs no Docker mutation; [Engine.applyUpdate] routes it
// through [Engine.restoreUpdateOnFailure], which restores the step-3
// snapshot byte-for-byte. The decline's
// ErrCodeUserCanceled code survives the restore wrapper.
func confirmUpdateDeployment(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *updateCheckPlan,
	rewrite *installPlan,
	localPorts []int,
	backupPath string,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before deployment",
			"pass a confirmer that can authorize docker compose recreate",
		)
	}
	if onProgress != nil {
		onProgress(types.StepUpdateConfirm, 45, "confirming update deployment")
	}

	confirmed, err := confirmer.Confirm(ctx, updateConfirmation(plan, rewrite, localPorts, backupPath))
	if err != nil {
		return fmt.Errorf("core.update: confirming deployment: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"update canceled before deployment",
			"re-run the update and confirm the recreate prompt",
		)
	}
	return nil
}

// updateConfirmation assembles the recreate consequence payload: the
// stack identity, the template version
// transition, the per-service old → new image changes, the localhost
// ports that will rebind, the volumes the recreate touches, the catalog
// networks that will be ensured, and the pre-update backup path. The
// payload carries no secret values (catalog image refs, ports, paths, and
// the backup directory name are all non-secret).
// localPorts are the preserved manifest [state.StackLock.LocalPorts] — the
// update reuses the installed ports and never re-plans them — so each entry
// names the localhost port the recreate re-binds, mirroring install's
// per-binding [installConfirmation] line. Every wdm-managed stack binds
// ports on 127.0.0.1, so the line renders against that loopback address.
func updateConfirmation(plan *updateCheckPlan, rewrite *installPlan, localPorts []int, backupPath string) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + rewrite.composeProject,
		fmt.Sprintf("template version: %s -> %s", plan.currentVersion, plan.candidateVersion),
	}
	for _, change := range plan.serviceChanges {
		lines = append(lines, "image change: "+updateServiceChangeMessage(change))
	}
	for _, port := range localPorts {
		lines = append(lines, fmt.Sprintf("rebinds 127.0.0.1:%d", port))
	}
	for _, mount := range rewrite.rendered.VolumeMounts {
		lines = append(lines, "recreates with volume "+mount)
	}
	for _, network := range rewrite.app.Networks {
		line := "ensures docker network " + network.Name
		if network.Internal {
			line += " (internal)"
		}
		lines = append(lines, line)
	}
	if backupPath != "" {
		lines = append(lines, "config backup: "+backupPath)
	}
	return types.Confirmation{
		Kind:    "update_deploy",
		Title:   "recreate " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// pullUpdateStack runs `docker compose pull` for the rewritten stack
// before the recreate (PRD §20 step 10). Unlike install,
// which lets `up -d` pull missing images, the update pulls first so a tag
// bump fetches the new image before the force-recreate swaps containers
// onto it. Client errors propagate unchanged.
func pullUpdateStack(
	ctx context.Context,
	client docker.Client,
	rewrite *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepUpdatePull, 60, "pulling updated images")
	}

	project, err := installComposeProject(rewrite)
	if err != nil {
		return err
	}
	return docker.ComposePull(ctx, client, project)
}

// deployUpdateStack runs `docker compose up -d --force-recreate` so the
// recreated containers pick up the rewritten compose, env, and freshly
// pulled images (PRD §20 step 10). Client errors propagate unchanged.
func deployUpdateStack(
	ctx context.Context,
	client docker.Client,
	rewrite *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepUpdateDeploy, 70, "recreating docker compose stack")
	}

	project, err := installComposeProject(rewrite)
	if err != nil {
		return err
	}
	return docker.ComposeUp(ctx, client, project, docker.ComposeUpOptions{ForceRecreate: true})
}

// updateBackupHistoryEntry is the per-update ledger record appended to
// [state.StackLock.BackupHistory] at the commit point (protocol step 6).
// The field is opaque [json.RawMessage] with no typed entry, so the update
// path defines this minimal record locally and marshals it; existing
// history entries are preserved verbatim. PRD §21 only requires the
// pre-update backup path be recorded, so the record carries the snapshot
// path, the operation kind, and the commit timestamp.
type updateBackupHistoryEntry struct {
	Path      string    `json:"path"`
	Operation string    `json:"operation"`
	At        time.Time `json:"at"`
}

// writeUpdateLockManifest persists the updated.wdm.lock through the held
// per-stack flock fd — the commit point (protocol step 6, PRD §30).
// [state.StackLockHandle.Write] uses the in-place
// truncate/seek/write/fsync pattern; tmp+rename is forbidden for lock
// files because rename would detach the flocked inode. After fsync the
// update is durable and no later failure undoes it.
// The manifest is the existing snapshot with only the update-affected
// fields changed: the template and catalog versions advance to the
// candidate, the image pins carry the new tags with opportunistically
// captured digests, generated-field names follow the
// rewritten secret set, last_successful_operation records the update, and
// the step-3 backup path is appended to backup_history. Every other field
// — selected domain, local ports, recommended resources, Compose project —
// is preserved because the update reuses the installed identity and never
// re-plans ports or resources (protocol step 5 lists the second port-check
// pass for Install only, not Update).
func (e *Engine) writeUpdateLockManifest(
	ctx context.Context,
	plan *updateCheckPlan,
	rewrite *installPlan,
	existing *state.StackLock,
	handle *state.StackLockHandle,
	pins []state.ImagePin,
	backupPath string,
	redactor security.Redactor,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepUpdateLockUpdate, 80, "updating stack lock manifest")
	}

	now := time.Now().UTC()
	lock := *existing
	lock.TemplateName = rewrite.app.TemplateName
	lock.TemplateVersion = plan.candidateVersion
	lock.CatalogChannel = plan.catalogChannel
	lock.CatalogVersion = plan.catalogVersion
	lock.ImagePins = pins
	lock.GeneratedFields = append([]string(nil), rewrite.generatedFields...)
	lock.LastSuccessfulOperation = &types.Operation{
		Kind:       "update",
		At:         now,
		WDMVersion: e.version,
	}

	history, err := appendUpdateBackupHistory(existing.BackupHistory, backupPath, now)
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack lock manifest could not be assembled",
			"check stack directory permissions and retry",
			newRedactedCause(redactor, err),
		)
	}
	lock.BackupHistory = history

	if err := handle.Write(lock); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack lock manifest could not be written",
			"check stack directory permissions and retry",
			newRedactedCause(redactor, err),
		)
	}
	return nil
}

// appendUpdateBackupHistory clones the existing backup_history and appends
// the new snapshot record (protocol step 6). An empty backupPath — which a
// managed stack never produces, since the backup step always snapshots the
// always-present config files — appends nothing, so the ledger only records
// real snapshots.
func appendUpdateBackupHistory(existing []json.RawMessage, backupPath string, at time.Time) ([]json.RawMessage, error) {
	history := make([]json.RawMessage, 0, len(existing)+1)
	for _, entry := range existing {
		history = append(history, append(json.RawMessage(nil), entry...))
	}
	if backupPath == "" {
		return history, nil
	}
	encoded, err := json.Marshal(updateBackupHistoryEntry{
		Path:      backupPath,
		Operation: "update",
		At:        at,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding backup history entry: %w", err)
	}
	return append(history, json.RawMessage(encoded)), nil
}

// verifyUpdateStatus inspects the recreated containers by Compose project
// and wdm labels and fuses the install-time PRD §18 condition subset
// (missing container, unexpected exit, restart loop, unhealthy, port
// mismatch) into a [types.AppStatus] through the shared
// [fuseManagedServiceStatuses] / [finalizeStatus] helpers (PRD §20 step
// 11, §18). The pass runs AFTER the protocol step 6 commit point, so a
// failed inspection never fails the durable update: it marks the result
// needs-attention with the status_check_failed reason instead. Context
// cancellation still propagates; the durable manifest stays either way.
// Expected services come from the rewritten Compose service labels; the
// port-mismatch check uses the preserved manifest local_ports (the update
// reuses the installed ports), mirroring the standalone Status path's
// lock-based port check rather than install's binding-based one.
func verifyUpdateStatus(
	ctx context.Context,
	client docker.Client,
	plan *updateCheckPlan,
	rewrite *installPlan,
	existing *state.StackLock,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepUpdateStatus, 90, "verifying updated stack status")
	}

	now := time.Now().UTC()
	status := &types.AppStatus{
		AppID:          plan.appID,
		ComposeProject: rewrite.composeProject,
		StackPath:      plan.stackPath,
		UpdatedAt:      &now,
	}

	containers, err := docker.InspectProjectContainers(ctx, client, rewrite.composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status.State = statusStateNeedsAttention
		status.NeedsAttention = true
		status.AttentionReasons = []string{statusReasonStatusCheckFailed}
		status.Message = "post-update status verification failed; run apps status for details"
		return status, nil
	}

	services := make([]string, 0, len(rewrite.rendered.ServiceLabels))
	for service := range rewrite.rendered.ServiceLabels {
		services = append(services, service)
	}
	sort.Strings(services)

	managed, reasons := fuseManagedServiceStatuses(plan.appID, services, nil, containers, status)
	status.LocalPorts = observedLocalPorts(managed)
	if lockPortsMismatch(existing.LocalPorts, managed) {
		reasons[statusReasonPortMismatch] = struct{}{}
	}

	finalizeStatus(
		status,
		reasons,
		"all managed services are running",
		"post-update verification found issues; run apps status for details",
	)
	return status, nil
}

// pruneUpdateBackups enforces the per-stack retention cap AFTER the commit
// point is durable, pinning this run's
// snapshot — now the most-recent-successful pre-update backup — so it is
// never evicted. A prune failure is logged and swallowed: it can only leave
// MORE backups than the cap, which is safe, and the update has already
// committed, so nothing past the commit point fails the operation (PRD §30
// durability).
// No progress event is emitted: pkg/types defines no StepUpdatePrune
// constant, and reusing an unrelated step ID would mislabel the retention
// cleanup. The prune is best-effort post-commit cleanup, not a user-facing
// milestone — mirroring the install path, whose retention pass has no step
// of its own.
func pruneUpdateBackups(
	ctx context.Context,
	logger *slog.Logger,
	stackPath string,
	pinnedSnapshot string,
) {
	if _, err := state.PruneConfigBackups(stackPath, pinnedSnapshot); err != nil && logger != nil {
		// The engine logger is redaction-wrapped, so attaching the error is
		// safe and losing the diagnosis would be worse.
		logger.WarnContext(ctx, "core: config backup retention prune failed after update",
			slog.String("stack_path", stackPath),
			slog.Any("error", err),
		)
	}
}

// restoreUpdateOnFailure is the protocol step 7 sad path for a failed
// update. It runs
// when any step 4-6 fault occurs AFTER [Engine.rewriteUpdateStack] has
// exposed the new compose /.env / additional_files on disk and BEFORE the
// manifest commit point — a validate, recreate-confirm, network, pull, or
// recreate fault. It restores the step-3 snapshot byte-for-byte via
// [state.RestoreConfigBackup] (config files only — never Docker volumes,
// containers, or app data), then surfaces a typed error.
// Bounded-context discipline: the restore MUST
// complete even when the operation ctx is already canceled, because a
// mid-deploy cancellation is itself one of the faults that triggers this
// path. [state.RestoreConfigBackup] takes no context, so it is immune to
// parent-ctx cancellation. The update sad path does no Docker-side
// cleanup, so — unlike install's [failFreshInstall], which bounds its
// Docker rollback with a cancellation-detached context — no
// context.WithTimeout is needed here.
// Error matrix:
//   - Restore SUCCEEDS, fault is a canceled parent ctx: the bare context
//     error propagates unchanged so upstream ctx.Err discipline still
//     observes context.Canceled. The previous config is back; the manifest
//     is never committed on a canceled apply.
//   - Restore SUCCEEDS, fault is a clean refusal — a confirmer decline
//     ([types.ErrCodeUserCanceled]) or a nil-confirmer / usage
//     refusal ([types.ErrCodeUsageValidation]): the error KEEPS its refusal
//     code and gains a hint naming the restored backup and the row-40
//     config-restore boundary. The original message rides in the cause.
//   - Restore SUCCEEDS, any other induced fault (validate / network / pull
//     / recreate / manifest write): surfaces [types.ErrCodeGeneric]
//     wrapping the original (already redacted) fault, with a hint naming
//     the restored backup path and the config-restore boundary (:357
//     mandates this code+hint for the induced-Compose-failure case). The
//     original fault stays reachable via errors.Is for docker-layer code
//     mapping.
//   - Restore FAILS: fail closed (:201). The returned [types.ErrCodeGeneric]
//     joins the original fault with the restore failure so BOTH causes are
//     reachable via errors.Is; the message conveys the uncertain state and
//     the hint names where the snapshot lives for manual recovery. The
//     original cause is never lost — this overrides even a clean-refusal
//     code, because once the config cannot be guaranteed restored the
//     operation is no longer a clean refusal. has two
//     conjuncts here — "return a typed error AND mark the app as needing
//     attention" — so this branch also persists a needs-attention marker
//     (see [Engine.markStackNeedsAttentionAfterFailedRestore]) so a later
//     [Engine.Status] surfaces the inconsistency through §18 condition 7
//     instead of reporting "running" over a half-restored stack
//     A
//     marker-write failure is logged WARN and joined into the cause chain
//     but NEVER masks the primary ErrCodeGeneric, changes the code, or
//     alters the message or hint.
//
// The word "rollback" never appears in any user-facing string on this path
// restore is described only as a config
// restore.
func (e *Engine) restoreUpdateOnFailure(
	fault error,
	plan *updateCheckPlan,
	existing *state.StackLock,
	backupPath string,
	redactor security.Redactor,
	onProgress types.ProgressFn,
) error {
	if onProgress != nil {
		onProgress(types.StepUpdateConfigRestore, 75, "restoring previous config after update failure")
	}

	if restoreErr := restoreConfigSnapshot(plan.stackPath, backupPath); restoreErr != nil {
		// Fail closed on uncertain state: the original fault
		// and the restore failure both ride in the join so neither cause is
		// lost (the restore error is filesystem-only — snapshot paths, no
		// secrets — and the original fault was already redacted by the docker
		// client). The displayed message is redacted defensively, but the
		// join is the unwrap target so errors.Is still reaches BOTH causes
		// (a plain newRedactedCause would sever the chain, since its Unwrap
		// only exposes render sentinels).
		// attention") and:381's persistent needs-attention state require
		// more than a typed error: a config that cannot be guaranteed
		// restored must show up on a LATER Status, not "running". So persist
		// a marker that nulls last_successful_operation, firing §18 condition
		// 7 (last_operation_failed) on every subsequent Status. The marker
		// targets the LIVE.wdm.lock through a FRESH file descriptor, never
		// the in-scope held handle: a partially successful restore may have
		// already renamed.wdm.lock to a new inode (state.RestoreConfigBackup
		// writes through WriteFileAtomic's tmp+rename), detaching the held fd
		// from the live path (reviewer finding 6). No per-stack flock is
		// taken — the still-held global runtime.lock is the cross-process
		// guard, and a fresh flock would deadlock against the in-scope handle
		// on this same goroutine (see
		// markStackNeedsAttentionAfterFailedRestore). A marker-write failure
		// is best-effort — logged WARN and joined into the cause chain — but
		// never masks the primary ErrCodeGeneric join, changes the code, or
		// alters the message or hint.
		markerErr := e.markStackNeedsAttentionAfterFailedRestore(plan.stackPath, existing)
		if markerErr != nil && e.logger != nil {
			// The engine logger is redaction-wrapped, so attaching the marker
			// error is safe; the marker path touches only.wdm.lock paths and
			// manifest metadata, never secret values.
			e.logger.Warn("core.update: could not mark stack needs-attention after failed restore",
				slog.String("stack_path", plan.stackPath),
				slog.Any("error", markerErr),
			)
		}
		joined := errors.Join(fault, restoreErr, markerErr)
		return types.WrapError(
			types.ErrCodeGeneric,
			"update failed and the previous config could not be fully restored",
			fmt.Sprintf("inspect the stack and restore config manually from %s", backupPath),
			redactedCause{message: redactJoinMessage(redactor, joined), unwrap: joined},
		)
	}

	// A canceled parent ctx that triggered this path propagates unchanged so
	// upstream ctx.Err discipline still observes context.Canceled (the
	// manifest is never committed either way); the restore above ran on the
	// contextless primitive, so the previous config is back.
	if errors.Is(fault, context.Canceled) || errors.Is(fault, context.DeadlineExceeded) {
		return fault
	}

	hint := fmt.Sprintf("%s (config backup: %s)", state.ConfigRestoreBoundaryNotice, backupPath)
	// A clean refusal keeps its code: a decline stays ErrCodeUserCanceled
	// and a nil-confirmer / usage refusal stays
	// ErrCodeUsageValidation. The hint reports the restore and the row-40
	// config-restore boundary; the original message survives in the cause.
	if code, ok := cleanRefusalCode(fault); ok {
		return types.WrapError(code, "update aborted; previous config restored", hint, fault)
	}

	// Induced failure with a clean restore::357 mandates ErrCodeGeneric
	// plus a hint naming the restored backup path. The original (already
	// redacted) fault is the cause so errors.Is keeps the docker-layer code
	// reachable.
	return types.WrapError(types.ErrCodeGeneric, "update failed; previous config restored", hint, fault)
}

// restoreConfigSnapshot is the single config-restore code path shared by
// the two entry points that restore a stack from a backup snapshot: the
// failed-update automatic restore
// ([Engine.restoreUpdateOnFailure]) and the user-initiated
// [Engine.RestoreBackup]. Both MUST funnel through here so neither drifts
// from the config-only guarantee or skips the boundary the restore enforces
// — a divergent second restore path is the §21/§20 risk 's risk
// table tracks.
// It is a deliberately minimal wrapper around [state.RestoreConfigBackup]:
// the state layer owns every restore invariant (config-files-only
// collection, traversal/symlink rejection, the managed-config allowlist,
// atomic write-through with preserved permission bits). This wrapper adds NO
// concern of its own — the update side keeps its marker ladder, error
// re-coding, and boundary-notice hint OUTSIDE this function, and the restore
// side builds its own result/next-action around it — so the shared core
// stays exactly the byte-for-byte config restore both callers need.
// Like [state.RestoreConfigBackup] it takes no [context.Context]: a config
// restore is fail-closed cleanup that must complete even when the operation
// context that triggered it is already canceled. The contextless primitive is
// immune to a canceled parent.
func restoreConfigSnapshot(stackPath string, snapshotPath string) error {
	return state.RestoreConfigBackup(stackPath, snapshotPath)
}

// markStackNeedsAttentionAfterFailedRestore persists the
// second-conjunct needs-attention marker for the fail-closed restore
// branch: it nulls last_successful_operation in the stack's .wdm.lock so
// §18 condition 7 (last_operation_failed) fires on every subsequent
// [Engine.Status]. It is best-effort and never panics; the caller logs and joins
// any error without masking the primary fault.
// The marker targets the LIVE.wdm.lock through a FRESH file descriptor,
// not the in-scope held handle. A partially successful restore may have
// already renamed.wdm.lock to a new inode ([state.RestoreConfigBackup]
// writes through [state.WriteFileAtomic]'s tmp+rename), detaching the
// held fd from the live path; writing through it would update an orphaned
// inode that Status never reads (reviewer finding 6). Re-opening the live
// path by name solves that.
// No per-stack flock is taken here. The still-held global runtime.lock
// ([Engine.Update] defers its release) is the authoritative cross-process
// state-mutation guard (PRD §26): every wdm write path — install, update,
// remove — acquires it first, so no other wdm process writes this.wdm.lock
// concurrently. A per-stack flock would only guard a SAME-process racer,
// but the only same-process holder is the in-scope handle on this
// goroutine. Worse, a fresh flock would deadlock or mis-report: flock locks
// belong to the open file description on every platform wdm runs on, so a
// second LOCK_EX through an independently opened fd self-conflicts with the
// lock this goroutine already holds (verified on darwin; flock(2) on Linux
// documents the same per-description semantics), while a non-blocking
// attempt self-reports busy. internal/state's lock readers
// ([state.AcquireStackLock], [state.ReadStackLock], [state.TryReadStackLock])
// all take a per-stack flock and so cannot be used while the in-scope
// exclusive flock is held; the marker therefore does its own raw,
// flock-free in-place write. It is contextless like the restore primitive:
// fail-closed cleanup that must complete even when the operation ctx that
// triggered the sad path is already canceled.
// Fallback ladder for the manifest the marker is based on:
//   - Live.wdm.lock parses as a schema-1 manifest: base the marker on the
//     freshly-read on-disk manifest so whatever a partial restore left
//     committed is preserved, with only last_successful_operation nulled.
//   - Live.wdm.lock is missing, empty, or torn/unparsable: fall back to
//     the in-scope pre-update snapshot (nulled).
//
// Both arms write through the same row-27 in-place primitive
// ([writeNeedsAttentionMarker]).
func (e *Engine) markStackNeedsAttentionAfterFailedRestore(stackPath string, existing *state.StackLock) error {
	lockPath := filepath.Join(stackPath, installLockFilename)

	base := readLiveStackLockOrFallback(lockPath, existing)
	if base == nil {
		return fmt.Errorf("core.update: no manifest available to mark stack needs-attention at %q", lockPath)
	}
	return writeNeedsAttentionMarker(lockPath, base)
}

// readLiveStackLockOrFallback reads the live on-disk.wdm.lock raw (no
// flock — see [Engine.markStackNeedsAttentionAfterFailedRestore]) and
// returns it when it parses as a schema-1 manifest, so the marker preserves
// whatever a partial restore committed and only nulls the operation flag. A
// missing, empty, or torn/unparsable file falls back to the in-scope
// pre-update snapshot (which may be nil only in a degenerate path the caller
// guards). The decode mirrors internal/state's schema-1 contract without
// importing its unexported decoder.
func readLiveStackLockOrFallback(lockPath string, existing *state.StackLock) *state.StackLock {
	// G304 is suppressed: lockPath is composed from the engine-controlled
	// stack base, mirroring internal/state's own.wdm.lock open sites.
	raw, err := os.ReadFile(lockPath) //nolint:gosec // G304: engine-controlled stack path
	if err != nil {
		return existing
	}
	var live state.StackLock
	if err := json.Unmarshal(raw, &live); err != nil || live.SchemaVersion != stackLockSchemaVersion {
		// Torn, empty, or unsupported-schema lock: fall back to the
		// pre-update snapshot so the marker can still be written.
		return existing
	}
	return &live
}

// stackLockSchemaVersion mirrors internal/state's per-stack lock schema
// version (locked to 1). The marker decoder checks it directly because
// internal/state's decoder is unexported; a schema mismatch routes to the
// pre-update-snapshot fallback.
const stackLockSchemaVersion = 1

// writeNeedsAttentionMarker writes lock to lockPath with
// last_successful_operation nulled, the durable half of the failed-restore
// needs-attention marker. It mirrors [state.StackLockHandle.Write]'s row-27
// in-place truncate/seek/write/fsync pattern: tmp+rename is forbidden for
// .wdm.lock because rename would detach the path from the inode the in-scope
// handle still references. No flock is taken —
// cross-process exclusion comes from the still-held global runtime.lock, and
// a per-stack flock would deadlock or self-report against the in-scope
// handle on this goroutine (see
// [Engine.markStackNeedsAttentionAfterFailedRestore]). The lock argument is
// copied before nulling so the caller's snapshot is never mutated.
func writeNeedsAttentionMarker(lockPath string, lock *state.StackLock) error {
	if lock == nil {
		return fmt.Errorf("core.update: no manifest available to mark stack needs-attention at %q", lockPath)
	}

	marker := *lock
	marker.LastSuccessfulOperation = nil
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("core.update: marshaling needs-attention marker: %w", err)
	}

	// G304 is suppressed: lockPath is composed from the engine-controlled
	// stack base, mirroring internal/state's own.wdm.lock open sites.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: engine-controlled stack path
	if err != nil {
		return fmt.Errorf("core.update: opening %q for needs-attention marker: %w", lockPath, err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup; the fsync below is the durability point

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("core.update: truncating %q for needs-attention marker: %w", lockPath, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("core.update: seeking %q for needs-attention marker: %w", lockPath, err)
	}
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("core.update: writing needs-attention marker to %q: %w", lockPath, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("core.update: fsyncing needs-attention marker to %q: %w", lockPath, err)
	}
	return nil
}

// redactJoinMessage renders an errors.Join cause to a single string and
// scrubs it through the active redactor for safe display. It serves the
// fail-closed restore branch, where the cause is kept structurally (so
// errors.Is reaches both joined causes) while its surfaced text is redacted
// defensively.
func redactJoinMessage(redactor security.Redactor, cause error) string {
	message := fmt.Sprint(cause)
	if redactor != nil {
		message = redactor.Redact(message)
	}
	return message
}

// cleanRefusalCode reports whether fault carries a clean typed code that
// must survive the sad-path restore wrapper. The rule is by code, not by
// origin: a clean refusal code — [types.ErrCodeUserCanceled] (a confirmer
// decline) or [types.ErrCodeUsageValidation] — keeps its
// code WHEREVER on the deploy span it arises. UsageValidation is not
// confined to the nil-confirmer refusal: the network internal-flag-drift
// mismatch maps to it, as does
// [installComposeProject]'s [security.SafeJoin] refusal reached from pull /
// deploy. Only infrastructure faults WITHOUT a clean code (a docker-layer
// validate / network / pull / recreate error or a manifest write fault)
// return false, so [Engine.restoreUpdateOnFailure] re-codes them to
// [types.ErrCodeGeneric].
func cleanRefusalCode(fault error) (types.ErrorCode, bool) {
	switch {
	case types.IsCode(fault, types.ErrCodeUserCanceled):
		return types.ErrCodeUserCanceled, true
	case types.IsCode(fault, types.ErrCodeUsageValidation):
		return types.ErrCodeUsageValidation, true
	default:
		return types.ErrCodeUnknown, false
	}
}

// buildUpdateResult assembles the structured update result (PRD §20 step
// 11, §32): the version transition, the sorted changed services, the
// catalog risk grouping, the pre-update backup path, and the fused
// post-update status snapshot. UpdatedServices and RiskClassifications are
// empty for an up-to-date no-op apply (no candidate change), but the backup
// path and status are always populated.
func buildUpdateResult(plan *updateCheckPlan, status *types.AppStatus, backupPath string) *types.UpdateResult {
	var services []string
	for _, change := range plan.serviceChanges {
		services = append(services, change.service)
	}
	return &types.UpdateResult{
		AppID:                   plan.appID,
		PreviousTemplateVersion: plan.currentVersion,
		NewTemplateVersion:      plan.candidateVersion,
		UpdatedServices:         services,
		RiskClassifications:     append([]string(nil), plan.riskClassifications...),
		BackupPath:              backupPath,
		Status:                  status,
	}
}
