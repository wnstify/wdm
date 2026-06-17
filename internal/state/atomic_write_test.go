//go:build unix

package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
)

func TestWriteFileAtomic_ReplacesExistingAndLeavesNoTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	err := state.WriteFileAtomic(path, []byte("new"), 0o644)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), got)

	_, err = os.Stat(path + ".tmp")
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"temporary path must be removed after successful rename")
}

func TestWriteFileAtomic_EnforcesNonSecretModeAfterRename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	require.NoError(t, state.WriteFileAtomic(path, []byte("k = 1\n"), 0o640))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestWriteFileAtomic_SecretModePassesValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "secrets.env")

	require.NoError(t, state.WriteFileAtomic(path, []byte("TOKEN=abc123\n"), security.SecretFileMode))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.NoError(t, security.ValidateSecretFileMode(info.Mode().Perm()))
}

func TestWriteFileAtomic_CreatesNestedParentsWithGeneratedMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "c", "state.json")

	require.NoError(t, state.WriteFileAtomic(path, []byte(`{"ok":true}`), 0o644))

	for _, dir := range []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
	} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir(), "%q must be a directory", dir)
		assert.Equal(t, state.GeneratedDirMode, info.Mode().Perm())
	}
}

func TestWriteFileAtomic_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	err := state.WriteFileAtomic("relative/path", []byte("data"), 0o644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestWriteFileAtomic_RejectsNonDirectoryParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parentFile := filepath.Join(root, "parent-file")
	require.NoError(t, os.WriteFile(parentFile, []byte("not a dir"), 0o600))

	err := state.WriteFileAtomic(filepath.Join(parentFile, "child"), []byte("data"), 0o644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestWriteFileAtomic_FinalUnchangedWhenTempCreateFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmpPath := path + ".tmp"

	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	require.NoError(t, os.WriteFile(tmpPath, []byte("leftover"), 0o600))

	err := state.WriteFileAtomic(path, []byte("replacement"), 0o644)
	require.Error(t, err)

	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("original"), got)
}

func TestWriteFileAtomic_RemovesTempWhenRenameFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "target")
	require.NoError(t, os.Mkdir(path, 0o755))

	err := state.WriteFileAtomic(path, []byte("replacement"), 0o644)
	require.Error(t, err)

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir(), "failed rename must leave the existing directory in place")

	_, statErr = os.Stat(path + ".tmp")
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"temporary file must be removed after rename failure")
}

func TestWriteFileAtomic_RejectsInsecureParentForSecretMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := filepath.Join(root, "insecure")
	require.NoError(t, os.Mkdir(parent, 0o777))
	require.NoError(t, os.Chmod(parent, 0o777))

	path := filepath.Join(parent, "secret.env")
	err := state.WriteFileAtomic(path, []byte("TOKEN=blocked\n"), security.SecretFileMode)
	require.Error(t, err)

	_, statErr := os.Stat(path)
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"final secret file must not be created when parent is insecure")

	_, statErr = os.Stat(path + ".tmp")
	assert.True(t, errors.Is(statErr, os.ErrNotExist),
		"temporary secret file must not be created when parent is insecure")
}

func TestSyncDirectory_DirectoryAndRegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, state.SyncDirectory(dir))

	file := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	err := state.SyncDirectory(file)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestSyncDirectory_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	err := state.SyncDirectory("relative")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestSyncDirectory_MissingDirectoryWrapsNotExist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing")

	err := state.SyncDirectory(path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"missing directory error must wrap os.ErrNotExist; got %v", err)
}
