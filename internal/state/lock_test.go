//go:build unix

package state_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// validStackLockJSON matches the shape from lines
// 244-268: every required field, the schema_version=1 const, and a
// non-nil last_successful_operation. Reused by lock_test, scanner_test,
// and the catalog-corruption matrix.
const validStackLockJSON = `{
  "schema_version": 1,
  "app_id": "vaultwarden",
  "template_name": "vaultwarden",
  "template_version": "1.2.3",
  "catalog_channel": "stable",
  "catalog_version": "2026.05.01",
  "stack_path": "/home/test/docker/vaultwarden",
  "selected_domain": "vault.example.com",
  "local_ports": [3012, 8080],
  "compose_project": "wdm-vaultwarden",
  "image_pins": [
    { "service": "app", "image": "vaultwarden/server", "tag": "1.30.1", "digest": "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" }
  ],
  "generated_fields": ["DB_PASSWORD", "ADMIN_TOKEN"],
  "last_successful_operation": {
    "kind": "install",
    "at": "2026-05-19T09:14:33Z",
    "wdm_version": "0.1.0"
  },
  "backup_history": []
}`

func writeLockFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".wdm.lock")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestReadStackLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.ReadStackLock(t.Context(), "relative/.wdm.lock")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestReadStackLock_RejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := state.ReadStackLock(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestReadStackLock_MissingFileWrapsNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".wdm.lock")

	_, err := state.ReadStackLock(t.Context(), path)
	require.Error(t, err)
	// The scanner depends on this distinction: missing → "user-owned
	// directory, silently skip"; stale → "corrupt managed stack,
	// surface as warning."
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"missing file must wrap os.ErrNotExist; got %v", err)
	assert.False(t, errors.Is(err, types.ErrStaleState),
		"missing file must NOT wrap ErrStaleState; got %v", err)
}

func TestReadStackLock_EmptyFileIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, "")

	_, err := state.ReadStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState),
		"empty file must wrap ErrStaleState; got %v", err)
	assert.Contains(t, err.Error(), "interrupted write")
}

func TestReadStackLock_BadJSONIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, "{ not valid json")

	_, err := state.ReadStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
}

func TestReadStackLock_BadSchemaVersionIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, `{
  "schema_version": 99,
  "app_id": "test",
  "template_name": "test",
  "template_version": "1.0.0",
  "catalog_channel": "stable",
  "catalog_version": "v1",
  "stack_path": "/tmp/test",
  "compose_project": "wdm-test",
  "last_successful_operation": null
}`)

	_, err := state.ReadStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
	assert.Contains(t, err.Error(), "schema_version 99")
}

func TestReadStackLock_ValidFile(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	lock, err := state.ReadStackLock(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, lock)

	assert.Equal(t, 1, lock.SchemaVersion)
	assert.Equal(t, "vaultwarden", lock.AppID)
	assert.Equal(t, "vaultwarden", lock.TemplateName)
	assert.Equal(t, "1.2.3", lock.TemplateVersion)
	assert.Equal(t, "stable", lock.CatalogChannel)
	assert.Equal(t, "2026.05.01", lock.CatalogVersion)
	assert.Equal(t, "/home/test/docker/vaultwarden", lock.StackPath)
	assert.Equal(t, "vault.example.com", lock.SelectedDomain)
	assert.Equal(t, []int{3012, 8080}, lock.LocalPorts)
	assert.Equal(t, "wdm-vaultwarden", lock.ComposeProject)
	assert.Len(t, lock.ImagePins, 1)
	assert.Equal(t, "app", lock.ImagePins[0].Service)
	assert.Equal(t, "vaultwarden/server", lock.ImagePins[0].Image)
	assert.Equal(t, "1.30.1", lock.ImagePins[0].Tag)
	assert.Equal(t, []string{"DB_PASSWORD", "ADMIN_TOKEN"}, lock.GeneratedFields)

	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "install", lock.LastSuccessfulOperation.Kind)
	assert.Equal(t, "0.1.0", lock.LastSuccessfulOperation.WDMVersion)
	assert.Equal(t,
		time.Date(2026, 5, 19, 9, 14, 33, 0, time.UTC),
		lock.LastSuccessfulOperation.At.UTC())
}

