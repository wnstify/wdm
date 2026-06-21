package core

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// removePlan is the outcome of the PRD §19 safe-removal planning stage:
// the managed stack resolved from req.AppID plus the consequence data
// [Engine.executeRemove] needs to confirm the removal, run `docker compose
// down`, and report what wdm left on disk.
// The plan is assembled read-only — no file write, no.env read, no render
// no Confirmer call
// and no manifest
// mutation (the confirmation rulesrecords the last_successful_operation kind="remove"
// at the execution commit point). It carries the observational fields
// forward so the execution stage consumes them without re-reading the
// stack, the catalog, or Docker — mirroring [updateCheckPlan].
// preservedPaths, remainingNamedVolumes, and remainingNetworks describe
// state that survives a safe removal (PRD §19):.env,
// Compose files, lock files, backups, app data, databases, named volumes,
// and networks. They are surfaced on [types.RemoveResult] so the user
// learns exactly what wdm did not delete.
type removePlan struct {
	appID                 string
	stackPath             string
	composeProject        string
	preservedPaths        []string
	remainingNamedVolumes []string
	remainingNetworks     []string
}

// Remove performs a safe removal of a managed stack (PRD §19): it stops and
// removes the stack's containers via `docker compose down` while keeping
// every file and every named volume on disk — `.env`, the Compose file, the
// `.wdm.lock` manifest, the `.wdm-backups/` snapshots, app data, and
// databases all survive (PRD §19 steps 3-5). The only
// on-disk mutation is the manifest's last_successful_operation, recorded as
// kind="remove" through the held per-stack flock fd (the confirmation rules — the lock
// records intent; the absence of running containers under the project is the
// live signal).
// Lock posture (PRD §26): Remove is a
// state-changing engine entry, so the global runtime.lock is acquired at
// entry and held until return. Planning reads the stack manifest through the
// non-blocking shared-flock path shared with Status and the update check — a
// stack mid-operation refuses with [types.ErrCodeRuntimeLockHeld] instead of
// stalling behind the writer — while the execution stage takes the exclusive
// per-stack flock of protocol step 2 and reconfirms managed identity through
// the held fd before any Docker mutation.
// Managed-only ordering (PRD §9, §10, §19): the stack must
// resolve to a directory whose .wdm.lock parses and names req.AppID before
// any Docker command runs. Unmanaged directories and uninstalled apps refuse
// with [types.ErrCodeUsageValidation]; corrupt manifests surface wrapped
// [types.ErrStaleState]; a manifest missing its compose project refuses with
// [types.ErrCodeUsageValidation] naming the corrupt lock. Resolution is
// AppID-driven like Status and the update check, so a supplied req.StackPath
// is verified against the resolved managed stack rather than used as an
// alternate resolution path: a mismatch (after filepath.Clean) refuses
// fail-closed before any Docker call, while a matching path
// proceeds.
// Execution order: exclusive flock →
// reconfirm managed identity → [types.Confirmer] (immediately before `down`,
// with a consequence payload listing what will be stopped and what will be
// preserved) → `docker compose down` → manifest commit
// through the held fd (the PRD §30 commit point) → post-down status
// verification. A nil confirmer refuses with [types.ErrCodeUsageValidation],
// a decline maps to [types.ErrCodeUserCanceled] and leaves ZERO trace
// (nothing downed, nothing written — the confirm precedes both), and a
// confirmer error propagates wrapped. The path never renders,
// never reads.env content, takes no pre-remove config backup (remove
// rewrites no config bytes, so the protocol step-3 snapshot has nothing to
// undo and pkg/types defines no StepRemoveBackup), and surfaces no "rollback"
// wording. Progress rides the frozen step_remove_* stream.
// A `down` failure surfaces the docker-layer typed error unchanged and
// leaves the manifest UNMARKED (the commit point is the post-down manifest
// write), so the files stay byte-identical and a later [Engine.Status] fuses
// the PRD §18 conditions against whatever Docker left behind. Past the commit
// point the removal is durable: a broken post-down inspection marks the
// result needs-attention with the status_check_failed reason rather than
// failing the durable removal.
func (e *Engine) Remove(
	ctx context.Context,
	req types.RemoveRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.RemoveResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "remove")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planRemove(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	return e.executeRemove(ctx, plan, confirmer, onProgress)
}

