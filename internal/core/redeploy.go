package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// redeployPlan is the outcome of the RedeployStack planning stage: the
// managed stack resolved from req.AppID plus the data [Engine.executeRedeploy]
// needs to confirm the redeploy, run `docker compose up -d`, and report which
// services were recreated. It mirrors [restartPlan]; the difference between
// redeploy and restart is the Docker verb, not the plan.
//
// Unlike Restart, RedeployStack APPLIES on-disk overlay edits: `docker compose
// up -d` re-reads the Compose file and the content-gated
// docker-compose.override.yml, and each service's env_file: [.env.user] is
// re-evaluated, so an edited .env.user / override takes effect. RedeployStack
// still rewrites NOTHING on disk: it re-renders no template, generates no
// secret, takes no backup, and never updates .wdm.lock. It deploys the EXISTING
// files as they already are. localPorts carries the manifest's recorded ports
// forward for the post-redeploy port-mismatch check.
type redeployPlan struct {
	appID          string
	stackPath      string
	composeProject string
	services       []string
	localPorts     []int
}

// RedeployStack recreates a managed stack from its EXISTING on-disk files so
// edits to the user overlay (.env.user, docker-compose.override.yml) take
// effect — the "apply overlay changes" action. It runs `docker compose up -d`,
// which re-reads the Compose file plus the content-gated override and
// re-evaluates each service's env_file, recreating only the containers whose
// effective config changed. It re-renders NO template, generates NO secret,
// changes NO image or version, takes NO backup, and never writes .wdm.lock — it
// deploys the files exactly as they are on disk. The whole stack is deployed
// together (RestartRequest carries no Services field).
//
// It is the apply-edits counterpart to Restart: plain `docker compose restart`
// reuses the running containers without re-reading config, so it does NOT pick
// up overlay edits; RedeployStack does. Both share the same managed-only
// resolution, lock posture, confirmer gate, result type, and post-op status
// verification.
//
// Fail-closed: an invalid override or env file surfaces from the
// `docker compose up` layer as a typed error and propagates unchanged (the
// redactor scrubs any interpolated secret); RedeployStack does not swallow it.
//
// Lock posture (PRD §26): a state-changing engine entry, so the global
// runtime.lock is acquired at entry — attributed "redeploy" — and held until
// return; the per-stack flock is taken in the execution stage. Managed-only
// ordering (PRD §9, §10) and the fail-closed StackPath cross-check match
// Restart exactly.
func (e *Engine) RedeployStack(
	ctx context.Context,
	req types.RestartRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestartResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "redeploy")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planRedeploy(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	return e.executeRedeploy(ctx, plan, confirmer, onProgress)
}

// planRedeploy runs the non-mutating redeploy planning under the held
// runtime.lock: managed-stack resolution (PRD §9, §10), the fail-closed
// StackPath cross-check, the corrupt-manifest Compose-project guard, and the
// whole-stack service set derived from the manifest's image pins. Only
// step_redeploy_* IDs appear on this path. Planning makes no Docker call.
func (e *Engine) planRedeploy(
	ctx context.Context,
	req types.RestartRequest,
	onProgress types.ProgressFn,
) (*redeployPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireAppID(req.AppID); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRedeployPlanning, 5, "planning redeploy")
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

	plan := &redeployPlan{
		appID:          req.AppID,
		stackPath:      stackPath,
		composeProject: lock.ComposeProject,
		services:       expectedStatusServices(lock),
		localPorts:     lock.LocalPorts,
	}
	reportRedeployPlan(plan, onProgress)
	return plan, nil
}

// reportRedeployPlan emits the planning outcome as a single
// [types.StepRedeployPlanning] event naming the stack and the count of services
// the redeploy may recreate. The plan carries no secret values, so the message
// is sink-safe.
func reportRedeployPlan(plan *redeployPlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	onProgress(types.StepRedeployPlanning, 15, fmt.Sprintf(
		"redeploy planned for %s: %d service(s) will be recreated",
		plan.appID,
		len(plan.services),
	))
}

