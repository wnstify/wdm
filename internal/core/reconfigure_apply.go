package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// applyReconfigure runs the consequential span of the reconfigure under
// the runtime.lock already held by [Engine.Reconfigure]. It mirrors
// [Engine.applyUpdate]'s ordering and sad-path discipline: it engages
// the per-stack exclusive flock, reconfirms managed identity through the
// held fd, takes a config backup BEFORE any byte changes, rewrites the
// stack (re-rendering with the new resource vars while preserving every
// secret and unrelated .env value), validates the rewritten compose,
// confirms the recreate, recreates with up -d --force-recreate, commits
// the manifest, and verifies status.
// Sad-path boundary: a fault after the rewrite exposed the new bytes and
// before the manifest commit restores the backup byte-for-byte via the
// shared [Engine.restoreUpdateOnFailure]. A backup-creation failure
// aborts before the rewrite, so nothing is exposed and no restore runs.
func (e *Engine) applyReconfigure(
	ctx context.Context,
	plan *reconfigurePlan,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.ReconfigureResult, error) {
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

	backupPath, err := createReconfigureBackup(plan, onProgress)
	if err != nil {
		return nil, err
	}

	// rewriteReconfigureStack is pure (resolve + render + verify, no
	// filesystem writes), so any fault it returns is pre-exposure and
	// propagates unchanged.
	rewrite, err := e.rewriteReconfigureStack(ctx, plan, onProgress)
	if err != nil {
		return nil, err
	}

	secretLiterals := slices.Concat(rewrite.generatedValues, rewrite.reusedSecretValues)
	redactor := security.NewActiveRedactor(secretLiterals)
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return nil, err
	}

	// The atomic write is the first byte-exposing step, so from here
	// through the manifest commit every fault routes through the sad path:
	// restore the snapshot byte-for-byte and surface a typed error.
	if err := writeUpdateFiles(ctx, rewrite, redactor); err != nil {
		return nil, e.restoreReconfigureOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}
	if err := runReconfigureDeployment(ctx, client, plan, rewrite, existing, confirmer, backupPath, onProgress); err != nil {
		return nil, e.restoreReconfigureOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}

	if err := e.writeReconfigureLockManifest(ctx, existing, handle, backupPath, redactor, onProgress); err != nil {
		return nil, e.restoreReconfigureOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}

	status, err := verifyReconfigureStatus(ctx, client, plan, rewrite, existing, onProgress)
	if err != nil {
		return nil, err
	}
	pruneReconfigureBackups(ctx, e, plan.stackPath, backupPath)
	return buildReconfigureResult(plan, status, backupPath), nil
}

// createReconfigureBackup snapshots the stack's config files before the
// rewrite touches a byte. It reuses the shared [state.CreateConfigBackup]
// with the catalog's additional_files / config_generation destinations,
// tagging the snapshot "reconfigure" so the backup ledger distinguishes
// it from an update snapshot.
func createReconfigureBackup(plan *reconfigurePlan, onProgress types.ProgressFn) (string, error) {
	if onProgress != nil {
		onProgress(types.StepReconfigureBackup, 25, "backing up config before reconfigure")
	}

	artifactPaths := updateBackupArtifactPaths(plan.app)
	snapshotPath, err := state.CreateConfigBackup(plan.stackPath, "reconfigure", artifactPaths)
	if err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"config backup could not be created",
			"check stack directory permissions and free space, then retry",
			err,
		)
	}
	return snapshotPath, nil
}

