package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// Uninstall tears down every managed stack and then removes wdm's own
// on-disk footprint, including the running binary (PRD §39, issue #29). It
// is destructive and irreversible for the wdm installation, so it is gated
// like the destructive deletion of §19 — never like the safe remove.
// Scope and data preservation (PRD §39): teardown is wdm-managed scope ONLY.
// For each managed stack discovered under the configured stack base (the
// same enumeration List/StopAll use) Uninstall runs `docker compose down
// --rmi all` (NEVER -v): containers, the project's default network, and the
// stack's images are removed, but all named volumes and every ~/docker/<app>/
// stack directory are KEPT. After every stack is down, the wdm-created Docker
// networks (declared external in the rendered compose, so compose never removes
// them) are dropped best-effort so "docker is clean"; a follow-up label-based
// sweep then drops every remaining wdm.managed=true network, including ones
// orphaned by an app the operator previously deleted. A network that cannot be
// removed is reported, never blocking footprint removal. Self-uninstall never
// deletes user data.
// Lock posture (PRD §26): Uninstall is a state-changing engine entry, so the
// global runtime.lock is acquired ONCE at entry — attributed "uninstall" —
// and held for the whole batch.
// Confirmation (PRD §37, §39): a single destructive confirmation gates the
// whole operation immediately before any teardown, with a payload naming the
// managed stacks, the kept data paths, and the footprint that will be
// removed. A nil confirmer refuses with [types.ErrCodeUsageValidation]; a
// decline maps to [types.ErrCodeUserCanceled] with zero side effects; a
// confirmer error propagates wrapped.
// Fail-closed teardown (PRD §39): teardown is all-or-nothing for the
// footprint removal. Every managed stack is attempted and per-stack failures
// are collected. If ANY stack fails, Uninstall ABORTS before removing any
// footprint — wdm stays installed, the runtime lock and state survive, and
// the result lists the failed stacks. Only when EVERY stack tears down
// cleanly does footprint removal proceed.
// Process exit (PRD §11, §28, §39): the core NEVER calls os.Exit. Uninstall
// returns a structured [types.UninstallResult]; the CLI/TUI layer handles
// process exit after the call returns. On Linux the running binary's open
// inode survives self-delete until process exit, so removing it here is safe.
func (e *Engine) Uninstall(
	ctx context.Context,
	_ types.UninstallRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.UninstallResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "uninstall")
	if err != nil {
		return nil, err
	}
	// The runtime lock is released explicitly during footprint removal (the
	// state dir holding it is removed there); this defer is the abort-path
	// and error-path backstop. Releasing twice is safe.
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	// The teardown reads no secrets and renders nothing, so the Docker client
	// carries the structural redactor only (mirrors the delete path).
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	apps, err := e.planUninstall(ctx, onProgress)
	if err != nil {
		return nil, err
	}

	if err := confirmUninstall(ctx, confirmer, apps, e.footprintPaths(), onProgress); err != nil {
		return nil, err
	}

	// Pre-flight the footprint removal targets BEFORE any teardown so an
	// out-of-root or symlink-escaping target is refused atomically up front
	// (PRD §39). Without this, a target refused mid-run would leave stacks
	// already torn down and earlier footprint dirs already removed — a partial
	// footprint. The per-dir guards in removeFootprint stay as defense-in-depth.
	if err := e.preflightFootprint(); err != nil {
		return nil, err
	}

	tornDown, failed, networks := e.teardownAllStacks(ctx, client, apps, onProgress)

	keptDataPaths := uninstallKeptDataPaths(apps)

	// Fail-closed: any teardown failure aborts before any footprint removal.
	// wdm stays installed; the result lists what failed.
	if len(failed) > 0 {
		return &types.UninstallResult{
			TornDown:      tornDown,
			Failed:        failed,
			KeptDataPaths: keptDataPaths,
		}, nil
	}

	// All stacks are down (no endpoints attached), so the wdm-created networks
	// can be dropped. This is best-effort: it never aborts and never blocks
	// footprint removal (PRD §39).
	removedNetworks, retainedNetworks := e.removeManagedNetworks(ctx, client, networks, onProgress)

	// Sweep every remaining wdm.managed=true network, including ones orphaned by
	// an app the operator previously deleted (its compose file is gone, so the
	// compose-derived discovery above can no longer find them). The sweep dedups
	// against the names already removed, so a network dropped via the compose
	// path is not listed twice. Like the step above it is best-effort and never
	// aborts the uninstall (PRD §39).
	removedNetworks, retainedNetworks = e.sweepManagedNetworks(
		ctx,
		client,
		removedNetworks,
		retainedNetworks,
		onProgress,
	)

	removed, err := e.removeFootprint(ctx, handle, onProgress)
	if err != nil {
		return nil, err
	}

	return &types.UninstallResult{
		TornDown:         tornDown,
		KeptDataPaths:    keptDataPaths,
		RemovedPaths:     removed,
		RemovedNetworks:  removedNetworks,
		RetainedNetworks: retainedNetworks,
	}, nil
}

