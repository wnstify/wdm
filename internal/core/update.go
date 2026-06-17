package core

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// databaseRiskClass is the PRD §20 risk tag that triggers the
// database-risk warning. It matches one member of the catalog's
// schema-validated risk_classification enum {safe, major, database,
// complex}.
const databaseRiskClass = "database"

// databaseRiskWarning is the PRD §20 database-risk warning, verbatim
// from the documented fenced block. PRD §20 requires
// this exact text — not a paraphrase — whenever a candidate update
// carries the "database" risk class, and the user must confirm it
// before any backup, rewrite, pull, or recreate runs. The wording,
// blank-line spacing, and absent trailing newline
// must stay byte-identical to the PRD fence — do not reflow, re-space,
// or soften it; TestUpdate_DatabaseRiskWarningTextIsVerbatim pins it.
const databaseRiskWarning = "This update may change the app database.\n" +
	"\n" +
	"wdm does not back up app data or databases.\n" +
	"If the app migrates its database, restoring old config later may not restore the app.\n" +
	"\n" +
	"Proceed only if you have your own backup."

// updateCheckPlan is the outcome of the PRD §20 steps 1-5 update
// check: the managed stack's recorded template state matched against
// the selected catalog channel's current entry, with per-service
// image-reference changes and the catalog-owned risk grouping resolved
// before any consequence-bearing step runs.
// app carries the resolved catalog entry forward to the apply slices so
// they do not re-load the catalog. The plan carries only the catalog
// app, never a manifest snapshot: the apply path re-reads the
// authoritative manifest through the exclusive-flock held fd
// ([reconfirmManagedStack]), so a stale planning-stage copy would only
// invite drift between the two reads.
type updateCheckPlan struct {
	appID               string
	stackPath           string
	currentVersion      string
	candidateVersion    string
	catalogChannel      string
	catalogVersion      string
	serviceChanges      []updateServiceChange
	riskClassifications []string
	updateAvailable     bool
	app                 catalog.App
}

// updateServiceChange records one service's image-reference drift
// between the stack's.wdm.lock pins and the catalog's current pins
// (PRD §20 image-pin rules). An empty currentRef marks a service the
// catalog adds; an empty candidateRef marks one the catalog no longer
// declares.
type updateServiceChange struct {
	service      string
	currentRef   string
	candidateRef string
}