// planRemove runs the non-mutating safe-removal planning under the held
// runtime.lock: managed-stack resolution first (PRD §19), then the
// opportunistic named-volume and network gathering that
// [types.RemoveResult] surfaces. The emitted [types.StepRemovePlanning]
// events carry the planning outcome so callers never parse prose for step
// identity; only step_remove_* IDs appear on this path.
func (e *Engine) planRemove(
	ctx context.Context,
	req types.RemoveRequest,
	onProgress types.ProgressFn,
) (*removePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireAppID(req.AppID); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRemovePlanning, 5, "planning safe removal")
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

	volumes, err := e.listRemoveNamedVolumes(ctx, lock.ComposeProject)
	if err != nil {
		return nil, err
	}

	networks, err := e.planRemainingNetworks(ctx, req.AppID)
	if err != nil {
		return nil, err
	}

	plan := &removePlan{
		appID:                 req.AppID,
		stackPath:             stackPath,
		composeProject:        lock.ComposeProject,
		preservedPaths:        []string{stackPath},
		remainingNamedVolumes: volumes,
		remainingNetworks:     networks,
	}
	reportRemovePlan(plan, onProgress)
	return plan, nil
}

// listRemoveNamedVolumes lists the Compose-project named volumes safe
// removal preserves (the confirmation rules: the
// `label=com.docker.compose.project=wdm-<app>` filter). The listing is
// opportunistic: the exit criterion surfaces remaining volumes "when Docker
// inspection data is available", mirroring the image-digest
// capture — a transient inspect failure
// must not block a removal, so it WARN-logs and reports an empty list. The
// hard carve-outs match the read-only Status path: context cancellation and
// an unreachable daemon ([types.ErrCodeDockerUnavailable]) propagate
// unchanged so a canceled operation and a dead daemon stay typed errors
// rather than a silent empty list. The client carries the read-only
// structural redactor — no secret literals exist on the remove path.
func (e *Engine) listRemoveNamedVolumes(ctx context.Context, composeProject string) ([]string, error) {
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
		e.logger.WarnContext(ctx, "core: skipping named-volume listing during safe-remove planning",
			slog.String("compose_project", composeProject),
			slog.Any("error", err),
		)
		return nil, nil
	}
	return volumes, nil
}

// relistRemoveNamedVolumesPostCommit re-lists the Compose-project named
// volumes AFTER the protocol step 6 commit point so [types.RemoveResult]
// proves what actually survived `docker compose down`.
// Unlike the pre-commit [Engine.listRemoveNamedVolumes], it has no daemon
// carve-out: past the durable commit a removal must never fail, so a
// daemon-down re-list WARN-logs and reports the empty list sanctioned for
// "inspection data unavailable", exactly as an ordinary inspect failure does.
// Context cancellation is the sole hard carve-out:
// a canceled operation still surfaces its typed error rather than a silent
// empty list. It reuses the client already built for the execution stage —
// the same structural redactor with no operation secret literals.
func (e *Engine) relistRemoveNamedVolumesPostCommit(
	ctx context.Context,
	client docker.Client,
	composeProject string,
) ([]string, error) {
	volumes, err := docker.ListProjectNamedVolumes(ctx, client, composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		e.logger.WarnContext(ctx, "core: skipping named-volume re-list after safe removal",
			slog.String("compose_project", composeProject),
			slog.Any("error", err),
		)
		return nil, nil
	}
	return volumes, nil
}

// planRemainingNetworks resolves the catalog-declared networks safe removal
// leaves in place so [types.RemoveResult] tells the user what wdm did not
// delete. The .wdm.lock manifest does not
// record networks, so the catalog is the only source for the set — but
// remove acts on a wdm-managed stack (valid lock + labels per PRD §9/§10),
// not on catalog availability, so this read is opportunistic: a catalog that
// is absent, unreadable, or no longer carries the app WARN-logs and yields
// an empty list rather than blocking the removal.
// The single hard carve-out is context cancellation: [loadInstallCatalog]
// checks ctx at entry, so a cancellation between the volume listing and this
// read surfaces as the canceled-context error rather than a false "catalog
// unavailable" WARN plus an empty list. Unlike the named-volume listing
// there is no daemon carve-out — the catalog read is filesystem-only and
// never touches the Docker daemon. Names are sorted and deduplicated.
func (e *Engine) planRemainingNetworks(ctx context.Context, appID string) ([]string, error) {
	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		e.logger.WarnContext(ctx, "core: catalog unavailable for remaining-network reporting during safe-remove planning",
			slog.String("app_id", appID),
			slog.Any("error", err),
		)
		return nil, nil
	}
	app, err := selectCatalogApp(cat, appID)
	if err != nil {
		e.logger.WarnContext(ctx, "core: app absent from catalog for remaining-network reporting during safe-remove planning",
			slog.String("app_id", appID),
			slog.Any("error", err),
		)
		return nil, nil
	}
	return catalogNetworkNames(app.Networks), nil
}