// planUninstall enumerates the managed stacks the teardown will target,
// reusing [Engine.List] so corrupt manifests are logged and excluded exactly
// as List does. An empty managed set is not an error: Uninstall still removes
// the wdm footprint after a no-op teardown.
func (e *Engine) planUninstall(ctx context.Context, onProgress types.ProgressFn) ([]types.AppInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepUninstallPlanning, 5, "finding managed apps to tear down")
	}

	apps, err := e.List(ctx)
	if err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(types.StepUninstallPlanning, 15, fmt.Sprintf(
			"uninstall planned: %d managed app(s) will be torn down; wdm footprint will be removed",
			len(apps),
		))
	}
	return apps, nil
}

// confirmUninstall asks the Confirmer to authorize the self-uninstall once,
// immediately before any teardown. A nil confirmer refuses with
// [types.ErrCodeUsageValidation] per the pkg/engine contract, a decline maps
// to [types.ErrCodeUserCanceled], and a confirmer error propagates wrapped.
// The confirm runs before any Docker mutation or filesystem removal, so a
// decline leaves wdm and every stack exactly as they were.
func confirmUninstall(
	ctx context.Context,
	confirmer types.Confirmer,
	apps []types.AppInfo,
	footprint []string,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before uninstalling wdm",
			"pass a confirmer that can authorize the destructive self-uninstall",
		)
	}
	if onProgress != nil {
		onProgress(types.StepUninstallConfirm, 25, "confirming self-uninstall")
	}

	confirmed, err := confirmer.Confirm(ctx, uninstallConfirmation(apps, footprint))
	if err != nil {
		return fmt.Errorf("core.uninstall: confirming uninstall: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"uninstall canceled before any teardown or removal",
			"re-run the uninstall and confirm the prompt",
		)
	}
	return nil
}

// uninstallConfirmation assembles the destructive consequence payload (PRD
// §39): the explicit statement that this tears down every managed stack and
// removes wdm itself, the managed stacks that will be torn down, the data
// that will be KEPT (named volumes and per-app stack directories), and the
// wdm footprint that will be removed. The payload carries no secret values
// (app ids and paths only), so it is sink-safe by construction.
func uninstallConfirmation(apps []types.AppInfo, footprint []string) types.Confirmation {
	lines := []string{
		fmt.Sprintf(
			"WARNING: this PERMANENTLY uninstalls wdm and tears down %d managed app(s) — it cannot be undone",
			len(apps),
		),
		"each app is torn down with docker compose down --rmi all (containers + images); no -v, no data loss",
		"wdm-created networks are removed after teardown; all named volumes and stack data are KEPT",
	}
	for _, app := range apps {
		lines = append(lines, "tears down app "+app.AppID)
	}
	lines = append(lines, "KEPT: all named volumes and every managed stack directory under ~/docker/<app>/")
	for _, app := range apps {
		lines = append(lines, "keeps data for "+app.AppID+" at "+app.StackPath)
	}
	lines = append(lines, "removes the wdm footprint:")
	for _, path := range footprint {
		lines = append(lines, "removes "+path)
	}
	return types.Confirmation{
		Kind:    types.ConfirmationKindUninstallDestructive,
		Title:   "uninstall wdm",
		Message: strings.Join(lines, "\n"),
	}
}

