package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// restartPlan is the outcome of the Restart planning stage (PRD §18:416
// "Restart app", the invariant): the managed stack resolved from
// req.AppID plus the consequence data [Engine.executeRestart] needs to
// confirm the restart, run `docker compose restart`, and report which
// services were restarted.
// The plan is assembled read-only — no file write, no.env read, no
// render, no Confirmer call (it lands immediately before the restart),
// no manifest mutation. The central the invariant invariant is that
// Restart changes nothing on disk: `docker compose restart` stops and
// starts the same containers without re-reading the Compose file, so
// there is no backup (the invariant / the frozen step set has no
// StepRestartBackup), no rewrite, and no.wdm.lock commit point. The
// restart is whole-stack only (RestartRequest has no Services field), so
// services carries every managed service the restart touches. localPorts
// carries the manifest's recorded local ports forward for the
// post-restart port-mismatch check — restart reuses the installed ports
// and rewrites nothing, so the lock value is authoritative.
type restartPlan struct {
	appID          string
	stackPath      string
	composeProject string
	services       []string
	localPorts     []int
}

// Restart restarts a managed stack's containers in place (PRD §18:416,
// the invariant). Plain `docker compose restart` stops and starts the
// SAME containers without re-reading the Compose file, so the restart
// never re-renders templates, writes config files, takes a backup, or
// updates the `.wdm.lock` manifest — there is no commit point because a
// restart changes nothing on disk. The whole stack restarts together
// (RestartRequest carries no Services field).
// Lock posture (PRD §26): Restart is a state-changing engine entry, so
// the global runtime.lock is acquired at entry — attributed "restart" —
// and held until return. Planning reads the stack manifest through the
// non-blocking shared-flock path shared with Status and the update check,
// so a stack mid-operation refuses with [types.ErrCodeRuntimeLockHeld]
// instead of stalling behind the writer, while the execution stage takes
// the exclusive per-stack flock and reconfirms managed identity through
// the held fd before any Docker mutation.
// Managed-only ordering (PRD §9, §10): the stack must resolve to a
// directory whose .wdm.lock parses and names req.AppID before any Docker
// command runs. Unmanaged directories and uninstalled apps refuse with
// [types.ErrCodeUsageValidation]; corrupt manifests surface wrapped
// [types.ErrStaleState]; a manifest missing its Compose project refuses
// with [types.ErrCodeUsageValidation] naming the corrupt lock. Resolution
// is AppID-driven, so a supplied req.StackPath is a fail-closed
// cross-check against the resolved managed stack, not an alternate
// resolution path: a mismatch (after filepath.Clean) refuses before any
// Docker call, while a matching path proceeds.
// Execution order: exclusive flock → reconfirm managed identity →
// [types.Confirmer] (immediately before the restart, with a SAFE
// consequence payload naming the app, stack path, Compose project, and
// the services that will be restarted) → `docker compose restart` →
// post-restart status verification. A nil confirmer refuses with
// [types.ErrCodeUsageValidation], a decline maps to
// [types.ErrCodeUserCanceled] with zero side effects (no Docker call),
// and a confirmer error propagates wrapped. The restart confirmation is
// SAFE (a restart preserves all data) so --yes auto-accepts it per the
// frozen step_restart_* stream.
// A `docker compose restart` failure surfaces the docker-layer typed
// error unchanged. The post-restart status verification mirrors the
// install/update verify posture: a failed inspection marks the result
// needs-attention with the status_check_failed reason rather than failing
// the restart (the restart already ran), carving out context cancellation
// only.
func (e *Engine) Restart(
	ctx context.Context,
	req types.RestartRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestartResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "restart")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planRestart(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	return e.executeRestart(ctx, plan, confirmer, onProgress)
}

// planRestart runs the non-mutating restart planning under the held
// runtime.lock: managed-stack resolution first (PRD §9, §10), the
// fail-closed StackPath cross-check, the corrupt-manifest Compose-project
// guard, and the whole-stack service set derived from the manifest's
// image pins. The emitted [types.StepRestartPlanning] events carry the
// planning outcome, so callers never parse prose for step identity; only
// step_restart_* IDs appear on this path. Planning makes no Docker call —
// it needs no volume or network listing because restart neither removes
// nor recreates anything.
func (e *Engine) planRestart(
	ctx context.Context,
	req types.RestartRequest,
	onProgress types.ProgressFn,
) (*restartPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireAppID(req.AppID); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRestartPlanning, 5, "planning restart")
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return nil, err
	}

	if err := stackPathCrossCheck(req.StackPath, req.AppID, stackPath); err != nil {
		return nil, err
	}
	if err := requireComposeProject(lock.ComposeProject, req.AppID); err != nil {
		return nil, err
	}

	plan := &restartPlan{
		appID:          req.AppID,
		stackPath:      stackPath,
		composeProject: lock.ComposeProject,
		services:       expectedStatusServices(lock),
		localPorts:     lock.LocalPorts,
	}
	reportRestartPlan(plan, onProgress)
	return plan, nil
}

// reportRestartPlan emits the planning outcome on the progress stream as
// a single [types.StepRestartPlanning] event naming the stack and the
// count of services that will be restarted. The plan carries no secret
// values (Compose project, stack path, and service names only), so the
// message is sink-safe.
func reportRestartPlan(plan *restartPlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	onProgress(types.StepRestartPlanning, 15, fmt.Sprintf(
		"restart planned for %s: %d service(s) will be restarted",
		plan.appID,
		len(plan.services),
	))
}

