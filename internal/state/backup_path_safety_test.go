package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// validateBackupStackPath and validateRestoreStackPath guard the
// stack-directory argument before either touches the backup tree, enforcing
// the no-write-outside-stack invariant (PRD §29). These tests drive the
// non-absolute and not-a-directory reject arms.

func TestValidatePruneBackupStackPath_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	err := validateBackupStackPath("state.PruneConfigBackups", "relative/stack")
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute path")
}

func TestValidatePruneBackupStackPath_RejectsNonDirectory(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	err := validateBackupStackPath("state.PruneConfigBackups", file)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a directory")
}

func TestValidateRestoreStackPath_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	err := validateRestoreStackPath("relative/stack")
	require.Error(t, err)
	require.Contains(t, err.Error(), "absolute path")
}

func TestValidateRestoreStackPath_RejectsNonDirectory(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	err := validateRestoreStackPath(file)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a directory")
}
