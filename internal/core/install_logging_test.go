package core_test

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/logging"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// newFileSinkInstallEngine builds an engine whose default PRD §24 file sink
// is active (WithLogger omitted), driving a real latest.log under stateDir so
// tests can read the emitted records back from disk. It returns the engine,
// the logs directory, and the planned stack path for app.
func newFileSinkInstallEngine(t *testing.T, app catalog.App, debug bool) (eng *core.Engine, logsDir, stackPath string) {
	t.Helper()

	tmp := coreTestTempDir(t)
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	configPath := filepath.Join(tmp, "nonexistent.toml")

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: "services:\n  app:\n    image: docker.io/example/app:1.0.0\n",
		app.EnvTemplate:     "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)

	opts := []core.Option{
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
		core.WithCatalog(catalogFS),
		core.WithVersion("9.9.9-test"),
		core.WithDebug(debug),
	}
	eng, err := core.New(opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	return eng, filepath.Join(stateDir, "logs"), filepath.Join(stackBase, app.AppID)
}

// newStructuralLoggerInstallEngine builds an install engine whose logger is
// supplied via WithLogger: a JSON handler over logFile wrapped with a
// STRUCTURAL-ONLY redactor (NewActiveRedactor(nil)). Because WithLogger sets
// logBase to nil, the install rebind is bypassed and a minted secret is never
// registered for scrubbing — so a secret survives on disk only if some log
// call actually passed it. That isolates PRD §24 rule (1). Returns the engine
// and the temp log file path.
func newStructuralLoggerInstallEngine(t *testing.T, app catalog.App) (eng *core.Engine, logFile string) {
	t.Helper()

	tmp := coreTestTempDir(t)
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	configPath := filepath.Join(tmp, "nonexistent.toml")
	logFile = filepath.Join(tmp, "structural.log")

	f, err := os.Create(logFile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	logger := slog.New(logging.NewRedactingHandler(
		slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}),
		security.NewActiveRedactor(nil),
	))

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: "services:\n  app:\n    image: docker.io/example/app:1.0.0\n",
		app.EnvTemplate:     "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)

	eng, err = core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
		core.WithCatalog(catalogFS),
		core.WithVersion("9.9.9-test"),
		core.WithLogger(logger),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	return eng, logFile
}

func secretInstallAppFixture(t *testing.T) catalog.App {
	t.Helper()
	app := appFixture("logging-app", freeLocalTCPPort(t))
	app.ComposeTemplate = "templates/logging-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/logging-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}
	return app
}

func runInstallToFileSink(t *testing.T, eng *core.Engine, app catalog.App, generatedSecret string) {
	t.Helper()
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generatedSecret, nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		nil,
		&fakeConfirmer{},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	// Close flushes/closes the file handle so the disk read sees every record.
	require.NoError(t, eng.Close())
}

func readLogRecords(t *testing.T, logsDir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(logsDir, "latest.log"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var records []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		records = append(records, rec)
	}
	require.NoError(t, scanner.Err())
	return records
}

func readLogBytes(t *testing.T, logsDir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logsDir, "latest.log"))
	require.NoError(t, err)
	return b
}

// TestInstall_EmitsPRD24NormalLogFields proves a successful install populates
// latest.log with the PRD §24 normal-log fields at Info: wdm version, OS+arch,
// action, app, stack path, checks performed, and command names. Before this
// change latest.log was empty after a successful op (issue #51).
func TestInstall_EmitsPRD24NormalLogFields(t *testing.T) {
	t.Parallel()

	app := secretInstallAppFixture(t)
	eng, logsDir, stackPath := newFileSinkInstallEngine(t, app, false)
	runInstallToFileSink(t, eng, app, "field-check-secret")

	records := readLogRecords(t, logsDir)
	require.NotEmpty(t, records, "successful install must write normal-log records (issue #51)")

	var start, done map[string]any
	for _, rec := range records {
		switch rec["msg"] {
		case "core: operation started":
			start = rec
		case "core: operation completed":
			done = rec
		}
	}
	require.NotNil(t, start, "missing op-start record")
	require.NotNil(t, done, "missing op-result record")

	// PRD §24 required identity fields on the result record.
	assert.Equal(t, "install", done["action"])
	assert.Equal(t, "9.9.9-test", done["wdm_version"])
	assert.NotEmpty(t, done["os"])
	assert.NotEmpty(t, done["arch"])
	assert.Equal(t, app.AppID, done["app"])
	assert.Equal(t, stackPath, done["stack_path"])

	// Checks performed and command names appear across the record set.
	all := string(readLogBytes(t, logsDir))
	assert.Contains(t, all, "compose config validated")
	assert.Contains(t, all, "stack deployed and manifest committed")
	assert.Contains(t, all, "generated_secret_fields")
}

