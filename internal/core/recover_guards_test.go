package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

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

			err := removeOrphanStackDir(path)

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