// executeRestart runs the Restart execution stage under the runtime.lock
// already held by [Engine.Restart]. It takes the per-stack exclusive
// flock, reconfirms managed identity through the held fd, asks the
// Confirmer to authorize the restart (immediately before the restart),
// runs `docker compose restart`, verifies the post-restart status, and
// returns the structured [types.RestartResult].
// the invariant (the no-write contract): the exclusive flock serializes
// against concurrent state-changing operations on the same stack, NOT
// because restart mutates the manifest — it does not. No backup is taken,
// no config byte is rewritten, and the `.wdm.lock` manifest is never
// written: a restart changes nothing on disk, so there is no commit
// point. A decline therefore leaves the stack exactly as it was, and a
// restart failure likewise owes no restore (nothing was rewritten).
func (e *Engine) executeRestart(
	ctx context.Context,
	plan *restartPlan,
	confirmer types.Confirmer,
	onProgress types.ProgressFn,
) (*types.RestartResult, error) {
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

	if err := confirmRestart(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}

	// The restart path generates no secrets and reads no.env content, so
	// the Docker client carries the structural redactor only — no operation
	// secret literals to register (mirrors the remove path's client).
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	if err := runRestartComposeRestart(ctx, client, plan, onProgress); err != nil {
		return nil, err
	}

	status, err := e.verifyRestartStatus(ctx, client, plan, onProgress)
	if err != nil {
		return nil, err
	}

	return &types.RestartResult{
		AppID:             plan.appID,
		ComposeProject:    plan.composeProject,
		RestartedServices: plan.services,
		Status:            status,
	}, nil
}

// confirmRestart asks the Confirmer to authorize the restart immediately
// before `docker compose restart`, mirroring remove's [confirmRemove]
// posture: a nil confirmer refuses with [types.ErrCodeUsageValidation]
// per the pkg/engine contract, a decline maps to
// [types.ErrCodeUserCanceled], and a confirmer error propagates wrapped.
// The confirm runs before any Docker mutation, so a decline leaves the
// stack exactly as it was.
func confirmRestart(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *restartPlan,
	onProgress types.ProgressFn,
) error {
	return confirmLifecycleOp(ctx, confirmer, restartConfirmation(plan), confirmStrings{
		stepID:         types.StepRestartConfirm,
		stepPct:        30,
		stepMessage:    "confirming restart",
		nilMessage:     "confirmer is required before restart",
		nilHint:        "pass a confirmer that can authorize docker compose restart",
		confirmErrWrap: "core.restart: confirming restart",
		declineMessage: "restart canceled before docker compose restart",
		declineHint:    "re-run the restart and confirm the prompt",
	}, onProgress)
}

// restartConfirmation assembles the restart consequence payload: the app
// name, stack path, and Compose project, an explicit statement that the
// containers will be stopped and started (brief downtime, no data loss),
// and the services the restart touches. The Kind is the SAFE
// "restart_safe" literal (mirroring remove's "remove_safe"): a restart
// preserves all files, volumes, and data, so --yes auto-accepts it per
// the the confirmation rulesgating matrix the CLI implements. The payload carries
// no secret values — stack path, Compose project, and service names only
// — so it is sink-safe.
func restartConfirmation(plan *restartPlan) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		"this stops and starts the stack's containers (brief downtime, no data loss)",
	}
	for _, service := range plan.services {
		lines = append(lines, "restarts service "+service)
	}
	return types.Confirmation{
		Kind:    "restart_safe",
		Title:   "restart " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// runRestartComposeRestart runs `docker compose restart` for the managed
// stack. [docker.ComposeRestart] stops and starts the same
// containers without re-reading the Compose file, so it never recreates a
// container or re-renders a template — the whole stack restarts together
// (no per-service argument is ever passed). Client errors propagate
// unchanged so internal/docker's typed error-code mapping stays
// authoritative.
func runRestartComposeRestart(
	ctx context.Context,
	client docker.Client,
	plan *restartPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepRestartExecute, 60, "restarting containers")
	}

	project, err := logsComposeProject(plan.stackPath, plan.composeProject)
	if err != nil {
		return err
	}
	return docker.ComposeRestart(ctx, client, project)
}

// verifyRestartStatus inspects the restarted containers by Compose
// project and wdm labels and fuses the PRD §18 condition subset (missing
// container, unexpected exit, restart loop, unhealthy, port mismatch)
// into a [types.AppStatus] through the shared [fuseManagedServiceStatuses]
// / [finalizeStatus] helpers (PRD §18). The pass runs AFTER the restart,
// so a failed inspection never fails the operation: it marks the result
// needs-attention with the status_check_failed reason instead (mirroring
// [verifyUpdateStatus] / [verifyInstallStatus], which carve out ctx.Err
// ONLY). A daemon-down inspect failure likewise fuses as needs-attention
// rather than propagating — the restart already completed.
// Expected services and the port-mismatch check both come from the
// manifest read during planning: restart reuses the installed ports and
// rewrites nothing, so the lock's image-pin services and local_ports are
// authoritative — mirroring the standalone Status path's lock-based view.
func (e *Engine) verifyRestartStatus(
	ctx context.Context,
	client docker.Client,
	plan *restartPlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRestartStatus, 90, "verifying restarted stack status")
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
		status.Message = "post-restart status verification failed; run apps status for details"
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
		"all managed services restarted and are running",
		"post-restart verification found issues; run apps status for details",
	)
	return status, nil
}