// Update checks a managed stack against the selected catalog channel
// and, for apply requests, recreates services on the new template
// (PRD §20). The check stage runs PRD §20 steps 1-5: scan managed
// stacks only, read the current image pins from the stack's .wdm.lock,
// use catalog metadata as the only update source, resolve the current
// and candidate template versions, and group the candidate into the
// PRD §20 risk classes (safe / major / database / complex) from the
// catalog's schema-validated risk_classification array. Progress is emitted
// in the [types.ProgressFn] stream under [types.StepUpdatePlanning].
// Lock posture (PRD §26): Update is a
// state-changing engine entry, so the global runtime.lock is acquired
// before planning and released when Update returns. The non-mutating
// check stage reads the stack manifest through the non-blocking
// shared-flock path shared with Status — a stack mid-operation refuses
// with [types.ErrCodeRuntimeLockHeld] instead of stalling — and the
// exclusive per-stack flock of protocol step 2 engages only once a
// later slice begins mutating.
// Managed-only ordering (PRD §9, §20 step 1): the
// stack must resolve to a directory whose .wdm.lock parses and names
// req.AppID before anything else. Unmanaged directories and
// uninstalled apps refuse with [types.ErrCodeUsageValidation]; corrupt
// manifests surface wrapped [types.ErrStaleState]. The check stage
// never runs a Docker command.
// A DryRun request returns the check result — previous and candidate
// template versions, changed services, and the risk grouping — without
// touching stack files, backups, Docker, or the Confirmer (the
// [types.UpdateRequest.DryRun] contract).
// An apply request whose candidate carries the "database" risk class
// must clear the PRD §20 database-risk warning before anything
// consequential happens: immediately after planning
// and before any backup, rewrite, pull, or recreate,
// [confirmDatabaseRiskUpdate] surfaces the exact PRD §20 warning text
// through the Confirmer. A nil confirmer refuses with
// [types.ErrCodeUsageValidation] (the install posture), a decline maps
// to [types.ErrCodeUserCanceled], and a confirmer error propagates
// wrapped — none leaving an on-disk side effect, since no consequential
// step has run. A non-database update and an up-to-date no-op apply
// skip the warning.
// Past that gate, the apply path runs PRD §20 steps 7-11 under the
// per-stack `.wdm.lock` exclusive flock,
// acquired once and held across backup → rewrite → validate → confirm
// → networks → pull → recreate → manifest write → status → prune →
// release:
//   - A pre-update config snapshot is taken under `.wdm-backups/`
//     BEFORE any byte changes (step 7); the stack is re-rendered,
//     reusing `regenerable: false` secrets from the existing `.env`
//     while `regenerable: true` secrets
//     regenerate, then written atomically with `0o600` on `.env`
//     (step 8). An up-to-date apply is a no-op rewrite that still takes
//     a backup and still redeploys so the rewritten
//     files become live.
//   - The operation Docker client is built over the COMBINED generated
//     and reused secret literals so Compose stderr is scrubbed of
//     reused install-time secrets. The rewritten bytes are validated
//     via `docker compose config` against a private tempdir copy
//     (step 9), then the [types.Confirmer] authorizes the recreate
//     with the
//     image changes, ports, volumes, and backup path as the
//     consequence payload. A nil confirmer refuses with
//     [types.ErrCodeUsageValidation]; a decline maps to
//     [types.ErrCodeUserCanceled].
//   - Catalog networks are pre-created, then `docker compose pull` and
//     `docker compose up -d --force-recreate` deploy the new template
//   - Image digests are captured opportunistically and
//     the `.wdm.lock` manifest is rewritten through the held flock fd
//     as the commit point (step 6, PRD §30): new template/catalog
//     version, image pins, `last_successful_operation` kind=update, and
//     the snapshot appended to `backup_history`, with the install-time
//     identity (domain, local ports, recommended resources) preserved.
//   - Any fault after the rewrite exposed the new bytes and before the
//     commit point — validation, a confirmer decline, a network failure,
//     pull, or recreate — restores the step-3 snapshot byte-for-byte via
//     [state.RestoreConfigBackup] (config files only, never Docker-side
//     state) and surfaces a typed error: the
//     induced-failure case carries [types.ErrCodeGeneric] with a hint
//     naming the restored backup path, a decline keeps
//     [types.ErrCodeUserCanceled], and a restore that itself
//     fails fails closed by joining both causes. The
//     restore runs on the contextless restore primitive so a canceled
//     operation ctx — itself a trigger — cannot interrupt it. No
//     user-facing string ever says "rollback".
//   - Post-commit status verification fuses the install-time PRD §18
//     subset; a failed status check marks the result needs-attention
//     instead of failing the durable update, and never restores
//     A subsequent [Engine.Status] surfaces a
//     restored-but-broken stack as needs-attention through the existing
//     §18 runtime-vs-config conditions. Retention pruning
//     runs last with this run's snapshot pinned; a
//     prune failure is logged, never fatal. The structured
//     [types.UpdateResult] carries the version transition, changed
//     services, risk grouping, backup path, and status snapshot.
func (e *Engine) Update(ctx context.Context, req types.UpdateRequest, onProgress types.ProgressFn, confirmer types.Confirmer) (*types.UpdateResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "update")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planUpdateCheck(ctx, req, onProgress)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return buildUpdateCheckResult(plan), nil
	}
	if err := confirmDatabaseRiskUpdate(ctx, confirmer, plan, onProgress); err != nil {
		return nil, err
	}
	return e.applyUpdate(ctx, plan, onProgress, confirmer)
}

