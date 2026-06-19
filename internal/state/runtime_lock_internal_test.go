//go:build unix

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validRuntimeLockInfo() RuntimeLockInfo {
	return RuntimeLockInfo{
		SchemaVersion: runtimeLockSchemaVersion,
		PID:           os.Getpid(),
		Command:       "install",
		StartedAt:     time.Now().UTC(),
		WDMVersion:    "0.0.0-test",
	}
}

// TestWriteRuntimeLock_FsyncFailure drives the fsync arm via the
// syncRuntimeLock seam: a real file is opened so truncate/seek/write
// all succeed, then the forced fsync error must surface wrapped under
// the "fsync" context.
// Not parallel: the swap mutates a process-global seam.
func TestWriteRuntimeLock_FsyncFailure(t *testing.T) {
	sentinel := errors.New("fsync denied by seam")
	prev := syncRuntimeLock
	syncRuntimeLock = func(*os.File) error { return sentinel }
	t.Cleanup(func() { syncRuntimeLock = prev })

	path := filepath.Join(t.TempDir(), "runtime.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	err = writeRuntimeLock(f, validRuntimeLockInfo())
	require.Error(t, err, "fsync failure must surface")
	assert.ErrorIs(t, err, sentinel,
		"underlying fsync error must remain reachable")
	assert.Contains(t, err.Error(), "fsync",
		"the error must be wrapped under the fsync context")
}

// TestWriteRuntimeLock_TruncateFailure covers the first error arm
// without a seam: Truncate on an already-closed descriptor fails, and
// the error must be wrapped under the "truncate" context.
func TestWriteRuntimeLock_TruncateFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	err = writeRuntimeLock(f, validRuntimeLockInfo())
	require.Error(t, err, "truncate on a closed fd must fail")
	assert.Contains(t, err.Error(), "truncate",
		"the error must be wrapped under the truncate context")
}
