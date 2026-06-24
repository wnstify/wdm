package core_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// uninstallDockerClient is the fake for Uninstall tests. Uninstall does not
// run the running-detection inspect StopAll uses; it tears down every managed
// stack unconditionally with docker compose down --rmi all. The fake answers
// only that teardown invocation, tracks which Compose projects it targeted
// and the exact argv the client built, and injects per-project failures.
type uninstallDockerClient struct {
	t                  *testing.T
	downErr            map[string]error     // project -> teardown failure to inject
	downCalls          []string             // Compose projects ComposeDownRemoveImages targeted
	lastDownArgv       []string             // argv of the most recent teardown invocation
	networkRemoveErr   map[string]error     // network name -> raw run error to inject
	networkRemoveStder map[string]string    // network name -> stderr to inject alongside the error
	networkRemoveCalls []string             // network names network rm targeted, in order
	onDown             func(project string) // optional hook fired after each teardown is recorded
	managedNetworks    []string             // names the label-based sweep list returns
	managedNetworkErr  error                // error to inject from the managed-network list
	onNetworkRemove    func(name string)    // optional hook fired before each network rm is answered
	unmanagedNetworks  map[string]bool      // names whose wdm.managed label inspect reports NOT owned
	networkLabelErr    map[string]error     // network name -> error to inject from the label inspect
	networkLabelStderr map[string]string    // network name -> stderr to inject alongside the label-inspect error
}

func newUninstallDockerClient(t *testing.T) *uninstallDockerClient {
	return &uninstallDockerClient{
		t:                  t,
		downErr:            map[string]error{},
		networkRemoveErr:   map[string]error{},
		networkRemoveStder: map[string]string{},
		unmanagedNetworks:  map[string]bool{},
		networkLabelErr:    map[string]error{},
		networkLabelStderr: map[string]string{},
	}
}

// addStack registers a managed stack so Uninstall's enumeration discovers it
// and the per-stack flock reconfirms a Compose project. It reuses the StopAll
// helper that writes a minimal managed manifest.
func (c *uninstallDockerClient) addStack(stackBase, appID string) {
	stopAllManagedStack(c.t, stackBase, appID)
}

// writeUninstallCompose writes a rendered docker-compose.yml into a managed
// stack directory declaring the given top-level networks: each name in external
// is declared external:true (a wdm-pre-created network), and each in internal is
// declared a normal compose-owned network. Only the external set should reach
// the network cleanup.
func writeUninstallCompose(t *testing.T, stackBase, appID string, external, internal []string) {
	t.Helper()

	var b strings.Builder
	b.WriteString("services:\n  app:\n    image: docker.io/example/app:1.0.0\n")
	b.WriteString("networks:\n")
	for _, name := range external {
		fmt.Fprintf(&b, "  %s:\n    external: true\n", name)
	}
	for _, name := range internal {
		fmt.Fprintf(&b, "  %s:\n    driver: bridge\n", name)
	}

	composePath := filepath.Join(stackBase, appID, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte(b.String()), 0o600))
}

func (c *uninstallDockerClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	switch fmt.Sprintf("%T", inv) {
	case "docker.composeDownRemoveImagesInvocation":
		project := invocationField(inv, "projectName:")
		c.downCalls = append(c.downCalls, project)
		c.lastDownArgv = []string{
			"compose", "-f", invocationField(inv, "composeFile:"),
			"--env-file", invocationField(inv, "envFile:"),
			"--project-name", project, "down", "--rmi", "all",
		}
		if c.onDown != nil {
			c.onDown(project)
		}
		if err := c.downErr[project]; err != nil {
			return docker.CommandResult{}, err
		}
		return docker.CommandResult{}, nil
	case "docker.networkManagedLabelInvocation":
		// Ownership gate before a compose-derived network rm: wdm-created
		// networks carry wdm.managed=true; a name in unmanagedNetworks reports
		// empty so the removal skips it.
		name := invocationField(inv, "name:")
		if err := c.networkLabelErr[name]; err != nil {
			return docker.CommandResult{Stderr: c.networkLabelStderr[name]}, err
		}
		if c.unmanagedNetworks[name] {
			return docker.CommandResult{Stdout: "\n"}, nil
		}
		return docker.CommandResult{Stdout: "true\n"}, nil
	case "docker.removeNetworkInvocation":
		name := invocationField(inv, "name:")
		if c.onNetworkRemove != nil {
			c.onNetworkRemove(name)
		}
		c.networkRemoveCalls = append(c.networkRemoveCalls, name)
		if err := c.networkRemoveErr[name]; err != nil {
			return docker.CommandResult{Stderr: c.networkRemoveStder[name]}, err
		}
		return docker.CommandResult{}, nil
	case "docker.managedNetworkListInvocation":
		if c.managedNetworkErr != nil {
			return docker.CommandResult{}, c.managedNetworkErr
		}
		return docker.CommandResult{Stdout: strings.Join(c.managedNetworks, "\n")}, nil
	default:
		require.Failf(c.t, "unexpected invocation",
			"uninstall must only run docker compose down --rmi all, network inspect, network rm, or network ls; got %T", inv)
		return docker.CommandResult{}, nil
	}
}

