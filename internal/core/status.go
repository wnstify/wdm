package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// runtimeLockFilename is the global runtime lock's file name under the
// engine's state dir (PRD §26). Shared by the state-changing acquire
// path in stubs.go and the read-only staleness probe here.
const runtimeLockFilename = "runtime.lock"

// staleRuntimeLockAge is the held-duration threshold beyond which a
// live-holder runtime lock counts as stale.
const staleRuntimeLockAge = 24 * time.Hour

// Status state labels and needs-attention reason IDs (PRD §18).
// statusReasonStatusCheckFailed is used by the post-commit verification
// pass of both install and update: that pass may not fail a durable
// operation, so a broken inspection downgrades to that reason instead of
// failing. The standalone read-only Status path never uses it; it
// propagates the docker-layer error instead.
const (
	statusStateRunning        = "running"
	statusStateNeedsAttention = "needs_attention"
	// statusStateCompleted marks a service that ran to a successful
	// exit and stays down by design — a one-shot init container that
	// exits 0 rather than staying up. It is reported instead of
	// container_exited only for services the signed catalog lists in
	// completed_services, and only when the container genuinely exited
	// with code 0; any other exit shape still surfaces needs-attention.
	statusStateCompleted = "completed"
	// statusStateRemoved is the post-down success state for the safe-remove
	// path (PRD §19 step 5): no managed container remains under the Compose
	// project, so the stack is recorded removed while its files and named
	// volumes stay on disk.
	statusStateRemoved = "removed"

	statusReasonContainerMissing        = "container_missing"
	statusReasonContainerExited         = "container_exited"
	statusReasonRestartLoop             = "restart_loop"
	statusReasonUnhealthy               = "unhealthy"
	statusReasonPortMismatch            = "port_mismatch"
	statusReasonComposeValidationFailed = "compose_validation_failed"
	statusReasonLastOperationFailed     = "last_operation_failed"
	statusReasonStaleRuntimeLock        = "stale_runtime_lock"
	statusReasonStatusCheckFailed       = "status_check_failed"
)

// Status reports the operational state of one managed stack by fusing
// the full PRD §18 condition set into a [types.AppStatus]:
//  1. a managed container is missing
//  2. a managed container exited unexpectedly
//  3. a managed container is in a restart loop
//  4. a healthcheck reports unhealthy
//  5. Compose validation fails
//  6. required local ports no longer match the lock file
//  7. the last wdm operation failed
//  8. a stale runtime lock affects the app
//
// Managed-only ordering (PRD §10): the stack must resolve to a
// directory whose .wdm.lock manifest parses and names appID BEFORE any
// Docker command runs; container inspection then lists by the
// manifest's Compose project and counts only containers carrying
// wdm.managed=true plus wdm.app=<app> — never a broad container
// listing. Unmanaged directories and uninstalled apps surface
// [types.ErrCodeUsageValidation] refusals without touching Docker.
// Read-only discipline (PRD §26): Status acquires neither the global
// runtime.lock nor a blocking per-stack flock and writes nothing. The
// manifest is read through [state.TryReadStackLock] — a non-blocking
// shared-lock read — so a stack mid-Install/Update surfaces a
// [types.ErrCodeRuntimeLockHeld] refusal (the PRD §26 "another
// operation is already running" outcome) instead of stalling behind
// the writer's flock for the operation's duration. The runtime lock is
// only probed ([state.ProbeRuntimeLock]) for condition 8's staleness
// detection; it is never acquired, written, or removed.
// Error semantics (PRD §27): docker-layer failures from container
// inspection propagate unchanged so internal/docker's typed code
// mapping stays authoritative; a failed `docker compose config`
// validation with a live ctx is condition 5, not an error, unless it
// carries [types.ErrCodeDockerUnavailable] — an unreachable daemon
// propagates unchanged like the other docker-layer failures; corrupt
// manifests surface wrapped [types.ErrStaleState]. Context cancellation
// always propagates as an error, never as a condition.
func (e *Engine) Status(ctx context.Context, appID string) (*types.AppStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.Status: %w", err)
	}
	if appID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return nil, err
	}

	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	containers, err := docker.InspectProjectContainers(ctx, client, lock.ComposeProject)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	status := &types.AppStatus{
		AppID:          appID,
		ComposeProject: lock.ComposeProject,
		StackPath:      stackPath,
		UpdatedAt:      &now,
	}
	managed, reasons := fuseManagedServiceStatuses(appID, expectedStatusServices(lock), completedServiceSet(lock.CompletedServices), containers, status)
	status.LocalPorts = observedLocalPorts(managed)

	if lockPortsMismatch(lock.LocalPorts, managed) {
		reasons[statusReasonPortMismatch] = struct{}{}
	}
	if err := checkComposeValidation(ctx, client, stackPath, reasons); err != nil {
		return nil, err
	}
	if lock.LastSuccessfulOperation == nil {
		reasons[statusReasonLastOperationFailed] = struct{}{}
	}
	stale, err := e.staleRuntimeLockCondition(ctx)
	if err != nil {
		return nil, err
	}
	if stale {
		reasons[statusReasonStaleRuntimeLock] = struct{}{}
	}

	finalizeStatus(
		status,
		reasons,
		"all managed services are running",
		"status checks found issues that need attention",
	)
	return status, nil
}