// teardownAllStacks runs `docker compose down --rmi all` for every managed
// app under the runtime.lock already held by [Engine.Uninstall]. Every stack
// is attempted; per-stack failures are collected into the failed slice so the
// caller can apply the fail-closed abort. Context cancellation is the only
// whole-operation abort: it stops the loop and records the current plus every
// remaining unprocessed app in failed, so the caller treats the canceled run
// as a failure (footprint untouched) and reports the full not-torn-down set.
func (e *Engine) teardownAllStacks(
	ctx context.Context,
	client docker.Client,
	apps []types.AppInfo,
	onProgress types.ProgressFn,
) (tornDown, failed []types.TornDownApp, externalNetworks []string) {
	tornDown = []types.TornDownApp{}
	failed = []types.TornDownApp{}
	seenNetworks := map[string]struct{}{}

	total := len(apps)
	for i, app := range apps {
		if err := ctx.Err(); err != nil {
			// Cancellation aborts the batch: record the current app plus every
			// remaining unprocessed app so the result reports the full set of
			// stacks that were not torn down. A non-empty failed slice keeps the
			// caller's fail-closed footprint skip intact.
			for _, remaining := range apps[i:] {
				failed = append(failed, types.TornDownApp{
					AppID: remaining.AppID,
					Error: err.Error(),
				})
			}
			return tornDown, failed, externalNetworks
		}
		if onProgress != nil {
			onProgress(types.StepUninstallTeardown, uninstallTeardownPct(i, total), fmt.Sprintf(
				"tearing down %s (%d/%d)",
				app.AppID,
				i+1,
				total,
			))
		}

		outcome, networks := e.teardownOneStack(ctx, client, app.AppID)
		if outcome.Error == "" {
			tornDown = append(tornDown, outcome)
			// Dedup the wdm-created networks across stacks: a shared external
			// network must be removed exactly once.
			for _, name := range networks {
				if _, seen := seenNetworks[name]; seen {
					continue
				}
				seenNetworks[name] = struct{}{}
				externalNetworks = append(externalNetworks, name)
			}
		} else {
			failed = append(failed, outcome)
		}
	}

	return tornDown, failed, externalNetworks
}

// teardownOneStack tears down a single managed stack: it takes the exclusive
// per-stack flock, reconfirms the manifest names appID and carries a Compose
// project, then runs `docker compose down --rmi all` (NEVER -v). Every
// failure short of a panic is folded into the returned [types.TornDownApp] so
// the batch continues, mirroring stopOneStack.
// The second return value lists the wdm-created (external) networks read from
// this stack's preserved rendered compose; it is non-empty only when the stack
// tore down cleanly. The names feed the best-effort network cleanup the caller
// runs after every stack is down.
func (e *Engine) teardownOneStack(
	ctx context.Context,
	client docker.Client,
	appID string,
) (types.TornDownApp, []string) {
	outcome := types.TornDownApp{AppID: appID}

	stackPath, err := security.SafeJoin(e.stackBase, appID)
	if err != nil {
		outcome.Error = fmt.Sprintf("app id is unsafe: %v", err)
		return outcome, nil
	}

	handle, err := acquireInstallStackLock(ctx, stackPath)
	if err != nil {
		outcome.Error = err.Error()
		return outcome, nil
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	lock, err := reconfirmManagedStack(handle, appID)
	if err != nil {
		outcome.Error = err.Error()
		return outcome, nil
	}
	if lock.ComposeProject == "" {
		outcome.Error = "stack manifest is missing its compose project"
		return outcome, nil
	}
	outcome.ComposeProject = lock.ComposeProject

	project, err := uninstallComposeProject(stackPath, lock)
	if err != nil {
		outcome.Error = err.Error()
		return outcome, nil
	}

	if err := docker.ComposeDownRemoveImages(ctx, client, project); err != nil {
		outcome.Error = err.Error()
		return outcome, nil
	}

	// Read the wdm-created networks from the rendered compose only after the
	// stack tore down cleanly. A read/parse failure is not fatal — the cleanup
	// is best-effort — so the networks are simply dropped from the set.
	networks := readExternalNetworkNames(project.ComposeFile)
	return outcome, networks
}

// uninstallComposeProject builds the validated [docker.ComposeProject] for
// one stack's teardown from the resolved stack path and the manifest's
// Compose project name, mirroring stopAllComposeProject (PRD §12, §13).
func uninstallComposeProject(stackPath string, lock *state.StackLock) (docker.ComposeProject, error) {
	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(stackPath, installEnvFilename)
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
		ProjectName: lock.ComposeProject,
	}, nil
}

// uninstallKeptDataPaths reports the per-app stack directories self-uninstall
// preserves (PRD §39 — user data is never deleted). Named volumes are also
// kept but are Docker objects, reported through the confirmation payload, not
// here.
func uninstallKeptDataPaths(apps []types.AppInfo) []string {
	if len(apps) == 0 {
		return nil
	}
	paths := make([]string, 0, len(apps))
	for _, app := range apps {
		paths = append(paths, app.StackPath)
	}
	return paths
}