func (c *uninstallDockerClient) StreamLogs(context.Context, docker.Invocation, docker.RawLogSink) error {
	return nil
}

func tornDownAppIDs(apps []types.TornDownApp) []string {
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, app.AppID)
	}
	sort.Strings(ids)
	return ids
}

// newUninstallTestEngine builds an engine whose footprint dirs live under a
// per-test temp tree (via newTestEngine) and whose running-binary seam points
// at a fake binary inside a temp dir, never the test runner. It returns the
// engine, its state dir, and the fake binary path so tests can assert removal.
func newUninstallTestEngine(t *testing.T, extra ...core.Option) (eng *core.Engine, stateDir, binaryPath string) {
	t.Helper()

	eng, stateDir, binaryPath, _ = newUninstallTestEngineWithConfigDir(t, extra...)
	return eng, stateDir, binaryPath
}

// newUninstallTestEngineWithConfigDir is newUninstallTestEngine plus the
// dedicated config dir path, so a test can make the config dir un-removable
// and exercise the removeFootprint failure path (the config dir is the FIRST
// footprint removed, before the state/logs dir holding the log sink).
func newUninstallTestEngineWithConfigDir(t *testing.T, extra ...core.Option) (eng *core.Engine, stateDir, binaryPath, configDir string) {
	t.Helper()

	binDir := t.TempDir()
	binaryPath = filepath.Join(binDir, "wdm")
	require.NoError(t, os.WriteFile(binaryPath, []byte("fake binary"), 0o755))

	// A dedicated config dir so footprint removal of the config dir does not
	// take the temp root the stacks live under, matching production's
	// ~/.config/wdm/config.toml dedicated directory. It is rooted under $HOME
	// (via coreTestTempDir) so a test that materializes it still passes the
	// footprint home-containment guard and reaches removeFootprint.
	configDir = filepath.Join(coreTestTempDir(t), "wdm")
	configPath := filepath.Join(configDir, "config.toml")

	opts := append([]core.Option{
		core.WithConfigPath(configPath),
		core.WithSelfUpdateDeps(
			func() (string, error) { return binaryPath, nil },
			func(p string) (string, error) { return p, nil },
			nil,
			nil,
		),
	}, extra...)

	eng, stateDir = newTestEngine(t, opts...)
	return eng, stateDir, binaryPath, configDir
}

func TestUninstall_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _, _ := newUninstallTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