// TestReadStackLock_NullLastOperation covers the interrupted-install
// signal: a nil LastSuccessfulOperation pointer is load-bearing per
// must round-trip through the reader unchanged.
func TestReadStackLock_NullLastOperation(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, `{
  "schema_version": 1,
  "app_id": "incomplete",
  "template_name": "incomplete",
  "template_version": "1.0.0",
  "catalog_channel": "stable",
  "catalog_version": "v1",
  "stack_path": "/tmp/incomplete",
  "compose_project": "wdm-incomplete",
  "last_successful_operation": null
}`)

	lock, err := state.ReadStackLock(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, lock)
	assert.Nil(t, lock.LastSuccessfulOperation,
		"null last_successful_operation must round-trip as a nil pointer (interrupted-install signal)")
}

func TestReadStackLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.ReadStackLock(ctx, path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestAcquireStackLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.AcquireStackLock(t.Context(), "relative/.wdm.lock")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestAcquireStackLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".wdm.lock")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.AcquireStackLock(ctx, path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestAcquireStackLock_MissingFileCreatesAndStartsEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".wdm.lock")

	_, err := os.Stat(path)
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	assert.Equal(t, path, handle.Path())
	assert.Nil(t, handle.Lock())

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

func TestAcquireStackLock_ValidExistingFile(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock := handle.Lock()
	require.NotNil(t, lock)
	assert.Equal(t, "vaultwarden", lock.AppID)
	assert.Equal(t, "1.2.3", lock.TemplateVersion)
}

func TestAcquireStackLock_ExistingEmptyFileIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, "")

	_, err := state.AcquireStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
}

func TestAcquireStackLock_BadJSONIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, "{ bad json")

	_, err := state.AcquireStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
}

func TestAcquireStackLock_BadSchemaVersionIsStale(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, `{
  "schema_version": 99,
  "app_id": "test",
  "template_name": "test",
  "template_version": "1.0.0",
  "catalog_channel": "stable",
  "catalog_version": "v1",
  "stack_path": "/tmp/test",
  "compose_project": "wdm-test",
  "last_successful_operation": null
}`)

	_, err := state.AcquireStackLock(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrStaleState))
}

func TestStackLockHandle_WritePreservesInode(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)
	before := fileINode(t, path)

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock := handle.Lock()
	require.NotNil(t, lock)
	lock.TemplateVersion = "2.0.0"

	require.NoError(t, handle.Write(*lock))

	after := fileINode(t, path)
	assert.Equal(t, before, after, "write must be in-place on the held inode")
}

func TestStackLockHandle_WritePersistsAndUpdatesCurrentLock(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock := handle.Lock()
	require.NotNil(t, lock)
	lock.TemplateVersion = "2.5.0"
	lock.CatalogVersion = "2026.07.01"
	lock.SelectedDomain = "new.example.com"

	require.NoError(t, handle.Write(*lock))

	got := handle.Lock()
	require.NotNil(t, got)
	assert.Equal(t, "2.5.0", got.TemplateVersion)
	assert.Equal(t, "2026.07.01", got.CatalogVersion)
	assert.Equal(t, "new.example.com", got.SelectedDomain)

	require.NoError(t, handle.Release())

	onDisk, err := state.ReadStackLock(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, "2.5.0", onDisk.TemplateVersion)
	assert.Equal(t, "2026.07.01", onDisk.CatalogVersion)
	assert.Equal(t, "new.example.com", onDisk.SelectedDomain)
}

