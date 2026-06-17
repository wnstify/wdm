//go:build unix

package state

import (
	"testing"
)

// StackLockMigrationForTest mirrors the unexported stackLockMigration so the
// external test package can register synthetic chains (a multi-step chain, a
// deliberately failing step) without exporting the production type. Migrate
// must set lock.SchemaVersion to FromVersion+1, exactly as production
// migrations do.
type StackLockMigrationForTest struct {
	FromVersion int
	Migrate     func(lock *StackLock) error
}

// SwapStackLockMigrationsForTest replaces the active migration registry with
// the supplied steps for the duration of the test, restoring the production
// registry on cleanup. Passing no steps installs an empty registry, which is
// how the chain-gap fail-closed path is exercised.
func SwapStackLockMigrationsForTest(t *testing.T, steps ...StackLockMigrationForTest) {
	t.Helper()

	prev := stackLockMigrations
	replacement := make(map[int]stackLockMigration, len(steps))
	for _, step := range steps {
		replacement[step.FromVersion] = stackLockMigration(step)
	}
	stackLockMigrations = replacement
	t.Cleanup(func() {
		stackLockMigrations = prev
	})
}

// SwapBackupBeforeMigrationForTest overrides the pre-migration backup seam so
// a test can assert ordering (the backup is taken before the persist) or
// inject a backup failure. The production seam routes to CreateConfigBackup
// with the reserved "migration" operation name.
func SwapBackupBeforeMigrationForTest(t *testing.T, fn func(stackPath string) (string, error)) {
	t.Helper()

	prev := backupBeforeMigration
	backupBeforeMigration = fn
	t.Cleanup(func() {
		backupBeforeMigration = prev
	})
}

// RegisteredMigrationFromVersionsForTest exposes the active registry's sorted
// FromVersion keys so tests can assert the production chain is non-empty and
// covers every version up to the current schema.
func RegisteredMigrationFromVersionsForTest() []int {
	return registeredMigrationFromVersions()
}

// StackLockSchemaVersionForTest exposes the current schema version constant so
// the external test package can assert the framework's no-op boundary without
// re-typing the literal.
func StackLockSchemaVersionForTest() int {
	return stackLockSchemaVersion
}
