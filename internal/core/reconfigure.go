package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// reconfigurePlan is the resolved outcome of reconfigure planning: the
// managed stack, its catalog entry, the targeted resource profile, and
// the validated new resource limit values to write into the stack's
// .env. The plan carries the existing manifest so the apply path can
// preserve every install-time identity field (domain, ports, image
// pins, recommended resources) verbatim — a reconfigure changes only
// the targeted resource vars.
type reconfigurePlan struct {
	appID          string
	service        string
	stackPath      string
	composeProject string
	app            catalog.App

	// memory, cpus, pids are the resource limit values that will be in
	// effect after the reconfigure: the requested value where the request
	// set one, otherwise the value already recorded in the stack's .env.
	memory string
	cpus   string
	pids   int
}

// Reconfigure changes one managed service's resource limits (memory,
// CPUs, PIDs) after install (issue #28). It mirrors [Engine.Update]'s
// state-changing posture: the global runtime.lock is held for the whole
// operation, the per-stack exclusive flock spans backup → rewrite →
// validate → confirm → recreate → manifest commit, a pre-change config
// backup is taken BEFORE any byte changes, and a fault after the rewrite
// exposed the new bytes and before the commit point restores the
// snapshot byte-for-byte.
// Unlike Update it does not re-check the catalog for a newer template,
// diff image pins, or pull images: the installed template version stays
// put and only the resource vars in .env change. The requested values
// are validated against the catalog's resource bands (min/max,
// allow_override) through the SAME validation install uses, so an
// out-of-band value or a service the catalog forbids overriding is
// refused fail-closed before any change.
func (e *Engine) Reconfigure(
	ctx context.Context,
	req types.ReconfigureRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.ReconfigureResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	// Uniform §24 start/result lines; deep per-step instrumentation is
	// deferred to install. Structural redaction guards the sink.
	lg := e.newOpLogger(e.logger, "reconfigure")
	lg.start(ctx, req.AppID)

	handle, err := e.acquireRuntimeLock(ctx, "reconfigure")
	if err != nil {
		lg.failure(ctx, req.AppID, "", "acquire_runtime_lock", err)
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	plan, err := e.planReconfigure(ctx, req, onProgress)
	if err != nil {
		lg.failure(ctx, req.AppID, "", "plan_reconfigure", err)
		return nil, err
	}
	res, err := e.applyReconfigure(ctx, plan, onProgress, confirmer)
	if err != nil {
		lg.failure(ctx, req.AppID, "", "apply_reconfigure", err)
		return nil, err
	}
	lg.success(ctx, req.AppID, "")
	return res, nil
}

// planReconfigure runs the non-mutating reconfigure planning under the
// held runtime.lock: managed-stack resolution first, the fail-closed
// StackPath cross-check, the catalog-entry lookup, the
// resource-profile + band validation of the requested values (reusing
// install's [applyResourceLimitOverride] and the allow_override gate),
// and the merge of requested vs installed resource values. Planning
// makes no Docker call and writes nothing.
func (e *Engine) planReconfigure(
	ctx context.Context,
	req types.ReconfigureRequest,
	onProgress types.ProgressFn,
) (*reconfigurePlan, error) {
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
	if req.Service == "" {
		return nil, usageValidationError(
			"service is required",
			"pass the service whose resource limits should change",
			nil,
		)
	}
	if req.Memory == nil && req.CPUs == nil && req.PIDs == nil {
		return nil, usageValidationError(
			"no resource limits were requested",
			"pass at least one of memory, cpus, or pids to change",
			nil,
		)
	}
	if onProgress != nil {
		onProgress(types.StepReconfigurePlanning, 5, "planning reconfigure")
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return nil, err
	}
	if req.StackPath != "" && filepath.Clean(req.StackPath) != stackPath {
		return nil, usageValidationError(
			"stack path does not match the managed stack for this app",
			fmt.Sprintf("the managed stack for %q is at %s", req.AppID, stackPath),
			nil,
		)
	}
	if lock.ComposeProject == "" {
		return nil, usageValidationError(
			"stack manifest is missing its compose project",
			"the .wdm.lock is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("stack lock for %q records no compose project", req.AppID),
		)
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, req.AppID)
	if err != nil {
		return nil, err
	}

	plan, err := buildReconfigurePlan(req, app, stackPath, lock.ComposeProject)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepReconfigurePlanning, 15, reconfigurePlanMessage(plan))
	}
	return plan, nil
}

