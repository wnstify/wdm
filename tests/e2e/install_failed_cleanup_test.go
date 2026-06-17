//go:build docker_e2e

// Package e2e holds real-Docker end-to-end smoke tests for wdm. This file
// proves the fresh-install cleanup: when an install fails mid-deploy —
// after the catalog network is created but before the .wdm.lock manifest is
// durable — the engine's failFreshInstall rollback removes exactly the
// Docker resources that install created (containers, project-labeled named
// volumes, and its own networks) and the partial stack files, leaving no
// orphans, and a corrected retry installs cleanly (PRD §18, §19).
//
// Like install_uptime_kuma_test.go, every assertion drives the engine
// exclusively through pkg/engine + pkg/types and never imports internal/*.
// The test is gated behind the docker_e2e build tag and requires a working
// Docker daemon with Compose V2; run it where a throwaway ~/docker and the
// fixture port are safe to use.
package e2e_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	// failedCleanupAppID is the fixture catalog app id. The install path
	// derives composeProject = "wdm-" + app id, so the residue-verification
	// commands filter on the stable wdm-failed-cleanup-app project label.
	failedCleanupAppID = "failed-cleanup-app"

	// failedCleanupProject is the Compose project the engine derives for the
	// fixture app. Container, volume, and network residue checks filter on
	// this label.
	failedCleanupProject = "wdm-" + failedCleanupAppID

	// failedCleanupNetwork is the single catalog-declared network the engine
	// pre-creates for the fixture. ensureInstallNetworks creates it before
	// `docker compose up`, so the failed install has a network to clean and
	// the residue check has a concrete network to assert gone.
	failedCleanupNetwork = "failed-cleanup-net"

	// failedInstallTimeout bounds the failing install. The image pull (if
	// any) dominates; the daemon rejects the bad container shape immediately
	// after, so a tight-but-safe budget keeps the test honest.
	failedInstallTimeout = 4 * time.Minute

	// retryInstallTimeout bounds the corrected retry install.
	retryInstallTimeout = 4 * time.Minute

	// failedCleanupConvergeTimeout bounds the poll for the retried stack to
	// report running. The fixture container has no healthcheck and sleeps, so
	// it reaches running quickly once created.
	failedCleanupConvergeTimeout = 2 * time.Minute

	// failedCleanupDockerTimeout bounds each docker CLI call the test makes
	// for residue verification and force-clean teardown.
	failedCleanupDockerTimeout = 90 * time.Second
)

// failingComposeTemplate renders a single-service stack whose deploy block
// sets a memory limit BELOW its memory reservation. `docker compose config`
// accepts this shape (wdm's pre-exposure validation passes), but the daemon
// rejects it at `docker compose up` with "Minimum memory limit can not be
// less than memory reservation limit" — a deterministic, network-independent
// config rejection. The failure lands inside commitInstall at
// deployInstallStack, after ensureInstallNetworks created the network and
// before writeInstallLockManifest, so the fresh-install rollback
// (failFreshInstall) is the code under test.
const failingComposeTemplate = `name: failed-cleanup-app

services:
  app:
    image: busybox:stable
    container_name: failed-cleanup-app
    restart: "no"
    command: ["sleep", "infinity"]
    ports:
      - "127.0.0.1:38099:38099"
    networks:
      - failed-cleanup-net
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    ipc: private
    deploy:
      resources:
        limits:
          memory: 64m
        reservations:
          memory: 256m

networks:
  failed-cleanup-net:
    external: true
`

// workingComposeTemplate is the corrected fixture: identical to the failing
// one except the reservation no longer exceeds the limit, so the daemon
// accepts the container at `docker compose up`. The retry installs cleanly,
// the container sleeps (stays running), and it publishes the manifest port,
// so Status converges to running.
const workingComposeTemplate = `name: failed-cleanup-app

services:
  app:
    image: busybox:stable
    container_name: failed-cleanup-app
    restart: unless-stopped
    command: ["sleep", "infinity"]
    ports:
      - "127.0.0.1:38099:38099"
    networks:
      - failed-cleanup-net
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    ipc: private
    deploy:
      resources:
        limits:
          memory: 64m
        reservations:
          memory: 64m

networks:
  failed-cleanup-net:
    external: true
`

// failedCleanupEnvTemplate carries no Go template references: the fixture
// declares no placeholders, so the env file is a static comment. The render
// uses Option("missingkey=error"), so referencing an undeclared var here
// would fail render; keeping it variable-free is intentional.
const failedCleanupEnvTemplate = "# wdm failed-install cleanup fixture — no secrets, no templated values\n"

