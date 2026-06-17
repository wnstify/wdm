package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the ValidateConfig engine method (PRD §18:418
// "Validate config", §18:427 compose-validation condition;,
// before any Docker call, a non-blocking shared flock so a busy stack refuses
// fast instead of stalling behind the writer, and zero writes — it never
// acquires, creates, or deletes the runtime.lock (PRD §26).
// Redaction (PRD §11, §24): ValidateConfig reads the stack's.env VALUES into
// memory to register them as literal secrets with a per-operation redactor
// (security.NewActiveRedactor). That redactor both wraps the Docker client
// and scrubs the compose-validation error text before it lands in
// ValidationResult.Detail, so a generated secret interpolated into the config
// cannot leak through the detail string. Reading the .env bytes is sanctioned
// because the path performs zero writes — the bytes never leave this function
// except as redacted literals.
// The.env outcome splits two ways, by whether the file exists on disk:
//   - ABSENT.env (state.ReadStackEnv surfaces os.ErrNotExist in its error
//     chain): nothing on disk to leak, so the redactor degrades to its
//     structural patterns only (env / JSON / Bearer / URL) and validation
//     proceeds — an absent.env is a config condition reported as
//     Valid:false, not a hard error.
//   - PRESENT but rejected by state.ReadStackEnv (malformed / duplicate-key /
//     empty-key / non-regular — forms docker compose itself may tolerate):
//     ValidateConfig FAILS CLOSED with the typed usage-validation error
//     naming the .env problem, BEFORE constructing the Docker client. When
//     the redaction guarantee cannot be established over an existing .env, a
//     BARE secret value (not in KEY=VALUE form, so untouched by the
//     structural env-assignment pattern) embedded in a leaky compose error
//     would reach Detail in cleartext. Refusing is the PRD §11/§24
//     fail-closed posture, and naming the corrupt.env is more actionable
//     than a possibly-leaky Valid:false.

// ValidateConfig runs `docker compose config --quiet` against the managed
// stack's ON-DISK Compose file and reports the outcome as a
// [types.ValidationResult] (PRD §18). It checks what is on
// disk — it does not re-render from the catalog.
// Result semantics: an invalid-but-readable Compose file is a SUCCESS payload
// with Valid false and a redactor-scrubbed Detail, NOT an error — like
// Status, which returns a needs-attention stack at exit 0 so the caller can
// render the detail and still offer next actions. The method returns a
// non-nil error only for operational faults:
//   - an unmanaged directory or uninstalled app ([types.ErrCodeUsageValidation])
//   - a busy stack whose flock a writer holds ([types.ErrCodeRuntimeLockHeld])
//   - a present-but-malformed.env that defeats the redaction guarantee
//     ([types.ErrCodeUsageValidation], fail-closed before any Docker call —
//     see the file header)
//   - an unreachable daemon ([types.ErrCodeDockerUnavailable], propagated
//     unchanged so internal/docker's mapping stays authoritative)
//   - context cancellation, which always propagates as an error
//
// Read-only discipline (PRD §26): the manifest is read through
// [state.TryReadStackLock] (non-blocking shared lock) and the path
// writes nothing.
func (e *Engine) ValidateConfig(ctx context.Context, appID string) (*types.ValidationResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ValidateConfig: %w", err)
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

	redactor, err := validateConfigRedactor(stackPath)
	if err != nil {
		return nil, err
	}

	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return nil, err
	}

	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}

	result := &types.ValidationResult{
		AppID:          appID,
		ComposeProject: lock.ComposeProject,
		ComposeFile:    composePath,
		Valid:          true,
	}

	if validateErr := docker.ValidateComposeConfig(ctx, client, stackPath, composePath); validateErr != nil {
		// Context cancellation and an unreachable daemon are operational
		// faults, not config problems: propagate them unchanged (like Status's
		// compose-validation carve-out) so the caller never reads a false
		// Valid:false.
		if ctx.Err() != nil {
			return nil, validateErr
		}
		if types.IsCode(validateErr, types.ErrCodeDockerUnavailable) {
			return nil, validateErr
		}
		result.Valid = false
		result.Detail = redactor.Redact(validateErr.Error())
	}

	return result, nil
}

// validateConfigRedactor builds the per-operation redactor for one
// ValidateConfig run. It registers the stack's.env VALUES as literal secrets
// so any value interpolated into the compose-validation error text is
// scrubbed to [security.RedactedPlaceholder].
// The.env outcome splits two ways (see the file header for the full
// rationale):
//   - ABSENT.env: [state.ReadStackEnv] surfaces [fs.ErrNotExist] in its
//     error chain. Nothing on disk to leak, so this degrades to the
//     structural-pattern-only redactor ([security.NewActiveRedactor] with no
//     literals) and returns nil so validation proceeds.
//   - PRESENT but rejected (malformed / duplicate-key / empty-key /
//     non-regular): [state.ReadStackEnv] returns a typed usage-validation
//     error with NO [fs.ErrNotExist] in its chain. The redaction guarantee
//     cannot be established over the existing file, so this fails closed by
//     propagating that typed error (wrapped to name the redaction-setup
//     context) — ValidateConfig then refuses before constructing the Docker
//     client.
func validateConfigRedactor(stackPath string) (security.Redactor, error) {
	env, err := state.ReadStackEnv(stackPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return security.NewActiveRedactor(nil), nil
		}
		return nil, fmt.Errorf(
			"core.ValidateConfig: cannot establish redaction over the stack .env: %w",
			err,
		)
	}
	values := make([]string, 0, len(env))
	for _, value := range env {
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return security.NewActiveRedactor(values), nil
}
