//go:build unix

package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/wnstify/wdm/pkg/types"
)

// The.wdm.lock schema migration framework (PRD §30:825-829).
// PRD §30 requires that state migrations be "explicit, tested, logged,
// and backed up before they run" and that "failed migrations must leave
// existing stacks untouched". This file implements that contract for the
// per-stack .wdm.lock manifest only — runtime.lock is a DISTINCT file with
// its own runtimeLockSchemaVersion and additive-only growth, and never
// passes through this framework.
// The framework is wired into [AcquireStackLock], the write-path open used
// by every state-changing operation. When a managed stack carries an older
// schema_version than [stackLockSchemaVersion], the held flock is already
// in hand, so the open path can back up the on-disk config, apply the
// explicit migration chain in memory, persist the migrated manifest through
// the held fd, and only then return the handle. The read-only entry points
// ([ReadStackLock], [TryReadStackLock]) never migrate — they keep refusing
// non-current versions with [types.ErrStaleState], because a read-only
// command must never write (PRD §26 read-only clause).
// Honesty note: no commit currently bumps the
// .wdm.lock schema. The framework ships with a single registered
// schema-0 → schema-1 identity migration so the chain is provable
// end-to-end against a synthetic older-version fixture. Version 0 never
// existed in the wild (schema versions started at 1), so the registered
// migration is harmless in production and exists to exercise the framework
// as a the invariant precondition guard.

// stackLockMigration is a single explicit migration step that upgrades a
// [StackLock] from FromVersion to FromVersion+1. The Migrate func mutates
// the manifest in place and MUST set SchemaVersion to FromVersion+1 (the
// chain runner asserts this so a migration that forgets to bump the version
// fails closed rather than looping).
// Migrations are explicit values registered in [stackLockMigrations] — the
// framework never reflects over fields or infers defaults. A new schema
// version is introduced by registering a new step keyed at the prior
// version; the chain runs each step in turn until [stackLockSchemaVersion]
// is reached.
type stackLockMigration struct {
	// FromVersion is the schema_version this step upgrades away from. The
	// step produces FromVersion+1.
	FromVersion int

	// Migrate applies the explicit field transformation in place and sets
	// the manifest's SchemaVersion to FromVersion+1. It returns an error to
	// fail the migration closed (the on-disk lock is then left untouched).
	Migrate func(lock *StackLock) error
}

// stackLockMigrations is the explicit registry of.wdm.lock migrations keyed
// by FromVersion. It is a package var rather than a const because Go has no
// const maps; it is treated as immutable after package init and is only ever
// swapped wholesale by the test seam (SwapStackLockMigrationsForTest) so a
// test can inject a synthetic chain or a deliberately failing step.
// The sole production entry is the schema-0 → schema-1 identity migration
// (see the package doc's honesty note). Its body only bumps the version: a
// v0 manifest never existed in the wild, so there are no fields to
// transform, but registering a real step keeps the framework non-empty and
// provable.
var stackLockMigrations = map[int]stackLockMigration{
	0: {
		FromVersion: 0,
		Migrate: func(lock *StackLock) error {
			// Identity migration: schema 0 → 1. v0 never shipped, so no
			// field transformation is owed; the version bump is the whole
			// step. A real future migration replaces this body with the
			// field changes the bump introduces.
			lock.SchemaVersion = 1
			return nil
		},
	},
}

// AcquireOption configures optional behavior of [AcquireStackLock]. Options
// are applied in order; later options win. The zero set (no options) keeps
// the historical behavior, so existing two-argument callers keep compiling.
type AcquireOption func(*acquireConfig)

// acquireConfig holds the resolved optional behavior for one
// [AcquireStackLock] call. logger is never nil after resolution — a nil
// caller logger is replaced with a discard logger so every migration log
// site can call it unconditionally.
type acquireConfig struct {
	logger *slog.Logger
}

// WithMigrationLogger supplies the [*slog.Logger] the .wdm.lock migration
// framework logs to when [AcquireStackLock] migrates an older manifest
// (PRD §30: migrations must be logged). A nil logger, or omitting the
// option entirely, routes migration log records to a discard handler — the
// migration still runs, it is simply not recorded.
// Only identifiers are logged (from/to schema version, stack path, backup
// snapshot path); raw manifest bytes are never logged, so wdm-generated
// field names that the manifest carries never reach a sink with their
// values (the values live in the rendered .env, never in the manifest).
func WithMigrationLogger(logger *slog.Logger) AcquireOption {
	return func(cfg *acquireConfig) {
		cfg.logger = logger
	}
}