// planUpdateCheck runs the non-mutating update check under the held
// runtime.lock: managed-stack resolution first (PRD §20 step 1), then
// the catalog-metadata-only candidate lookup, the per-service pin diff,
// and the catalog-owned risk grouping. The
// emitted [types.StepUpdatePlanning] events carry the old → new image
// references and the check summary, so callers never parse prose for
// step identity.
func (e *Engine) planUpdateCheck(
	ctx context.Context,
	req types.UpdateRequest,
	onProgress types.ProgressFn,
) (*updateCheckPlan, error) {
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
	if onProgress != nil {
		onProgress(types.StepUpdatePlanning, 5, "planning update check")
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return nil, err
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, req.AppID)
	if err != nil {
		return nil, err
	}
	if req.TargetTemplateVersion != "" && req.TargetTemplateVersion != app.TemplateVersion {
		return nil, usageValidationError(
			"requested template version is not available in the selected catalog",
			fmt.Sprintf("the catalog currently offers template version %s", app.TemplateVersion),
			fmt.Errorf("requested template version %q, catalog offers %q", req.TargetTemplateVersion, app.TemplateVersion),
		)
	}

	plan := &updateCheckPlan{
		appID:            req.AppID,
		stackPath:        stackPath,
		currentVersion:   lock.TemplateVersion,
		candidateVersion: app.TemplateVersion,
		catalogChannel:   e.settings.CatalogChannel,
		catalogVersion:   cat.GeneratedAt.UTC().Format(time.RFC3339),
		serviceChanges:   diffUpdateServicePins(lock.ImagePins, app.ImagePins),
		app:              app,
	}
	plan.updateAvailable = plan.currentVersion != plan.candidateVersion || len(plan.serviceChanges) > 0
	if plan.updateAvailable {
		// Risk grouping is catalog-owned: the loader's schema validation
		// already constrains the
		// array to the PRD §20 closed set {safe, major, database,
		// complex} with at least one unique entry, so the values are
		// copied verbatim in catalog order. An up-to-date stack has no
		// candidate to group, so the field stays empty.
		plan.riskClassifications = append([]string(nil), app.RiskClassification...)
	}

	// Registry-derived digest VISIBILITY for the planning stream only
	// resolve the registry digest behind
	// each changed service's catalog-pinned CANDIDATE tag so the user
	// sees the actual digest the catalog tag resolves to (PRD §20
	// tag+digest visibility). This is opportunistic and never-fail — a
	// registry-unreachable run degrades to no disclosed digest and the
	// plan is unchanged (the invariant: a registry failure during
	// planning mutates nothing and does not fail the update; the confirmation rules
	// opportunistic posture). The digests feed ONLY the progress stream
	// — they are NOT stored on the plan, so the apply path's
	// confirmation, risk grouping, backups, and catalog-as-source
	// behavior are untouched, and the registry can never change which
	// image is applied. The lookup is skipped when no
	// progress sink would show it, so a DryRun-without-progress (and the
	// apply path's own re-plan) makes no network call.
	// PRE-LOOKUP DISCLOSURE (the invariant, "no silent
	// network work"): planUpdateCheck runs for BOTH the dry-run check and
	// the real apply, so this block performs a registry round-trip during
	// `wdm apps update <app>` apply too. Emit a disclosure event naming the
	// network action BEFORE the lookup — on both check and apply, plain
	// and JSON, whether or not the registry turns out reachable —
	// mirroring the engine-layer pre-action disclosure precedent
	// ([types.StepCatalogUpdateDownload], [types.StepSelfUpdateDownload]).
	if onProgress != nil && len(plan.serviceChanges) > 0 {
		onProgress(types.StepUpdatePlanning, 8, "checking the registry for image digests")
		registryDigests := e.resolveRegistryDigests(ctx, plan.serviceChanges)
		reportUpdateCheckWithRegistry(plan, registryDigests, onProgress)
		return plan, nil
	}
	reportUpdateCheck(plan, onProgress)
	return plan, nil
}

