package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// This file holds [Engine.UpdateSettings] plus the shared
// [acquireRuntimeLock] helper.
// [Engine.UpdateSettings] honors the engine.Engine contract by checking
// the closed flag first ([ErrClosed] takes precedence when the engine is
// closed), then acquires the global runtime.lock per PRD §26 /
// exit criterion 396 before validating and persisting the settings.
// Callback parameter types reference pkg/types directly
// (types.ProgressFn, types.LogLineFn, types.Confirmer) rather than the
// pkg/engine aliases: internal/core must not import pkg/engine, and the
// callback types were relocated to pkg/types to break that import cycle.
// pkg/engine keeps type aliases (engine.ProgressFn = types.ProgressFn) so
// external callers see no surface change.

// acquireRuntimeLock prepares the engine's state dir and acquires the
// global runtime.lock under it (PRD §26), attributed to the supplied
// command name. The caller MUST release the returned handle via
// `defer handle.Release` — each state-changing method owns the release
// lifecycle so the acquire/release brackets stay visible at the call site
// rather than buried in a helper's defer.
// On contention the error is a [*types.Error] carrying
// [types.ErrCodeRuntimeLockHeld] so cmd/wdm maps it to PRD §27 exit
// code 4 via errors.As; the underlying [state.LockHeldError] /
// [state.ErrRuntimeLockHeld] chain stays detectable via errors.Is for
// tests and audit logs. Other failures (mkdir, flock syscall, JSON
// write) are wrapped with the "core.acquireRuntimeLock:" prefix.
// MkdirAll is needed because [state.AcquireRuntimeLock] opens the lock
// file with O_CREATE but does not create the parent directory — on a
// fresh box with no XDG_STATE_HOME the open would fail with ENOENT. Mode
// 0o700 matches the XDG Base Directory Specification's 0700 state-dir
// recommendation: state data is per-user, never world-readable.
// The ctx.Err check runs BEFORE MkdirAll so a caller that canceled
// before a state-changing call leaves no on-disk artifact.
// state.AcquireRuntimeLock also rejects a canceled ctx, but only after
// MkdirAll has already run — too late for the directory creation.
func (e *Engine) acquireRuntimeLock(ctx context.Context, command string) (*state.RuntimeLockHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.acquireRuntimeLock: %w", err)
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("core.acquireRuntimeLock: ensuring state dir: %w", err)
	}
	handle, err := state.AcquireRuntimeLock(
		ctx,
		filepath.Join(e.stateDir, runtimeLockFilename),
		state.RuntimeLockMetadata{Command: command, WDMVersion: e.version},
	)
	if err != nil {
		var lockHeld *state.LockHeldError
		if errors.As(err, &lockHeld) {
			hint := "wait for the in-progress operation to finish"
			if lockHeld.Holder.Command != "" {
				hint = fmt.Sprintf("in-progress: %q (pid %d)", lockHeld.Holder.Command, lockHeld.Holder.PID)
			}
			return nil, types.WrapError(types.ErrCodeRuntimeLockHeld, "another wdm operation is in progress", hint, err)
		}
		return nil, fmt.Errorf("core.acquireRuntimeLock: %w", err)
	}
	return handle, nil
}