// footprintPaths returns wdm's on-disk footprint directories in the PRD §39
// removal order: config dir, data/share dir, then the state dir LAST (it holds
// the runtime lock still in use until removal). The running binary and its
// .previous sibling are NOT included — they are resolved and removed last of
// all by removeFootprint through the executable-path seam.
func (e *Engine) footprintPaths() []string {
	return []string{
		filepath.Dir(e.configPath),
		e.dataDir,
		e.stateDir,
	}
}

// removeFootprint removes wdm's on-disk footprint after every managed stack
// has torn down cleanly (PRD §39). Order is load-bearing: the config dir and
// the data/share dir go first; the runtime lock is released and the state dir
// removed next (the lock is in use until this point); then the running binary
// and its .previous rollback sibling are self-deleted LAST of all. Each
// directory is resolved symlink-safe and refused if it escapes the user home
// or resolves to a suspiciously shallow path. The binary is resolved through
// the same os.Executable/EvalSymlinks seam the self-update gate uses.
func (e *Engine) removeFootprint(
	ctx context.Context,
	lockHandle *state.RuntimeLockHandle,
	onProgress types.ProgressFn,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepUninstallRemoveFootprint, 90, "removing wdm footprint")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"could not resolve the home directory for footprint removal",
			"",
			err,
		)
	}

	removed := []string{}

	// Config dir and data/share dir first.
	for _, dir := range []string{filepath.Dir(e.configPath), e.dataDir} {
		removedPath, err := removeFootprintDir(home, dir)
		if err != nil {
			return nil, err
		}
		if removedPath != "" {
			removed = append(removed, removedPath)
		}
	}

	// Release the runtime lock before removing the state dir that holds it,
	// then remove the state dir LAST among directories.
	if err := lockHandle.Release(); err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"could not release the runtime lock before removing the state directory",
			"",
			err,
		)
	}
	stateRemoved, err := removeFootprintDir(home, e.stateDir)
	if err != nil {
		return nil, err
	}
	if stateRemoved != "" {
		removed = append(removed, stateRemoved)
	}

	// Self-delete the running binary and its .previous sibling LAST of all.
	binaryPaths, err := e.removeRunningBinary()
	if err != nil {
		return nil, err
	}
	removed = append(removed, binaryPaths...)

	return removed, nil
}

// removeFootprintDir validates dir with [resolveFootprintDir] then removes the
// resolved path. A directory that does not exist is a no-op (empty path, no
// error): a partially installed wdm may be missing a footprint directory.
func removeFootprintDir(home, dir string) (string, error) {
	cleaned, err := resolveFootprintDir(home, dir)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return "", nil
	}

	if err := os.RemoveAll(cleaned); err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"a wdm footprint directory could not be removed",
			fmt.Sprintf("inspect %s and remove it manually", dir),
			err,
		)
	}
	return cleaned, nil
}

// resolveFootprintDir resolves dir symlink-safe and refuses it if it escapes
// the user home or resolves to a suspiciously shallow path, returning the
// cleaned resolved path safe to remove. A directory that does not exist
// resolves to the empty path with no error (a partially installed wdm may be
// missing a footprint directory). The escape and shallow guards mirror
// resolveDeleteTarget so a symlinked footprint path can never trick the
// removal into deleting an out-of-tree directory. The pre-flight runs these
// same checks against every target before any teardown so an out-of-root
// target is refused atomically (PRD §39).
func resolveFootprintDir(home, dir string) (string, error) {
	if dir == "" {
		return "", nil
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"a wdm footprint directory could not be resolved",
			fmt.Sprintf("inspect %s and remove it manually", dir),
			err,
		)
	}

	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"the home directory could not be resolved",
			"",
			err,
		)
	}

	cleaned := filepath.Clean(resolved)
	if err := security.EnsureWithinRoot(filepath.Clean(resolvedHome), cleaned); err != nil {
		return "", usageValidationError(
			"a wdm footprint path resolves outside the home directory",
			"wdm refuses to remove paths outside the home directory (PRD §39)",
			err,
		)
	}
	if isSuspiciouslyShallowPath(cleaned) {
		return "", usageValidationError(
			"a wdm footprint path resolves to a suspiciously shallow location",
			"wdm refuses to remove a near-root directory (PRD §39)",
			fmt.Errorf("resolved footprint path %q is too shallow to remove", cleaned),
		)
	}
	return cleaned, nil
}