func TestUninstall_TearsDownEveryStackAndRemovesFootprint(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	previousPath := binaryPath + ".previous"
	require.NoError(t, os.WriteFile(previousPath, []byte("old binary"), 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	client.addStack(base, "freshrss")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{}
	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{"freshrss", "uptime-kuma"}, tornDownAppIDs(result.TornDown))
	assert.Empty(t, result.Failed)

	sort.Strings(client.downCalls)
	assert.Equal(t, []string{"wdm-freshrss", "wdm-uptime-kuma"}, client.downCalls)

	// The kept data paths name every managed stack directory.
	keptApps := []string{}
	for _, p := range result.KeptDataPaths {
		keptApps = append(keptApps, filepath.Base(p))
	}
	sort.Strings(keptApps)
	assert.Equal(t, []string{"freshrss", "uptime-kuma"}, keptApps)

	// The footprint was removed: state dir, data dir, config dir, binary, and
	// the .previous sibling.
	assert.NotEmpty(t, result.RemovedPaths)
	assert.NoFileExists(t, binaryPath)
	assert.NoFileExists(t, previousPath)
	assert.NoDirExists(t, stateDir)

	// The managed stack directories survive (no data loss).
	assert.DirExists(t, filepath.Join(base, "uptime-kuma"))
	assert.DirExists(t, filepath.Join(base, "freshrss"))

	// The batch confirms exactly once with the destructive payload.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, types.ConfirmationKindUninstallDestructive, confirmer.calls[0].Kind)
}

func TestUninstall_TeardownArgvHasNoVolumeFlag(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// The teardown argv ends in down --rmi all and NEVER carries -v.
	assert.Contains(t, client.lastDownArgv, "--rmi")
	assert.Contains(t, client.lastDownArgv, "all")
	assert.NotContains(t, client.lastDownArgv, "-v")
}

func TestUninstall_NilConfirmerFailsClosed(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))

	// A nil confirmer refuses before any teardown or removal: wdm stays installed.
	assert.Empty(t, client.downCalls)
	assert.FileExists(t, binaryPath)
}

func TestUninstall_DeclinedConfirmationCancelsWithNoSideEffects(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled))

	// A decline runs no teardown and removes nothing.
	assert.Empty(t, client.downCalls)
	assert.FileExists(t, binaryPath)
}

// One stack failing aborts the whole operation BEFORE any footprint removal:
// wdm stays installed, the state dir survives, the result lists the failed
// stack, and the partial-teardown path still emits a §24 result line naming
// failure_point=teardown_stacks rather than orphaning the op-start line.
func TestUninstall_OneTeardownFailureAbortsAndKeepsWDMInstalled(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, stateDir, binaryPath := newUninstallTestEngine(t, core.WithLogger(logger))
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	client.addStack(base, "freshrss")
	client.downErr["wdm-freshrss"] = errors.New("daemon unreachable")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err, "an aborted uninstall is not a whole-operation error; the failure is in the result")
	require.NotNil(t, result)

	require.Len(t, result.Failed, 1)
	assert.Equal(t, "freshrss", result.Failed[0].AppID)
	assert.Contains(t, result.Failed[0].Error, "daemon unreachable")
	// The result's failed entry carries no whole-operation error (nil error;
	// the per-stack reason lives in Error).
	assert.NoError(t, err)

	// §24: the partial-teardown path emits an "operation failed" result line
	// pointing at teardown_stacks, not an orphaned op-start line.
	rec := findOpFailureRecord(t, logs.Bytes(), "teardown_stacks")
	require.NotNil(t, rec, "partial teardown must emit a teardown_stacks failure record")
	assert.Equal(t, "uninstall", rec["action"])

	// Fail-closed: NO footprint removed, wdm still installed.
	assert.Empty(t, result.RemovedPaths)
	assert.FileExists(t, binaryPath)
	assert.DirExists(t, stateDir)
}

// A removeFootprint failure (here the config dir, removed FIRST, is made
// un-removable) aborts the uninstall AFTER teardown but BEFORE the state/logs
// dir is touched, so the §24 log sink survives and the best-effort
// "operation failed" record naming failure_point=remove_footprint lands and is
// assertable. preflightFootprint only path-validates, so a permission failure
// passes it and surfaces specifically at removeFootprint.
func TestUninstall_RemoveFootprintFailureLogsResult(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("chmod-based removal denial does not apply to root")
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, stateDir, _, configDir := newUninstallTestEngineWithConfigDir(t, core.WithLogger(logger))

	// Empty managed set: mkdir the stack base with no apps so teardown is a
	// clean no-op and the op reaches removeFootprint.
	require.NoError(t, os.MkdirAll(stopAllStackBase(stateDir), 0o755))
	client := newUninstallDockerClient(t)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	// Make the config dir un-removable: a child file plus a read+execute-only
	// (no write) mode makes os.RemoveAll fail with EACCES on the child. Restore
	// write before anything that could fail so t.TempDir cleanup always works.
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o755) })
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.Chmod(configDir, 0o500))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)

	// §24: the failure record names remove_footprint, not preflight_footprint.
	rec := findOpFailureRecord(t, logs.Bytes(), "remove_footprint")
	require.NotNil(t, rec, "a removeFootprint failure must emit a remove_footprint failure record")
	assert.Equal(t, "uninstall", rec["action"])

	// The abort happened at the config-dir step, before the state/logs dir that
	// holds the sink: the state dir survives.
	assert.DirExists(t, stateDir)

	// No false success: the op-completed record must NOT be present.
	assert.NotContains(t, logs.String(), "core: operation completed")
}

