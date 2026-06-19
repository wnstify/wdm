package core_test

import (
	"context"
	"errors"
	"fmt"
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
}

func newUninstallDockerClient(t *testing.T) *uninstallDockerClient {
	return &uninstallDockerClient{
		t:                  t,
		downErr:            map[string]error{},
		networkRemoveErr:   map[string]error{},
		networkRemoveStder: map[string]string{},
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
	case "docker.removeNetworkInvocation":
		name := invocationField(inv, "name:")
		c.networkRemoveCalls = append(c.networkRemoveCalls, name)
		if err := c.networkRemoveErr[name]; err != nil {
			return docker.CommandResult{Stderr: c.networkRemoveStder[name]}, err
		}
		return docker.CommandResult{}, nil
	default:
		require.Failf(c.t, "unexpected invocation",
			"uninstall must only run docker compose down --rmi all or network rm; got %T", inv)
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
func newUninstallTestEngine(t *testing.T) (eng *core.Engine, stateDir, binaryPath string) {
	t.Helper()

	binDir := t.TempDir()
	binaryPath = filepath.Join(binDir, "wdm")
	require.NoError(t, os.WriteFile(binaryPath, []byte("fake binary"), 0o755))

	// A dedicated config dir so footprint removal of the config dir does not
	// take the temp root the stacks live under, matching production's
	// ~/.config/wdm/config.toml dedicated directory.
	configPath := filepath.Join(t.TempDir(), "wdm", "config.toml")

	eng, stateDir = newTestEngine(t,
		core.WithConfigPath(configPath),
		core.WithSelfUpdateDeps(
			func() (string, error) { return binaryPath, nil },
			func(p string) (string, error) { return p, nil },
			nil,
			nil,
		),
	)
	return eng, stateDir, binaryPath
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
// wdm stays installed, the state dir survives, and the result lists the
// failed stack.
func TestUninstall_OneTeardownFailureAbortsAndKeepsWDMInstalled(t *testing.T) {
	t.Parallel()

	eng, stateDir, binaryPath := newUninstallTestEngine(t)
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

	// Fail-closed: NO footprint removed, wdm still installed.
	assert.Empty(t, result.RemovedPaths)
	assert.FileExists(t, binaryPath)
	assert.DirExists(t, stateDir)
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
