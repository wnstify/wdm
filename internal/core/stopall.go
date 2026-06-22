package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// StopAll stops the RUNNING managed stacks at once (issue #27). It runs
// `docker compose stop` against each managed stack that has at least one
// running container under the configured stack base: the running containers
// stop but stay defined, so containers, networks, and named volumes are
// preserved and all data survives — this is NOT `docker compose down`. The
// operation is whole-stack and all-apps only; [types.StopAllRequest] carries
// no selector.
// Running-only targeting: the plan filters the managed set to the stacks
// with at least one running container, judged live through the same
// [docker.InspectProjectContainers] read the status path uses. Stacks that
// are already not running (cleanly stopped or removed) are SKIPPED — they are
// reported in [types.StopAllResult.AlreadyStopped] but never confirmed,
// stopped, or counted as failures. When NO managed stack is running the
// confirmer is not consulted at all and StopAll returns cleanly with empty
// Stopped/Failed slices, so "nothing to stop" is a success no-op, not a
// prompt for zero apps.
// Lock posture (PRD §26): StopAll is a state-changing engine entry, so
// the global runtime.lock is acquired ONCE at entry — attributed
// "stop-all" — and held for the whole batch. Each per-stack stop then
// takes the exclusive per-stack flock and reconfirms managed identity
// through the held fd before the Docker call, mirroring Restart. The
// managed set is enumerated through the shared non-blocking scan
// ([state.ScanStacks]); a stack with a corrupt manifest is folded into a
// scan warning and skipped, exactly as Engine.List does. The running-detection
// inspect is read-only and acquires no per-stack flock (PRD §26 read posture),
// mirroring Status/ListStatus; the exclusive flock is taken only for the
// stacks that proceed to a stop.
// Confirmation (PRD §37): a single SAFE confirmation gates the whole
// batch of running stacks immediately before any stop. `docker compose stop`
// preserves all data, so the payload Kind is "stop_all_safe" and --yes
// auto-accepts it. A nil confirmer refuses with [types.ErrCodeUsageValidation]
// (only when there is at least one running stack to stop); a decline maps to
// [types.ErrCodeUserCanceled] with zero side effects (no Docker stop); a
// confirmer error propagates wrapped.
// Partial failure: StopAll is continue-on-error. Every TARGETED (running)
// stack is attempted even if some fail; a stack that stops between plan and
// execution is a harmless no-op. Per-stack failures are captured into
// [types.StopAllResult.Failed] with the redacted docker-layer detail; the
// stacks that stopped land in [types.StopAllResult.Stopped].
// A non-nil error is returned ONLY for whole-operation failures — a nil
// confirmer (when a stop was planned), a declined confirmation, lock
// contention, the enumeration or inspection itself failing, or context
// cancellation — never for a single stack that failed to stop.
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

	// The stop path generates no secrets and reads no .env content, so the
	// Docker client carries the structural redactor only (mirrors the
	// restart path's client). It is built once and reused by the
	// running-detection inspect and the per-stack stop.
	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	running, alreadyStopped, err := e.planStopAll(ctx, client, onProgress)
	if err != nil {
		return nil, err
	}

	// Nothing running: skip the confirmer entirely and return a clean
	// no-op. The user is not prompted to confirm zero stops.
	if len(running) == 0 {
		if onProgress != nil {
			onProgress(types.StepStopAllPlanning, 100, "no running apps to stop")
		}
		return &types.StopAllResult{
			Stopped:        []types.StoppedApp{},
			Failed:         []types.StoppedApp{},
			AlreadyStopped: skippedStoppedApps(alreadyStopped),
		}, nil
	}

	if err := confirmStopAll(ctx, confirmer, running, onProgress); err != nil {
		return nil, err
	}

	return e.executeStopAll(ctx, client, running, alreadyStopped, onProgress)
}

// planStopAll enumerates the managed stacks and partitions them into the
// stacks to stop (at least one running container) and the stacks already not
// running (skipped). It reuses [Engine.List]'s scan so corrupt manifests are
// logged as warnings and excluded, matching the List contract, then inspects
// each managed stack's containers read-only to judge whether any is running.
// An empty managed set is not an error: it yields two empty slices and
// StopAll returns a clean no-op result.
func (e *Engine) planStopAll(
	ctx context.Context,
	client docker.Client,
	onProgress types.ProgressFn,
) (running, alreadyStopped []types.AppInfo, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if onProgress != nil {
		onProgress(types.StepStopAllPlanning, 5, "finding managed apps to stop")
	}

	apps, err := e.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	for _, app := range apps {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		isRunning, err := e.stackHasRunningContainer(ctx, client, app.AppID)
		if err != nil {
			return nil, nil, err
		}
		if isRunning {
			running = append(running, app)
		} else {
			alreadyStopped = append(alreadyStopped, app)
		}
	}

	if onProgress != nil {
		onProgress(types.StepStopAllPlanning, 15, fmt.Sprintf(
			"stop planned for %d running app(s); %d already stopped",
			len(running),
			len(alreadyStopped),
		))
	}
	return running, alreadyStopped, nil
}

