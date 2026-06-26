package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// applyUpdate runs PRD §20 steps 7-11 under the runtime.lock already
// held by [Engine.Update]. It engages the per-stack exclusive flock,
// replacing the shared planning read with the authoritative held-fd view,
// and holds it across backup → rewrite →
// validate → confirm → networks → pull → recreate → manifest write →
// status → prune (or, on a fault, → restore), releasing it on every
// return via defer (the manifest commit goes through the held fd before
// that defer fires; status, prune, and restore do not need the flock).
// Ordering is load-bearing:
//   - The database-risk warning ([Engine.Update] runs it before calling
//     here) precedes backup and rewrite.
//   - The backup precedes the rewrite (PRD §20 step 7 before step 8;
//     restorable snapshot.
//   - Compose validation (PRD §20 step 9) and the recreate confirmation
//     precede pull + recreate (step 10).
//   - The manifest write is the commit point (PRD §30, protocol step 6);
//     retention pruning runs only after it is durable.
//
// Sad-path boundary: a fault
// anywhere after the rewrite exposed the new bytes and before the
// manifest commit — rewrite, validation, a confirmer decline, a network
// failure, pull, or recreate — routes through [Engine.restoreUpdateOnFailure],
// which restores the step-3 snapshot byte-for-byte (config files only,
// never Docker-side state) and surfaces a typed error: the
// induced-failure case carries [types.ErrCodeGeneric] with a hint naming
// the restored backup path, a decline keeps
// [types.ErrCodeUserCanceled], and a restore that itself fails
// fails closed by joining both causes. A backup-creation
// failure (step 3) aborts BEFORE the rewrite, so nothing is exposed and
// no restore runs. Faults after the commit point — status
// verification trouble or a prune failure — never fail the durable
// update: status marks needs-attention, a prune failure is logged
// and neither restores.
func (e *Engine) applyUpdate(
	ctx context.Context,
	plan *updateCheckPlan,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.UpdateResult, error) {
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

	backupPath, err := createUpdateBackup(plan, onProgress)
	if err != nil {
		return nil, err
	}
	// rewriteUpdateStack is pure (resolve + render + verify, no filesystem
	// writes), so any fault it returns is pre-exposure and propagates
	// UNCHANGED with its own typed code and hint: the locked
	// missing-secret refusal must not inherit a config-restore hint, and no
	// restore is owed because nothing was exposed.
	rewrite, err := e.rewriteUpdateStack(ctx, plan, onProgress)
	if err != nil {
		return nil, err
	}

	// The operation Docker client carries BOTH generated and reused secret
	// literals so Compose stderr is scrubbed of reused install-time secrets
	// too. The same literals drive every error redaction
	// on this path, including the sad-path restore wrapper.
	secretLiterals := slices.Concat(rewrite.generatedValues, rewrite.reusedSecretValues)
	redactor := security.NewActiveRedactor(secretLiterals)
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return nil, err
	}

	// The atomic write (PRD §20 step 8) is the first byte-exposing step, so
	// from here through the manifest commit every fault routes through the
	// step 7 sad path: restore the step-3 snapshot byte-for-byte and surface
	// a typed error. The restore is
	// config-files-only and never touches Docker-side state. A
	// partial write (compose written,.env not) is the "step 4 fault" the
	// snapshot exists to undo.
	if err := writeUpdateFiles(ctx, rewrite, redactor); err != nil {
		return nil, e.restoreUpdateOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}
	// Steps 4 validate/confirm through 5 deploy run after the write exposed
	// the new bytes and before the manifest commit point.
	if err := runUpdateDeployment(ctx, client, plan, rewrite, existing, confirmer, backupPath, onProgress); err != nil {
		return nil, e.restoreUpdateOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}

	pins, err := captureInstallImagePins(ctx, client, rewrite)
	if err != nil {
		// captureInstallImagePins is opportunistic and never
		// faults on ordinary digest absence; a hard error here is still
		// pre-commit, so the sad path restores.
		return nil, e.restoreUpdateOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}
	if err := e.writeUpdateLockManifest(ctx, plan, rewrite, existing, handle, pins, backupPath, redactor, onProgress); err != nil {
		// The manifest write is the commit point: a fault here means it
		// never became durable, so the step-3 snapshot still restores the
		// previous config.
		return nil, e.restoreUpdateOnFailure(err, plan, existing, backupPath, redactor, onProgress)
	}

	// Past the commit point the update is durable: status trouble marks
	// needs-attention rather than failing, and the retention prune is
	// best-effort.
	status, err := verifyUpdateStatus(ctx, client, plan, rewrite, existing, onProgress)
	if err != nil {
		return nil, err
	}
	pruneUpdateBackups(ctx, e.logger, plan.stackPath, backupPath)
	return buildUpdateResult(plan, status, backupPath), nil
}