// resolveManagedStack maps appID to its managed stack directory and
// parsed.wdm.lock manifest for the non-mutating paths (Status, Logs,
// and the update check-planning stage, which shares this read posture
// before any mutation begins per protocol step 2). The
// conventional location <stackBase>/<appID> is tried first; when it is
// absent, unmanaged, or its manifest names a different app, the stack
// base is scanned one level deep for a manifest whose app_id matches —
// mirroring List's scan universe so a stack installed under a custom
// directory name stays reachable by the same identifier List reports.
// Refusal semantics: a present-but-unmanaged conventional directory
// and a fully missing stack both surface distinct
// [types.ErrCodeUsageValidation] errors BEFORE any Docker call
// (PRD §10). A conventional stack whose flock is held by an in-flight
// operation refuses with [types.ErrCodeRuntimeLockHeld]; a corrupt
// manifest refuses with a wrapped [types.ErrStaleState].
func (e *Engine) resolveManagedStack(ctx context.Context, appID string) (string, *state.StackLock, error) {
	candidate, err := security.SafeJoin(e.stackBase, appID)
	if err != nil {
		return "", nil, usageValidationError(
			"app id is unsafe",
			"pass a plain app id without path separators",
			err,
		)
	}

	candidateExists, err := installStackPathExists(candidate)
	if err != nil {
		return "", nil, err
	}
	if candidateExists {
		lock, err := readManagedStackLock(ctx, candidate)
		switch {
		case err == nil && lock.AppID == appID:
			return candidate, lock, nil
		case err == nil:
			// The conventional directory belongs to a different app; the
			// requested app may still live under a custom directory name.
		case errors.Is(err, fs.ErrNotExist):
			// Present but unmanaged; fall through to the scan before
			// composing the refusal so a custom-named stack wins.
		default:
			return "", nil, err
		}
	}

	stackPath, lock, found, err := e.findManagedStackByAppID(ctx, appID, candidate)
	if err != nil {
		return "", nil, err
	}
	if found {
		return stackPath, lock, nil
	}

	if candidateExists {
		return "", nil, usageValidationError(
			"stack directory is not managed by wdm",
			"wdm only operates on stacks it installed",
			fmt.Errorf("no readable wdm manifest names app %q under %q", appID, e.stackBase),
		)
	}
	return "", nil, usageValidationError(
		"app is not installed",
		"run wdm apps list to see installed apps",
		fmt.Errorf("no managed stack found for app %q under %q", appID, e.stackBase),
	)
}

