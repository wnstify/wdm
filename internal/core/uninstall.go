package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// stack directory are KEPT. Self-uninstall never deletes user data.
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

	tornDown, failed := e.teardownAllStacks(ctx, client, apps, onProgress)

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

	removed, err := e.removeFootprint(ctx, handle, onProgress)
	if err != nil {
		return nil, err
	}

	return &types.UninstallResult{
		TornDown:      tornDown,
		KeptDataPaths: keptDataPaths,
		RemovedPaths:  removed,
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
		"each app is torn down with docker compose down --rmi all (containers + project network + images); no -v, no data loss",
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
// whole-operation abort — it stops the loop, and the caller treats the
// canceled run as a failure (footprint untouched).
func (e *Engine) teardownAllStacks(
	ctx context.Context,
	client docker.Client,
	apps []types.AppInfo,
	onProgress types.ProgressFn,
) (tornDown, failed []types.TornDownApp) {
	tornDown = []types.TornDownApp{}
	failed = []types.TornDownApp{}

	total := len(apps)
	for i, app := range apps {
		if err := ctx.Err(); err != nil {
			failed = append(failed, types.TornDownApp{
				AppID: app.AppID,
				Error: err.Error(),
			})
			return tornDown, failed
		}
		if onProgress != nil {
			onProgress(types.StepUninstallTeardown, uninstallTeardownPct(i, total), fmt.Sprintf(
				"tearing down %s (%d/%d)",
				app.AppID,
				i+1,
				total,
			))
		}

		outcome := e.teardownOneStack(ctx, client, app.AppID)
		if outcome.Error == "" {
			tornDown = append(tornDown, outcome)
		} else {
			failed = append(failed, outcome)
		}
	}

	return tornDown, failed
}

// teardownOneStack tears down a single managed stack: it takes the exclusive
// per-stack flock, reconfirms the manifest names appID and carries a Compose
// project, then runs `docker compose down --rmi all` (NEVER -v). Every
// failure short of a panic is folded into the returned [types.TornDownApp] so
// the batch continues, mirroring stopOneStack.
func (e *Engine) teardownOneStack(
	ctx context.Context,
	client docker.Client,
	appID string,
) types.TornDownApp {
	outcome := types.TornDownApp{AppID: appID}

	stackPath, err := security.SafeJoin(e.stackBase, appID)
	if err != nil {
		outcome.Error = fmt.Sprintf("app id is unsafe: %v", err)
		return outcome
	}

	handle, err := acquireInstallStackLock(ctx, stackPath)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	lock, err := reconfirmManagedStack(handle, appID)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	if lock.ComposeProject == "" {
		outcome.Error = "stack manifest is missing its compose project"
		return outcome
	}
	outcome.ComposeProject = lock.ComposeProject

	project, err := uninstallComposeProject(stackPath, lock)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}

	if err := docker.ComposeDownRemoveImages(ctx, client, project); err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	return outcome
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