// reconfirmManagedStack re-validates, through the exclusive flock held
// over the stack manifest, that the stack is still wdm-managed and still
// names the requested app, returning the authoritative
// manifest snapshot for the manifest rewrite. The planning
// stage read the manifest under a non-blocking shared flock; the
// exclusive acquisition is the authoritative snapshot, so a manifest that
// vanished, emptied, or was replaced by a different app's stack between
// the two reads is refused here BEFORE any backup or rewrite. A nil
// snapshot means the acquisition created the lock file empty (no manifest
// on disk) — an uninstalled or concurrently-removed stack.
// The returned manifest is the basis for the durable update commit: the
// rewrite changes the template/catalog version, image pins,
// last_successful_operation, and backup_history, while every other
// field — domain, local ports, recommended resources, Compose project —
// is preserved from this snapshot (update does not re-plan them).
func reconfirmManagedStack(handle *state.StackLockHandle, appID string) (*state.StackLock, error) {
	lock := handle.Lock()
	if lock == nil {
		return nil, usageValidationError(
			"stack is no longer installed",
			"run wdm apps list to see installed apps",
			fmt.Errorf("stack lock at %q carries no manifest", handle.Path()),
		)
	}
	if lock.AppID != appID {
		return nil, usageValidationError(
			"stack directory is not managed by wdm for this app",
			"wdm only operates on stacks it installed",
			fmt.Errorf("stack lock at %q names app %q, not %q", handle.Path(), lock.AppID, appID),
		)
	}
	return lock, nil
}