func TestStackLockHandle_WritePersistsRecommendedResources(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock := handle.Lock()
	require.NotNil(t, lock)
	require.Nil(t, lock.RecommendedResources, "field absent in older locks must parse as nil")
	lock.RecommendedResources = &state.RecommendedResources{
		MemoryBytes: 6 * 1024 * 1024 * 1024,
		CPUs:        2.5,
	}
	require.NoError(t, handle.Write(*lock))

	// The handle clones the manifest on Write: mutating the caller's
	// value afterwards must not leak into the held snapshot.
	lock.RecommendedResources.MemoryBytes = 1

	got := handle.Lock()
	require.NotNil(t, got)
	require.NotNil(t, got.RecommendedResources)
	assert.Equal(t, uint64(6*1024*1024*1024), got.RecommendedResources.MemoryBytes)
	assert.InDelta(t, 2.5, got.RecommendedResources.CPUs, 0.0001)

	require.NoError(t, handle.Release())

	onDisk, err := state.ReadStackLock(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, onDisk.RecommendedResources)
	assert.Equal(t, uint64(6*1024*1024*1024), onDisk.RecommendedResources.MemoryBytes)
	assert.InDelta(t, 2.5, onDisk.RecommendedResources.CPUs, 0.0001)
}

// TestStackLockHandle_WritePersistsCompletedServices mirrors the
// RecommendedResources persistence test for the completed_services
// field: a lock written before the field existed parses as nil (back
// compat), the value round-trips through Write and ReadStackLock, and the
// handle clones the slice on Write so a later caller-side mutation cannot
// leak into the held snapshot.
func TestStackLockHandle_WritePersistsCompletedServices(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock := handle.Lock()
	require.NotNil(t, lock)
	require.Nil(t, lock.CompletedServices, "field absent in older locks must parse as nil")
	lock.CompletedServices = []string{"mongo-init", "garage-init"}
	require.NoError(t, handle.Write(*lock))

	// The handle clones the manifest on Write: mutating the caller's
	// slice afterwards must not leak into the held snapshot.
	lock.CompletedServices[0] = "tampered"

	got := handle.Lock()
	require.NotNil(t, got)
	assert.Equal(t, []string{"mongo-init", "garage-init"}, got.CompletedServices)

	require.NoError(t, handle.Release())

	onDisk, err := state.ReadStackLock(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, []string{"mongo-init", "garage-init"}, onDisk.CompletedServices)
}

func TestStackLockHandle_ReleaseIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".wdm.lock")

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)

	require.NoError(t, handle.Release())
	assert.NoError(t, handle.Release())
}

func TestStackLockHandle_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()

	var handle *state.StackLockHandle
	assert.Equal(t, "", handle.Path())
	assert.Nil(t, handle.Lock())
	assert.NoError(t, handle.Release())

	err := handle.Write(state.StackLock{SchemaVersion: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestHelperHoldsStackLock(t *testing.T) {
	if os.Getenv(helperEnvVar) != "stack" {
		t.Skip("helper-only test; gated on " + helperEnvVar + "=stack")
	}

	path := os.Getenv(helperPathEnvVar)
	signal := os.Getenv(helperSignalEnv)
	require.NotEmpty(t, path, helperPathEnvVar+" must be set when helper runs")
	require.NotEmpty(t, signal, helperSignalEnv+" must be set when helper runs")

	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err, "helper subprocess failed to acquire the stack lock")
	defer handle.Release()

	require.NoError(t, os.WriteFile(signal, []byte("ok"), 0o600))

	time.Sleep(helperHoldFor)
}

func TestStackLockHandle_HoldsFlockUntilRelease(t *testing.T) {
	t.Parallel()

	lockPath, _ := startStackLockHelper(t)

	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	got, err := state.TryLockExclusive(f)
	require.NoError(t, err, "held StackLockHandle flock must surface as clean contention")
	assert.False(t, got, "stack lock flock must remain held until StackLockHandle.Release closes the fd")
}

func startStackLockHelper(t *testing.T) (string, *exec.Cmd) {
	t.Helper()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wdm.lock")
	signal := filepath.Join(dir, "acquired.signal")

	cmd := exec.Command(
		os.Args[0],
		"-test.run", "^TestHelperHoldsStackLock$",
		"-test.timeout", "60s",
	)
	cmd.Env = append(os.Environ(),
		helperEnvVar+"=stack",
		helperPathEnvVar+"="+lockPath,
		helperSignalEnv+"="+signal,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	require.NoError(t, cmd.Start(), "starting stack-lock helper subprocess")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(signal); err == nil {
			return lockPath, cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("stack-lock helper subprocess did not signal lock acquisition within 5s at %q", signal)
	return "", nil
}

func fileINode(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return st.Ino
}

func TestTryReadStackLock_ValidFile(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)

	lock, err := state.TryReadStackLock(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, lock)
	assert.Equal(t, "vaultwarden", lock.AppID)
	assert.Equal(t, "wdm-vaultwarden", lock.ComposeProject)
	assert.Equal(t, []int{3012, 8080}, lock.LocalPorts)
	require.NotNil(t, lock.LastSuccessfulOperation)
}

func TestTryReadStackLock_MissingFileWrapsNotExist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".wdm.lock")

	lock, err := state.TryReadStackLock(t.Context(), path)
	require.Error(t, err)
	assert.Nil(t, lock)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NotErrorIs(t, err, types.ErrStaleState)
}