// failedCleanupCatalog is a minimal schema-valid stable catalog carrying only
// the fixture app. It validates against catalog/schema.json (the loader gate):
// every required app field is present, app_id matches the pattern, and the
// arrays the schema requires (placeholders, ports, image_pins) are present.
const failedCleanupCatalog = `schema_version: 2
channel: stable
generated_at: "2026-05-21T00:00:00Z"

apps:
  - app_id: failed-cleanup-app
    name: Failed Cleanup Fixture
    summary: Fixture app that fails mid-deploy to exercise the fresh-install rollback.
    description: |
      Real-Docker test fixture. Its compose deploy block sets a memory limit
      below its reservation, which the daemon rejects at compose up while
      compose config accepts it — proving the pre-manifest fresh-install
      rollback removes the network and any partial resources.
    template_name: failed-cleanup-app
    template_version: "2026-05-21"
    compose_template: templates/failed-cleanup-app/docker-compose.yml.tmpl
    env_template: templates/failed-cleanup-app/.env.tmpl
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    placeholders: []
    ports:
      - service: app
        container: 38099
        host: 38099
    image_pins:
      - service: app
        image: busybox
        tag: stable
    networks:
      - name: failed-cleanup-net
        internal: false
    pangolin_guidance: {}
    risk_classification:
      - safe
`