// findOpFailureRecord scans newline-delimited slog JSON for a
// "core: operation failed" record whose failure_point matches, returning the
// decoded record or nil.
func findOpFailureRecord(t *testing.T, raw []byte, failurePoint string) map[string]any {
	t.Helper()

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec["msg"] == "core: operation failed" && rec["failure_point"] == failurePoint {
			return rec
		}
	}
	require.NoError(t, scanner.Err())
	return nil
}

// Pre-flight validates EVERY footprint removal target before any teardown, so
// a target that resolves out of root (here the running binary resolves to a
// suspiciously shallow near-root path) is refused atomically up front: no stack
// is torn down and no footprint is removed, even though the confirmer accepts.
func TestUninstall_PreflightRefusalAbortsBeforeAnyTeardownOrRemoval(t *testing.T) {
	t.Parallel()

	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "wdm")
	require.NoError(t, os.WriteFile(binaryPath, []byte("fake binary"), 0o755))
	previousPath := binaryPath + ".previous"
	require.NoError(t, os.WriteFile(previousPath, []byte("old binary"), 0o755))

	configPath := filepath.Join(t.TempDir(), "wdm", "config.toml")

	// The executable seam resolves the binary to a suspiciously shallow path,
	// which the same shallow guard the removal uses refuses. The real fake
	// binary on disk is left untouched so the test can assert no removal.
	eng, stateDir := newTestEngine(t,
		core.WithConfigPath(configPath),
		core.WithSelfUpdateDeps(
			func() (string, error) { return binaryPath, nil },
			func(string) (string, error) { return "/wdm", nil },
			nil,
			nil,
		),
	)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	client.addStack(base, "freshrss")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	// A confirmer that WOULD accept: the pre-flight, not a decline, aborts.
	confirmer := &fakeConfirmer{}
	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))

	// (a) it returns the refusal error.
	assert.Contains(t, err.Error(), "suspiciously shallow")

	// (b) NO stack teardown occurred.
	assert.Empty(t, client.downCalls, "pre-flight refusal must abort before any teardown")

	// (c) NO footprint path was removed.
	assert.FileExists(t, binaryPath)
	assert.FileExists(t, previousPath)
	assert.DirExists(t, stateDir)
	assert.DirExists(t, base)

	// The confirmer is still consulted once before the pre-flight.
	require.Len(t, confirmer.calls, 1)
}

func TestUninstall_EmptyManagedSetStillRemovesFootprint(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	client := newUninstallDockerClient(t)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, result.TornDown)
	assert.Empty(t, result.Failed)
	assert.Empty(t, client.downCalls, "no managed stacks means no docker teardown")
	assert.NotEmpty(t, result.RemovedPaths, "the footprint is removed even with no managed apps")
	assert.NoFileExists(t, binaryPath)
	assert.NoDirExists(t, stateDir)
}

func TestUninstall_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))
	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := eng.Uninstall(ctx, types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Empty(t, client.downCalls)
	assert.FileExists(t, binaryPath, "a canceled uninstall removes nothing")
}