// rewriteReconfigureStack resolves the placeholder map from the running
// stack, seeds the targeted service's NEW resource limit values, renders
// the artifacts in memory, and verifies no secret leaks into a non-secret
// artifact. It is PURE — it writes nothing — so any fault propagates
// unchanged with its own typed code and hint, and the caller can route
// the later write and deploy through the restore sad path without a
// pre-exposure refusal inheriting a config-restore hint.
// The new resource values are injected into resolvedValues BEFORE the
// install-built-in reuse pass, which skips keys already resolved, so the
// reconfigure values win while every secret and unrelated .env value is
// reused byte-for-byte exactly as the update rewrite does.
func (e *Engine) rewriteReconfigureStack(
	ctx context.Context,
	plan *reconfigurePlan,
	onProgress types.ProgressFn,
) (*installPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureRender, 30, "rendering reconfigured stack")
	}

	rewrite, err := e.resolveReconfigureRewritePlan(plan)
	if err != nil {
		return nil, err
	}

	secretLiterals := slices.Concat(rewrite.generatedValues, rewrite.reusedSecretValues)
	redactor := security.NewActiveRedactor(secretLiterals)

	input, err := e.installRenderInput(ctx, rewrite)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, redactedVerificationError(
			redactor,
			"reconfigure templates could not be loaded",
			"refresh the catalog and retry",
			err,
		)
	}
	envStack, err := render.RenderEnv(input)
	if err != nil {
		return nil, redactedVerificationError(
			redactor,
			"env template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	composeStack, err := render.RenderLabels(input)
	if err != nil {
		return nil, redactedVerificationError(
			redactor,
			"compose template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}

	rewrite.rendered = render.RenderedStack{
		ComposeBytes:    composeStack.ComposeBytes,
		EnvBytes:        envStack.EnvBytes,
		AdditionalFiles: composeStack.AdditionalFiles,
		ConfigArtifacts: composeStack.ConfigArtifacts,
		ServiceLabels:   composeStack.ServiceLabels,
		VolumeMounts:    composeStack.VolumeMounts,
	}

	// Reuse the install-arc catalog-vs-template guards exactly as the
	// update rewrite does: a reconfigure must not let a drifted catalog
	// slip past the same image-pin, public-bind, privilege, socket,
	// module-mount, or IPAM checks.
	if err := verifyImagePinsMatchTemplate(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyPublicBindsMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyContainerPrivilegeMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifySocketPolicyMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyHostModuleMountMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyNetworkIPAMMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyRenderedNonSecretArtifacts(redactor, secretLiterals, rewrite.rendered, nil); err != nil {
		return nil, err
	}
	return rewrite, nil
}

// resolveReconfigureRewritePlan builds the [installPlan] the shared
// render and write helpers consume, with the resolved placeholder map
// sourced from the running stack — identical to the update rewrite
// ([Engine.resolveUpdateRewritePlan]) EXCEPT that the targeted service's
// three resource vars are seeded with the new values before the
// install-built-in reuse pass. Because [reuseUpdateBuiltins] skips keys
// already present in resolvedValues, the seeded reconfigure values are
// authoritative while every other .env key — secrets, non-secret
// placeholders, the other services' resource vars, UID/GID — is reused
// verbatim.
func (e *Engine) resolveReconfigureRewritePlan(plan *reconfigurePlan) (*installPlan, error) {
	existingEnv, err := state.ReadStackEnv(plan.stackPath)
	if err != nil {
		return nil, err
	}

	rewrite := &installPlan{
		app:            plan.app,
		stackPath:      plan.stackPath,
		composeProject: plan.composeProject,
		resolvedValues: make(map[string]string, len(existingEnv)),
	}

	declared := make(map[string]catalog.Placeholder, len(plan.app.Placeholders))
	for _, ph := range plan.app.Placeholders {
		if _, ok := declared[ph.Name]; ok {
			return nil, catalogVerificationError(
				"catalog contains duplicate placeholders",
				"refresh the catalog and retry",
				fmt.Errorf("duplicate placeholder %q", ph.Name),
			)
		}
		declared[ph.Name] = ph
		rewrite.placeholders = append(rewrite.placeholders, render.Placeholder{
			Name:        ph.Name,
			Type:        render.Type(ph.Type),
			Required:    ph.Required,
			Default:     ph.Default,
			Regenerable: ph.Regenerable,
		})

		if err := rewrite.resolveUpdatePlaceholder(ph, existingEnv, e.generateSecret); err != nil {
			return nil, err
		}
	}

	// Seed the new resource vars BEFORE reuseUpdateBuiltins so they win
	// over the existing .env copies; reuseUpdateBuiltins then reuses every
	// other key verbatim.
	key := serviceKey(plan.service)
	if err := rewrite.addSyntheticResolvedValue("MEMORY_LIMIT_"+key, plan.memory); err != nil {
		return nil, err
	}
	if err := rewrite.addSyntheticResolvedValue("CPUS_LIMIT_"+key, plan.cpus); err != nil {
		return nil, err
	}
	if err := rewrite.addSyntheticResolvedValue("PIDS_LIMIT_"+key, strconv.Itoa(plan.pids)); err != nil {
		return nil, err
	}

	if err := reuseUpdateBuiltins(rewrite, existingEnv, declared); err != nil {
		return nil, err
	}
	return rewrite, nil
}

// runReconfigureDeployment runs the post-write, pre-commit span: compose
// config validation, the recreate confirmation, catalog network
// pre-creation, and up -d --force-recreate. It mirrors
// [runUpdateDeployment] but takes no pull leg — a reconfigure changes no
// image, so there is nothing new to fetch. Every step runs after the
// write exposed the new bytes and before the manifest commit, so the
// caller routes any error through the restore sad path.
func runReconfigureDeployment(
	ctx context.Context,
	client docker.Client,
	plan *reconfigurePlan,
	rewrite *installPlan,
	existing *state.StackLock,
	confirmer types.Confirmer,
	backupPath string,
	onProgress types.ProgressFn,
) error {
	if err := validateReconfigureComposeConfig(ctx, client, rewrite, onProgress); err != nil {
		return err
	}
	if err := confirmReconfigureDeployment(ctx, confirmer, plan, existing.LocalPorts, backupPath, onProgress); err != nil {
		return err
	}
	if err := ensureInstallNetworks(ctx, client, rewrite, nil, nil); err != nil {
		return err
	}
	return deployReconfigureStack(ctx, client, rewrite, onProgress)
}

// validateReconfigureComposeConfig runs docker compose config against a
// private tempdir copy of the rewritten artifact set, fail-closed: any
// validation error aborts before the recreate. It reuses the shared
// [validateRenderedComposeConfig].
func validateReconfigureComposeConfig(
	ctx context.Context,
	client docker.Client,
	rewrite *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureComposeValidate, 40, "validating reconfigured compose config")
	}
	return validateRenderedComposeConfig(ctx, client, &rewrite.rendered)
}

// confirmReconfigureDeployment gates the recreate on the [types.Confirmer]
// after the rewrite and before the recreate, mirroring
// [confirmUpdateDeployment]: a nil confirmer refuses with
// [types.ErrCodeUsageValidation], a decline maps to
// [types.ErrCodeUserCanceled], and a confirmer error propagates wrapped.
// A decline runs no Docker mutation; the caller restores the snapshot.
func confirmReconfigureDeployment(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *reconfigurePlan,
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
			"confirmer is required before reconfigure",
			"pass a confirmer that can authorize docker compose recreate",
		)
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureConfirm, 45, "confirming reconfigure deployment")
	}

	confirmed, err := confirmer.Confirm(ctx, reconfigureConfirmation(plan, localPorts, backupPath))
	if err != nil {
		return fmt.Errorf("core.reconfigure: confirming deployment: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"reconfigure canceled before deployment",
			"re-run the reconfigure and confirm the recreate prompt",
		)
	}
	return nil
}

