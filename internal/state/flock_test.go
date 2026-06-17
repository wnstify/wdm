//go:build unix

package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

func TestLockExclusive_AcquiresOnFreshFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer f.Close()

	require.NoError(t, state.LockExclusive(f))
}

func TestTryLockExclusive_TrueOnFreshFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer f.Close()

	got, err := state.TryLockExclusive(f)
	require.NoError(t, err)
	assert.True(t, got, "fresh file must accept the LOCK_EX|LOCK_NB attempt")
}

func TestUnlock_AfterLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer f.Close()

	require.NoError(t, state.LockExclusive(f))
	require.NoError(t, state.Unlock(f))

	// After Unlock, a subsequent TryLockExclusive on the same fd
	// re-acquires successfully.
	got, err := state.TryLockExclusive(f)
	require.NoError(t, err)
	assert.True(t, got)
}

// TestLockExclusive_ErrorOnClosedFile covers the syscall.Flock failure
// path on a closed file descriptor (EBADF). The closed-fd shape is the
// cheapest realistic trigger of the wrapped error — no fixture, no
// permission games, just verifies the "state.LockExclusive: flock:"
// wrap is exercised so a future refactor that breaks the wrapping
// fails loud.
func TestLockExclusive_ErrorOnClosedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	err = state.LockExclusive(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state.LockExclusive")
}

func TestTryLockExclusive_ErrorOnClosedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, err := state.TryLockExclusive(f)
	require.Error(t, err, "TryLockExclusive must distinguish non-EWOULDBLOCK syscall failures from the (false, nil) contention case")
	assert.False(t, got)
	assert.Contains(t, err.Error(), "state.TryLockExclusive")
}

func TestUnlock_ErrorOnClosedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	err = state.Unlock(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state.Unlock")
}

// TestTryLockExclusive_FalseWhileHeldByOtherProcess is the subprocess
// exclusion test for flock(2). The helper test [TestHelperHoldsRuntimeLock]
// runs in a child process spawned by this test; the child acquires the
// runtime lock (which internally calls TryLockExclusive(LOCK_EX|LOCK_NB))
// and holds it via a sleep. We then open the same path here in the
// parent and attempt TryLockExclusive — flock(2) per-OFD semantics on
// Linux mean the parent's attempt must return (false, nil) because the
// child holds the lock.
// Same-process goroutines competing for the same flock would not
// exclude each other (flock is per open-file-description, not
// per-process), so the subprocess pattern is the only correct test of
// the cross-process exclusion invariant PRD §26 depends on.
func TestTryLockExclusive_FalseWhileHeldByOtherProcess(t *testing.T) {
	t.Parallel()

	lockPath, _ := startRuntimeLockHelper(t)

	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	got, err := state.TryLockExclusive(f)
	require.NoError(t, err, "TryLockExclusive must return (false, nil) under contention, never an error")
	assert.False(t, got, "flock must be unavailable while the helper subprocess holds it")
}

func TestTryLockShared_TrueOnFreshFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer f.Close()

	got, err := state.TryLockShared(f)
	require.NoError(t, err)
	assert.True(t, got)
}

// TestTryLockShared_SharedHoldersCoexist proves the read-side
// property the status path depends on: two shared holders on
// distinct open file descriptions never contend with each other, so
// concurrent status checks cannot produce a spurious "operation in
// progress" signal.
func TestTryLockShared_SharedHoldersCoexist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	first, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer second.Close()

	got, err := state.TryLockShared(first)
	require.NoError(t, err)
	require.True(t, got)

	got, err = state.TryLockShared(second)
	require.NoError(t, err)
	assert.True(t, got, "shared holders must not exclude each other")
}

// TestTryLockShared_FalseWhileExclusiveHeld exercises contention via
// two distinct open file descriptions: flock(2) locks belong to the
// open file description, so a second open — even within the same
// process — observes the first description's LOCK_EX exactly the way
// another process would.
func TestTryLockShared_FalseWhileExclusiveHeld(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	writer, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	defer writer.Close()
	got, err := state.TryLockExclusive(writer)
	require.NoError(t, err)
	require.True(t, got)

	reader, err := os.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer reader.Close()

	got, err = state.TryLockShared(reader)
	require.NoError(t, err, "TryLockShared must return (false, nil) under contention, never an error")
	assert.False(t, got, "a shared probe must observe the exclusive holder")
}

func TestTryLockShared_ErrorOnClosedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, err := state.TryLockShared(f)
	require.Error(t, err)
	assert.False(t, got)
	assert.Contains(t, err.Error(), "state.TryLockShared")
}