// stackHasRunningContainer reports whether the managed stack appID has at
// least one running managed container, read live and read-only through the
// shared inspect the status path uses. It resolves the stack the same way
// the read-only Status path does (no exclusive flock) and reuses the shared
// managed-container fusion and running-count primitives so the StopAll plan
// and the "stopped" status classification can never drift.
func (e *Engine) stackHasRunningContainer(
	ctx context.Context,
	client docker.Client,
	appID string,
) (bool, error) {
	_, lock, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return false, err
	}

	containers, err := docker.InspectProjectContainers(ctx, client, lock.ComposeProject)
	if err != nil {
		return false, err
	}

	scratch := &types.AppStatus{}
	managed, _ := fuseManagedServiceStatuses(
		appID,
		expectedStatusServices(lock),
		completedServiceSet(lock.CompletedServices),
		containers,
		scratch,
	)
	return runningManagedCount(managed) > 0, nil
}

// confirmStopAll asks the Confirmer to authorize the running batch once,
// immediately before any stop. It is only reached when at least one stack is
// running (the empty-plan case returns before confirming), so the payload
// always names a non-empty set. A nil confirmer refuses with
// [types.ErrCodeUsageValidation] per the pkg/engine contract, a decline
// maps to [types.ErrCodeUserCanceled], and a confirmer error propagates
// wrapped. The confirm runs before any Docker mutation, so a decline
// leaves every stack exactly as it was.
func confirmStopAll(
	ctx context.Context,
	confirmer types.Confirmer,
	apps []types.AppInfo,
	onProgress types.ProgressFn,
) error {
	return confirmLifecycleOp(ctx, confirmer, stopAllConfirmation(apps), confirmStrings{
		stepID:         types.StepStopAllConfirm,
		stepPct:        25,
		stepMessage:    "confirming stop all",
		nilMessage:     "confirmer is required before stopping all apps",
		nilHint:        "pass a confirmer that can authorize docker compose stop",
		confirmErrWrap: "core.stopall: confirming stop all",
		declineMessage: "stop all canceled before docker compose stop",
		declineHint:    "re-run the stop and confirm the prompt",
	}, onProgress)
}

// stopAllConfirmation assembles the SAFE batch consequence payload: an
// explicit statement that the running apps' containers will be stopped
// (preserved, not removed, no data loss) and the list of those apps. The Kind
// is the SAFE "stop_all_safe" literal (mirroring restart's
// "restart_safe"): `docker compose stop` removes nothing, so --yes
// auto-accepts it. The payload carries no secret values (app ids only),
// so it is sink-safe. It is only built for a non-empty running set.
func stopAllConfirmation(apps []types.AppInfo) types.Confirmation {
	lines := []string{fmt.Sprintf(
		"this stops the containers of %d running app(s) (no removal, no data loss)",
		len(apps),
	)}
	for _, app := range apps {
		lines = append(lines, "stops app "+app.AppID)
	}
	return types.Confirmation{
		Kind:    "stop_all_safe",
		Title:   "stop all apps",
		Message: strings.Join(lines, "\n"),
	}
}

// skippedStoppedApps maps the already-not-running managed apps the plan
// skipped into [types.StoppedApp] entries for [types.StopAllResult.AlreadyStopped].
// They carry the app id only: no stop ran, so there is no Compose project to
// report from a held flock and no failure detail. A nil or empty input
// yields a nil slice so the result omits the field.
func skippedStoppedApps(apps []types.AppInfo) []types.StoppedApp {
	if len(apps) == 0 {
		return nil
	}
	skipped := make([]types.StoppedApp, 0, len(apps))
	for _, app := range apps {
		skipped = append(skipped, types.StoppedApp{AppID: app.AppID})
	}
	return skipped
}

// executeStopAll runs `docker compose stop` for each running managed app
// under the runtime.lock already held by [Engine.StopAll], using the client
// the plan stage already built. It is continue-on-error: a single stack's
// failure is captured into the result and the loop moves on, so one
// unreachable stack never blocks the rest. The already-stopped stacks the
// plan skipped are carried straight into the result for transparency.
// Context cancellation is the only whole-operation abort — it stops the
// loop and propagates, because a canceled batch should not keep issuing
// Docker calls.
func (e *Engine) executeStopAll(
	ctx context.Context,
	client docker.Client,
	apps, alreadyStopped []types.AppInfo,
	onProgress types.ProgressFn,
) (*types.StopAllResult, error) {
	result := &types.StopAllResult{
		Stopped:        []types.StoppedApp{},
		Failed:         []types.StoppedApp{},
		AlreadyStopped: skippedStoppedApps(alreadyStopped),
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
	_, composeProject, errMsg := e.perStackOp(ctx, appID, func(opCtx context.Context, project docker.ComposeProject) error {
		return docker.ComposeStop(opCtx, client, project)
	})
	outcome.ComposeProject = composeProject
	outcome.Error = errMsg
	return outcome
}

// stopAllProgressPct spreads the per-stack execution events across the
// 30-95 band so the progress bar advances as stacks are stopped.
func stopAllProgressPct(index, total int) float64 {
	const start, span = 30.0, 65.0
	return start + span*float64(index)/float64(total)
}