// Canceling mid-teardown must report EVERY not-torn-down stack in Failed, not
// just the app at the cancellation index, and must keep the fail-closed
// footprint skip intact (PR #32 Greptile P2).
func TestUninstall_MidTeardownCancellationReportsAllRemainingStacks(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	// Three stacks; alphabetical enumeration tears down "aaa" first, then the
	// loop's pre-iteration ctx check aborts before "mmm" and "zzz".
	client.addStack(base, "aaa")
	client.addStack(base, "mmm")
	client.addStack(base, "zzz")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	// Cancel right after the first stack is torn down so the next iteration's
	// ctx.Err() check fires with two stacks still unprocessed.
	client.onDown = func(string) { cancel() }

	result, err := eng.Uninstall(ctx, types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// The first stack completed before the cancel landed.
	assert.Equal(t, []string{"aaa"}, tornDownAppIDs(result.TornDown))
	// Every stack the abort left unprocessed — both remaining ones, not just
	// the index where cancellation was observed — must appear in Failed.
	assert.Equal(t, []string{"mmm", "zzz"}, tornDownAppIDs(result.Failed))
	for _, f := range result.Failed {
		assert.Equal(t, context.Canceled.Error(), f.Error)
	}

	// Fail-closed: a non-empty Failed slice skips footprint removal entirely.
	assert.Empty(t, result.RemovedPaths)
	assert.FileExists(t, binaryPath, "a canceled uninstall removes nothing")
	assert.DirExists(t, stateDir)
}

// removeFootprintDir refuses a footprint path whose symlink resolves outside
// the home directory, so a tampered footprint can never trick the removal
// into deleting an out-of-tree directory.
func TestUninstall_FootprintRemovalRefusesSymlinkEscape(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	// A real out-of-home directory the symlink points at; it must survive.
	outside := t.TempDir() // t.TempDir is under the OS temp root, not $HOME.
	require.DirExists(t, outside)

	// The config dir is a symlink (inside home) pointing at the outside dir.
	homeTmp, err := os.MkdirTemp(home, ".wdm-uninstall-escape-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(homeTmp) })
	configDir := filepath.Join(homeTmp, "config")
	require.NoError(t, os.Symlink(outside, configDir))

	configPath := filepath.Join(configDir, "config.toml")
	// Point the engine's config path at the escaping symlink dir. Config dir
	// is the FIRST footprint removed, so the escape is refused before any
	// other footprint is touched.
	eng, _ := newTestEngine(t,
		core.WithConfigPath(configPath),
		core.WithSelfUpdateDeps(
			func() (string, error) { return filepath.Join(t.TempDir(), "wdm"), nil },
			func(p string) (string, error) { return p, nil },
			nil,
			nil,
		),
	)
	client := newUninstallDockerClient(t)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))

	// The escaping target survives: the removal refused before touching it.
	assert.DirExists(t, outside)
	assert.True(t, strings.Contains(err.Error(), "outside the home directory"))
}

// After teardown, the wdm-created (external) networks read from each stack's
// rendered compose are removed via network rm, deduped across stacks, and the
// compose-owned (non-external) networks are NEVER targeted.
func TestUninstall_RemovesExternalNetworksAfterTeardown(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	client.addStack(base, "freshrss")
	// Both stacks share the "wdm_proxy" external network: it must be removed
	// once. Each also has its own external network. "internal_app" is a
	// compose-owned network and must NOT be targeted.
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_proxy", "wdm_kuma"}, []string{"internal_app"})
	writeUninstallCompose(t, base, "freshrss", []string{"wdm_proxy", "wdm_rss"}, []string{"internal_app"})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Exactly the external networks, deduped, were requested.
	requested := append([]string(nil), client.networkRemoveCalls...)
	sort.Strings(requested)
	assert.Equal(t, []string{"wdm_kuma", "wdm_proxy", "wdm_rss"}, requested)
	assert.NotContains(t, client.networkRemoveCalls, "internal_app")

	removed := append([]string(nil), result.RemovedNetworks...)
	sort.Strings(removed)
	assert.Equal(t, []string{"wdm_kuma", "wdm_proxy", "wdm_rss"}, removed)
	assert.Empty(t, result.RetainedNetworks)
}

// A not-found result on network rm is tolerated as success (idempotent): the
// network is still reported removed and the uninstall completes cleanly.
func TestUninstall_ToleratesAlreadyAbsentNetwork(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_proxy"}, nil)
	client.networkRemoveErr["wdm_proxy"] = errors.New("exit status 1")
	client.networkRemoveStder["wdm_proxy"] = "Error: No such network: wdm_proxy"
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{"wdm_proxy"}, result.RemovedNetworks)
	assert.Empty(t, result.RetainedNetworks)
	// Footprint removal still proceeded.
	assert.NotEmpty(t, result.RemovedPaths)
	assert.NoFileExists(t, binaryPath)
}