// createUpdateBackup snapshots the stack's config files into
// `<stackPath>/.wdm-backups/<unix-nanos>-update/` before the rewrite
// touches a single byte (PRD §20 step 7, §21; protocol step 3).
// [state.CreateConfigBackup] copies docker-compose.yml, .env, and
// .wdm.lock automatically; the catalog candidate's additional_files and
// config_generation destinations are passed explicitly so sidecar
// artifacts and generated config files the rewrite will overwrite are
// captured too. A backup-write failure aborts the update BEFORE the
// rewrite so the user's existing files stay untouched.
// An up-to-date apply still reaches this step and still takes a backup
// a managed stack always has docker-compose.yml, .env, and
// .wdm.lock, so the snapshot is never empty.
// The returned snapshot path is the most-recent-successful pre-update
// backup once the update commits: protocol step 6 appends it to
// .wdm.lock backup_history (the commit point) and protocol step 7 pins
// it against retention eviction so the snapshot this
// run created is never the one pruned.
func createUpdateBackup(plan *updateCheckPlan, onProgress types.ProgressFn) (string, error) {
	if onProgress != nil {
		onProgress(types.StepUpdateBackup, 25, "backing up config before update")
	}

	artifactPaths := updateBackupArtifactPaths(plan.app)
	snapshotPath, err := state.CreateConfigBackup(plan.stackPath, "update", artifactPaths)
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

// updateBackupArtifactPaths returns the catalog candidate's
// additional_files and config_generation destinations as stack-relative
// paths so the backup snapshot covers every sidecar file and generated
// config artifact the rewrite may overwrite (PRD §17, §21, the
// confirmation rules). Empty destinations are skipped; the state-layer
// backup writer skips any path absent on disk, so passing the full
// declared set is safe even when an optional file was never written at
// install time.
func updateBackupArtifactPaths(app catalog.App) []string {
	total := len(app.AdditionalFiles) + len(app.ConfigGeneration)
	if total == 0 {
		return nil
	}
	paths := make([]string, 0, total)
	for _, file := range app.AdditionalFiles {
		if file.Dest == "" {
			continue
		}
		paths = append(paths, file.Dest)
	}
	for _, artifact := range app.ConfigGeneration {
		if artifact.Dest == "" {
			continue
		}
		paths = append(paths, artifact.Dest)
	}
	return paths
}

// rewriteUpdateStack resolves the placeholder map from the existing
// stack, renders the new artifacts in memory, and verifies generated and
// reused secrets do not leak into non-secret artifacts (PRD §20 step 8
// render clause; / 39). It is PURE: it touches no
// filesystem state, so every fault it returns — a missing
// `regenerable: false` secret, a render
// failure, or a non-secret leak — is pre-exposure and propagates UNCHANGED
// with its own typed code and hint. The atomic write that exposes the
// rewritten bytes is the separate [writeUpdateFiles] step the caller runs
// next, so [Engine.applyUpdate] can route the write and everything after
// it through the restore sad path without a pre-exposure refusal
// inheriting a config-restore hint.
// The resolved map is built entirely from the running stack so the
// rewrite preserves its identity: every
// value the install wrote to `.env` is reused verbatim — non-secret
// placeholders, resource-limit and UID/GID built-ins, and
// `regenerable: false` secrets — while `regenerable: true` secrets draw
// fresh `crypto/rand` entropy. Reused values were already validated at
// install time and are not re-validated here (re-validation could reject a
// path that legitimately changed on the host).
// On success the populated rewrite [installPlan] is returned so the
// deployment path can build the operation Docker client over the combined
// generated + reused secret literals (scrubbing Compose stderr of reused
// install-time secrets too), write the bytes, validate, deploy, and commit
// the manifest — all without re-resolving the stack.
func (e *Engine) rewriteUpdateStack(
	ctx context.Context,
	plan *updateCheckPlan,
	onProgress types.ProgressFn,
) (*installPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepUpdateRender, 30, "rendering updated stack")
	}

	rewrite, err := e.resolveUpdateRewritePlan(plan)
	if err != nil {
		return nil, err
	}
	// Both provenances of secret-typed literals — freshly generated and
	// reused regenerable=false — feed the redactor and the non-secret leak
	// check so a reused secret spliced into a non-secret artifact is
	// scrubbed and rejected exactly like a generated one (BLOCKING fix;
	secretLiterals := slices.Concat(rewrite.generatedValues, rewrite.reusedSecretValues)
	redactor := security.NewActiveRedactor(secretLiterals)

	input, err := e.installRenderInput(ctx, rewrite)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, redactedVerificationError(
			redactor,
			"update templates could not be loaded",
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
	// The catalog pins drive the update diff while the template image:
	// lines drive what the recreate deploys; refuse a drifted catalog on
	// the update arc exactly as install does (PRD §9, §22).
	if err := verifyImagePinsMatchTemplate(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// The bind interface is catalog-declared; refuse a public bind with no
	// backing public:true declaration (or a public declaration that drifted
	// to 127.0.0.1) on the update arc exactly as install does (PRD §11.1).
	if err := verifyPublicBindsMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// The container-privilege posture is catalog-declared against the closed
	// PRD §12.2 allow-list; refuse a declaration or a rendered cap/sysctl/
	// device/privileged escalation outside it on the update arc exactly as
	// install does.
	if err := verifyContainerPrivilegeMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// Docker API access is socket-proxy-only on an --internal network; refuse a
	// direct docker.sock bind on the update arc exactly as install does (PRD §12.1).
	if err := verifySocketPolicyMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// A host /lib/modules mount is permitted only on a service the catalog
	// declares host_module_mount:true for, and only read-only; refuse an
	// undeclared or read-write module mount on the update arc exactly as install
	// does (PRD §9, §12.2).
	if err := verifyHostModuleMountMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// Static IPs are catalog-declared per network; refuse a rendered ipv4_address
	// with no backing catalog IPAM declaration (or a declared address that
	// drifted) on the update arc exactly as install does (PRD §9).
	if err := verifyNetworkIPAMMatchCatalog(redactor, rewrite.app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	// Guidance is not produced or surfaced by the update path, so the
	// non-secret leak check covers compose plus the sidecar files only —
	// passing nil guidance.
	if err := verifyRenderedNonSecretArtifacts(redactor, secretLiterals, rewrite.rendered, nil); err != nil {
		return nil, err
	}
	// End of the update plan's producing region: secrets resolved, redactor
	// bound, stack rendered and leak-checked. Freeze so the shared write and
	// deploy helpers consume a read-only plan (issue #120).
	if err := rewrite.freeze(); err != nil {
		return nil, err
	}
	return rewrite, nil
}

// resolveUpdateRewritePlan builds the [installPlan] the shared render
// and write helpers consume, with the resolved placeholder map sourced
// from the running stack. It reads the
// existing `.env` once, then for every catalog placeholder reuses the
// recorded value (non-secret placeholders and `regenerable: false`
// secrets) or regenerates it (`regenerable: true` secrets), re-adds the
// wdm built-ins (`UID`/`GID`) freshly, and reuses every other key the
// install wrote to `.env` (resource limits and any future built-in).
// A `regenerable: false` secret absent from the existing `.env` is a hard
// refusal with the locked hint BEFORE any byte changes (,
// depends on.
func (e *Engine) resolveUpdateRewritePlan(plan *updateCheckPlan) (*installPlan, error) {
	existingEnv, err := state.ReadStackEnv(plan.stackPath)
	if err != nil {
		return nil, err
	}

	rewrite := &installPlan{
		app:            plan.app,
		stackPath:      plan.stackPath,
		composeProject: "wdm-" + plan.app.AppID,
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

	if err := reuseUpdateBuiltins(rewrite, existingEnv, declared); err != nil {
		return nil, err
	}
	return rewrite, nil
}

// resolveUpdatePlaceholder resolves one catalog placeholder for the
// rewrite. Non-secret placeholders reuse their recorded `.env` value
// verbatim. Secret placeholders split on [catalog.Placeholder.Regenerable]
// (nil means regenerable per the documented default): `regenerable: true`
// draws a fresh value from the generator; `regenerable: false` reuses the
// existing `.env` value and refuses fail-closed when it is missing
// The redactor and the non-secret leak check are driven by SECRET-TYPED
// values, not by provenance. At install "generated" and "secret-typed"
// coincide, so the install contract tracks only generated literals; at
// update they diverge — a `regenerable: false` secret is secret-typed but
// reused, not generated. So generated secrets register in generatedValues
// and reused secrets in reusedSecretValues; the rewrite combines both at
// the redactor and verifier so a reused DB password spliced into a
// non-secret artifact is scrubbed and rejected exactly like a generated
// one. Non-secret reused values (SITE_NAME, TZ, resource vars, UID/GID)
// deliberately enter neither slice — sweeping them in would scrub and
// reject the legitimate non-secret values a template echoes by design.
func (p *installPlan) resolveUpdatePlaceholder(
	ph catalog.Placeholder,
	existingEnv map[string]string,
	generate func(security.Encoding) (string, error),
) error {
	if render.Type(ph.Type) != render.TypeSecret {
		value, ok := existingEnv[ph.Name]
		if !ok {
			return usageValidationError(
				"existing .env is missing a required value",
				"restore the stack's .env or reinstall the app",
				fmt.Errorf("placeholder %q is absent from the existing .env", ph.Name),
			)
		}
		p.resolvedValues[ph.Name] = value
		// A sensitive non-secret placeholder carries user-supplied plaintext
		// that must reach the redactor and the non-secret leak check exactly
		// like a reused secret, even though it is not secret-typed.
		if ph.Sensitive && value != "" {
			p.reusedSecretValues = append(p.reusedSecretValues, value)
		}
		return nil
	}

	p.generatedFields = append(p.generatedFields, ph.Name)
	if placeholderRegenerable(ph) {
		value, err := generateUpdateSecret(ph, generate)
		if err != nil {
			return err
		}
		p.resolvedValues[ph.Name] = value
		p.generatedValues = append(p.generatedValues, value)
		return nil
	}

	value, ok := existingEnv[ph.Name]
	if !ok {
		// The Hint is the exact / the confirmation ruleslocked
		// string — keep it byte-identical so the exit-criterion grep
		// matches; the Message stays a lowercase summary.
		return usageValidationError(
			"a regenerable=false secret is missing from the existing .env",
			"regenerable=false secret missing from existing .env",
			fmt.Errorf("regenerable=false secret %q is absent from the existing .env", ph.Name),
		)
	}
	p.resolvedValues[ph.Name] = value
	// A reused regenerable=false secret is secret-typed, so it must reach
	// the redactor and the non-secret leak check even though it was not
	// produced here.
	p.reusedSecretValues = append(p.reusedSecretValues, value)
	return nil
}

// generateUpdateSecret draws a fresh value for a regenerable secret,
// validating the catalog encoding before the entropy read (mirrors
// install's [installPlan.generateInstallSecrets]). A nil generator fails
// closed.
// argon2id is a recognized encoding here (so it is not misreported as an
// invalid catalog encoding) but is refused on this path: argon2id secrets
// are assumed regenerable:false, so they are reused byte-identically from
// the existing .env by [installPlan.resolveUpdatePlaceholder] and never
// reach this regeneration arm. Regenerating one would mint a new plaintext
// that must be re-surfaced to the operator, which the update path does not
// do — so it fails closed rather than silently rotating an unrecoverable
// credential.
func generateUpdateSecret(ph catalog.Placeholder, generate func(security.Encoding) (string, error)) (string, error) {
	if generate == nil {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"secret generator is required",
			"construct the engine with a secret generator",
		)
	}
	encoding := security.Encoding(ph.Encoding)
	switch encoding {
	case security.EncodingBase64URL, security.EncodingBase64Std, security.EncodingHex:
		return generate(encoding)
	case security.EncodingArgon2id:
		return "", catalogVerificationError(
			"argon2id secrets cannot be regenerated on update",
			"mark the argon2id placeholder regenerable:false so it is reused from the existing .env",
			fmt.Errorf("placeholder %q has encoding %q with regenerable:true; argon2id assumes regenerable:false", ph.Name, ph.Encoding),
		)
	default:
		return "", catalogVerificationError(
			"catalog contains an invalid secret encoding",
			"refresh the catalog and retry",
			fmt.Errorf("placeholder %q has encoding %q", ph.Name, ph.Encoding),
		)
	}
}

// placeholderRegenerable reports whether a secret placeholder
// regenerates on update. The catalog pointer is nil-means-true per the
// documented [catalog.Placeholder.Regenerable] default (install always
// regenerates), so only an explicit false marks the secret
// fixed-after-install.
func placeholderRegenerable(ph catalog.Placeholder) bool {
	return ph.Regenerable == nil || *ph.Regenerable
}

// reuseUpdateBuiltins re-resolves the wdm built-in template values and
// reuses every remaining install-written `.env` key so the rewrite's
// resolved map exactly matches the declared placeholder set the render
// validator enforces ([render.ValidateResolution]).
// `UID`/`GID` are re-resolved freshly from the running process, matching
// install time. Every other `.env` key that is neither a catalog
// placeholder nor a built-in — resource-limit vars (`MEMORY_LIMIT_*` /
// `CPUS_LIMIT_*` / `PIDS_LIMIT_*`, the confirmation rules) and any future
// built-in — is reused verbatim and declared as a synthetic string
// placeholder so the resolved map and the declared set stay in lockstep.
// The update path deliberately does NOT re-plan resources or re-probe the
// host: a rewrite preserves the installed sizing rather than silently
// resizing the stack.
func reuseUpdateBuiltins(
	p *installPlan,
	existingEnv map[string]string,
	declared map[string]catalog.Placeholder,
) error {
	if err := p.addSyntheticResolvedValue("UID", strconv.Itoa(os.Getuid())); err != nil {
		return err
	}
	if err := p.addSyntheticResolvedValue("GID", strconv.Itoa(os.Getgid())); err != nil {
		return err
	}
	// Re-resolve the rootless Docker socket source freshly (issue #134),
	// matching install. Added before the existing-.env reuse so a stack
	// installed before this built-in existed still gets it rather than
	// failing the render on a missing key.
	if err := p.addSyntheticResolvedValue(dockerSocketSourceValueName, resolveDockerSocketSource()); err != nil {
		return err
	}

	for key, value := range existingEnv {
		if _, isPlaceholder := declared[key]; isPlaceholder {
			continue
		}
		if _, alreadyResolved := p.resolvedValues[key]; alreadyResolved {
			// UID/GID were just re-resolved freshly; the existing .env copy
			// is ignored so the running process stays authoritative.
			continue
		}
		if err := p.addSyntheticResolvedValue(key, value); err != nil {
			return err
		}
	}
	return nil
}

// writeUpdateFiles writes the rewritten artifacts atomically under the
// held per-stack flock.
// Unlike the install writer it does NOT refuse the existing stack
// directory — the rewrite is expected to overwrite a managed stack — and
// does NOT acquire the flock (the caller holds it across backup →
// rewrite). Each destination is re-validated for symlink-free ancestry
// before the write (PRD §12, §13). A write fault surfaces a typed error;
// [state.WriteFileAtomic] guarantees no file is left half-written, and the
// step 3 backup remains intact for the restore path.
func writeUpdateFiles(ctx context.Context, rewrite *installPlan, redactor security.Redactor) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	writes, err := installFileWrites(rewrite)
	if err != nil {
		return err
	}
	for _, write := range writes {
		if err := validateInstallWritePath(rewrite.stackPath, write.path); err != nil {
			return usageValidationError(
				"update file path is unsafe",
				"remove symlinks from the stack path and retry",
				err,
			)
		}
		if err := state.WriteFileAtomic(write.path, write.data, write.mode); err != nil {
			// A filesystem fault is unlikely to echo a secret, but the
			// rewrite bytes carry generated literals, so the cause is
			// scrubbed before any sink.
			return types.WrapError(
				types.ErrCodeGeneric,
				"updated stack files could not be written",
				"check stack directory permissions and retry",
				newRedactedCause(redactor, err),
			)
		}
	}
	return nil
}