// reconfigureConfirmation assembles the recreate consequence payload: the
// stack identity, the targeted service and its new resource limits, the
// localhost ports the recreate rebinds, and the pre-change backup path.
// Recreating a container is briefly disruptive, so the message states the
// downtime. The payload carries no secret values.
func reconfigureConfirmation(plan *reconfigurePlan, localPorts []int, backupPath string) types.Confirmation {
	lines := []string{
		"app: " + plan.appID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
		fmt.Sprintf("service: %s", plan.service),
		fmt.Sprintf("memory limit: %s", plan.memory),
		fmt.Sprintf("cpu limit: %s", plan.cpus),
		fmt.Sprintf("pids limit: %d", plan.pids),
		"recreates the container (brief downtime)",
	}
	for _, port := range localPorts {
		lines = append(lines, fmt.Sprintf("rebinds 127.0.0.1:%d", port))
	}
	if backupPath != "" {
		lines = append(lines, "config backup: "+backupPath)
	}
	return types.Confirmation{
		Kind:    "reconfigure_deploy",
		Title:   "reconfigure " + plan.appID,
		Message: strings.Join(lines, "\n"),
	}
}

// deployReconfigureStack runs docker compose up -d --force-recreate so
// the recreated container picks up the rewritten compose and .env. It
// runs no pull — a reconfigure changes no image. Client errors propagate
// unchanged so internal/docker's typed error mapping stays authoritative.
func deployReconfigureStack(
	ctx context.Context,
	client docker.Client,
	rewrite *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureDeploy, 70, "recreating docker compose stack")
	}

	project, err := installComposeProject(rewrite)
	if err != nil {
		return err
	}
	return docker.ComposeUp(ctx, client, project, docker.ComposeUpOptions{ForceRecreate: true})
}

// reconfigureBackupHistoryEntry is the per-reconfigure ledger record
// appended to [state.StackLock.BackupHistory] at the commit point. The
// field is opaque [json.RawMessage], so the reconfigure path defines this
// minimal record locally and marshals it; existing history is preserved
// verbatim.
type reconfigureBackupHistoryEntry struct {
	Path      string    `json:"path"`
	Operation string    `json:"operation"`
	At        time.Time `json:"at"`
}

