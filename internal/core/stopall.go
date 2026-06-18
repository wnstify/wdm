package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// StopAll stops every managed stack at once (issue #27). It runs
// `docker compose stop` against each managed stack discovered under the
// configured stack base: the running containers stop but stay defined,
// so containers, networks, and named volumes are preserved and all data
// survives — this is NOT `docker compose down`. The operation is
// whole-stack and all-apps only; [types.StopAllRequest] carries no
// selector.
// Lock posture (PRD §26): StopAll is a state-changing engine entry, so
// the global runtime.lock is acquired ONCE at entry — attributed
// "stop-all" — and held for the whole batch. Each per-stack stop then
// takes the exclusive per-stack flock and reconfirms managed identity
// through the held fd before the Docker call, mirroring Restart. The
// managed set is enumerated through the shared non-blocking scan
// ([state.ScanStacks]); a stack with a corrupt manifest is folded into a
// scan warning and skipped, exactly as Engine.List does.
// Confirmation (PRD §37): a single SAFE confirmation gates the whole
// batch immediately before any stop. `docker compose stop` preserves all
// data, so the payload Kind is "stop_all_safe" and --yes auto-accepts it.
// A nil confirmer refuses with [types.ErrCodeUsageValidation]; a decline
// maps to [types.ErrCodeUserCanceled] with zero side effects (no Docker
// call); a confirmer error propagates wrapped.
// Partial failure: StopAll is continue-on-error. Every managed stack is
// attempted even if some fail (`docker compose stop` is idempotent, so an
// already-stopped stack is a success no-op). Per-stack failures are
// captured into [types.StopAllResult.Failed] with the redacted docker-layer
// detail; the stacks that stopped land in [types.StopAllResult.Stopped].
// A non-nil error is returned ONLY for whole-operation failures — a nil
// confirmer, a declined confirmation, lock contention, the enumeration
// itself failing, or context cancellation — never for a single stack that
// failed to stop.
func (e *Engine) StopAll(
	ctx context.Context,
	_ types.StopAllRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.StopAllResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "stop-all")
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	apps, err := e.planStopAll(ctx, onProgress)
	if err != nil {
		return nil, err
	}

	if err := confirmStopAll(ctx, confirmer, apps, onProgress); err != nil {
		return nil, err
	}

	return e.executeStopAll(ctx, apps, onProgress)
}

// planStopAll enumerates the managed stacks to stop under the held
// runtime.lock. It reuses [Engine.List]'s scan so corrupt manifests are
// logged as warnings and excluded, matching the List contract. The scan
// makes no Docker call. An empty managed set is not an error: it yields
// an empty app list and StopAll returns an empty result.
func (e *Engine) planStopAll(
	ctx context.Context,
	onProgress types.ProgressFn,
) ([]types.AppInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepStopAllPlanning, 5, "finding managed apps to stop")
	}

	apps, err := e.List(ctx)
	if err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(types.StepStopAllPlanning, 15, fmt.Sprintf(
			"stop planned for %d managed app(s)",
			len(apps),
		))
	}
	return apps, nil
}

// confirmStopAll asks the Confirmer to authorize the whole batch once,
// immediately before any stop. A nil confirmer refuses with
// [types.ErrCodeUsageValidation] per the pkg/engine contract, a decline
// maps to [types.ErrCodeUserCanceled], and a confirmer error propagates
// wrapped. The confirm runs before any Docker mutation, so a decline
// leaves every stack exactly as it was.
// An empty managed set still requires the confirmer to be non-nil (the
// fail-closed contract is uniform), but the payload states there is
// nothing to stop.
func confirmStopAll(
	ctx context.Context,
	confirmer types.Confirmer,
	apps []types.AppInfo,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before stopping all apps",
			"pass a confirmer that can authorize docker compose stop",
		)
	}
	if onProgress != nil {
		onProgress(types.StepStopAllConfirm, 25, "confirming stop all")
	}

	confirmed, err := confirmer.Confirm(ctx, stopAllConfirmation(apps))
	if err != nil {
		return fmt.Errorf("core.stopall: confirming stop all: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"stop all canceled before docker compose stop",
			"re-run the stop and confirm the prompt",
		)
	}
	return nil
}