// executeRedeploy runs the redeploy execution stage under the runtime.lock
// already held by [Engine.RedeployStack]. It takes the per-stack exclusive
// flock, reconfirms managed identity through the held fd, asks the Confirmer to
// authorize the redeploy, runs `docker compose up -d`, verifies the
// post-redeploy status, and returns the structured [types.RestartResult].
//
// No config byte is rewritten and .wdm.lock is never written: a decline leaves
// the stack exactly as it was, and a redeploy failure owes no restore (the
// on-disk files are unchanged).
func (e *Engine) executeRedeploy(
	ctx context.Context,
	plan *redeployPlan,
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

	if err := confirmRedeploy(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}

	// Redeploy re-reads .env / .env.user, whose values (including user-added
	// secrets in .env.user) may be interpolated into a compose-layer error.
	// Build the active redactor over the stack's on-disk .env + .env.user
	// secret set — the same fail-closed builder ValidateConfig uses — so any
	// interpolated secret is scrubbed before it leaves the engine (PRD §11,
	// §24). Over-redaction is acceptable.
	redactor, err := validateConfigRedactor(plan.stackPath)
	if err != nil {
		return nil, err
	}
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return nil, err
	}

	if err := runRedeployComposeUp(ctx, client, plan, onProgress); err != nil {
		return nil, err
	}

	status, err := e.verifyRedeployStatus(ctx, client, plan, onProgress)
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

// confirmRedeploy asks the Confirmer to authorize the redeploy immediately
// before `docker compose up -d`, mirroring [confirmRestart]: a nil confirmer
// refuses with [types.ErrCodeUsageValidation], a decline maps to
// [types.ErrCodeUserCanceled] with zero side effects, and a confirmer error
// propagates wrapped.
func confirmRedeploy(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *redeployPlan,
	onProgress types.ProgressFn,
) error {
	return confirmLifecycleOp(ctx, confirmer, redeployConfirmation(plan), confirmStrings{
		stepID:         types.StepRedeployConfirm,
		stepPct:        30,
		stepMessage:    "confirming redeploy",
		nilMessage:     "confirmer is required before redeploy",
		nilHint:        "pass a confirmer that can authorize docker compose up -d",
		confirmErrWrap: "core.redeploy: confirming redeploy",
		declineMessage: "redeploy canceled before docker compose up -d",
		declineHint:    "re-run the redeploy and confirm the prompt",
	}, onProgress)
}

// redeployConfirmation assembles the redeploy consequence payload: the app
// name, stack path, Compose project, an explicit statement that the stack will
// be recreated from its on-disk files to apply overlay edits (brief downtime,
// no data loss), and the services it may recreate. The Kind is the SAFE
// "redeploy_safe" literal: a redeploy preserves all files, volumes, and data,
// so --yes auto-accepts it. The payload carries no secret values.
func redeployConfirmation(plan *redeployPlan) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		"this recreates the stack from its on-disk files to apply .env.user / override edits (brief downtime, no data loss)",
	}
	for _, service := range plan.services {
		lines = append(lines, "recreates service "+service)
	}
	return types.Confirmation{
		Kind:    "redeploy_safe",
		Title:   "apply overlay changes to " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// runRedeployComposeUp runs `docker compose up -d` for the managed stack.
// [docker.ComposeUp] re-reads the Compose file and the content-gated override
// and re-evaluates each service's env_file, so overlay edits take effect; it
// recreates only the containers whose effective config changed. No
// force-recreate is requested: an unchanged stack is a no-op. Client errors
// (including an invalid override surfaced by `docker compose up`) propagate
// unchanged so internal/docker's typed error-code mapping stays authoritative.
func runRedeployComposeUp(
	ctx context.Context,
	client docker.Client,
	plan *redeployPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepRedeployExecute, 60, "recreating containers")
	}

	project, err := logsComposeProject(plan.stackPath, plan.composeProject)
	if err != nil {
		return err
	}
	return docker.ComposeUp(ctx, client, project, docker.ComposeUpOptions{})
}

// verifyRedeployStatus inspects the recreated containers by Compose project and
// wdm labels and fuses the PRD §18 condition subset into a [types.AppStatus]
// through the shared helpers, mirroring [verifyRestartStatus]. The pass runs
// AFTER the redeploy, so a failed inspection never fails the operation: it
// marks the result needs-attention with the status_check_failed reason instead.
func (e *Engine) verifyRedeployStatus(
	ctx context.Context,
	client docker.Client,
	plan *redeployPlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRedeployStatus, 90, "verifying redeployed stack status")
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
		status.Message = "post-redeploy status verification failed; run apps status for details"
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
		"all managed services redeployed and are running",
		"post-redeploy verification found issues; run apps status for details",
	)
	return status, nil
}