// catalogNetworkNames projects catalog network declarations into a
// sorted, deduplicated name list. Empty names are skipped.
func catalogNetworkNames(networks []catalog.Network) []string {
	seen := make(map[string]struct{}, len(networks))
	names := make([]string, 0, len(networks))
	for _, network := range networks {
		if network.Name == "" {
			continue
		}
		if _, ok := seen[network.Name]; ok {
			continue
		}
		seen[network.Name] = struct{}{}
		names = append(names, network.Name)
	}
	sort.Strings(names)
	return names
}

// reportRemovePlan emits the planning outcome as a single
// [types.StepRemovePlanning] event naming the stack and the counts of
// preserved volumes and networks. The plan carries no secret values
// (Compose project, stack path, volume and network names only), so the
// message is sink-safe by construction.
func reportRemovePlan(plan *removePlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	onProgress(types.StepRemovePlanning, 15, removePlanSummaryMessage(plan))
}

func removePlanSummaryMessage(plan *removePlan) string {
	return fmt.Sprintf(
		"safe removal planned for %s: %d named volume(s) and %d network(s) will be preserved",
		plan.appID,
		len(plan.remainingNamedVolumes),
		len(plan.remainingNetworks),
	)
}

// executeRemove runs the PRD §19 safe-removal execution stage under the
// runtime.lock already held by [Engine.Remove]. It takes the per-stack
// exclusive flock (step 2), reconfirms
// managed identity through the held fd, asks the Confirmer to authorize the
// removal (step 4-equivalent — remove skips backup and render), runs
// `docker compose down` (step 5, NEVER -v), records the
// last_successful_operation kind="remove"
// through the held fd as the commit point (step 6, PRD §30), verifies the
// post-down status, and returns the structured [types.RemoveResult].
// Ordering is load-bearing for the row-38 "leaves any completed safe state
// intact" guarantee: the confirm precedes BOTH `down` and the manifest
// write, so a decline leaves zero trace — no container stopped, no manifest
// byte changed. The `down` precedes the manifest write, so a down failure
// leaves the manifest UNMARKED and the files byte-identical (no restore is
// owed because remove rewrites no config bytes). Past the manifest commit
// the removal is durable: a broken post-down inspection marks the result
// needs-attention rather than failing the removal.
// No pre-remove config backup is taken. makes the protocol
// identical across the three write operations "only [in] step 5's subcommand
// and the step 4 render content" — and remove skips render entirely, so step
// 4 exposes no new bytes and the protocol step-3 snapshot (which exists to
// undo a step-4 rewrite per / 40) has nothing to restore: the bytes
// a backup would copy are the bytes safe removal leaves in place. pkg/types
// defines no StepRemoveBackup, and the confirmation rulesrecords that remove's only
// manifest change is the intent marker — so a snapshot here would be an
// unobservable, never-consulted copy of unchanged files.
func (e *Engine) executeRemove(
	ctx context.Context,
	plan *removePlan,
	confirmer types.Confirmer,
	onProgress types.ProgressFn,
) (*types.RemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	handle, err := acquireInstallStackLock(ctx, plan.stackPath)
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	existing, err := reconfirmManagedStack(handle, plan.appID)
	if err != nil {
		return nil, err
	}

	if err := confirmRemove(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}

	// The remove path generates no secrets and reads no.env content, so the
	// Docker client carries the structural redactor only — there are no
	// operation secret literals to register (mirrors the c39 planning client).
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	if err := runRemoveComposeDown(ctx, client, plan, onProgress); err != nil {
		return nil, err
	}
	// The manifest write is the commit point (PRD §30, protocol step 6):
	// after it returns, the removal intent is durable. It runs only after
	// `down` succeeded, so a failed down never records a "remove" the user
	// did not get.
	if err := e.writeRemoveLockManifest(ctx, existing, handle, onProgress); err != nil {
		return nil, err
	}

	return e.buildRemoveResult(ctx, client, plan, onProgress)
}