// TestInstall_DiskReadbackSecretAwareScrubsGeneratedSecret validates PRD §24
// rule (2), the secret-aware scrub: it runs a real install through the
// production file sink (latest.log + any archives) and asserts the minted
// value never survives on disk. The minted value IS registered in the
// per-install redactor set here, so this proves the rebound redactor scrubs
// it from every sink — it does NOT prove rule (1) (that we never pass a secret
// to a log call); that is TestInstall_StructuralOnlyLogger_NeverEmitsSecretValue.
func TestInstall_DiskReadbackSecretAwareScrubsGeneratedSecret(t *testing.T) {
	t.Parallel()

	const generatedSecret = "S3cr3t-disk-readback-canary-value-xyz"
	app := secretInstallAppFixture(t)
	eng, logsDir, _ := newFileSinkInstallEngine(t, app, false)
	runInstallToFileSink(t, eng, app, generatedSecret)

	entries, err := os.ReadDir(logsDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(logsDir, entry.Name()))
		require.NoError(t, err)
		assert.NotContainsf(t, string(body), generatedSecret,
			"generated secret leaked into log file %s", entry.Name())
	}
}

// TestInstall_StructuralOnlyLogger_NeverEmitsSecretValue validates PRD §24
// rule (1) — that no install log call ever passes a secret value — INDEPENDENT
// of the secret-aware redactor. The engine logs through a WithLogger sink
// wrapped with a STRUCTURAL-ONLY redactor (NewActiveRedactor(nil)), so the
// minted secret is NOT registered for scrubbing. A real install mints the
// secret; it survives on disk only if some install log line actually passed
// it. The disk-readback assertion therefore fails if rule (1) regresses.
func TestInstall_StructuralOnlyLogger_NeverEmitsSecretValue(t *testing.T) {
	t.Parallel()

	const generatedSecret = "S3cr3t-structural-only-canary-value-xyz"
	app := secretInstallAppFixture(t)
	eng, logFile := newStructuralLoggerInstallEngine(t, app)

	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generatedSecret, nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		nil,
		&fakeConfirmer{},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NoError(t, eng.Close())

	body, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.NotContains(t, string(body), generatedSecret,
		"structural-only logger: a secret value reached a log call (rule (1) regression)")
}

// TestInstall_DebugRecordsGatedByDebugFlag proves Info-vs-Debug gating: the
// command-argv debug lines are absent without --debug and present with it,
// and the secret stays redacted in both modes.
func TestInstall_DebugRecordsGatedByDebugFlag(t *testing.T) {
	t.Parallel()

	const generatedSecret = "debug-gate-secret-value"

	t.Run("info_omits_debug_lines", func(t *testing.T) {
		t.Parallel()
		app := secretInstallAppFixture(t)
		eng, logsDir, _ := newFileSinkInstallEngine(t, app, false)
		runInstallToFileSink(t, eng, app, generatedSecret)

		all := string(readLogBytes(t, logsDir))
		assert.NotContains(t, all, "docker compose up -d",
			"debug-only argv summary must not appear at Info level")
		assert.NotContains(t, all, generatedSecret)
	})

	t.Run("debug_includes_debug_lines", func(t *testing.T) {
		t.Parallel()
		app := secretInstallAppFixture(t)
		eng, logsDir, _ := newFileSinkInstallEngine(t, app, true)
		runInstallToFileSink(t, eng, app, generatedSecret)

		all := string(readLogBytes(t, logsDir))
		assert.Contains(t, all, "docker compose up -d",
			"--debug must surface command-argv summaries")
		assert.Contains(t, all, "DEBUG", "debug records must be written at DEBUG level")
		assert.NotContains(t, all, generatedSecret)
	})
}

// TestEngine_LogPathResolvesFileSink covers the PRD §24 failure-UX accessor:
// the default file sink exposes its latest.log path, and a WithLogger engine
// reports the empty string (the caller owns the sink).
func TestEngine_LogPathResolvesFileSink(t *testing.T) {
	t.Parallel()

	app := secretInstallAppFixture(t)
	eng, logsDir, _ := newFileSinkInstallEngine(t, app, false)
	assert.Equal(t, filepath.Join(logsDir, "latest.log"), eng.LogPath())

	// A test engine built via newTestEngine supplies WithLogger, so LogPath
	// is empty (no owned file sink).
	withLogger, _ := newTestEngine(t)
	assert.Empty(t, withLogger.LogPath())
}