// removeRunningBinary resolves the running executable through the
// os.Executable/EvalSymlinks seam (the same one the self-update gate uses)
// and removes both the resolved binary and its .previous rollback sibling
// (PRD §14, §39). It is the LAST removal step: on Linux the open inode
// survives until process exit, so the running process finishes cleanly. A
// missing .previous sibling is a no-op. The resolved binary is refused if it
// sits at a suspiciously shallow path.
func (e *Engine) removeRunningBinary() ([]string, error) {
	cleaned, err := e.resolveRunningBinary()
	if err != nil {
		return nil, err
	}

	removed := []string{cleaned}
	if err := os.Remove(cleaned); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"the wdm binary could not be removed",
			fmt.Sprintf("remove %s manually", cleaned),
			err,
		)
	}

	previous := cleaned + previousBinarySuffix
	if err := os.Remove(previous); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, types.WrapError(
				types.ErrCodeGeneric,
				"the previous wdm binary could not be removed",
				fmt.Sprintf("remove %s manually", previous),
				err,
			)
		}
	} else {
		removed = append(removed, previous)
	}

	return removed, nil
}

// resolveRunningBinary resolves the running executable through the
// os.Executable/EvalSymlinks seam (the same one removeRunningBinary and the
// self-update gate use) and refuses it if it resolves to a suspiciously
// shallow path, returning the cleaned resolved path. It carries no removal so
// the pre-flight can validate the binary target without deleting anything.
func (e *Engine) resolveRunningBinary() (string, error) {
	exe, err := e.selfUpdateDeps.executablePath()
	if err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"could not determine the wdm executable path",
			"",
			err,
		)
	}
	resolved, err := e.selfUpdateDeps.resolveSymlinks(exe)
	if err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"could not resolve the wdm executable path",
			"",
			err,
		)
	}

	cleaned := filepath.Clean(resolved)
	if isSuspiciouslyShallowPath(cleaned) {
		return "", usageValidationError(
			"the wdm executable resolves to a suspiciously shallow location",
			"wdm refuses to delete a near-root path (PRD §39)",
			fmt.Errorf("resolved executable path %q is too shallow to remove", cleaned),
		)
	}
	return cleaned, nil
}

// preflightFootprint runs the SAME containment, symlink, and shallow-path
// checks the removal path uses against every footprint removal target — the
// config dir, the data/share dir, the state dir, and the running binary (whose
// .previous sibling shares its directory and is covered by the binary's shallow
// guard) — BEFORE any stack teardown begins (PRD §39). If any target fails
// validation it returns the refusal error and the caller performs no teardown
// and no removal, so an out-of-root target is refused atomically up front
// instead of mid-run leaving a partial footprint. It removes nothing; the
// per-dir guards in removeFootprint stay in place as defense-in-depth.
func (e *Engine) preflightFootprint() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"could not resolve the home directory for footprint removal",
			"",
			err,
		)
	}

	for _, dir := range e.footprintPaths() {
		if _, err := resolveFootprintDir(home, dir); err != nil {
			return err
		}
	}

	if _, err := e.resolveRunningBinary(); err != nil {
		return err
	}
	return nil
}

// uninstallTeardownPct spreads the per-stack teardown events across the 30-85
// band so the progress bar advances as stacks tear down, leaving room for the
// footprint-removal step at 90.
func uninstallTeardownPct(index, total int) float64 {
	if total <= 0 {
		return 85
	}
	const start, span = 30.0, 55.0
	return start + span*float64(index)/float64(total)
}

// removeManagedNetworks drops the wdm-created Docker networks after every stack
// tore down cleanly and BEFORE any footprint removal (PRD §39). wdm pre-creates
// these networks at install and the rendered compose declares them external, so
// `docker compose down` never owns or removes them; without this sub-phase they
// linger after a self-uninstall. It runs only once all containers are down, so
// no endpoint is attached. The whole sub-phase is best-effort: a network already
// absent counts as removed (idempotent), and a network that genuinely cannot be
// removed is recorded in retained and the loop continues. It NEVER triggers the
// fail-closed abort and NEVER blocks footprint removal — that abort stays
// reserved for stack `down` failure.
func (e *Engine) removeManagedNetworks(
	ctx context.Context,
	client docker.Client,
	names []string,
	onProgress types.ProgressFn,
) (removed []string, retained []types.RetainedNetwork) {
	if len(names) == 0 {
		return nil, nil
	}
	if onProgress != nil {
		onProgress(types.StepUninstallTeardown, 88, fmt.Sprintf(
			"removing %d wdm-created network(s)",
			len(names),
		))
	}

	for _, name := range names {
		if err := docker.RemoveNetworkIfPresent(ctx, client, name); err != nil {
			retained = append(retained, types.RetainedNetwork{
				Name:   name,
				Reason: err.Error(),
			})
			continue
		}
		removed = append(removed, name)
	}
	return removed, retained
}