// confirmRemove asks the Confirmer to authorize the safe removal
// immediately before `docker compose down`, mirroring install's
// [confirmInstallDeployment]: a nil confirmer
// refuses with [types.ErrCodeUsageValidation] per the pkg/engine contract, a
// decline maps to [types.ErrCodeUserCanceled], and a confirmer error
// propagates wrapped. The confirm runs before any Docker mutation and before
// the manifest write, so a decline leaves the stack exactly as it was
// (PRD §25 "preserve any completed safe state").
func confirmRemove(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *removePlan,
	onProgress types.ProgressFn,
) error {
	return confirmLifecycleOp(ctx, confirmer, removeConfirmation(plan), confirmStrings{
		stepID:         types.StepRemoveConfirm,
		stepPct:        30,
		stepMessage:    "confirming safe removal",
		nilMessage:     "confirmer is required before removal",
		nilHint:        "pass a confirmer that can authorize docker compose down",
		confirmErrWrap: "core.remove: confirming removal",
		declineMessage: "removal canceled before docker compose down",
		declineHint:    "re-run the removal and confirm the prompt",
	}, onProgress)
}

// removeConfirmation assembles the safe-removal consequence payload (PRD §19
// steps 1-3): the app name and stack path,
// an explicit statement that containers will be stopped and removed while
// files and data are kept, and the preserved paths, named
// volumes, and networks safe removal leaves in place. The
// payload carries no secret values — stack path, Compose project, and
// volume/network names only — so it is sink-safe by construction.
func removeConfirmation(plan *removePlan) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		"this stops and removes the stack's containers",
		"files and data are kept: .env, compose file, lock file, backups, app data, databases",
	}
	for _, path := range plan.preservedPaths {
		lines = append(lines, "keeps "+path)
	}
	for _, volume := range plan.remainingNamedVolumes {
		lines = append(lines, "keeps named volume "+volume)
	}
	for _, network := range plan.remainingNetworks {
		lines = append(lines, "keeps docker network "+network)
	}
	return types.Confirmation{
		Kind:    "remove_safe",
		Title:   "remove " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// runRemoveComposeDown runs `docker compose down` for the managed stack
// (PRD §19 step 4). [docker.ComposeDown] is
// structurally free of -v: it stops and removes the
// stack's containers and the default network Compose created for the
// project, but never touches named volumes or the catalog-declared networks
// safe removal preserves. Client errors propagate unchanged
// so internal/docker's typed error-code mapping stays authoritative
// a down failure leaves the manifest unmarked because
// the commit point is the later manifest write.
func runRemoveComposeDown(
	ctx context.Context,
	client docker.Client,
	plan *removePlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepRemoveComposeDown, 55, "stopping and removing containers")
	}

	project, err := logsComposeProject(plan.stackPath, plan.composeProject)
	if err != nil {
		return err
	}
	return docker.ComposeDown(ctx, client, project)
}

// writeRemoveLockManifest persists the removed-state manifest through the
// held per-stack flock fd — the commit point (PRD §30, protocol step 6).
// [state.StackLockHandle.Write] uses the in-place
// truncate/seek/write/fsync pattern; tmp+rename is forbidden for lock files
// because rename would detach the flocked inode.
// The.wdm.lock REMAINS on disk (PRD §19 keeps lock files): the only change
// is last_successful_operation, set to kind="remove" with the commit
// timestamp and the wdm version, recording intent (the confirmation rules — the live
// signal is the absence of running containers, not a lock field). Every
// other field is preserved byte-equivalent from the reconfirmed manifest —
// identity, ports, image pins, generated fields, backup history, recommended
// resources — so a later reinstall or status read still sees the stack's
// full provenance.
func (e *Engine) writeRemoveLockManifest(
	ctx context.Context,
	existing *state.StackLock,
	handle *state.StackLockHandle,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepRemoveLockUpdate, 75, "recording removal in stack manifest")
	}

	lock := *existing
	lock.LastSuccessfulOperation = &types.Operation{
		Kind:       "remove",
		At:         time.Now().UTC(),
		WDMVersion: e.version,
	}
	if err := handle.Write(lock); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack lock manifest could not be written",
			"check stack directory permissions and retry",
			err,
		)
	}
	return nil
}