// TestTryReadStackLock_BusyWhileExclusiveHeld is the load-bearing
// contention proof for the read-only status path: while a writer
// holds the per-stack LOCK_EX (here via [state.AcquireStackLock] on
// its own open file description), TryReadStackLock returns the
// wrapped [state.ErrStackLockBusy] sentinel promptly instead of
// blocking — and succeeds again after the writer releases.
func TestTryReadStackLock_BusyWhileExclusiveHeld(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)
	handle, err := state.AcquireStackLock(t.Context(), path)
	require.NoError(t, err)
	defer handle.Release()

	lock, readErr := state.TryReadStackLock(t.Context(), path)
	require.Error(t, readErr)
	assert.Nil(t, lock)
	require.ErrorIs(t, readErr, state.ErrStackLockBusy)
	assert.NotErrorIs(t, readErr, types.ErrStaleState)

	require.NoError(t, handle.Release())

	// Eventually as robustness only: Release now unlocks explicitly
	// before closing the fd, so same-process readers observe the
	// release deterministically and this converges on the first poll.
	// The poll guards the historical fork-inheritance window (a child
	// forked between Release and exec kept the open file description
	// alive until CLOEXEC) from ever flaking this test again.
	require.Eventually(t, func() bool {
		readable, eventualErr := state.TryReadStackLock(context.Background(), path)
		if eventualErr != nil {
			return false
		}
		lock = readable
		return true
	}, 5*time.Second, 10*time.Millisecond,
		"released stack lock must become readable")
	require.NotNil(t, lock)
	assert.Equal(t, "vaultwarden", lock.AppID)
}

func TestTryReadStackLock_CorruptShapesAreStale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		contents string
	}{
		{name: "empty file", contents: ""},
		{name: "bad json", contents: `{not-json`},
		{name: "bad schema version", contents: `{"schema_version": 99}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeLockFile(t, tt.contents)

			lock, err := state.TryReadStackLock(t.Context(), path)
			require.Error(t, err)
			assert.Nil(t, lock)
			require.ErrorIs(t, err, types.ErrStaleState)
			assert.NotErrorIs(t, err, state.ErrStackLockBusy)
		})
	}
}

func TestTryReadStackLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.TryReadStackLock(t.Context(), "relative/.wdm.lock")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestTryReadStackLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	path := writeLockFile(t, validStackLockJSON)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lock, err := state.TryReadStackLock(ctx, path)
	require.Error(t, err)
	assert.Nil(t, lock)
	require.ErrorIs(t, err, context.Canceled)
}