// diffUpdateServicePins compares the stack's recorded image pins
// against the catalog's current pins service by service and returns the
// changed set sorted by service name (PRD §20 step 2; the .wdm.lock
// pins are the configured-tag record the update path rewrites in
// lockstep with docker-compose.yml at the later rewrite slice).
// Opportunistically captured digests are ignored here — the configured
// tag is the update-candidate surface per, and a
// digest-only refresh is not a tag change. Duplicate service entries
// keep their first occurrence, mirroring the Status path's pin
// handling.
func diffUpdateServicePins(current []state.ImagePin, candidate []catalog.ImagePin) []updateServiceChange {
	currentRefs := map[string]string{}
	for _, pin := range current {
		if pin.Service == "" {
			continue
		}
		if _, ok := currentRefs[pin.Service]; ok {
			continue
		}
		currentRefs[pin.Service] = updateImageRef(pin.Image, pin.Tag)
	}
	candidateRefs := map[string]string{}
	for _, pin := range candidate {
		if pin.Service == "" {
			continue
		}
		if _, ok := candidateRefs[pin.Service]; ok {
			continue
		}
		candidateRefs[pin.Service] = updateImageRef(pin.Image, pin.Tag)
	}

	services := make([]string, 0, len(currentRefs)+len(candidateRefs))
	for service := range currentRefs {
		services = append(services, service)
	}
	for service := range candidateRefs {
		if _, ok := currentRefs[service]; !ok {
			services = append(services, service)
		}
	}
	sort.Strings(services)

	var changes []updateServiceChange
	for _, service := range services {
		currentRef := currentRefs[service]
		candidateRef := candidateRefs[service]
		if currentRef == candidateRef {
			continue
		}
		changes = append(changes, updateServiceChange{
			service:      service,
			currentRef:   currentRef,
			candidateRef: candidateRef,
		})
	}
	return changes
}

// updateImageRef renders an image pin as the image[:tag] reference used
// for the old → new comparison and progress surface. Tag-less pins
// (digest-only lock entries per PRD §22) compare by image name alone.
func updateImageRef(image, tag string) string {
	if tag == "" {
		return image
	}
	return image + ":" + tag
}

// reportUpdateCheck emits the planning outcome on the progress stream:
// one [types.StepUpdatePlanning] event per changed service naming the
// old → new image references, followed by one summary event
// carrying the version transition and risk grouping, or the up-to-date
// no-op outcome. Catalog metadata carries no secrets, so the messages
// are sink-safe.
func reportUpdateCheck(plan *updateCheckPlan, onProgress types.ProgressFn) {
	if onProgress == nil {
		return
	}
	for _, change := range plan.serviceChanges {
		onProgress(types.StepUpdatePlanning, 10, updateServiceChangeMessage(change))
	}
	onProgress(types.StepUpdatePlanning, 15, updateCheckSummaryMessage(plan))
}

// reportUpdateCheckWithRegistry is reportUpdateCheck enriched with the
// opportunistic registry-digest VISIBILITY for the planning stream
// It emits the same per-service old → new event and the
// same summary event as reportUpdateCheck — byte-identical when the
// registry disclosed no digest for a service — and, when a service's
// candidate (catalog-pinned) tag resolved to a registry digest, emits
// one ADDITIONAL [types.StepUpdatePlanning] event disclosing that digest
// (PRD §20 tag+digest visibility). The disclosure carries only the
// catalog-derived candidate ref and the public registry digest, so it
// is sink-safe (no secrets). The registry never changes the applied
// image — these are visibility-only events on top of the unchanged
// catalog-driven plan. registryDigests maps service name
// -> registry digest for the services that resolved cleanly; a service
// absent from the map (removed, digest-only, or registry-unreachable)
// gets no extra event, so the stream is unchanged.
func reportUpdateCheckWithRegistry(
	plan *updateCheckPlan,
	registryDigests map[string]string,
	onProgress types.ProgressFn,
) {
	if onProgress == nil {
		return
	}
	for _, change := range plan.serviceChanges {
		onProgress(types.StepUpdatePlanning, 10, updateServiceChangeMessage(change))
		if digest := registryDigests[change.service]; digest != "" {
			onProgress(types.StepUpdatePlanning, 10, updateRegistryDigestMessage(change, digest))
		}
	}
	onProgress(types.StepUpdatePlanning, 15, updateCheckSummaryMessage(plan))
}

// updateRegistryDigestMessage renders the registry-digest disclosure
// line for one changed service: the catalog-pinned candidate ref and the
// canonical registry digest it currently resolves to (PRD §20 tag+digest
// visibility). It is called only for a service the registry resolved, so
// both the ref and digest are non-empty.
func updateRegistryDigestMessage(change updateServiceChange, registryDigest string) string {
	return fmt.Sprintf(
		"service %s: registry digest for %s is %s",
		change.service,
		change.candidateRef,
		registryDigest,
	)
}