// buildRemoveResult verifies the post-down state and assembles the
// structured [types.RemoveResult] (PRD §19, §32). The named volumes are
// re-listed AFTER `down` so the result proves what actually survived the
// removal (-369 — volumes must remain; a post-down re-list is
// the honest "remain after removal" evidence) rather than echoing the
// pre-down plan snapshot. The re-list runs through the post-commit
// [Engine.relistRemoveNamedVolumesPostCommit] posture, so any transient
// inspect failure — a dead daemon included — yields the empty list
// failing the durable removal; only a canceled ctx still
// propagates. It runs over the client already passed in for status
// verification rather than constructing another one. Preserved paths and
// remaining networks come from the plan (networks are never touched by
// `down` per, so the plan value is current).
func (e *Engine) buildRemoveResult(
	ctx context.Context,
	client docker.Client,
	plan *removePlan,
	onProgress types.ProgressFn,
) (*types.RemoveResult, error) {
	status, err := e.verifyRemoveStatus(ctx, client, plan, onProgress)
	if err != nil {
		return nil, err
	}

	remainingVolumes, err := e.relistRemoveNamedVolumesPostCommit(ctx, client, plan.composeProject)
	if err != nil {
		return nil, err
	}

	return &types.RemoveResult{
		AppID:                 plan.appID,
		StackPath:             plan.stackPath,
		ComposeProject:        plan.composeProject,
		PreservedPaths:        plan.preservedPaths,
		RemainingNamedVolumes: remainingVolumes,
		RemainingNetworks:     plan.remainingNetworks,
		Status:                status,
	}, nil
}

// verifyRemoveStatus confirms `docker compose down` left no managed
// container behind (PRD §19 step 5, §18). It runs AFTER the protocol step 6
// commit point, so it can never fail the durable removal: any inspection
// failure marks the result needs-attention with the status_check_failed
// reason (mirroring [verifyUpdateStatus] and [verifyInstallStatus], which
// carve out ctx.Err ONLY), and a managed container that survived the down
// is likewise surfaced as status_check_failed rather than failing the
// removal. Context cancellation is the sole hard carve-out: it propagates
// unchanged. A daemon-down inspect failure fuses as needs-attention like any
// other post-commit inspect failure rather than failing the removal the
// belongs to the read-only pre-commit Status path, not here.
// The success state inverts install/update: there, expected services must be
// present; here, the absence of managed containers under the Compose project
// IS success, so the result is State "removed" with a preservation message.
// The shared [fuseManagedServiceStatuses] helper is NOT used — its
// empty-expected/empty-managed branch raises container_missing, which is
// backwards for a removal — so the managed-container fusion is done locally
// for any lingering container.
func (e *Engine) verifyRemoveStatus(
	ctx context.Context,
	client docker.Client,
	plan *removePlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepRemoveStatus, 90, "verifying containers were removed")
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
		status.Message = "post-remove status verification failed; run apps status for details"
		return status, nil
	}

	managed := managedContainersForApp(plan.appID, containers)
	if len(managed) == 0 {
		status.State = statusStateRemoved
		status.Message = "containers stopped and removed; files and data preserved"
		return status, nil
	}

	for _, container := range managed {
		// A container that survived `docker compose down` should not exist
		// after a successful removal, so every lingering container is flagged
		// needs-attention regardless of its runtime state.
		status.Services = append(status.Services, types.ServiceStatus{
			Service:        container.Service,
			ContainerName:  container.Name,
			State:          container.State.Status,
			Health:         container.State.Health,
			PublishedPorts: publishedPortBindings(container),
			NeedsAttention: true,
			Message:        "container still present after removal",
		})
	}
	sort.Slice(status.Services, func(i, j int) bool {
		return status.Services[i].Service < status.Services[j].Service
	})
	status.State = statusStateNeedsAttention
	status.NeedsAttention = true
	status.AttentionReasons = []string{statusReasonStatusCheckFailed}
	status.Message = fmt.Sprintf(
		"%d managed container(s) remain after removal; run apps status for details",
		len(managed),
	)
	return status, nil
}

// managedContainersForApp filters an inspected container set to the
// wdm-managed containers for appID, deduplicating by service so a lingering
// container is reported once (PRD §10, §18).
func managedContainersForApp(appID string, containers []docker.ContainerInfo) []docker.ContainerInfo {
	seen := make(map[string]struct{}, len(containers))
	managed := make([]docker.ContainerInfo, 0, len(containers))
	for _, container := range containers {
		if container.Labels["wdm.managed"] != "true" || container.Labels["wdm.app"] != appID {
			continue
		}
		if _, ok := seen[container.Service]; ok {
			continue
		}
		seen[container.Service] = struct{}{}
		managed = append(managed, container)
	}
	return managed
}