// resolveAcquireConfig folds opts onto the defaults and guarantees a
// non-nil logger so callers downstream never branch on nil.
func resolveAcquireConfig(opts []AcquireOption) acquireConfig {
	cfg := acquireConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.logger == nil {
		// slog.DiscardHandler (Go 1.24+) drops every record, so the
		// framework can log unconditionally without a nil check and without
		// pulling internal/logging into this leaf package.
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	return cfg
}

// migrateOlderStackLock backs up, migrates, and persists an older-schema
// .wdm.lock through the already-held flock fd, returning the migrated
// manifest. It is the framework's fail-closed entry point and is called by
// [AcquireStackLock] only after the exclusive flock is held and only when
// the on-disk schema_version is older than [stackLockSchemaVersion].
// Ordering is load-bearing and fail-closed (PRD §30:828-829):
//  1. Run the explicit migration chain entirely IN MEMORY. A chain gap, a
//     failing step, or a step that fails to advance the version is caught
//     here, before a single byte of the stack is touched.
//  2. Back up the stack's config files via [CreateConfigBackup] under the
//     stack-local.wdm-backups/ directory. Nothing on disk has changed yet,
//     so a backup failure leaves the stack byte-identical.
//  3. Persist the migrated manifest through the held fd using the same
//     in-place truncate/write/fsync protocol [StackLockHandle.Write] uses.
//     This is the only on-disk mutation; a failure here leaves the original
//     bytes (the truncate is the first write syscall, so a fault before it
//     leaves the file untouched, and the backup taken in step 2 is the
//     recovery path the PRD §30 contract promises).
//
// Every failure returns a [types.ErrCodeMigrationFailure] *Error with the
// underlying cause reachable via errors.Is/As, and (except for the persist
// step, which is the commit point) leaves the on-disk lock untouched.
func migrateOlderStackLock(
	stackPath string,
	path string,
	f writableFile,
	fromVersion int,
	raw []byte,
	logger *slog.Logger,
) (*StackLock, error) {
	migrated, err := runStackLockMigrations(raw, fromVersion)
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeMigrationFailure,
			"stack lock migration failed",
			"the stack was left untouched; report this so the schema can be migrated",
			fmt.Errorf("state.migrateOlderStackLock %q: %w", path, err),
		)
	}

	// Back up BEFORE the first on-disk byte changes (PRD §30:828). The
	// snapshot walks the stack's standard config files; the operation name
	// "migration" satisfies validateBackupOperation's lowercase schema.
	backupPath, err := backupBeforeMigration(stackPath)
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeMigrationFailure,
			"stack lock migration failed",
			"the stack was left untouched; the pre-migration backup could not be created",
			fmt.Errorf("state.migrateOlderStackLock %q: backing up before migration: %w", path, err),
		)
	}

	if err := writeStackLockThroughHeldFile(f, *migrated); err != nil {
		return nil, types.WrapError(
			types.ErrCodeMigrationFailure,
			"stack lock migration failed",
			fmt.Sprintf("a pre-migration backup was saved at %q", backupPath),
			fmt.Errorf("state.migrateOlderStackLock %q: persisting migrated manifest: %w", path, err),
		)
	}

	logger.Info(
		"migrated .wdm.lock schema",
		slog.String("stack_path", stackPath),
		slog.Int("from_schema_version", fromVersion),
		slog.Int("to_schema_version", migrated.SchemaVersion),
		slog.String("backup_path", backupPath),
	)

	return migrated, nil
}

// backupBeforeMigration is the seam the test harness overrides to prove the
// fail-closed ordering (e.g. assert the backup exists before the persist
// runs, or inject a backup failure). Production routes straight to
// [CreateConfigBackup] with the reserved "migration" operation name.
var backupBeforeMigration = func(stackPath string) (string, error) {
	return CreateConfigBackup(stackPath, "migration", nil)
}

// runStackLockMigrations decodes raw and runs the explicit migration chain
// from fromVersion up to [stackLockSchemaVersion], returning the migrated
// manifest. The whole chain runs in memory: raw is never mutated and no
// on-disk side effect occurs here.
// Fail-closed cases (all returned as plain errors the caller wraps with
// [types.ErrCodeMigrationFailure]):
//   - fromVersion is not older than the current version — a programmer
//     error, since the caller only invokes this on an older lock.
//   - the JSON does not decode — though the caller's version probe already
//     parsed schema_version, the full decode is re-validated here.
//   - no registered migration exists for some version in the chain (a gap),
//     so the lock cannot reach the current version.
//   - a migration step returns an error.
//   - a step does not advance the version to exactly fromVersion+1, which
//     would otherwise loop or silently corrupt the chain.
func runStackLockMigrations(raw []byte, fromVersion int) (*StackLock, error) {
	if fromVersion >= stackLockSchemaVersion {
		return nil, fmt.Errorf(
			"refusing to migrate schema_version %d: not older than current version %d",
			fromVersion, stackLockSchemaVersion,
		)
	}

	var lock StackLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, fmt.Errorf("decoding manifest at schema_version %d: %w", fromVersion, err)
	}

	for current := fromVersion; current < stackLockSchemaVersion; {
		migration, ok := stackLockMigrations[current]
		if !ok {
			return nil, fmt.Errorf(
				"no migration registered for schema_version %d (chain to current version %d is incomplete)",
				current, stackLockSchemaVersion,
			)
		}

		if err := migration.Migrate(&lock); err != nil {
			return nil, fmt.Errorf("migrating schema_version %d to %d: %w", current, current+1, err)
		}

		next := current + 1
		if lock.SchemaVersion != next {
			return nil, fmt.Errorf(
				"migration from schema_version %d set version to %d, expected %d",
				current, lock.SchemaVersion, next,
			)
		}
		current = next
	}

	return &lock, nil
}

// registeredMigrationFromVersions returns the sorted FromVersion keys of the
// active migration registry. It exists only for the test harness's
// chain-coverage assertions; production code branches on the version number
// directly.
func registeredMigrationFromVersions() []int {
	versions := make([]int, 0, len(stackLockMigrations))
	for version := range stackLockMigrations {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return versions
}