// A network that genuinely cannot be removed is recorded in RetainedNetworks
// and footprint removal STILL proceeds — network cleanup never triggers the
// fail-closed abort.
func TestUninstall_RetainsUnremovableNetworkButRemovesFootprint(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_proxy", "wdm_kuma"}, nil)
	client.networkRemoveErr["wdm_proxy"] = errors.New("network wdm_proxy has active endpoints")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, result.RetainedNetworks, 1)
	assert.Equal(t, "wdm_proxy", result.RetainedNetworks[0].Name)
	assert.Contains(t, result.RetainedNetworks[0].Reason, "active endpoints")
	assert.Equal(t, []string{"wdm_kuma"}, result.RemovedNetworks)

	// Best-effort: footprint removal still proceeded; wdm is gone.
	assert.NotEmpty(t, result.RemovedPaths)
	assert.NoFileExists(t, binaryPath)
	assert.NoDirExists(t, stateDir)
}

// A stack whose compose file is missing contributes no networks (best-effort)
// and never aborts the uninstall.
func TestUninstall_MissingComposeContributesNoNetworks(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma") // no compose file written
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, client.networkRemoveCalls)
	assert.Empty(t, result.RemovedNetworks)
	assert.Empty(t, result.RetainedNetworks)
	assert.NotEmpty(t, result.RemovedPaths)
}

// A teardown failure aborts before any network cleanup: no network rm runs.
func TestUninstall_TeardownFailureSkipsNetworkCleanup(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_proxy"}, nil)
	client.downErr["wdm-uptime-kuma"] = errors.New("daemon unreachable")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, result.Failed, 1)
	assert.Empty(t, client.networkRemoveCalls, "an aborted teardown must skip network cleanup")
	assert.Empty(t, result.RemovedNetworks)
	assert.Empty(t, result.RemovedPaths)
}

// The label-based sweep removes an orphaned wdm.managed=true network whose app
// the operator already deleted — its compose file is gone, so compose-derived
// discovery cannot find it, but the label can.
func TestUninstall_SweepsOrphanedManagedNetwork(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_kuma"}, nil)
	// "wdm_orphan" carries the managed label but belongs to no installed app.
	client.managedNetworks = []string{"wdm_kuma", "wdm_orphan"}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	removed := append([]string(nil), result.RemovedNetworks...)
	sort.Strings(removed)
	assert.Equal(t, []string{"wdm_kuma", "wdm_orphan"}, removed)
	assert.Empty(t, result.RetainedNetworks)
}

// A network already removed via the compose-derived path is not attempted or
// listed twice when the sweep list also reports it.
func TestUninstall_SweepDedupsComposeRemovedNetwork(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_kuma"}, nil)
	// The sweep list reports the compose-removed "wdm_kuma" again plus an orphan.
	client.managedNetworks = []string{"wdm_kuma", "wdm_orphan"}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// "wdm_kuma" was removed exactly once (compose path); the sweep skipped it.
	kumaCount := 0
	for _, name := range client.networkRemoveCalls {
		if name == "wdm_kuma" {
			kumaCount++
		}
	}
	assert.Equal(t, 1, kumaCount, "a compose-removed network must not be swept again")

	removed := append([]string(nil), result.RemovedNetworks...)
	sort.Strings(removed)
	assert.Equal(t, []string{"wdm_kuma", "wdm_orphan"}, removed)
}

