//go:build docker_e2e

// Package e2e holds real-Docker end-to-end smoke tests for wdm. Every
// test here drives the engine exclusively through its public facade —
// pkg/engine plus pkg/types, the same surface the future GUI consumes
// (PRD §29, §31). Nothing in this package may import internal/*: the
// whole point of the e2e tier is to prove the public API installs,
// inspects, and removes a real stack against a real Docker daemon
// without reaching behind the facade.
// These tests are gated behind the docker_e2e build tag (matching the
// Makefile `e2e` target) and are excluded from `make test`. They
// require a working Docker daemon with Compose V2 and bind host port
// 3008 on 127.0.0.1; run them on a machine with a working Docker
// daemon, never on a host whose real ~/docker state or port 3008 you
// care about.
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	// kumaAppID is the catalog identifier for the uptime-kuma app.
	kumaAppID = "uptime-kuma"

	// kumaComposeProject is the Compose project the engine derives for
	// uptime-kuma ("wdm-" + app id, see internal/core install planning).
	// Container, volume, and network cleanup filter on this label.
	kumaComposeProject = "wdm-uptime-kuma"

	// kumaHostPort is the fixed host port uptime-kuma binds on 127.0.0.1
	// (catalog ports[].host). The e2e environment owns this port.
	kumaHostPort = "3008"

	// kumaNetworks are the catalog-declared external networks the engine
	// pre-creates for uptime-kuma. Compose does not remove external
	// networks on `down`, so the test removes them explicitly in cleanup.
	kumaFrontNetwork = "kuma"
	kumaDBNetwork    = "kuma-db"

	// installTimeout bounds the whole install call. Real first-boot pulls
	// (mariadb ~150MB, uptime-kuma ~400MB) plus mariadb's health-gated
	// startup (depends_on: service_healthy) dominate the wall-clock, so
	// the budget is generous.
	installTimeout = 8 * time.Minute

	// convergeTimeout bounds the poll for the stack to report "running"
	// after install. mariadb start_period is 30s; uptime-kuma starts only
	// once mariadb is healthy. A wide margin keeps the test non-flaky.
	convergeTimeout = 4 * time.Minute

	// httpTimeout bounds the wait for uptime-kuma to answer on its local
	// port. The app serves its setup page on first boot.
	httpTimeout = 3 * time.Minute

	// cleanupTimeout bounds each individual docker CLI hygiene call.
	cleanupTimeout = 2 * time.Minute
)

// recordingConfirmer is a Confirmer that always authorizes and captures
// the consequence payload the engine presents. The e2e accepts the
// install-deploy confirmation and asserts on what the engine surfaced.
type recordingConfirmer struct {
	mu       sync.Mutex
	captured []types.Confirmation
}

func (c *recordingConfirmer) Confirm(_ context.Context, conf types.Confirmation) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, conf)
	return true, nil
}

func (c *recordingConfirmer) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	kinds := make([]string, 0, len(c.captured))
	for _, conf := range c.captured {
		kinds = append(kinds, conf.Kind)
	}
	return kinds
}

// stepRecorder collects the progress step IDs the engine emits so the
// test can assert the real install streamed the expected pipeline.
type stepRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *stepRecorder) fn() types.ProgressFn {
	return func(step string, _ float64, _ string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.steps = append(r.steps, step)
	}
}

func (r *stepRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.steps))
	copy(out, r.steps)
	return out
}