// readManagedStackLock reads stackDir's.wdm.lock without blocking,
// translating the state-layer outcomes into the typed managed-stack
// refusals shared by the read-only Status and Logs paths. A missing
// manifest propagates as wrapped [fs.ErrNotExist] so the caller can tell
// "unmanaged directory" from the other refusal shapes.
func readManagedStackLock(ctx context.Context, stackDir string) (*state.StackLock, error) {
	lockPath := filepath.Join(stackDir, installLockFilename)
	info, err := os.Lstat(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("core.resolveManagedStack: %w", err)
	}
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be inspected",
			"check stack directory permissions and retry",
			err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, stackPathUnsafeError(fmt.Errorf("stack lock %q is a symlink", lockPath))
	}
	if !info.Mode().IsRegular() {
		return nil, usageValidationError(
			"stack lock is not a regular file",
			"remove the conflicting file or choose a different stack path",
			fmt.Errorf("stack lock %q is not a regular file", lockPath),
		)
	}

	lock, err := state.TryReadStackLock(ctx, lockPath)
	switch {
	case err == nil:
		return lock, nil
	case errors.Is(err, state.ErrStackLockBusy):
		return nil, types.WrapError(
			types.ErrCodeRuntimeLockHeld,
			"another wdm operation is working on this stack",
			"wait for the in-progress operation to finish, then try again",
			err,
		)
	case errors.Is(err, types.ErrStaleState):
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack state file is corrupt",
			"re-run the interrupted operation or restore a config backup",
			err,
		)
	case errors.Is(err, fs.ErrNotExist):
		// The manifest vanished between Lstat and open; treat as unmanaged
		// like the Lstat miss above.
		return nil, fmt.Errorf("core.resolveManagedStack: %w", err)
	default:
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be read",
			"check stack directory permissions and retry",
			err,
		)
	}
}

// findManagedStackByAppID scans the stack base one level deep for a
// directory whose .wdm.lock names appID, skipping the already-checked
// conventional candidate. Unreadable manifests — busy, corrupt, missing,
// or otherwise — are skipped silently: they cannot positively match, and
// a read-only scan must not fail on a neighbor stack's state (mirroring
// the List scan posture).
func (e *Engine) findManagedStackByAppID(
	ctx context.Context,
	appID string,
	candidate string,
) (string, *state.StackLock, bool, error) {
	entries, err := os.ReadDir(e.stackBase)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, types.WrapError(
			types.ErrCodeGeneric,
			"stack base could not be scanned",
			"check stack base directory permissions and retry",
			err,
		)
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", nil, false, fmt.Errorf("core.resolveManagedStack: %w", err)
		}
		if !entry.IsDir() {
			continue
		}
		stackDir := filepath.Join(e.stackBase, entry.Name())
		if stackDir == filepath.Clean(candidate) {
			continue
		}
		lock, err := state.TryReadStackLock(ctx, filepath.Join(stackDir, installLockFilename))
		if err != nil || lock.AppID != appID {
			continue
		}
		return stackDir, lock, true, nil
	}
	return "", nil, false, nil
}

// expectedStatusServices derives the expected Compose service set from
// the manifest's image pins — the per-service record both install and
// update persist. The result is sorted and deduplicated; manifests
// without pins yield an empty set, in which case the zero-managed-
// containers fallback in [fuseManagedServiceStatuses] still detects an
// absent stack.
func expectedStatusServices(lock *state.StackLock) []string {
	seen := map[string]struct{}{}
	services := make([]string, 0, len(lock.ImagePins))
	for _, pin := range lock.ImagePins {
		if pin.Service == "" {
			continue
		}
		if _, ok := seen[pin.Service]; ok {
			continue
		}
		seen[pin.Service] = struct{}{}
		services = append(services, pin.Service)
	}
	sort.Strings(services)
	return services
}