// A network that FAILED removal via the compose-derived path lands in
// RetainedNetworks; the sweep seeds its seen set from retained as well as
// removed, so the same network reported by the sweep list is not attempted a
// second time and is not duplicated in RetainedNetworks.
func TestUninstall_SweepDedupsComposeRetainedNetwork(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_kuma"}, nil)
	// The compose-derived removal of "wdm_kuma" fails, so it is retained.
	client.networkRemoveErr["wdm_kuma"] = errors.New("network wdm_kuma has active endpoints")
	// The sweep list reports the retained "wdm_kuma" again plus an orphan.
	client.managedNetworks = []string{"wdm_kuma", "wdm_orphan"}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// "wdm_kuma" removal was attempted exactly once (compose path); the sweep
	// skipped it because it was already in the retained seen set.
	kumaCount := 0
	for _, name := range client.networkRemoveCalls {
		if name == "wdm_kuma" {
			kumaCount++
		}
	}
	assert.Equal(t, 1, kumaCount, "a compose-retained network must not be swept again")

	// "wdm_kuma" appears exactly once in RetainedNetworks (no duplicate), and the
	// orphan the sweep found was removed.
	require.Len(t, result.RetainedNetworks, 1)
	assert.Equal(t, "wdm_kuma", result.RetainedNetworks[0].Name)
	assert.Contains(t, result.RetainedNetworks[0].Reason, "active endpoints")
	assert.Equal(t, []string{"wdm_orphan"}, result.RemovedNetworks)
}

// A sweep removal failure is recorded in RetainedNetworks and the uninstall
// still succeeds — the sweep never triggers the fail-closed abort.
func TestUninstall_SweepRemovalFailureRetainsButRemovesFootprint(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma") // no external networks in compose
	writeUninstallCompose(t, base, "uptime-kuma", nil, nil)
	client.managedNetworks = []string{"wdm_orphan"}
	client.networkRemoveErr["wdm_orphan"] = errors.New("network wdm_orphan has active endpoints")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, result.RetainedNetworks, 1)
	assert.Equal(t, "wdm_orphan", result.RetainedNetworks[0].Name)
	assert.Contains(t, result.RetainedNetworks[0].Reason, "active endpoints")
	assert.Empty(t, result.RemovedNetworks)

	assert.NotEmpty(t, result.RemovedPaths)
	assert.NoFileExists(t, binaryPath)
}

// A managed-network list failure is a non-fatal cleanup degradation: the sweep
// is skipped and the uninstall completes with footprint removal.
func TestUninstall_SweepListErrorIsNonFatal(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_kuma"}, nil)
	client.managedNetworkErr = errors.New("docker daemon unreachable")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	// The compose-derived removal still stands; only the sweep degraded.
	assert.Equal(t, []string{"wdm_kuma"}, result.RemovedNetworks)
	assert.Empty(t, result.RetainedNetworks)
	assert.NotEmpty(t, result.RemovedPaths)
	assert.NoFileExists(t, binaryPath)
}

// No extra labeled networks beyond the compose-derived set makes the sweep a
// no-op: nothing extra is removed or retained.
func TestUninstall_SweepNoExtraNetworksIsNoOp(t *testing.T) {
	t.Parallel()

	eng, stateDir, _ := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", []string{"wdm_kuma"}, nil)
	client.managedNetworks = []string{"wdm_kuma"} // already handled via compose path
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(t.Context(), types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{"wdm_kuma"}, result.RemovedNetworks)
	assert.Equal(t, []string{"wdm_kuma"}, client.networkRemoveCalls)
	assert.Empty(t, result.RetainedNetworks)
}

// Context cancellation between sweep removals stops the sweep before the next
// removal: only the network attempted before cancellation reaches the daemon.
// The subsequent footprint-removal step observes the canceled context and the
// run returns a context error with no result.
func TestUninstall_SweepRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
	base := stopAllStackBase(stateDir)
	require.NoError(t, os.MkdirAll(base, 0o755))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	client := newUninstallDockerClient(t)
	client.addStack(base, "uptime-kuma")
	writeUninstallCompose(t, base, "uptime-kuma", nil, nil)
	client.managedNetworks = []string{"wdm_first", "wdm_second"}
	// Cancel the moment the first sweep removal runs, so the second is skipped.
	client.onNetworkRemove = func(string) { cancel() }
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.Uninstall(ctx, types.UninstallRequest{}, nil, &fakeConfirmer{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)

	// The sweep stopped at the second removal: only the first was attempted, and
	// the footprint survives because removal aborted on the canceled context.
	assert.Equal(t, []string{"wdm_first"}, client.networkRemoveCalls)
	assert.FileExists(t, binaryPath)
}
