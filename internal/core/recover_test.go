package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
)

const recoverComposeProject = "wdm-recoverapp"

// recoverStackDir creates an orphan-like stack dir under the engine's stack
// base (which lives under $HOME so the within-home guard passes) and writes
// the given .wdm.lock bytes. A nil lockBytes leaves no lock file.
func recoverStackDir(t *testing.T, lockBytes []byte) string {
	t.Helper()

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	base, err := os.MkdirTemp(home, ".wdm-recover-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	stackPath := filepath.Join(base, "recoverapp")
	require.NoError(t, os.MkdirAll(stackPath, 0o700))
	if lockBytes != nil {
		require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".wdm.lock"), lockBytes, 0o600))
	}
	return stackPath
}

// recoverNoContainersClient answers the project container list with an empty
// list so InspectProjectContainers reports no running containers.
func recoverNoContainersClient() *fakeDockerClient {
	return &fakeDockerClient{
		runFn: func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			if fmt.Sprintf("%T", inv) == "docker.projectContainerListInvocation" {
				return docker.CommandResult{}, nil // empty stdout → no containers
			}
			return docker.CommandResult{}, nil
		},
	}
}

// recoverRunningContainerClient answers the list with one container id and
// inspects it as running, so the recovery must refuse.
func recoverRunningContainerClient(t *testing.T) *fakeDockerClient {
	t.Helper()
	id := listStatusContainerID(0x5a)
	inspect := statusContainerInspectStdout(t, "app", "recoverapp", 8080, "running", true, false, 0, "healthy")
	return &fakeDockerClient{
		runFn: func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			switch fmt.Sprintf("%T", inv) {
			case "docker.projectContainerListInvocation":
				return docker.CommandResult{Stdout: id + "\n"}, nil
			case "docker.containerInspectInvocation":
				return docker.CommandResult{Stdout: inspect}, nil
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
}

// recoverHelperUnavailableClient reports no running containers but fails
// both the digest-pinned bind-cleanup helper probe and the fallback pinned
// pull (issue #174 offline case), so the recovery preflight must refuse
// before any state mutation.
func recoverHelperUnavailableClient() *fakeDockerClient {
	return &fakeDockerClient{
		runFn: func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			switch fmt.Sprintf("%T", inv) {
			case "docker.projectContainerListInvocation":
				return docker.CommandResult{}, nil // no containers
			case "docker.imageDigestInspectInvocation":
				return docker.CommandResult{}, errors.New("no such image")
			case "docker.bindCleanupImagePullInvocation":
				return docker.CommandResult{}, errors.New("dial tcp: no such host")
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
}

func validRecoverLockBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(state.StackLock{
		SchemaVersion:  1,
		AppID:          "recoverapp",
		ComposeProject: recoverComposeProject,
	})
	require.NoError(t, err)
	return raw
}

// TestRecoverOrphanedStack_RunningContainerRefused proves a stack with a
// running container is refused and the directory is left intact.
func TestRecoverOrphanedStack_RunningContainerRefused(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, nil) // empty lock dir
	client := recoverRunningContainerClient(t)

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.Error(t, err)
	assertUsageValidation(t, err)

	_, statErr := os.Stat(stackPath)
	assert.NoError(t, statErr, "a running stack must not be removed")
}

// TestRecoverOrphanedStack_StaleLockRemoved proves an empty (hard-killed)
// lock with no running containers leads to the directory being removed.
func TestRecoverOrphanedStack_StaleLockRemoved(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, []byte("")) // empty .wdm.lock → stale residue
	client := recoverNoContainersClient()

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.NoError(t, err)

	_, statErr := os.Stat(stackPath)
	assert.True(t, os.IsNotExist(statErr), "a stale orphan dir must be removed")
}

// TestRecoverOrphanedStack_ManagedRefused proves a valid manifest is refused
// and the directory survives.
func TestRecoverOrphanedStack_ManagedRefused(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, validRecoverLockBytes(t))
	client := recoverNoContainersClient()

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.Error(t, err)
	assertUsageValidation(t, err)

	_, statErr := os.Stat(stackPath)
	assert.NoError(t, statErr, "a managed stack must not be removed")
}

// TestRecoverOrphanedStack_AbsentLockNonEmptyRefused proves a directory with
// NO .wdm.lock but other files (not a wdm orphan) is refused without removal.
func TestRecoverOrphanedStack_AbsentLockNonEmptyRefused(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, nil)
	// A foreign file makes the dir non-empty with no wdm lock.
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "user-data.txt"), []byte("mine"), 0o600))
	client := recoverNoContainersClient()

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.Error(t, err)
	assertUsageValidation(t, err)

	_, statErr := os.Stat(stackPath)
	assert.NoError(t, statErr, "a non-wdm user directory must not be removed")
}

// TestRecoverOrphanedStack_AbsentLockEmptyRemoved proves an empty directory
// with no lock is treated as a safe leftover and removed.
func TestRecoverOrphanedStack_AbsentLockEmptyRemoved(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, nil) // empty, no lock
	client := recoverNoContainersClient()

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.NoError(t, err)

	_, statErr := os.Stat(stackPath)
	assert.True(t, os.IsNotExist(statErr), "an empty leftover dir must be removed")
}

// TestRecoverOrphanedStack_HelperImageUnavailableRefusesBeforeLockTouched
// pins the #166 wedge class: removeOrphanStackDir may need the digest-pinned
// bind-cleanup helper to clear subuid-owned files on EACCES, so recovery MUST
// prove the image is present before clearing the .wdm.lock. If the preflight
// failed only after the lock was gone, a failed removal would leave a
// lock-less, non-empty directory that every later --force refuses. A stale
// (empty) lock is used so a proceeding recovery WOULD clear it and remove the
// dir; the assertions prove neither happened.
func TestRecoverOrphanedStack_HelperImageUnavailableRefusesBeforeLockTouched(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	stackPath := recoverStackDir(t, []byte("")) // stale lock: cleared+removed if it proceeded
	lockPath := filepath.Join(stackPath, ".wdm.lock")
	client := recoverHelperUnavailableClient()

	err := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.Error(t, err)

	_, statErr := os.Stat(lockPath)
	require.NoError(t, statErr, "the .wdm.lock must survive: recovery must refuse before clearing it")
	_, statErr = os.Stat(stackPath)
	assert.NoError(t, statErr, "the orphan directory must not be removed when the preflight fails")
}

// TestRecoverOrphanedStack_NothingPresentNoop proves recovery is a no-op when
// the stack path does not exist.
func TestRecoverOrphanedStack_NothingPresentNoop(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	stackPath := filepath.Join(home, ".wdm-recover-test-absent", "recoverapp")
	client := recoverNoContainersClient()

	recoverErr := eng.RecoverOrphanedStackForTest(context.Background(), client, stackPath, recoverComposeProject)
	require.NoError(t, recoverErr)
}