// TestInstallUptimeKuma is the first real-Docker smoke test: it installs
// uptime-kuma through pkg/engine against an isolated temp HOME, proves
// the stack comes up and answers HTTP, and tears everything down — files
// via the engine's safe Remove, then Docker volumes/networks via direct
// CLI calls (wdm never deletes those by design, PRD §19).
func TestInstallUptimeKuma(t *testing.T) {
	requireDocker(t)
	requirePortFree(t, kumaHostPort)

	// Isolation: every path the engine derives lands under one temp root.
	// stateDir holds runtime.lock, dataDir holds the seeded catalog,
	// stackBase holds ~/docker/<app>, configPath points at a missing file
	// so PRD §34 defaults apply. The engine never reads the real $HOME.
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	configPath := filepath.Join(tmp, "config.toml") // absent → defaults
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	// The stack uses bind mounts (./data,./db); the containers write
	// those as their own internal UIDs (root, mysql=999), which the host
	// test user cannot unlink. Registered right after stackBase is
	// created so it runs LAST among the test's cleanups — just before
	// t.TempDir's RemoveAll — reowning the tree so RemoveAll succeeds.
	t.Cleanup(func() { reownStackFiles(t, stackBase) })

	seedStableCatalog(t, dataDir)

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

	// Cleanup is registered BEFORE install so it runs even if Install
	// fails partway through (LIFO: this fires after any later cleanups).
	// It removes Docker-side state the engine intentionally preserves.
	t.Cleanup(func() { dockerHygiene(t) })

	confirmer := &recordingConfirmer{}
	steps := &stepRecorder{}

	installCtx, cancel := context.WithTimeout(t.Context(), installTimeout)
	defer cancel()

	result, err := eng.Install(
		installCtx,
		types.InstallRequest{AppID: kumaAppID},
		steps.fn(),
		confirmer,
	)
	require.NoError(t, err, "install must succeed against the real daemon")
	require.NotNil(t, result)

	// The engine asked for exactly one authorization: the install-deploy
	// confirmation carrying the ports/volumes/networks it will touch.
	assert.Equal(t, []string{"install_deploy"}, confirmer.kinds(),
		"install must gate on the install_deploy confirmation")

	// The real install streamed the locked pipeline. Assert the load-
	// bearing endpoints are present rather than pinning the exact set,
	// since resource-degraded emission is host-budget conditional.
	seen := steps.seen()
	for _, want := range []string{
		types.StepInstallPlanning,
		types.StepInstallRender,
		types.StepInstallWriteFiles,
		types.StepInstallConfirm,
		types.StepInstallDeploy,
		types.StepInstallLockUpdate,
		types.StepInstallStatus,
	} {
		assert.Contains(t, seen, want, "progress stream must include %s", want)
	}

	// InstallResult facts.
	assert.Equal(t, kumaAppID, result.AppID)
	assert.Equal(t, kumaComposeProject, result.ComposeProject)
	assert.Equal(t, filepath.Join(stackBase, kumaAppID), result.StackPath)

	require.NotEmpty(t, result.LocalPorts, "install must report local ports")
	assert.True(t, hasHostPort(result.LocalPorts, 3008),
		"uptime-kuma must publish host port 3008, got %+v", result.LocalPorts)

	require.NotNil(t, result.Status)
	// Install returns result+nil whether the post-deploy snapshot is
	// running or needs_attention (a still-converging stack is not an
	// install failure, PRD §17/§18). Both are valid here; convergence is
	// proven separately below.
	assert.Contains(t,
		[]string{"running", "needs_attention"},
		result.Status.State,
		"post-install state must be running or needs_attention",
	)

	// On-disk state, asserted only through stdlib reads (pkg/types
	// exposes no lock parser, and the e2e must not import internal/state).
	assertStackOnDisk(t, result.StackPath)

	// Convergence: poll the public Status API until the stack reports
	// running. mariadb gates uptime-kuma via depends_on: service_healthy,
	// so the stack needs time to settle even after a successful install.
	requireConverged(t, eng, kumaAppID)

	// The app actually answers on its local port.
	requireHTTPAnswers(t, "http://127.0.0.1:"+kumaHostPort)

	// Safe removal through the public API: files stay, the engine stops
	// and removes containers (no -v, volumes preserved). The Docker-side
	// volume/network hygiene happens in the deferred dockerHygiene.
	removeCtx, removeCancel := context.WithTimeout(t.Context(), cleanupTimeout)
	defer removeCancel()
	removeResult, err := eng.Remove(
		removeCtx,
		types.RemoveRequest{AppID: kumaAppID},
		nil,
		&recordingConfirmer{},
	)
	require.NoError(t, err, "safe remove through the engine must succeed")
	require.NotNil(t, removeResult)
	assert.Equal(t, kumaComposeProject, removeResult.ComposeProject)
}

