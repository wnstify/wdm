package core

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// stackPathCrossCheck enforces the AppID-driven fail-closed cross-check the
// lifecycle planning stages share (PRD §9, §19): a supplied req.StackPath is
// verified against the already-resolved managed stack rather than used as an
// alternate resolution path, so a stale or wrong --stack-path can never act on
// a different managed stack. An empty stackPath skips the check; a mismatch
// (after filepath.Clean) refuses with a typed usage-validation error before any
// Docker call.
func stackPathCrossCheck(reqStackPath, appID, resolvedStackPath string) error {
	if reqStackPath != "" && filepath.Clean(reqStackPath) != resolvedStackPath {
		return usageValidationError(
			"stack path does not match the managed stack for this app",
			fmt.Sprintf("the managed stack for %q is at %s", appID, resolvedStackPath),
			nil,
		)
	}
	return nil
}

// requireComposeProject refuses a manifest missing its Compose project (PRD
// §9, §30): a managed stack always records its Compose project at install time,
// so an empty value is a corrupt manifest. Refusing here — before any volume
// listing or `down` — names the corrupt manifest rather than degrading to a
// generic late docker-layer refusal.
func requireComposeProject(composeProject, appID string) error {
	if composeProject == "" {
		return usageValidationError(
			"stack manifest is missing its compose project",
			"the .wdm.lock is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("stack lock for %q records no compose project", appID),
		)
	}
	return nil
}

// requireAppID refuses an empty AppID, the resolution key every managed verb
// needs. It is a typed usage-validation error so cmd/wdm maps it to PRD §27
// exit code 2.
func requireAppID(appID string) error {
	if appID == "" {
		return usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}
	return nil
}

// confirmStrings carries the per-verb values the shared confirm skeleton
// varies: the progress step ID and percent, its message, the nil-confirmer
// refusal message and hint, the confirm-error wrap prefix, and the decline
// message and hint. The Confirmation itself is built by each verb and passed
// to confirmLifecycleOp so per-verb payloads stay distinct.
type confirmStrings struct {
	stepID         string
	stepPct        float64
	stepMessage    string
	nilMessage     string
	nilHint        string
	confirmErrWrap string
	declineMessage string
	declineHint    string
}

// confirmLifecycleOp is the shared confirm skeleton for the destroy/lifecycle
// verbs (remove, delete, restart, stop-all, uninstall): ctx.Err → nil-confirmer
// usage-validation refusal → progress event → Confirmer.Confirm → wrapped
// confirmer-error → declined user-canceled refusal. Every per-verb code,
// message, and the Confirmation payload are supplied by the caller, so the
// observable behavior is byte-identical to the inlined helpers it replaces.
func confirmLifecycleOp(
	ctx context.Context,
	confirmer types.Confirmer,
	confirmation types.Confirmation,
	s confirmStrings,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			s.nilMessage,
			s.nilHint,
		)
	}
	if onProgress != nil {
		onProgress(s.stepID, s.stepPct, s.stepMessage)
	}

	confirmed, err := confirmer.Confirm(ctx, confirmation)
	if err != nil {
		return fmt.Errorf("%s: %w", s.confirmErrWrap, err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			s.declineMessage,
			s.declineHint,
		)
	}
	return nil
}

// perStackOp runs the shared per-stack preparation the batch teardown verbs
// share (stop-all, uninstall): SafeJoin the app id under the stack base, take
// the exclusive per-stack flock, reconfirm the manifest names appID and carries
// a Compose project, build the validated [docker.ComposeProject], then run the
// supplied dockerOp (`docker compose stop` or `docker compose down --rmi all`).
// On any failure short of a panic it returns a redaction-safe error string for
// folding into the batch outcome, plus the resolved Compose project (set once
// reconfirm succeeds, so the caller can record it even when a later step fails,
// mirroring the inlined helpers) and the built project for post-op use
// (teardown reads the rendered compose's external networks). The per-stack
// flock is released before perStackOp returns.
func (e *Engine) perStackOp(
	ctx context.Context,
	appID string,
	dockerOp func(context.Context, docker.ComposeProject) error,
) (project docker.ComposeProject, composeProject, errMsg string) {
	stackPath, err := security.SafeJoin(e.stackBase, appID)
	if err != nil {
		return docker.ComposeProject{}, "", fmt.Sprintf("app id is unsafe: %v", err)
	}

	handle, err := acquireInstallStackLock(ctx, stackPath)
	if err != nil {
		return docker.ComposeProject{}, "", err.Error()
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	lock, err := reconfirmManagedStack(handle, appID)
	if err != nil {
		return docker.ComposeProject{}, "", err.Error()
	}
	if lock.ComposeProject == "" {
		return docker.ComposeProject{}, "", "stack manifest is missing its compose project"
	}
	composeProject = lock.ComposeProject

	project, err = logsComposeProject(stackPath, lock.ComposeProject)
	if err != nil {
		return docker.ComposeProject{}, composeProject, err.Error()
	}

	if err := dockerOp(ctx, project); err != nil {
		return project, composeProject, err.Error()
	}
	return project, composeProject, ""
}
