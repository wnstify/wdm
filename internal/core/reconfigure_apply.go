package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
// stack (editing ONLY the targeted resource-limit lines in the existing
// .env in place while preserving every secret, derived value, comment,
// and unrelated line byte-for-byte), validates the unchanged compose
// against the edited .env, confirms the recreate, recreates with up -d
// --force-recreate, commits the manifest, and verifies status.
// Unlike the update path it does NOT re-render the .env or compose from
// the catalog template: a re-render recomputes derived values (such as a
// public-URL built from an install-only domain input) that were never
// persisted to .env, so it would fail for derived-domain apps. The
// in-place edit changes only the resource vars and leaves the rest of
// the install-time .env intact.
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
	// restore the snapshot byte-for-byte and surface a typed error. Only
	// .env changes — the compose and sidecar artifacts are untouched — so
	// the reconfigure writes the .env alone rather than the full file set.
	if err := writeReconfigureEnv(ctx, rewrite, redactor); err != nil {
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

// rewriteReconfigureStack edits ONLY the targeted service's three
// resource-limit lines in the existing stack .env, in place, preserving
// every other line — secrets, derived values, comments, ordering —
// byte-for-byte. It does NOT re-render the .env or compose from the
// catalog template: a re-render would recompute derived values (such as
// a public URL built from an install-only domain input) that were never
// persisted to .env, so it would fail for derived-domain apps. The
// compose template is left exactly as installed; only the resource vars
// the compose already reads from .env at up time change.
// It is PURE — it writes nothing — so any fault propagates unchanged
// with its own typed code and hint, and the caller can route the later
// write and deploy through the restore sad path. The returned plan
// carries the edited .env bytes and the unchanged on-disk compose bytes
// for the fail-closed compose-config validation; the recreate reads both
// files from disk via [installComposeProject].
func (e *Engine) rewriteReconfigureStack(
	ctx context.Context,
	plan *reconfigurePlan,
	onProgress types.ProgressFn,
) (*installPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigureRender, 30, "rewriting reconfigured stack .env")
	}

	existingEnv, err := state.ReadStackEnv(plan.stackPath)
	if err != nil {
		return nil, err
	}
	envBytes, err := readStackFile(plan.stackPath, installEnvFilename)
	if err != nil {
		return nil, err
	}
	composeBytes, err := readStackFile(plan.stackPath, installComposeFilename)
	if err != nil {
		return nil, err
	}

	// Collect the secret-typed values already in the .env so the redactor
	// scrubs them from any later write or deploy fault. Provenance does not
	// matter here: every secret-typed placeholder value is sensitive.
	secretLiterals := collectStackSecretValues(plan.app, existingEnv)
	redactor := security.NewActiveRedactor(secretLiterals)

	key := serviceKey(plan.service)
	newEnv, err := rewriteResourceEnvLines(envBytes, key, plan.memory, plan.cpus, plan.pids)
	if err != nil {
		return nil, redactedVerificationError(
			redactor,
			"resource limits could not be applied to the stack .env",
			"the stack .env is corrupt; reinstall the app to restore managed state",
			err,
		)
	}

	rewrite := &installPlan{
		app:                plan.app,
		stackPath:          plan.stackPath,
		composeProject:     plan.composeProject,
		reusedSecretValues: secretLiterals,
		rendered: render.RenderedStack{
			ComposeBytes: composeBytes,
			EnvBytes:     newEnv,
		},
	}

	// The reconfigure recreates the ON-DISK compose without re-rendering it,
	// so re-run the install-arc catalog-vs-compose guards against the
	// unchanged on-disk bytes (each verifier also runs its own catalog
	// declaration check) to catch post-install tampering before the
	// force-recreate. The order mirrors the install path. These read the
	// compose and catalog only — they do not re-render the .env, so the
	// derived-placeholder bug the in-place edit avoids cannot reappear.
	if err := verifyImagePinsMatchTemplate(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	if err := verifyPublicBindsMatchCatalog(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	if err := verifyContainerPrivilegeMatchCatalog(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	if err := verifySocketPolicyMatchCatalog(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	if err := verifyHostModuleMountMatchCatalog(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	if err := verifyNetworkIPAMMatchCatalog(redactor, plan.app, composeBytes); err != nil {
		return nil, err
	}
	// The non-secret leak guard confirms the unchanged compose carries no
	// secret literal, a defense-in-depth check that costs nothing.
	if err := verifyRenderedNonSecretArtifacts(redactor, secretLiterals, rewrite.rendered, nil); err != nil {
		return nil, err
	}
	return rewrite, nil
}

// readStackFile reads a single regular file under the stack directory by
// its leaf name, path-safe-joined so a symlinked component cannot
// redirect the read outside the stack. The bytes are returned verbatim so
// the reconfigure can edit the .env in place and stage the unchanged
// compose for validation.
func readStackFile(stackPath, name string) ([]byte, error) {
	path, err := security.SafeJoin(stackPath, name)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"remove symlinks from the stack path and retry",
			err,
		)
	}
	// G304 is suppressed: path is SafeJoin'd against the engine-controlled
	// absolute stack path, so no symlinked component can redirect the read.
	data, err := os.ReadFile(path) //nolint:gosec // G304: SafeJoin rejects symlink/traversal escapes from the stack root
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeUsageValidation,
			"stack file could not be read",
			fmt.Sprintf("ensure %q exists and is readable", path),
			err,
		)
	}
	return data, nil
}