// buildReconfigurePlan validates the requested limits against the
// targeted service's catalog band and merges them with the values
// already recorded in the stack's .env. It REUSES install's band
// validation: the allow_override gate and the min/max checks come from
// [applyResourceLimitOverride] applied to a [selectedResource] seeded
// from the existing .env, so the reconfigure path never duplicates band
// logic. A service the catalog declares no resource band for, or one
// whose band forbids overrides, is refused fail-closed.
func buildReconfigurePlan(
	req types.ReconfigureRequest,
	app catalog.App,
	stackPath string,
	composeProject string,
) (*reconfigurePlan, error) {
	profiles, err := indexResourceProfiles(app.Resources)
	if err != nil {
		return nil, err
	}
	profile, ok := profiles[req.Service]
	if !ok {
		return nil, usageValidationError(
			"service does not declare resource limits",
			"choose a service the app sizes in the catalog",
			fmt.Errorf("app %q declares no resource band for service %q", req.AppID, req.Service),
		)
	}
	if !profile.AllowOverride {
		return nil, usageValidationError(
			"resource limits are not adjustable for this service",
			"this service's resource limits are fixed by the catalog",
			fmt.Errorf("service %q disallows overrides", req.Service),
		)
	}

	// A non-nil pointer is an explicit set that must carry a usable value.
	// applyResourceLimitOverride reuses install's empty-string/zero
	// sentinels to mean "leave unchanged", so a set-but-empty memory/cpus
	// or a sub-1 pids would be silently dropped. Reject those here, before
	// any backup or mutation, so the engine API is safe for every caller.
	if req.Memory != nil && strings.TrimSpace(*req.Memory) == "" {
		return nil, usageValidationError(
			fmt.Sprintf("memory limit must be between %s and %s", profile.Memory.Min, profile.Memory.Max),
			fmt.Sprintf("pass a Docker memory value between %s and %s for %s", profile.Memory.Min, profile.Memory.Max, req.Service),
			fmt.Errorf("service %q memory override is empty", req.Service),
		)
	}
	if req.CPUs != nil && strings.TrimSpace(*req.CPUs) == "" {
		return nil, usageValidationError(
			fmt.Sprintf("cpus limit must be between %s and %s", profile.CPUs.Min, profile.CPUs.Max),
			fmt.Sprintf("pass a cpu quota between %s and %s for %s", profile.CPUs.Min, profile.CPUs.Max, req.Service),
			fmt.Errorf("service %q cpus override is empty", req.Service),
		)
	}
	if req.PIDs != nil && *req.PIDs < 1 {
		return nil, usageValidationError(
			fmt.Sprintf("pids limit must be between 1 and %d", profile.PIDs.Max),
			fmt.Sprintf("choose a pids value between 1 and %d for %s", profile.PIDs.Max, req.Service),
			fmt.Errorf("service %q pids override %d is below 1", req.Service, *req.PIDs),
		)
	}

	current, err := readServiceResourceValues(stackPath, req.Service)
	if err != nil {
		return nil, err
	}

	override := types.ResourceOverride{Service: req.Service}
	if req.Memory != nil {
		override.Memory = *req.Memory
	}
	if req.CPUs != nil {
		override.CPUs = *req.CPUs
	}
	if req.PIDs != nil {
		override.PIDs = *req.PIDs
	}

	merged, err := applyResourceLimitOverride(current, profile, override)
	if err != nil {
		return nil, err
	}

	return &reconfigurePlan{
		appID:          req.AppID,
		service:        req.Service,
		stackPath:      stackPath,
		composeProject: composeProject,
		app:            app,
		memory:         merged.memory,
		cpus:           merged.cpus,
		pids:           merged.pids,
	}, nil
}