// completedServiceSet builds a membership set of the Compose service
// names that complete by design. It is the shared bridge between
// the persisted lock (lock.CompletedServices) and the in-flight catalog
// app (app.CompletedServices) and the fusion's completed parameter. A
// nil or empty input yields an empty set, so the fusion sees the
// behavior from before completed-service tracking for every caller that
// has no completed services.
func completedServiceSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// fuseManagedServiceStatuses is the shared PRD §18 container-condition
// fusion used by both the install-time verification pass and the
// standalone Status path. A container counts as managed only when its
// Compose service label matches AND it carries wdm.managed=true plus
// wdm.app=<app> (PRD §10, §18) — a project container with drifted
// labels surfaces as a missing managed container for its service.
// The per-service walk covers the union of the expected services and
// the observed managed services so every managed container is
// condition-checked even when it is not in the expected set:
//   - expected but unmatched → container_missing (condition 1)
//   - restarting → restart_loop (condition 3)
//   - not running → container_exited (condition 2)
//   - health "unhealthy" → unhealthy (condition 4)
//
// completed names the services that complete by design — one-shot init
// containers that exit 0 rather than staying up. A service in this
// set is reported "completed" (no needs-attention) ONLY when its
// container genuinely exited with code 0; a nonzero exit, or any other
// down state ("dead"/"created"/"removing"), still surfaces
// container_exited. Restarting still wins over completed, so a completed
// service stuck in a restart loop is reported restart_loop, not done. A
// nil completed map is safe and reproduces the behavior from before
// completed-service tracking for every service, which is why the
// restart, restore, and update callers
// pass nil.
//
// When the expected set is empty (a manifest without image pins) and no
// managed container exists, container_missing still fires — a managed
// stack with nothing running is the condition's degenerate case. Returns
// the managed-container index for the callers' port checks plus the
// accumulated reason set.
func fuseManagedServiceStatuses(
	appID string,
	expectedServices []string,
	completed map[string]struct{},
	containers []docker.ContainerInfo,
	status *types.AppStatus,
) (map[string]docker.ContainerInfo, map[string]struct{}) {
	managed := make(map[string]docker.ContainerInfo, len(containers))
	for _, container := range containers {
		if container.Labels["wdm.managed"] != "true" || container.Labels["wdm.app"] != appID {
			continue
		}
		if _, ok := managed[container.Service]; ok {
			continue
		}
		managed[container.Service] = container
	}

	services := append([]string(nil), expectedServices...)
	expected := make(map[string]struct{}, len(expectedServices))
	for _, service := range expectedServices {
		expected[service] = struct{}{}
	}
	for service := range managed {
		if _, ok := expected[service]; !ok {
			services = append(services, service)
		}
	}
	sort.Strings(services)

	reasons := map[string]struct{}{}
	for _, service := range services {
		container, ok := managed[service]
		if !ok {
			status.Services = append(status.Services, types.ServiceStatus{
				Service:        service,
				State:          "missing",
				NeedsAttention: true,
				Message:        "managed container is missing",
			})
			reasons[statusReasonContainerMissing] = struct{}{}
			continue
		}

		serviceStatus := types.ServiceStatus{
			Service:        service,
			ContainerName:  container.Name,
			State:          container.State.Status,
			Health:         container.State.Health,
			PublishedPorts: publishedPortBindings(container),
		}
		switch {
		case container.State.Restarting:
			serviceStatus.NeedsAttention = true
			serviceStatus.Message = "container is in a restart loop"
			reasons[statusReasonRestartLoop] = struct{}{}
		case !container.State.Running:
			_, isCompleted := completed[service]
			if isCompleted && strings.EqualFold(container.State.Status, "exited") && container.State.ExitCode == 0 {
				// Completed by design: a one-shot init container the
				// signed catalog lists in completed_services that ran to a
				// clean exit. No needs-attention, no container_exited reason.
				serviceStatus.State = statusStateCompleted
			} else {
				serviceStatus.NeedsAttention = true
				serviceStatus.Message = fmt.Sprintf("container exited unexpectedly (exit code %d)", container.State.ExitCode)
				reasons[statusReasonContainerExited] = struct{}{}
			}
		case strings.EqualFold(container.State.Health, "unhealthy"):
			serviceStatus.NeedsAttention = true
			serviceStatus.Message = "container healthcheck reports unhealthy"
			reasons[statusReasonUnhealthy] = struct{}{}
		}
		status.Services = append(status.Services, serviceStatus)
	}

	if len(expectedServices) == 0 && len(managed) == 0 {
		reasons[statusReasonContainerMissing] = struct{}{}
	}
	return managed, reasons
}

// lockPortsMismatch reports PRD §18 condition 6 for the standalone
// Status path: every host port recorded in the manifest's local_ports
// must still be published by some managed container. The manifest stores
// plain port numbers (no protocol), so matching is by host port; the
// install-time pass keeps its stricter protocol-aware check because the
// plan carries full bindings there.
func lockPortsMismatch(lockPorts []int, managed map[string]docker.ContainerInfo) bool {
	if len(lockPorts) == 0 {
		return false
	}
	published := map[int]struct{}{}
	for _, container := range managed {
		for _, port := range container.Ports {
			published[port.HostPort] = struct{}{}
		}
	}
	for _, port := range lockPorts {
		if _, ok := published[port]; !ok {
			return true
		}
	}
	return false
}

// observedLocalPorts aggregates the published host ports observed across
// the managed containers, ordered by service name for deterministic
// output.
func observedLocalPorts(managed map[string]docker.ContainerInfo) []types.PortBinding {
	services := make([]string, 0, len(managed))
	for service := range managed {
		services = append(services, service)
	}
	sort.Strings(services)

	var bindings []types.PortBinding
	for _, service := range services {
		bindings = append(bindings, publishedPortBindings(managed[service])...)
	}
	return bindings
}

