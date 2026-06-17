package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
)

// TestNew_DefaultLoggerProductionPathConstructsCleanly exercises the
// default-logger branch: when [core.WithLogger] is not
// supplied, [core.New] must construct a redaction-wrapped logger over
// [os.Stderr] via internal/logging + internal/security rather than
// falling back to the bare slog.Default. The construction must
// succeed without surfacing the logging.New error wrap so callers
// without a logger can rely on engine construction never failing on
// the logger branch alone (PRD §11, §24).
// Verifying redaction behavior itself lives in internal/security and
// internal/logging tests; the observable behavior at the core boundary is
// "engine construction still succeeds when WithLogger is omitted." The test
// uses a t.TempDir scaffold (no caller-supplied paths) so the
// existing resolve-dirs + load-config-or-defaults path runs through
// cleanly without contaminating the user's real $XDG dirs.
func TestNew_DefaultLoggerProductionPathConstructsCleanly(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	configPath := filepath.Join(tmp, "nonexistent.toml")

	// WithLogger intentionally omitted to exercise the new branch.
	eng, err := core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
	)
	require.NoError(t, err,
		"core.New without WithLogger must succeed via the redaction-wrapped default logger branch")
	require.NotNil(t, eng,
		"core.New without WithLogger must return a non-nil engine")
	t.Cleanup(func() { _ = eng.Close() })
}

func TestSettings_ReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)

	settings, err := eng.Settings(t.Context())
	require.NoError(t, err)
	settings.BaseStackPath = "/mutated"
	settings.DefaultDockerNetwork = "mutated"

	again, err := eng.Settings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "~/docker", again.BaseStackPath)
	assert.Equal(t, "wdm_default", again.DefaultDockerNetwork)
}

func TestSettings_HonorsClosedAndCanceled(t *testing.T) {
	t.Parallel()

	t.Run("closed", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		require.NoError(t, eng.Close())

		settings, err := eng.Settings(t.Context())
		require.ErrorIs(t, err, core.ErrClosed)
		assert.Nil(t, settings)
	})

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		settings, err := eng.Settings(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, settings)
	})
}

func TestList_ReturnsAppsAndLogsScanWarnings(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	writeCoreStackFixture(t, stackBase, "vaultwarden", `{
  "schema_version": 1,
  "app_id": "vaultwarden",
  "template_name": "vaultwarden",
  "template_version": "1.2.3",
  "catalog_channel": "stable",
  "catalog_version": "2026.05.01",
  "stack_path": "/home/test/docker/vaultwarden",
  "compose_project": "wdm-vaultwarden",
  "last_successful_operation": {
    "kind": "install",
    "at": "2026-05-19T09:14:33Z",
    "wdm_version": "0.1.0"
  }
}`)
	badLockPath := writeCoreStackFixture(t, stackBase, "broken", `{not-json`)

	var logs bytes.Buffer
	eng, err := core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(filepath.Join(tmp, "missing.toml")),
		core.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	apps, err := eng.List(t.Context())
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "vaultwarden", apps[0].AppID)
	assert.Equal(t, "vaultwarden", apps[0].TemplateName)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 1)

	var warning map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &warning))
	assert.Equal(t, "WARN", warning["level"])
	assert.Equal(t, "core: stack scan warning", warning["msg"])
	assert.Equal(t, badLockPath, warning["path"])
	assert.Contains(t, warning["cause"], "stack state is stale or corrupt")
}

func TestList_HonorsClosedAndCanceled(t *testing.T) {
	t.Parallel()

	t.Run("closed", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		require.NoError(t, eng.Close())

		apps, err := eng.List(t.Context())
		require.ErrorIs(t, err, core.ErrClosed)
		assert.Nil(t, apps)
	})

	t.Run("canceled", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		apps, err := eng.List(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, apps)
	})
}

func writeCoreStackFixture(t *testing.T, root, app, contents string) string {
	t.Helper()

	dir := filepath.Join(root, app)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	lockPath := filepath.Join(dir, ".wdm.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(contents), 0o600))
	return lockPath
}
