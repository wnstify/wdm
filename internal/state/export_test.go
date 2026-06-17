//go:build unix

package state

import (
	"os"
	"testing"
	"time"
)

func SwapBackupNowForTest(t *testing.T, now func() time.Time) {
	t.Helper()

	prev := backupNowUTC
	backupNowUTC = now
	t.Cleanup(func() {
		backupNowUTC = prev
	})
}

func SwapBackupWriteFileForTest(
	t *testing.T,
	fn func(path string, data []byte, mode os.FileMode) error,
) {
	t.Helper()

	prev := backupWriteFile
	backupWriteFile = fn
	t.Cleanup(func() {
		backupWriteFile = prev
	})
}

func SwapBackupSyncDirectoryForTest(t *testing.T, fn func(path string) error) {
	t.Helper()

	prev := backupSyncDirectory
	backupSyncDirectory = fn
	t.Cleanup(func() {
		backupSyncDirectory = prev
	})
}

func SwapBackupRemoveAllForTest(t *testing.T, fn func(path string) error) {
	t.Helper()

	prev := backupRemoveAll
	backupRemoveAll = fn
	t.Cleanup(func() {
		backupRemoveAll = prev
	})
}

// SwapProcessStartTimeReaderForTest swaps the package-private
// readProcStartTime seam so runtime-lock PID-reuse tests can supply a
// deterministic start-time (or a forced failure) on platforms with no
// /proc as well as on Linux. The previous reader is restored on cleanup.
func SwapProcessStartTimeReaderForTest(t *testing.T, fn func(pid int) (uint64, bool)) {
	t.Helper()

	prev := readProcStartTime
	readProcStartTime = fn
	t.Cleanup(func() {
		readProcStartTime = prev
	})
}

// ParseProcStatForTest exposes the production /proc/<pid>/stat line parser
// so its hostile-comm handling can be tested directly with a stat-line
// fixture, independent of the live /proc filesystem.
func ParseProcStatForTest(stat string) (uint64, bool) {
	return parseProcStat(stat)
}

// SwapAfterRuntimeLockFlockForTest swaps the package-private
// afterRuntimeLockFlock seam so an acquire-side inode-swap test can run
// at exactly the moment between the flock success and the inode-identity
// verification in AcquireRuntimeLock — unlinking and recreating the lock
// path so the locked fd ends up on a detached inode. The previous hook
// is restored on cleanup.
func SwapAfterRuntimeLockFlockForTest(t *testing.T, fn func()) {
	t.Helper()

	prev := afterRuntimeLockFlock
	afterRuntimeLockFlock = fn
	t.Cleanup(func() {
		afterRuntimeLockFlock = prev
	})
}
