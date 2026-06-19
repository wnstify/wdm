//go:build unix

package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCloseAndRemoveTempFile_HappyPath verifies the nominal cleanup:
// an open temp file is closed and removed, and the helper reports no
// error.
func TestCloseAndRemoveTempFile_HappyPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "temp")
	f, err := os.Create(path)
	require.NoError(t, err)

	err = closeAndRemoveTempFile(f, path)
	require.NoError(t, err)

	_, statErr := os.Stat(path)
	assert.True(t, errors.Is(statErr, fs.ErrNotExist),
		"temp file must be removed on the happy path")
}

// TestCloseAndRemoveTempFile_AggregatesCloseError drives the
// close-error arm: an already-closed file makes the second Close fail,
// and the helper must surface that failure (the file is already gone,
// so the remove arm contributes nothing).
func TestCloseAndRemoveTempFile_AggregatesCloseError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "temp")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(path))

	err = closeAndRemoveTempFile(f, path)
	require.Error(t, err, "double-close must surface a close error")
	assert.ErrorIs(t, err, os.ErrClosed,
		"close failure on an already-closed fd must be reachable")
}

// TestCloseAndRemoveTempFile_AggregatesRemoveError drives the
// non-ErrNotExist remove arm: pointing path at a non-empty directory
// makes os.Remove fail with ENOTEMPTY, which the helper must surface.
// f is nil so the close arm is skipped, isolating the remove failure.
func TestCloseAndRemoveTempFile_AggregatesRemoveError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	nonEmpty := filepath.Join(dir, "nonempty")
	require.NoError(t, os.Mkdir(nonEmpty, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(nonEmpty, "child"), []byte("x"), 0o600))

	err := closeAndRemoveTempFile(nil, nonEmpty)
	require.Error(t, err, "removing a non-empty directory must fail")
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"a non-ErrNotExist remove failure must be surfaced, not swallowed")
	assert.Contains(t, err.Error(), "removing temp file",
		"the surfaced error must come from the remove arm")
}