// sweepManagedNetworks drops every remaining wdm.managed=true Docker network
// after the compose-derived [Engine.removeManagedNetworks] pass and BEFORE any
// footprint removal (PRD §39). It closes the orphan gap: a network labeled by a
// previous install whose app the operator later deleted no longer has a compose
// file, so the compose-derived discovery cannot find it — but the label still
// can. The list is discovered through [docker.ListManagedNetworks]; each name
// not already handled (deduped against the names this run already removed or
// retained) is dropped best-effort with [docker.RemoveNetworkIfPresent].
//
// Every failure mode is a non-fatal cleanup degradation, never a fail-closed
// abort: a list failure (docker problem) is swallowed and the prior results are
// returned unchanged; a per-network removal failure is appended to retained and
// the loop continues; context cancellation between removals stops the sweep and
// returns what was gathered. Footprint removal proceeds regardless.
func (e *Engine) sweepManagedNetworks(
	ctx context.Context,
	client docker.Client,
	removed []string,
	retained []types.RetainedNetwork,
	onProgress types.ProgressFn,
) ([]string, []types.RetainedNetwork) {
	if err := ctx.Err(); err != nil {
		return removed, retained
	}

	names, err := docker.ListManagedNetworks(ctx, client)
	if err != nil {
		// Best-effort cleanup degradation: a daemon problem listing the managed
		// networks must not fail the uninstall. The compose-derived results
		// stand and footprint removal proceeds.
		return removed, retained
	}

	// Seed the seen set from the names already handled this run so a network
	// dropped (or retained) via the compose path is not attempted or listed
	// twice.
	seen := make(map[string]struct{}, len(removed)+len(retained))
	for _, name := range removed {
		seen[name] = struct{}{}
	}
	for _, network := range retained {
		seen[network.Name] = struct{}{}
	}

	swept := 0
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if err := ctx.Err(); err != nil {
			return removed, retained
		}

		if onProgress != nil && swept == 0 {
			onProgress(types.StepUninstallNetworkSweep, 89, "sweeping orphaned wdm-managed networks")
		}
		swept++

		if err := docker.RemoveNetworkIfPresent(ctx, client, name); err != nil {
			retained = append(retained, types.RetainedNetwork{
				Name:   name,
				Reason: err.Error(),
			})
			continue
		}
		removed = append(removed, name)
	}

	return removed, retained
}

// uninstallComposeNetworks is the minimal slice of a rendered docker-compose.yml
// needed to read the top-level networks block. Only networks[].external is
// decoded; yaml.v3 ignores every other key. A network declared external:true is
// exactly a wdm-pre-created network (install declares them external under their
// real substituted name); a non-external entry is compose-owned and was already
// removed by `down`, so it is never targeted here.
type uninstallComposeNetworks struct {
	Networks map[string]uninstallComposeNetwork `yaml:"networks"`
}

type uninstallComposeNetwork struct {
	External bool `yaml:"external"`
}

// readExternalNetworkNames parses the rendered compose at composePath and
// returns the names of its top-level networks declared external:true — the
// wdm-pre-created networks (PRD §39). The names are sorted for deterministic
// iteration. The read is best-effort: a missing or unparseable compose file
// yields no names rather than an error, because the network cleanup that
// consumes them never aborts the uninstall.
func readExternalNetworkNames(composePath string) []string {
	raw, err := os.ReadFile(composePath) //nolint:gosec // G304: composePath is project.ComposeFile, built via security.SafeJoin under the engine-controlled stack base
	if err != nil {
		return nil
	}

	var projection uninstallComposeNetworks
	if err := yaml.Unmarshal(raw, &projection); err != nil {
		return nil
	}

	names := make([]string, 0, len(projection.Networks))
	for name, network := range projection.Networks {
		if network.External {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
