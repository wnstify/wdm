package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"

	"golang.org/x/sync/errgroup"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// listStatusInspectConcurrency bounds how many per-stack Docker
// inspections run at once. A handful of stacks is the common case, so a
// small fixed ceiling keeps the dashboard responsive without spawning an
// unbounded fan-out against the Docker daemon.
const listStatusInspectConcurrency = 4

// ListStatus returns one [types.AppRuntimeStatus] per managed stack —
// the same universe [Engine.List] reports — each carrying a LIVE runtime
// summary derived from Docker container inspection (PRD §18). It backs the
// TUI "Check my apps" list and `wdm apps list --json` so every entry
// reflects real container state instead of a hardcoded "running".
//
// Lightweight by design (PRD §26 read-only posture): it acquires neither
// the global runtime.lock nor a blocking per-stack flock, and it does NOT
// run the per-stack `docker compose config` validation shell the full
// [Engine.Status] runs. The State is fused from container inspection plus
// the manifest alone — managed-container conditions (1-4), the manifest
// port-mismatch check (condition 6), and the last-operation marker
// (condition 7). The two heavier conditions Status also evaluates — compose
// validation (5) and stale-runtime-lock (8) — are intentionally skipped
// for the list summary; per-app Status remains the full signal.
//
// Each stack is read through [state.TryReadStackLock] — the same
// non-blocking shared-lock read List/Status use — so a stack mid
// Install/Update is summarized as needs-attention rather than stalling the
// whole list behind a writer's flock. Per-stack inspections run
// concurrently with a bounded worker pool that honors ctx cancellation;
// the result is sorted by app id so output is deterministic regardless of
// completion order.
// Corrupt-lock handling has two stages. A lock that is corrupt at SCAN time
// ([state.ScanStacks]) never enters the result: it surfaces as a WARN-level
// scan warning and is excluded, mirroring List. A lock that becomes corrupt
// or unreadable AFTER the scan but before its per-stack re-read still
// produces an entry — marked needs-attention with status_check_failed — so a
// stack that decays mid-list stays visible rather than vanishing silently.
// An app whose expected managed containers all exist but none run is
// reported with the calm "stopped" state, not "needs_attention".
// Returns [ErrClosed] when called after [Engine.Close].
func (e *Engine) ListStatus(ctx context.Context) ([]types.AppRuntimeStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ListStatus: %w", err)
	}

	scan, err := state.ScanStacks(ctx, e.stackBase)
	if err != nil {
		return nil, fmt.Errorf("core.ListStatus: %w", err)
	}
	for _, w := range scan.Warnings {
		e.logger.WarnContext(ctx, "core: stack scan warning",
			slog.String("path", w.Path),
			slog.String("cause", w.Cause.Error()),
		)
	}
	if len(scan.Apps) == 0 {
		return []types.AppRuntimeStatus{}, nil
	}

	client, err := e.buildDockerClient(security.NewActiveRedactor(nil))
	if err != nil {
		return nil, err
	}

	statuses := make([]types.AppRuntimeStatus, len(scan.Apps))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(listStatusInspectConcurrency)
	for i, app := range scan.Apps {
		group.Go(func() error {
			status, err := e.summarizeAppRuntime(groupCtx, client, app)
			if err != nil {
				return err
			}
			statuses[i] = status
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].AppID < statuses[j].AppID
	})
	return statuses, nil
}

// summarizeAppRuntime derives one stack's live runtime summary from
// container inspection and its manifest, without the compose-config
// validation or runtime-lock probe the full Status path runs. A manifest
// that vanished or became unreadable since the scan, or a stack whose
// flock is held by an in-flight operation, surfaces as needs-attention so
// the list stays honest rather than failing the whole batch.
func (e *Engine) summarizeAppRuntime(
	ctx context.Context,
	client docker.Client,
	app types.AppInfo,
) (types.AppRuntimeStatus, error) {
	summary := types.AppRuntimeStatus{AppInfo: app}

	lock, err := readManagedStackLock(ctx, app.StackPath)
	if err != nil {
		if ctx.Err() != nil {
			return types.AppRuntimeStatus{}, fmt.Errorf("core.ListStatus: %w", ctx.Err())
		}
		if errors.Is(err, fs.ErrNotExist) {
			applyRuntimeReason(&summary, statusReasonContainerMissing)
			return summary, nil
		}
		applyRuntimeReason(&summary, statusReasonStatusCheckFailed)
		return summary, nil
	}

	containers, err := docker.InspectProjectContainers(ctx, client, lock.ComposeProject)
	if err != nil {
		return types.AppRuntimeStatus{}, err
	}

	scratch := &types.AppStatus{}
	expected := expectedStatusServices(lock)
	completed := completedServiceSet(lock.CompletedServices)
	managed, reasons := fuseManagedServiceStatuses(
		app.AppID,
		expected,
		completed,
		containers,
		scratch,
	)
	if lockPortsMismatch(lock.LocalPorts, managed) {
		reasons[statusReasonPortMismatch] = struct{}{}
	}
	if lock.LastSuccessfulOperation == nil {
		reasons[statusReasonLastOperationFailed] = struct{}{}
	}

	finalizeStatus(scratch, reasons, "", "")
	applyStoppedState(scratch, expected, completed, managed)
	summary.State = scratch.State
	summary.NeedsAttention = scratch.NeedsAttention
	summary.AttentionReasons = scratch.AttentionReasons
	return summary, nil
}

// applyRuntimeReason folds a single PRD §18 reason ID into an
// AppRuntimeStatus, marking it needs-attention. It is the manifest-error
// shortcut for stacks ListStatus cannot inspect (vanished, busy, or
// unreadable lock) where the full container fusion never runs.
func applyRuntimeReason(summary *types.AppRuntimeStatus, reason string) {
	summary.State = statusStateNeedsAttention
	summary.NeedsAttention = true
	summary.AttentionReasons = []string{reason}
}