func updateServiceChangeMessage(change updateServiceChange) string {
	switch {
	case change.currentRef == "":
		return fmt.Sprintf("service %s: new service %s", change.service, change.candidateRef)
	case change.candidateRef == "":
		return fmt.Sprintf("service %s: removed from template (was %s)", change.service, change.currentRef)
	default:
		return fmt.Sprintf("service %s: %s -> %s", change.service, change.currentRef, change.candidateRef)
	}
}

func updateCheckSummaryMessage(plan *updateCheckPlan) string {
	if !plan.updateAvailable {
		return fmt.Sprintf("already up to date at template version %s", plan.currentVersion)
	}
	message := fmt.Sprintf(
		"update available: template version %s -> %s",
		plan.currentVersion,
		plan.candidateVersion,
	)
	if len(plan.riskClassifications) > 0 {
		message += " (risk: " + strings.Join(plan.riskClassifications, ", ") + ")"
	}
	return message
}

// buildUpdateCheckResult projects the check plan into the
// [types.UpdateResult] shape for DryRun callers (PRD §20 steps 4-5):
// previous and candidate template versions, the sorted changed
// services, and the catalog risk grouping. BackupPath and Status stay
// unset — a check takes no backup and runs no deployment.
func buildUpdateCheckResult(plan *updateCheckPlan) *types.UpdateResult {
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
	}
}

// confirmDatabaseRiskUpdate gates an apply request on the PRD §20
// database-risk warning when the candidate update carries the
// "database" risk class. It is the apply path's first consequential
// checkpoint and runs immediately after planning, before any backup,
// rewrite, pull, or recreate. Mirrors install's
// [confirmInstallDeployment] posture: a nil
// confirmer refuses with [types.ErrCodeUsageValidation] per the
// pkg/engine contract, a decline maps to [types.ErrCodeUserCanceled],
// and a confirmer error propagates wrapped.
// No-op-apply decision (PRD §20): gates
// the warning on "any update with risk_classification including
// database", and PRD §20 ties the stronger warning to "database-risk
// updates" — an actual update that may migrate the database. An
// up-to-date apply (no template version change, no image-pin change) is
// not such an update: plan.riskClassifications stays empty for an
// up-to-date stack (plan.updateAvailable is false), so a no-op carries
// no "database" class and skips the warning, then proceeds through the
// normal apply path (backup → rewrite → validate → confirm → deploy →
// commit) with no version change. Gating on plan.updateAvailable makes
// that explicit and independent of the slice that owns the no-op
// backup. The warning text — "This update may change the app database"
// — would be false on a no-op, which confirms the gate.
func confirmDatabaseRiskUpdate(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *updateCheckPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !plan.updateAvailable || !planHasDatabaseRisk(plan) {
		return nil
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required for a database-risk update",
			"pass a confirmer that can authorize the database-risk warning",
		)
	}
	if onProgress != nil {
		onProgress(types.StepUpdateConfirm, 20, "confirming database-risk update")
	}

	confirmed, err := confirmer.Confirm(ctx, databaseRiskConfirmation(plan))
	if err != nil {
		return fmt.Errorf("core.update: confirming database-risk update: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"update canceled at the database-risk warning",
			"re-run the update and confirm the database-risk prompt only if you have your own backup",
		)
	}
	return nil
}

// planHasDatabaseRisk reports whether the candidate update's catalog
// risk grouping includes the "database" class. The slice is the
// schema-validated catalog risk_classification copied verbatim in
// [Engine.planUpdateCheck], so a linear scan over the closed enum
// suffices.
func planHasDatabaseRisk(plan *updateCheckPlan) bool {
	return slices.Contains(plan.riskClassifications, databaseRiskClass)
}

// databaseRiskConfirmation assembles the [types.Confirmation] payload
// for the database-risk warning. Message is the exact PRD §20 text
// (databaseRiskWarning) so the user sees it verbatim — the app identity
// and version transition ride in Title and Kind, never spliced into the
// warning body, so the text stays character-identical to the PRD. The
// payload carries no secret values.
func databaseRiskConfirmation(plan *updateCheckPlan) types.Confirmation {
	return types.Confirmation{
		Kind: "update_database_risk",
		Title: fmt.Sprintf(
			"database-risk update for %s: template version %s -> %s",
			plan.appID,
			plan.currentVersion,
			plan.candidateVersion,
		),
		Message: databaseRiskWarning,
	}
}