// configNetworkNamePattern is the catalog network-name schema reproduced
// for the [Settings.DefaultDockerNetwork] validation arm of
// [Engine.UpdateSettings]. It is byte-identical to
// internal/docker/network.go's networkNamePattern and to the catalog
// network-name schema (catalog/schema.json) — lowercase ASCII, leading
// letter, then lowercase letters/digits/underscore/hyphen, length 1-63.
// internal/docker exposes no name-only validator (only EnsureNetwork,
// which performs Docker calls), so 's regex is compiled locally
// rather than reaching across the package boundary for a Docker
// round-trip just to validate a string. This is STRICTER than
// config/schema.json's broader Docker pattern
// (^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$): the matrix binds the
// stricter catalog form, and the pre-write check below runs before the
// loader round-trip, so the stricter rule decides first.
var configNetworkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// UpdateSettings validates s and persists it to config.toml (PRD §29,
// §34). It acquires the global runtime.lock (Command "update-settings" to
// disambiguate from "update", the stack lifecycle command), defers
// release, validates every field of s against the matrix, then
// atomically writes the TOML to the engine's config path. The write is
// config-only: no deployed-app reconciliation, no stack-directory access
// (§34:1074, §30:821).
// Validation (all arms → [types.ErrCodeUsageValidation], fail-closed,
// config.toml untouched on any reject):
//  1. SchemaVersion MUST equal 1 (the locked v1 schema, §34).
//  2. UpdateCheckPreference MUST be one of manual / daily-on-launch /
//     disabled (§34:1060-1062).
//  3. CatalogChannel MUST equal "stable" (§34:1041 lock).
//  4. BaseStackPath MUST pass the install path's stack-root posture:
//     leading "~/" expanded, the result absolute, and
//     [security.RejectUnsafeRoot] clean.
//  5. DefaultDockerNetwork MUST match the catalog network-name schema
//     (see [configNetworkNamePattern]).
//  6. Timezone: an empty string is legal — the documented
//     detect-at-install sentinel (pkg/types.Settings.Timezone) — and a
//     non-empty value MUST resolve via [time.LoadLocation], matching the
//
// After validation, s is marshaled with the loader's TOML module
// (BurntSushi/toml) and round-tripped through [state.LoadConfigBytes]
// BEFORE the write, so the bytes provably parse and schema-validate
// (defense against struct-tag drift). The file is written via
// [state.WriteFileAtomic] at 0o600: config.toml carries no secrets, but a
// per-user mode keeps it consistent with the rest of wdm's XDG state and
// the existing lock-file mode.
// Because 0o600 equals [security.SecretFileMode], the write takes
// [state.WriteFileAtomic]'s secret-mode path, which calls
// [security.RejectInsecureParent] on the config parent. A group- or
// world-writable config parent therefore refuses the save with
// [types.ErrCodePermissionDenied] (PRD §27 exit 6) rather than writing
// into it, while a missing nested parent on a fresh box is created at
// [state.GeneratedDirMode] and the write succeeds.
func (e *Engine) UpdateSettings(ctx context.Context, s types.Settings) error {
	if e.isClosed() {
		return ErrClosed
	}
	handle, err := e.acquireRuntimeLock(ctx, "update-settings")
	if err != nil {
		return err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup

	if err := validateSettings(s); err != nil {
		return err
	}

	raw, err := toml.Marshal(s)
	if err != nil {
		return fmt.Errorf("core.UpdateSettings: marshaling settings: %w", err)
	}

	// Round-trip the marshaled bytes through the loader before writing so
	// the file provably parses and schema-validates, catching any drift
	// between the toml/json struct tags and config/schema.json before a
	// malformed config.toml reaches disk; the loader wraps
	// [types.ErrConfigInvalid] (→ exit code 2) on failure.
	if _, err := state.LoadConfigBytes(ctx, raw); err != nil {
		return fmt.Errorf("core.UpdateSettings: validating marshaled settings: %w", err)
	}

	if err := state.WriteFileAtomic(e.configPath, raw, 0o600); err != nil {
		return fmt.Errorf("core.UpdateSettings: writing config: %w", err)
	}
	return nil
}

// validateSettings enforces the settings validation matrix. Every
// rejection is a [types.ErrCodeUsageValidation] error so cmd/wdm maps it
// to PRD §27 exit code 2, and the caller writes nothing on a non-nil
// return. The checks run before any marshal/write, so a reject leaves
// config.toml byte-identical.
func validateSettings(s types.Settings) error {
	if s.SchemaVersion != 1 {
		return usageValidationError(
			"config schema_version must be 1",
			"set schema_version = 1 (the locked v1 config schema)",
			fmt.Errorf("schema_version %d is not supported", s.SchemaVersion),
		)
	}

	switch s.UpdateCheckPreference {
	case "manual", "daily-on-launch", "disabled":
	default:
		return usageValidationError(
			"update_check_preference is invalid",
			"set update_check_preference to one of: manual, daily-on-launch, disabled",
			fmt.Errorf("update_check_preference %q is not a recognized value", s.UpdateCheckPreference),
		)
	}

	if s.CatalogChannel != "stable" {
		return usageValidationError(
			"catalog_channel is invalid",
			"set catalog_channel to \"stable\" (the only channel available in v1)",
			fmt.Errorf("catalog_channel %q is not supported", s.CatalogChannel),
		)
	}

	if err := validateBaseStackPath(s.BaseStackPath); err != nil {
		return err
	}

	if !configNetworkNamePattern.MatchString(s.DefaultDockerNetwork) {
		return usageValidationError(
			"default_docker_network is invalid",
			"use lowercase ascii, start with a letter, then lowercase letters/digits/underscore/hyphen, length 1-63",
			fmt.Errorf("default_docker_network %q does not match allowed format", s.DefaultDockerNetwork),
		)
	}

	return validateSettingsTimezone(s.Timezone)
}

// validateBaseStackPath applies the install path's stack-root posture to
// the configured base stack path: leading "~/" expanded, the result
// absolute, and [security.RejectUnsafeRoot] clean (mirroring
// planInstallStackPath in install.go). An empty path is rejected here
// rather than deferred to install — config.toml's schema marks
// base_stack_path required, minLength 1, so persisting an empty value
// would write a config the loader then refuses.
func validateBaseStackPath(basePath string) error {
	if basePath == "" {
		return usageValidationError(
			"base_stack_path is required",
			"set base_stack_path to a directory under your home, e.g. ~/docker",
			errors.New("base_stack_path is empty"),
		)
	}
	expanded, err := expandHome(basePath)
	if err != nil {
		return fmt.Errorf("core.UpdateSettings: expanding base stack path: %w", err)
	}
	if !filepath.IsAbs(expanded) {
		return usageValidationError(
			"base_stack_path must be absolute",
			"set base_stack_path to an absolute path or one starting with ~/",
			fmt.Errorf("base stack path %q is not absolute", basePath),
		)
	}
	if err := security.RejectUnsafeRoot(expanded); err != nil {
		return usageValidationError(
			"base_stack_path is unsafe",
			"choose a base stack path under your home directory",
			err,
		)
	}
	return nil
}

// validateSettingsTimezone enforces the Timezone arm: an empty string is
// the documented detect-at-install sentinel
// (pkg/types.Settings.Timezone) and is accepted, while a non-empty value
// MUST resolve through [time.LoadLocation] — the the confirmation rules
// lookup contract the install path validates with (validateTimezone in
// install.go). time.LoadLocation is called directly (no test seam)
// because it consults the host tzdata the same way the install path does.
func validateSettingsTimezone(tz string) error {
	if tz == "" {
		return nil
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return usageValidationError(
			"timezone is invalid",
			"set timezone to a valid IANA timezone such as Europe/Bratislava, or leave it empty to detect at install",
			err,
		)
	}
	return nil
}