// checkComposeValidation runs PRD §18 condition 5: `docker compose
// config --quiet` against the stack's existing files. A validation
// failure with a live ctx marks the condition and continues — a broken
// config is a needs-attention state, not a Status error — while context
// cancellation propagates unchanged. A [types.ErrCodeDockerUnavailable]
// failure also propagates unchanged: a daemon that died after container
// inspection is a hard docker-layer error, not evidence of a stack-config
// problem.
func checkComposeValidation(
	ctx context.Context,
	client docker.Client,
	stackPath string,
	reasons map[string]struct{},
) error {
	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	if err := docker.ValidateComposeConfig(ctx, client, stackPath, composePath); err != nil {
		if ctx.Err() != nil {
			return err
		}
		if types.IsCode(err, types.ErrCodeDockerUnavailable) {
			return err
		}
		reasons[statusReasonComposeValidationFailed] = struct{}{}
	}
	return nil
}

// staleRuntimeLockCondition evaluates PRD §18 condition 8 against the
// engine's runtime.lock. The lock is stale when it is currently HELD and
// either the recorded holder process no longer exists (an inherited or
// orphaned fd keeping the flock alive) or the hold has lasted longer than
// [staleRuntimeLockAge] (a wedged operation). A lock that is merely
// present but unheld is the normal post-release residue and never stale —
// the kernel released the flock with the holder's last fd, so nothing
// starves (PRD §26, risk row "Stale runtime lock starves all
// writes"). Detection only: Status never deletes or rewrites the lock.
func (e *Engine) staleRuntimeLockCondition(ctx context.Context) (bool, error) {
	probe, err := state.ProbeRuntimeLock(ctx, filepath.Join(e.stateDir, runtimeLockFilename))
	if err != nil {
		return false, types.WrapError(
			types.ErrCodeGeneric,
			"runtime lock could not be inspected",
			"check the wdm state directory permissions and retry",
			err,
		)
	}
	return runtimeLockProbeStale(probe), nil
}

// runtimeLockProbeStale evaluates PRD §18 condition 8's staleness policy
// against a [state.RuntimeLockProbe]. It is the single engine-side
// staleness classifier shared by [Engine.staleRuntimeLockCondition]
// (Status), [Engine.RuntimeLockStatus], and [Engine.ClearStaleRuntimeLock]
// so the three consumers can never drift: a lock the recovery flow would
// clear is exactly a lock Status flags stale and RuntimeLockStatus reports
// Stale.
// The lock is stale when it is currently HELD and either the recorded
// holder process is gone (dead or recycled PID — [state.RuntimeLockProbe.HolderAlive]
// is false) or a still-live holder has held the lock longer than
// [staleRuntimeLockAge]. The held-duration uses the recorded StartedAt,
// falling back to the file's ModTime when StartedAt is zero (an old lock
// file or a non-Linux acquisition that recorded no timestamp). A lock
// present but unheld is the normal post-release residue and never stale —
// the kernel released the flock with the holder's last fd, so nothing
// starves.
func runtimeLockProbeStale(probe state.RuntimeLockProbe) bool {
	if !probe.Held {
		return false
	}
	if !probe.HolderAlive {
		return true
	}

	heldSince := probe.Holder.StartedAt
	if heldSince.IsZero() {
		heldSince = probe.ModTime
	}
	if heldSince.IsZero() {
		return false
	}
	return time.Since(heldSince) > staleRuntimeLockAge
}

// finalizeStatus folds the accumulated reason set into the app-level
// state, message, and sorted attention-reason list shared by the
// install-time verification pass and the standalone Status path.
func finalizeStatus(status *types.AppStatus, reasons map[string]struct{}, runningMessage, attentionMessage string) {
	if len(reasons) == 0 {
		status.State = statusStateRunning
		status.Message = runningMessage
		return
	}
	attentionReasons := make([]string, 0, len(reasons))
	for reason := range reasons {
		attentionReasons = append(attentionReasons, reason)
	}
	sort.Strings(attentionReasons)
	status.State = statusStateNeedsAttention
	status.NeedsAttention = true
	status.AttentionReasons = attentionReasons
	status.Message = attentionMessage
}
