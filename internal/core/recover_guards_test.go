package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// recoverFakeClient is a minimal docker.Client for white-box recovery tests:
// it records each invocation type and delegates to runFn.
type recoverFakeClient struct {
	runFn       func(inv docker.Invocation) (docker.CommandResult, error)
	invokeTypes []string
}

func (c *recoverFakeClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	c.invokeTypes = append(c.invokeTypes, fmt.Sprintf("%T", inv))
	if c.runFn != nil {
		return c.runFn(inv)
	}
	return docker.CommandResult{}, nil
}

// TestRemoveOrphanStackDir_Guards exercises removeOrphanStackDir's
// containment wiring through the REAL security seam (RejectUnsafeRoot,
// validateInstallPathAncestors, EnsureWithinRoot). HOME is set to a
// controlled temp dir so the within-home check resolves predictably.
// The "suspiciously shallow" case is unreachable here without a near-root
// HOME (a path under HOME is at least HOME-depth + 1 segments), so it is not
// covered — that arm only fires for a pathological one-segment HOME.
func TestRemoveOrphanStackDir_Guards(t *testing.T) {
	// HOME must sit under the real home dir so RejectUnsafeRoot (which
	// rejects /var, where t.TempDir lives on macOS) and the within-home
	// check both pass, mirroring coreTestTempDir.
	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	home, err := os.MkdirTemp(realHome, ".wdm-recover-guard-home-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)

	// A second root under the real home but NOT under the fake HOME, for the
	// outside-home case.
	outsideRoot, err := os.MkdirTemp(realHome, ".wdm-recover-guard-outside-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(outsideRoot) })

	cases := []struct {
		name        string
		setup       func(t *testing.T) string // returns the path passed to removeOrphanStackDir
		wantErr     bool
		wantGoneArg bool // when no error: the path must be removed
	}{
		{
			name: "happy path removes deep dir under home",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := filepath.Join(home, "docker", "recoverapp")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600))
				return dir
			},
			wantGoneArg: true,
		},
		{
			name: "unsafe root refused",
			setup: func(t *testing.T) string {
				t.Helper()
				return "/"
			},
			wantErr: true,
		},
		{
			name: "symlinked ancestor refused",
			setup: func(t *testing.T) string {
				t.Helper()
				real := filepath.Join(home, "real")
				require.NoError(t, os.MkdirAll(filepath.Join(real, "recoverapp"), 0o700))
				link := filepath.Join(home, "link")
				require.NoError(t, os.Symlink(real, link))
				return filepath.Join(link, "recoverapp")
			},
			wantErr: true,
		},
		{
			name: "outside home refused",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := filepath.Join(outsideRoot, "recoverapp")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				return dir
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)

			err := removeOrphanStackDir(t.Context(), nil, path)

			if tc.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				_, statErr := os.Stat(path)
				assert.NoError(t, statErr, "a refused path must not be removed")
				return
			}

			require.NoError(t, err)
			if tc.wantGoneArg {
				_, statErr := os.Stat(path)
				assert.True(t, os.IsNotExist(statErr), "a recovered orphan dir must be removed")
			}
		})
	}
}

// TestRemoveOrphanStackDir_PermissionDeniedBindFilesUsePathContainedDockerCleanup
// pins issue #166: an interrupted install can leave subuid-owned bind files
// under the orphan stack directory, so the host user's os.RemoveAll gets
// EACCES. removeOrphanStackDir must recover exactly as deleteStackFiles does —
// run one bounded, containment-proven Docker cleanup over the stack path, then
// retry the removal — instead of aborting recovery. The removal boundary is
// REAL: a 0o500 subdirectory holding a file makes the first os.RemoveAll fail
// with os.ErrPermission; only the fallback lets the retry succeed.
func TestRemoveOrphanStackDir_PermissionDeniedBindFilesUsePathContainedDockerCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are required to reproduce the bind-file removal failure")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the protected fixture directly, so the fallback would not be exercised")
	}

	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	home, err := os.MkdirTemp(realHome, ".wdm-recover-perm-home-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)

	stackPath := filepath.Join(home, "docker", "recoverapp")
	protectedDir := filepath.Join(stackPath, "db")
	protectedFile := filepath.Join(protectedDir, "ib_buffer_pool")
	require.NoError(t, os.MkdirAll(protectedDir, 0o700))
	require.NoError(t, os.WriteFile(protectedFile, []byte("subuid-owned db metadata"), 0o600))
	require.NoError(t, os.Chmod(protectedDir, 0o500)) // r-x: unlink of the file fails
	t.Cleanup(func() {
		_ = os.Chmod(protectedDir, 0o700)
		_ = os.RemoveAll(stackPath)
	})

	var helperCalls int
	fake := &recoverFakeClient{
		runFn: func(inv docker.Invocation) (docker.CommandResult, error) {
			if fmt.Sprintf("%T", inv) == "docker.bindMountCleanupInvocation" {
				helperCalls++
				// The real helper runs as a subuid-mapped root and clears the
				// contained path; emulate that by restoring perms and removing
				// the bind contents so the retried os.RemoveAll succeeds.
				require.NoError(t, os.Chmod(protectedDir, 0o700))
				require.NoError(t, os.RemoveAll(protectedDir))
			}
			return docker.CommandResult{}, nil
		},
	}

	err = removeOrphanStackDir(t.Context(), fake, stackPath)
	require.NoError(t, err)

	assert.Equal(t, 1, helperCalls,
		"permission-denied bind files should trigger exactly one Docker cleanup helper")
	assert.Contains(t, fake.invokeTypes, "docker.bindMountCleanupInvocation")
	_, statErr := os.Stat(stackPath)
	assert.True(t, os.IsNotExist(statErr),
		"the orphan stack directory should be removed after helper cleanup")
}