// collectStackSecretValues returns the secret-typed placeholder values
// recorded in the stack .env. They seed the redactor so a later write or
// deploy fault never echoes a secret, mirroring the update rewrite's
// reusedSecretValues without re-resolving the placeholder map. A
// secret-typed placeholder absent from the .env contributes nothing: the
// in-place edit never touches it, so its absence cannot leak.
func collectStackSecretValues(app catalog.App, env map[string]string) []string {
	secrets := make([]string, 0, len(app.Placeholders))
	for _, ph := range app.Placeholders {
		if render.Type(ph.Type) != render.TypeSecret {
			continue
		}
		if value, ok := env[ph.Name]; ok && value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

// rewriteResourceEnvLines returns env with the targeted service's three
// resource-limit assignments set to the new values, editing matching
// lines in place and preserving every other byte — secrets, derived
// values, comments, blank lines, and ordering. A targeted var absent from
// the .env is appended in KEY=VALUE form in the stable
// MEMORY/CPUS/PIDS order, matching the install writer's convention. A
// trailing newline is preserved when present and added when an appended
// line would otherwise abut a no-newline final line.
func rewriteResourceEnvLines(env []byte, serviceKey, memory, cpus string, pids int) ([]byte, error) {
	targets := map[string]string{
		"MEMORY_LIMIT_" + serviceKey: memory,
		"CPUS_LIMIT_" + serviceKey:   cpus,
		"PIDS_LIMIT_" + serviceKey:   strconv.Itoa(pids),
	}

	hadTrailingNewline := len(env) > 0 && env[len(env)-1] == '\n'
	lines := strings.Split(string(env), "\n")
	// strings.Split on a trailing-newline buffer yields a final empty
	// element; drop it so appends land before it and a single trailing
	// newline is restored deterministically.
	if hadTrailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	seen := make(map[string]bool, len(targets))
	for i, line := range lines {
		separator := strings.IndexByte(line, '=')
		if separator < 0 {
			continue
		}
		key := strings.TrimSpace(line[:separator])
		newValue, ok := targets[key]
		if !ok {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("resource var %q is duplicated in the stack .env", key)
		}
		seen[key] = true
		lines[i] = key + "=" + newValue
	}

	// Append any targeted var the .env lacked, in stable order so the
	// output is deterministic across runs.
	for _, key := range []string{"MEMORY_LIMIT_" + serviceKey, "CPUS_LIMIT_" + serviceKey, "PIDS_LIMIT_" + serviceKey} {
		if seen[key] {
			continue
		}
		lines = append(lines, key+"="+targets[key])
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNewline || len(out) > 0 {
		out += "\n"
	}
	return []byte(out), nil
}

// writeReconfigureEnv writes the edited .env atomically at secret-file
// mode under the held per-stack flock. Only the .env changes — the
// compose and sidecar artifacts are untouched by a reconfigure — so this
// writes that single file rather than the full install file set. The
// destination is re-validated for symlink-free ancestry before the write
// (PRD §12, §13); [state.WriteFileAtomic] guarantees no half-written
// file, so the step 3 backup stays intact for the restore path.
func writeReconfigureEnv(ctx context.Context, rewrite *installPlan, redactor security.Redactor) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	envPath, err := security.SafeJoin(rewrite.stackPath, installEnvFilename)
	if err != nil {
		return usageValidationError(
			"stack path is unsafe",
			"remove symlinks from the stack path and retry",
			err,
		)
	}
	if err := validateInstallWritePath(rewrite.stackPath, envPath); err != nil {
		return usageValidationError(
			"reconfigure file path is unsafe",
			"remove symlinks from the stack path and retry",
			err,
		)
	}
	if err := state.WriteFileAtomic(envPath, rewrite.rendered.EnvBytes, security.SecretFileMode); err != nil {
		// The .env bytes carry secret literals, so a filesystem fault is
		// scrubbed before any sink.
		return types.WrapError(
			types.ErrCodeGeneric,
			"reconfigured stack .env could not be written",
			"check stack directory permissions and retry",
			newRedactedCause(redactor, err),
		)
	}
	return nil
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
