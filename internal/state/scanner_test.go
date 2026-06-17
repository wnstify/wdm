//go:build unix

package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// writeStackFixture creates <root>/<app>/.wdm.lock with contents.
// Returns the lock file's absolute path so callers can refer to it
// for warning-cause assertions.
func writeStackFixture(t *testing.T, root, app, contents string) string {
	t.Helper()
	dir := filepath.Join(root, app)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	lockPath := filepath.Join(dir, ".wdm.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(contents), 0o600))
	return lockPath
}

func TestScanStacks_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.ScanStacks(t.Context(), "relative/docker")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestScanStacks_RejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := state.ScanStacks(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

// TestScanStacks_MissingBaseReturnsEmpty covers the first-launch
// case: a user without ~/docker yet must see an empty result with
// nil error, NOT a fatal directory-missing error.
func TestScanStacks_MissingBaseReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	result, err := state.ScanStacks(t.Context(), missing)
	require.NoError(t, err)
	assert.Empty(t, result.Apps)
	assert.Empty(t, result.Warnings)
}

func TestScanStacks_EmptyBaseReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, result.Apps)
	assert.Empty(t, result.Warnings)
}

// TestScanStacks_IgnoresSubdirsWithoutLock covers the
// "List semantics" rule: a subdirectory without .wdm.lock
// belongs to the user and is silently ignored — no entry in Apps,
// no entry in Warnings.
func TestScanStacks_IgnoresSubdirsWithoutLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "user-owned"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "another"), 0o755))

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, result.Apps)
	assert.Empty(t, result.Warnings)
}

func TestScanStacks_IgnoresTopLevelFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{}, 0o600))

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, result.Apps)
	assert.Empty(t, result.Warnings)
}

func TestScanStacks_ValidStackAppearsInApps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStackFixture(t, dir, "vaultwarden", validStackLockJSON)

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	require.Len(t, result.Apps, 1)

	app := result.Apps[0]
	assert.Equal(t, "vaultwarden", app.AppID)
	assert.Equal(t, "vaultwarden", app.TemplateName)
	assert.Equal(t, "/home/test/docker/vaultwarden", app.StackPath)
	assert.Equal(t, "stable", app.CatalogChannel)
	assert.Equal(t, "2026.05.01", app.CatalogVersion)
	require.NotNil(t, app.LastSuccessfulOperation)
	assert.Equal(t, "install", app.LastSuccessfulOperation.Kind)
	assert.False(t, app.NeedsAttention,
		"the state-layer List always returns NeedsAttention=false; Engine.Status derives it")
}

// TestScanStacks_CorruptLockAppearsAsWarning covers PRD §26's "Detect
// stale locks where practical" — a subdirectory with a corrupt
// .wdm.lock must surface as a warning (so cmd/wdm can hint
// the user) without blanking the rest of the list.
func TestScanStacks_CorruptLockAppearsAsWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	badPath := writeStackFixture(t, dir, "broken", "{ not json")

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, result.Apps)
	require.Len(t, result.Warnings, 1)

	w := result.Warnings[0]
	assert.Equal(t, badPath, w.Path)
	require.NotNil(t, w.Cause)
	assert.True(t, errors.Is(w.Cause, types.ErrStaleState),
		"corrupt lock must wrap ErrStaleState; got %v", w.Cause)
}

// TestScanStacks_MixedStacksValidAndCorrupt covers the load-bearing
// invariant: one unreadable stack must not blank the rest of the
// list. The good stack appears in Apps, the bad one in Warnings,
// independently.
func TestScanStacks_MixedStacksValidAndCorrupt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStackFixture(t, dir, "good-app", validStackLockJSON)
	badPath := writeStackFixture(t, dir, "bad-app", "")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "user-owned"), 0o755))

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, result.Apps, 1, "good stack must appear despite bad sibling")
	assert.Equal(t, "vaultwarden", result.Apps[0].AppID)

	require.Len(t, result.Warnings, 1)
	assert.Equal(t, badPath, result.Warnings[0].Path)
	assert.True(t, errors.Is(result.Warnings[0].Cause, types.ErrStaleState))
}

// TestScanStacks_OrderedLexically asserts the
// docs.ReadDir-on-Linux ordering guarantee — Apps come back in
// lexical filename order. Future callers (e.g. TUI list
// rendering) rely on this for stable display.
func TestScanStacks_OrderedLexically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		// Each fixture needs a unique app_id inside the lock JSON;
		// reusing the same JSON produces three Apps with the same
		// AppID, which is fine for the order test but write-distinct
		// values for clarity.
		fixture := `{
  "schema_version": 1,
  "app_id": "` + name + `",
  "template_name": "` + name + `",
  "template_version": "1.0.0",
  "catalog_channel": "stable",
  "catalog_version": "v1",
  "stack_path": "/tmp/` + name + `",
  "compose_project": "wdm-` + name + `",
  "last_successful_operation": null
}`
		writeStackFixture(t, dir, name, fixture)
	}

	result, err := state.ScanStacks(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, result.Apps, 3)
	assert.Equal(t, "alpha", result.Apps[0].AppID)
	assert.Equal(t, "bravo", result.Apps[1].AppID)
	assert.Equal(t, "charlie", result.Apps[2].AppID)
}

// TestScanStacks_FilePathReturnsReadError covers the os.ReadDir
// non-ENOENT error wrap by pointing baseStackPath at a regular file.
// ReadDir on a file returns ENOTDIR (not ENOENT), so the first-launch
// "missing → empty result" branch must NOT fire; the function must
// fall through to the "reading %q" wrap. This is the cheapest realistic
// trigger of the non-ENOENT branch — no permission games required.
func TestScanStacks_FilePathReturnsReadError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0o600))

	_, err := state.ScanStacks(t.Context(), filePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading")
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"file-as-directory error must not wrap os.ErrNotExist (the path exists, just is not a dir); got %v", err)
}

func TestScanStacks_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeStackFixture(t, dir, "vaultwarden", validStackLockJSON)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.ScanStacks(ctx, dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}