// requireDocker fails loudly when the docker CLI or daemon is
// unavailable. The e2e tier requires a real daemon; a missing one is an
// environment problem, surfaced rather than silently passed.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker CLI not found on PATH: %v (e2e tests require a working Docker daemon)", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("docker daemon not reachable: %v\n%s", err, out)
	}
}

// requirePortFree fails loudly when the host port is already bound. The
// e2e environment owns 127.0.0.1:3008; a busy port means a previous run
// left a container behind or another service holds it. We do not skip —
// a dirty environment must be visible, not papered over.
func requirePortFree(t *testing.T, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("host port %s is not free: %v — clean up leftover containers/services before running the e2e", port, err)
	}
	require.NoError(t, ln.Close())
}

// seedStableCatalog copies the repo's stable catalog and the app
// templates into dataDir in the on-disk shape the engine's catalog FS
// expects: <dataDir>/catalogs/stable/catalog.yaml plus
// <dataDir>/catalogs/templates/<app>/*.tmpl (the engine reads templates
// by the catalog's "templates/<app>/..." paths relative to the catalogs
// root). has no catalog downloader yet, so the e2e provisions
// the catalog the same way a future `wdm` sync would.
func seedStableCatalog(t *testing.T, dataDir string) {
	t.Helper()
	repoRoot := repoRoot(t)
	catalogsRoot := filepath.Join(dataDir, "catalogs")

	copyFile(t,
		filepath.Join(repoRoot, "catalog", "stable", "catalog.yaml"),
		filepath.Join(catalogsRoot, "stable", "catalog.yaml"),
	)
	copyTree(t,
		filepath.Join(repoRoot, "templates"),
		filepath.Join(catalogsRoot, "templates"),
	)
}

// repoRoot walks up from the test's working directory (tests/e2e) until
// it finds the module's go.mod, returning the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "reached filesystem root without finding go.mod")
		dir = parent
	}
}

// copyFile copies src to dst byte-for-byte, creating parent directories.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	require.NoError(t, err, "reading catalog fixture %s", src)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, data, 0o644))
}

// copyTree recursively copies the regular files under src into dst,
// preserving the relative directory layout.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(src, p)
		if relErr != nil {
			return relErr
		}
		copyFile(t, p, filepath.Join(dst, rel))
		return nil
	})
	require.NoError(t, err, "copying template tree %s", src)
}

// assertStackOnDisk verifies the rendered stack artifacts through stdlib
// reads only: the secret-bearing.env is 0600, the Compose file carries
// the mandatory wdm labels, and .wdm.lock parses as the expected JSON.
func assertStackOnDisk(t *testing.T, stackPath string) {
	t.Helper()

	envPath := filepath.Join(stackPath, ".env")
	info, err := os.Stat(envPath)
	require.NoError(t, err, ".env must exist")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		".env must be mode 0600 (secrets live here)")

	composeBytes, err := os.ReadFile(filepath.Join(stackPath, "docker-compose.yml"))
	require.NoError(t, err, "docker-compose.yml must exist")
	compose := string(composeBytes)
	assert.Contains(t, compose, "wdm.managed", "rendered Compose must inject wdm.managed labels")
	assert.Contains(t, compose, "wdm.app", "rendered Compose must inject wdm.app labels")

	lockBytes, err := os.ReadFile(filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err, ".wdm.lock must exist")
	var lock struct {
		SchemaVersion  int    `json:"schema_version"`
		AppID          string `json:"app_id"`
		ComposeProject string `json:"compose_project"`
	}
	require.NoError(t, json.Unmarshal(lockBytes, &lock), ".wdm.lock must be valid JSON")
	assert.Equal(t, kumaAppID, lock.AppID)
	assert.Equal(t, kumaComposeProject, lock.ComposeProject)
	assert.Positive(t, lock.SchemaVersion)
}