// TestInstallFailedCleanup proves the fresh-install rollback against a real Docker daemon: a fresh
// install that fails mid-deploy (pre-manifest) leaves no orphaned containers,
// volumes, or networks for wdm-failed-cleanup-app, and a corrected retry
// installs successfully on the clean slate.
func TestInstallFailedCleanup(t *testing.T) {
	requireDocker(t)

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	configPath := filepath.Join(tmp, "config.toml") // absent → §34 defaults
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	composeTemplatePath := seedFailedCleanupCatalog(t, dataDir)

	// Force-clean teardown owned by the test, separate from the wdm rollback
	// under test: even if that rollback is buggy and an assertion fails, the
	// host is left clean. Registered first → runs last (LIFO), after every
	// other cleanup and the engine Close.
	t.Cleanup(func() { forceCleanFailedCleanup(t) })

	eng, err := engine.New(
		engine.WithConfigPath(configPath),
		engine.WithStateDir(stateDir),
		engine.WithDataDir(dataDir),
		engine.WithStackBaseDir(stackBase),
		engine.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		engine.WithVersion("e2e-test"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	// ---- Attempt 1: the install must fail mid-deploy (pre-manifest). ----
	require.NoError(t, os.WriteFile(composeTemplatePath, []byte(failingComposeTemplate), 0o644),
		"seed the failing compose template")

	failCtx, failCancel := context.WithTimeout(t.Context(), failedInstallTimeout)
	_, err = eng.Install(
		failCtx,
		types.InstallRequest{AppID: failedCleanupAppID},
		nil,
		&recordingConfirmer{},
	)
	failCancel()
	require.Error(t, err, "install must fail: the daemon rejects limit<reservation at compose up")

	// ---- The fresh-install rollback must have left zero residue for the project. ----
	assert.Empty(t,
		dockerLines(t, "ps", "-aq", "--filter", "label=wdm.managed=true",
			"--filter", "label=com.docker.compose.project="+failedCleanupProject),
		"no wdm-managed containers may remain for the failed project")
	assert.Empty(t,
		dockerLines(t, "volume", "ls", "-q",
			"--filter", "label=com.docker.compose.project="+failedCleanupProject),
		"no project-labeled named volumes may remain for the failed project")
	assert.Empty(t,
		dockerLines(t, "network", "ls", "-q",
			"--filter", "label=com.docker.compose.project="+failedCleanupProject),
		"no compose-project-labeled networks may remain for the failed project")
	// The catalog network this install created must also be gone by name: the
	// rollback removes its own created networks, which compose `down` (no -v)
	// would otherwise leave behind for an external network.
	assert.Empty(t,
		dockerLines(t, "network", "ls", "-q", "--filter", "name=^"+failedCleanupNetwork+"$"),
		"the install-created catalog network must be removed by the rollback")

	// The partial stack directory must be gone, so the retry sees a clean
	// path (writeInstallFiles refuses an existing stack path).
	_, statErr := os.Stat(filepath.Join(stackBase, failedCleanupAppID))
	assert.True(t, os.IsNotExist(statErr),
		"the failed install's stack directory must be removed, got stat err=%v", statErr)

	// ---- Attempt 2: a corrected retry must install successfully. ----
	require.NoError(t, os.WriteFile(composeTemplatePath, []byte(workingComposeTemplate), 0o644),
		"seed the corrected compose template")

	retryCtx, retryCancel := context.WithTimeout(t.Context(), retryInstallTimeout)
	result, err := eng.Install(
		retryCtx,
		types.InstallRequest{AppID: failedCleanupAppID},
		nil,
		&recordingConfirmer{},
	)
	retryCancel()
	require.NoError(t, err, "the corrected retry must install on the clean slate")
	require.NotNil(t, result)
	assert.Equal(t, failedCleanupAppID, result.AppID)
	assert.Equal(t, failedCleanupProject, result.ComposeProject)

	// The retried stack converges to running, proving the rollback left a
	// clean slate the engine could fully deploy onto.
	requireFailedCleanupConverged(t, eng)

	// Safe removal through the public API winds the successful retry down so
	// the deferred force-clean has little to do; the test's own teardown is
	// still the authoritative host cleanup.
	removeCtx, removeCancel := context.WithTimeout(t.Context(), failedCleanupDockerTimeout)
	_, err = eng.Remove(
		removeCtx,
		types.RemoveRequest{AppID: failedCleanupAppID},
		nil,
		&recordingConfirmer{},
	)
	removeCancel()
	require.NoError(t, err, "safe remove of the retried stack must succeed")
}

// seedFailedCleanupCatalog writes the fixture catalog and an empty-ish env
// template into dataDir in the on-disk shape the engine's catalog FS expects
// (<dataDir>/catalogs/stable/catalog.yaml and
// <dataDir>/catalogs/templates/<app>/...), mirroring seedStableCatalog. It
// returns the compose template path so the test can swap the failing and
// corrected bodies between attempts. The compose body itself is written by
// the caller, not here, because it differs per attempt.
func seedFailedCleanupCatalog(t *testing.T, dataDir string) string {
	t.Helper()
	catalogsRoot := filepath.Join(dataDir, "catalogs")
	templateDir := filepath.Join(catalogsRoot, "templates", failedCleanupAppID)
	require.NoError(t, os.MkdirAll(templateDir, 0o755))

	require.NoError(t, os.MkdirAll(filepath.Join(catalogsRoot, "stable"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(catalogsRoot, "stable", "catalog.yaml"),
		[]byte(failedCleanupCatalog), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(templateDir, ".env.tmpl"),
		[]byte(failedCleanupEnvTemplate), 0o644,
	))
	return filepath.Join(templateDir, "docker-compose.yml.tmpl")
}

// requireFailedCleanupConverged polls Engine.Status until the retried stack
// reports running. The fixture container sleeps with no healthcheck, so it
// reaches running quickly; a wide-enough deadline keeps the test non-flaky.
func requireFailedCleanupConverged(t *testing.T, eng engine.Engine) {
	t.Helper()
	deadline := time.Now().Add(failedCleanupConvergeTimeout)
	var last *types.AppStatus
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		status, err := eng.Status(ctx, failedCleanupAppID)
		cancel()
		require.NoError(t, err, "Status must not error while polling the retried stack")
		last = status
		if status.State == "running" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	require.NotNil(t, last)
	t.Fatalf("retried stack did not reach running within %s; last state=%q reasons=%v services=%+v",
		failedCleanupConvergeTimeout, last.State, last.AttentionReasons, last.Services)
}

// forceCleanFailedCleanup is the test's own teardown: it force-removes ANY
// wdm-failed-cleanup-app containers, project-labeled volumes, and the fixture
// network regardless of test outcome, so a buggy rollback under test never
// leaves junk on the host. It is best-effort — failures are logged, not
// fatal — and uses the shared dockerLines/runDocker helpers, which run under
// a fresh context.Background since t.Context is canceled by teardown time.
func forceCleanFailedCleanup(t *testing.T) {
	t.Helper()
	for _, id := range dockerLines(t, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+failedCleanupProject) {
		runDocker(t, "rm", "-f", id)
	}
	// The fixture sets container_name, so also catch a container created
	// directly under that name even if the project label is absent.
	for _, id := range dockerLines(t, "ps", "-aq", "--filter", "name=^"+failedCleanupAppID+"$") {
		runDocker(t, "rm", "-f", id)
	}
	for _, vol := range dockerLines(t, "volume", "ls", "-q",
		"--filter", "label=com.docker.compose.project="+failedCleanupProject) {
		runDocker(t, "volume", "rm", vol)
	}
	runDocker(t, "network", "rm", failedCleanupNetwork)
}