// stopAllConfirmation assembles the SAFE batch consequence payload: an
// explicit statement that every managed app's containers will be stopped
// (preserved, not removed, no data loss) and the list of apps. The Kind
// is the SAFE "stop_all_safe" literal (mirroring restart's
// "restart_safe"): `docker compose stop` removes nothing, so --yes
// auto-accepts it. The payload carries no secret values (app ids only),
// so it is sink-safe.
func stopAllConfirmation(apps []types.AppInfo) types.Confirmation {
	var lines []string
	if len(apps) == 0 {
		lines = append(lines, "there are no managed apps to stop")
	} else {
		lines = append(lines, fmt.Sprintf(
			"this stops the containers of %d managed app(s) (no removal, no data loss)",
			len(apps),
		))
		for _, app := range apps {
			lines = append(lines, "stops app "+app.AppID)
		}
	}
	return types.Confirmation{
		Kind:    "stop_all_safe",
		Title:   "stop all apps",
		Message: strings.Join(lines, "\n"),
	}
}

// executeStopAll runs `docker compose stop` for each managed app under
// the runtime.lock already held by [Engine.StopAll]. It is
// continue-on-error: a single stack's failure is captured into the result
// and the loop moves on, so one unreachable stack never blocks the rest.
// Context cancellation is the only whole-operation abort — it stops the
// loop and propagates, because a canceled batch should not keep issuing
// Docker calls.
func (e *Engine) executeStopAll(
	ctx context.Context,
	apps []types.AppInfo,
	onProgress types.ProgressFn,
) (*types.StopAllResult, error) {
	result := &types.StopAllResult{
		Stopped: []types.StoppedApp{},
		Failed:  []types.StoppedApp{},
	}

	// The stop path generates no secrets and reads no .env content, so the
	// Docker client carries the structural redactor only (mirrors the
	// restart path's client).
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	total := len(apps)
	for i, app := range apps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if onProgress != nil {
			onProgress(types.StepStopAllExecute, stopAllProgressPct(i, total), fmt.Sprintf(
				"stopping %s (%d/%d)",
				app.AppID,
				i+1,
				total,
			))
		}

		outcome := e.stopOneStack(ctx, client, app.AppID)
		if outcome.Error == "" {
			result.Stopped = append(result.Stopped, outcome)
		} else {
			result.Failed = append(result.Failed, outcome)
		}
	}

	return result, nil
}

// stopOneStack stops a single managed stack: it takes the exclusive
// per-stack flock, reconfirms the manifest names appID and carries a
// Compose project, then runs `docker compose stop`. Every failure short
// of context cancellation is folded into the returned [types.StoppedApp]
// so the batch continues. Context cancellation surfaces through the
// caller's ctx.Err check on the next iteration; here a canceled flock
// acquisition is captured like any other per-stack failure, and the loop
// guard aborts the batch on the following pass.
func (e *Engine) stopOneStack(
	ctx context.Context,
	client docker.Client,
	appID string,
) types.StoppedApp {
	outcome := types.StoppedApp{AppID: appID}

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

	project, err := stopAllComposeProject(stackPath, lock)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}

	if err := docker.ComposeStop(ctx, client, project); err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	return outcome
}

// stopAllComposeProject builds the validated [docker.ComposeProject] for
// one stack's stop from the resolved stack path and the manifest's
// Compose project name, resolving the compose and env file paths under
// the stack path via [security.SafeJoin] (PRD §12, §13). It mirrors
// restart's restartComposeProject.
func stopAllComposeProject(stackPath string, lock *state.StackLock) (docker.ComposeProject, error) {
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

// stopAllProgressPct spreads the per-stack execution events across the
// 30-95 band so the progress bar advances as stacks are stopped. With no
// apps the planning band already covered the work, so this is never
// called.
func stopAllProgressPct(index, total int) float64 {
	if total <= 0 {
		return 95
	}
	const start, span = 30.0, 65.0
	return start + span*float64(index)/float64(total)
}