// writeReconfigureLockManifest persists the updated .wdm.lock through the
// held per-stack flock fd — the commit point. Only the
// last_successful_operation and backup_history fields change: a
// reconfigure keeps the installed template/catalog version, image pins,
// generated fields, domain, ports, and recommended resources untouched.
// The new resource values live in the rewritten .env, not the manifest.
func (e *Engine) writeReconfigureLockManifest(
	ctx context.Context,
	existing *state.StackLock,
	handle *state.StackLockHandle,
	backupPath string,
	redactor security.Redactor,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureLockUpdate, 80, "updating stack lock manifest")
	}

	now := time.Now().UTC()
	lock := *existing
	lock.LastSuccessfulOperation = &types.Operation{
		Kind:       "reconfigure",
		At:         now,
		WDMVersion: e.version,
	}

	history, err := appendReconfigureBackupHistory(existing.BackupHistory, backupPath, now)
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

// appendReconfigureBackupHistory clones the existing backup_history and
// appends the new snapshot record. An empty backupPath appends nothing,
// so the ledger records only real snapshots.
func appendReconfigureBackupHistory(existing []json.RawMessage, backupPath string, at time.Time) ([]json.RawMessage, error) {
	history := make([]json.RawMessage, 0, len(existing)+1)
	for _, entry := range existing {
		history = append(history, append(json.RawMessage(nil), entry...))
	}
	if backupPath == "" {
		return history, nil
	}
	encoded, err := json.Marshal(reconfigureBackupHistoryEntry{
		Path:      backupPath,
		Operation: "reconfigure",
		At:        at,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding backup history entry: %w", err)
	}
	return append(history, json.RawMessage(encoded)), nil
}

// restoreReconfigureOnFailure is the sad path for a failed reconfigure.
// It delegates to the shared [Engine.restoreUpdateOnFailure], which
// restores the snapshot byte-for-byte (config files only) and surfaces a
// typed error, after emitting the reconfigure-scoped restore progress
// event. The update-scoped plan it needs is a minimal projection of the
// reconfigure plan — only stackPath and appID are read on the restore
// path.
func (e *Engine) restoreReconfigureOnFailure(
	fault error,
	plan *reconfigurePlan,
	existing *state.StackLock,
	backupPath string,
	redactor security.Redactor,
	onProgress types.ProgressFn,
) error {
	if onProgress != nil {
		onProgress(types.StepReconfigureConfigRestore, 75, "restoring previous config after reconfigure failure")
	}
	return e.restoreUpdateOnFailure(
		fault,
		&updateCheckPlan{appID: plan.appID, stackPath: plan.stackPath},
		existing,
		backupPath,
		redactor,
		nil,
	)
}

// verifyReconfigureStatus inspects the recreated containers and fuses the
// install-time PRD §18 condition subset into a [types.AppStatus] through
// the shared status helpers. It runs AFTER the commit point, so a failed
// inspection marks the result needs-attention rather than failing the
// durable reconfigure. It reuses [verifyUpdateStatus] with the
// reconfigure plan projected onto the update plan shape.
func verifyReconfigureStatus(
	ctx context.Context,
	client docker.Client,
	plan *reconfigurePlan,
	rewrite *installPlan,
	existing *state.StackLock,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if onProgress != nil {
		onProgress(types.StepReconfigureStatus, 90, "verifying reconfigured stack status")
	}
	return verifyUpdateStatus(
		ctx,
		client,
		&updateCheckPlan{appID: plan.appID, stackPath: plan.stackPath},
		rewrite,
		existing,
		nil,
	)
}

// pruneReconfigureBackups enforces the per-stack retention cap after the
// commit point, pinning this run's snapshot. A prune failure is logged
// and swallowed — it can only leave MORE backups than the cap, which is
// safe, and the reconfigure has already committed. It reuses
// [pruneUpdateBackups].
func pruneReconfigureBackups(ctx context.Context, e *Engine, stackPath, pinnedSnapshot string) {
	pruneUpdateBackups(ctx, e.logger, stackPath, pinnedSnapshot)
}

// buildReconfigureResult assembles the structured result: the app,
// service, the applied resource limits, the pre-change backup path, and
// the fused post-recreate status snapshot.
func buildReconfigureResult(plan *reconfigurePlan, status *types.AppStatus, backupPath string) *types.ReconfigureResult {
	return &types.ReconfigureResult{
		AppID:          plan.appID,
		Service:        plan.service,
		ComposeProject: plan.composeProject,
		Memory:         plan.memory,
		CPUs:           plan.cpus,
		PIDs:           plan.pids,
		BackupPath:     backupPath,
		Status:         status,
	}
}