// requireConverged polls Engine.Status until the stack reports running.
// A still-converging stack returns needs_attention; an unrecoverable
// one stays that way until the deadline, at which point the test fails
// with the last observed status for diagnosis.
func requireConverged(t *testing.T, eng engine.Engine, appID string) {
	t.Helper()
	deadline := time.Now().Add(convergeTimeout)
	var last *types.AppStatus
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		status, err := eng.Status(ctx, appID)
		cancel()
		require.NoError(t, err, "Status must not error while polling")
		last = status
		if status.State == "running" {
			return
		}
		time.Sleep(5 * time.Second)
	}
	require.NotNil(t, last)
	t.Fatalf("stack did not reach running within %s; last state=%q reasons=%v services=%+v",
		convergeTimeout, last.State, last.AttentionReasons, last.Services)
}

// requireHTTPAnswers polls url until uptime-kuma serves a non-5xx
// response. The setup page answers with 200/302 on first boot; a 5xx or
// no response within the deadline fails the test.
func requireHTTPAnswers(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(httpTimeout)
	var lastErr error
	var lastCode int
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(5 * time.Second)
			continue
		}
		_ = resp.Body.Close()
		lastCode = resp.StatusCode
		if resp.StatusCode < 500 {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("uptime-kuma did not answer on %s within %s; lastErr=%v lastStatus=%d",
		url, httpTimeout, lastErr, lastCode)
}

// dockerHygiene removes the Docker-side state wdm intentionally
// preserves so repeated e2e runs and the later smoke matrix start clean:
// any container still tagged with the project, then named volumes by
// project label (individually — never `down -v`), then the external
// networks the install pre-created. Every call is context-bounded and
// best-effort; failures are logged, not fatal, because cleanup must not
// mask the test's own outcome.
func dockerHygiene(t *testing.T) {
	t.Helper()

	// Force-remove any lingering project containers.
	for _, id := range dockerLines(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+kumaComposeProject) {
		runDocker(t, "rm", "-f", id)
	}

	// Remove named volumes by project label, one by one (no `down -v`).
	for _, vol := range dockerLines(t, "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+kumaComposeProject) {
		runDocker(t, "volume", "rm", vol)
	}

	// Remove the catalog-declared external networks compose leaves behind.
	for _, name := range []string{kumaFrontNetwork, kumaDBNetwork} {
		runDocker(t, "network", "rm", name)
	}
}

// reownStackFiles chowns the stack base tree back to the host test user
// via a throwaway root container. Container processes write bind-mounted
// files (./data as root,./db as mysql=999) that the unprivileged test
// user cannot unlink, which would make t.TempDir's RemoveAll fail. The
// alpine:3 image is not part of the stack's pull set; `docker run`
// pulls it on first use, best-effort. Failures are logged, not fatal —
// they only degrade temp-dir cleanup.
func reownStackFiles(t *testing.T, stackBase string) {
	t.Helper()
	if _, err := os.Stat(stackBase); err != nil {
		return
	}
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	runDocker(t,
		"run", "--rm",
		"-v", stackBase+":/work",
		"alpine:3",
		"chown", "-R", owner, "/work",
	)
}

// dockerLines runs a docker query and returns its non-empty stdout lines.
// Query failures (e.g. nothing to list) yield no lines and are not fatal.
// Cleanup runs after the test function returns, when t.Context is
// already canceled, so hygiene uses a fresh context.Background with its
// own deadline.
func dockerLines(t *testing.T, args ...string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		t.Logf("docker %v: %v", args, err)
		return nil
	}
	return splitNonEmptyLines(string(out))
}

// runDocker runs a docker mutation best-effort, logging any failure. A
// missing volume/network is an expected no-op (e.g. bind-mounted stacks
// declare no named volumes), so errors here never fail the test.
func runDocker(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Logf("docker %v (best-effort cleanup): %v\n%s", args, err, out)
	}
}

// splitNonEmptyLines splits s on newlines, trimming carriage returns and
// dropping blank lines.
func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

// hasHostPort reports whether bindings publish the given host port.
func hasHostPort(bindings []types.PortBinding, hostPort int) bool {
	for _, b := range bindings {
		if b.HostPort == hostPort {
			return true
		}
	}
	return false
}