// readServiceResourceValues reads the targeted service's installed
// resource limits from the stack's .env (MEMORY_LIMIT_<KEY> /
// CPUS_LIMIT_<KEY> / PIDS_LIMIT_<KEY>). The values seed the override
// merge so a request that changes only one limit leaves the other two
// at their installed values. A missing or non-integer PIDS_LIMIT is a
// corrupt-stack refusal rather than a silent default.
func readServiceResourceValues(stackPath, service string) (selectedResource, error) {
	env, err := state.ReadStackEnv(stackPath)
	if err != nil {
		return selectedResource{}, err
	}

	key := serviceKey(service)
	memory, ok := env["MEMORY_LIMIT_"+key]
	if !ok {
		return selectedResource{}, usageValidationError(
			"stack .env is missing the service memory limit",
			"the stack .env is incomplete; reinstall the app to restore managed state",
			fmt.Errorf("MEMORY_LIMIT_%s is absent from the stack .env", key),
		)
	}
	cpus, ok := env["CPUS_LIMIT_"+key]
	if !ok {
		return selectedResource{}, usageValidationError(
			"stack .env is missing the service cpu limit",
			"the stack .env is incomplete; reinstall the app to restore managed state",
			fmt.Errorf("CPUS_LIMIT_%s is absent from the stack .env", key),
		)
	}
	pidsText, ok := env["PIDS_LIMIT_"+key]
	if !ok {
		return selectedResource{}, usageValidationError(
			"stack .env is missing the service pids limit",
			"the stack .env is incomplete; reinstall the app to restore managed state",
			fmt.Errorf("PIDS_LIMIT_%s is absent from the stack .env", key),
		)
	}
	pids, err := strconv.Atoi(strings.TrimSpace(pidsText))
	if err != nil {
		return selectedResource{}, usageValidationError(
			"stack .env has an invalid pids limit",
			"the stack .env is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("PIDS_LIMIT_%s value %q is not an integer", key, pidsText),
		)
	}
	return selectedResource{memory: memory, cpus: cpus, pids: pids}, nil
}

// ResourceSettings reports a managed app's per-service resource limits:
// the values currently in effect (read from the stack's .env) and the
// catalog's allowed bands. Read-only — it acquires no runtime.lock and
// runs no Docker command, mirroring [Engine.Status]. A service whose
// .env resource var is absent reports empty current values rather than
// failing, so a partially-installed stack still renders its bands.
func (e *Engine) ResourceSettings(ctx context.Context, appID string) (*types.ResourceSettings, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, appID)
	if err != nil {
		return nil, err
	}

	env, err := state.ReadStackEnv(stackPath)
	if err != nil {
		return nil, err
	}

	settings := &types.ResourceSettings{
		AppID:    appID,
		Services: make([]types.ResourceServiceSettings, 0, len(app.Resources)),
	}
	for _, profile := range app.Resources {
		settings.Services = append(settings.Services, resourceServiceSettings(profile, env))
	}
	return settings, nil
}

// resourceServiceSettings projects one catalog resource band plus the
// matching current .env values into the read-only view. Absent .env
// values render empty rather than erroring — the view is informational.
func resourceServiceSettings(profile catalog.ResourceProfile, env map[string]string) types.ResourceServiceSettings {
	key := serviceKey(profile.Service)
	entry := types.ResourceServiceSettings{
		Service:           profile.Service,
		Adjustable:        profile.AllowOverride,
		CurrentMemory:     env["MEMORY_LIMIT_"+key],
		CurrentCPUs:       env["CPUS_LIMIT_"+key],
		MemoryMin:         profile.Memory.Min,
		MemoryRecommended: profile.Memory.Recommended,
		MemoryMax:         profile.Memory.Max,
		CPUsMin:           profile.CPUs.Min,
		CPUsRecommended:   profile.CPUs.Recommended,
		CPUsMax:           profile.CPUs.Max,
		PIDsDefault:       profile.PIDs.Default,
		PIDsMax:           profile.PIDs.Max,
	}
	if pidsText, ok := env["PIDS_LIMIT_"+key]; ok {
		if pids, convErr := strconv.Atoi(strings.TrimSpace(pidsText)); convErr == nil {
			entry.CurrentPIDs = pids
		}
	}
	return entry
}

// reconfigurePlanMessage renders the planning outcome for the progress
// stream: the app, service, and the resource limits that will be in
// effect. The values are catalog-shaped sizing strings, never secrets,
// so the message is sink-safe.
func reconfigurePlanMessage(plan *reconfigurePlan) string {
	return fmt.Sprintf(
		"reconfigure planned for %s service %s: memory=%s cpus=%s pids=%d",
		plan.appID,
		plan.service,
		plan.memory,
		plan.cpus,
		plan.pids,
	)
}
