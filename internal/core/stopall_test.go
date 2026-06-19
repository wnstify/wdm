package core_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// stopAllManagedStack writes a minimal managed-stack manifest under the
// engine's stack base so StopAll's enumeration (Engine.List) discovers it
// and the per-stack flock reconfirms a Compose project. A single "app" image
// pin is recorded so the running-detection inspect has an expected service to
// match. The compose and .env files are not required on disk: ComposeStop
// validates the project paths but the fake docker client never reads them.
func stopAllManagedStack(t *testing.T, stackBase, appID string) {
	t.Helper()

	writeStatusStackLock(t, stackBase, appID, state.StackLock{
		SchemaVersion:  1,
		AppID:          appID,
		TemplateName:   appID,
		CatalogChannel: "stable",
		CatalogVersion: "2026.05.29",
		StackPath:      filepath.Join(stackBase, appID),
		ComposeProject: "wdm-" + appID,
		ImagePins: []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		},
	})
}

// stopAllStackBase returns the engine's stack base directory. newTestEngine
// places it next to the state dir as "<tmp>/stacks".
func stopAllStackBase(stateDir string) string {
	return filepath.Join(filepath.Dir(stateDir), "stacks")
}

// stopAllProjectNames collects the Compose project names ComposeStop
// targeted, sorted, so tests can assert which stacks were stopped
// regardless of scan order.
func stoppedProjectNames(result *types.StopAllResult) []string {
	names := make([]string, 0, len(result.Stopped))
	for _, app := range result.Stopped {
		names = append(names, app.ComposeProject)
	}
	sort.Strings(names)
	return names
}

func stopAllAppIDs(apps []types.StoppedApp) []string {
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, app.AppID)
	}
	sort.Strings(ids)
	return ids
}

// stopAllDockerClient is the fake for StopAll tests. It answers the
// running-detection inspect (list + per-container inspect) per Compose
// project and the stop invocation, tracking which projects ComposeStop
// targeted. Stop failures are injected per Compose project. StopAll runs
// sequentially under the runtime lock, so no locking is needed here.
type stopAllDockerClient struct {
	t              *testing.T
	runningProject map[string]bool  // project -> has a running managed container
	port           map[string]int   // project -> published host port for inspect
	stopErr        map[string]error // project -> stop failure to inject
	stopCalls      []string         // Compose projects ComposeStop targeted
	listCalls      int              // running-detection list invocations
}

func newStopAllDockerClient(t *testing.T) *stopAllDockerClient {
	return &stopAllDockerClient{
		t:              t,
		runningProject: map[string]bool{},
		port:           map[string]int{},
		stopErr:        map[string]error{},
	}
}

// addStack registers a managed stack's runtime shape for the inspect: running
// true wires one running managed container, false wires one cleanly exited
// container (present but not running -> "stopped").
func (c *stopAllDockerClient) addStack(stackBase, appID string, hostPort int, running bool) {
	stopAllManagedStack(c.t, stackBase, appID)
	project := "wdm-" + appID
	c.runningProject[project] = running
	c.port[project] = hostPort
}

func (c *stopAllDockerClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	switch fmt.Sprintf("%T", inv) {
	case "docker.projectContainerListInvocation":
		c.listCalls++
		// A single fabricated container id; the inspect reply below carries
		// the matching managed labels for the project being planned.
		return docker.CommandResult{Stdout: statusTestContainerID + "\n"}, nil
	case "docker.containerInspectInvocation":
		project := c.projectForInspect()
		running := c.runningProject[project]
		state := "exited"
		exit := 0
		if running {
			state = "running"
		}
		inspect := statusContainerInspectStdout(
			c.t, "app", strings.TrimPrefix(project, "wdm-"), c.port[project], state, running, false, exit, "",
		)
		return docker.CommandResult{Stdout: inspect}, nil
	default:
		// docker compose stop. The bare project name is the unique
		// projectName field of composeStopInvocation.
		project := invocationField(inv, "projectName:")
		c.stopCalls = append(c.stopCalls, project)
		if err := c.stopErr[project]; err != nil {
			return docker.CommandResult{}, err
		}
		return docker.CommandResult{}, nil
	}
}

func (c *stopAllDockerClient) StreamLogs(context.Context, docker.Invocation, docker.RawLogSink) error {
	return nil
}

// projectForInspect resolves which project the inspect that immediately
// followed a list belongs to. StopAll's plan inspects stacks in List's sorted
// order one at a time, list-then-inspect, so the most recent list call names
// the project. The list and inspect invocations do not both carry the project
// (inspect carries only the container id), so the plan order is tracked via
// listCalls against the sorted projects.
func (c *stopAllDockerClient) projectForInspect() string {
	projects := make([]string, 0, len(c.runningProject))
	for project := range c.runningProject {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	if c.listCalls == 0 || c.listCalls > len(projects) {
		return ""
	}
	return projects[c.listCalls-1]
}

func TestStopAll_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

func TestStopAll_StopsEveryRunningStack(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	client.addStack(base, "freshrss", freeLocalTCPPort(t), true)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Len(t, result.Stopped, 2)
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.AlreadyStopped)
	assert.Equal(t, []string{"wdm-freshrss", "wdm-uptime-kuma"}, stoppedProjectNames(result))

	sort.Strings(client.stopCalls)
	assert.Equal(t, []string{"wdm-freshrss", "wdm-uptime-kuma"}, client.stopCalls)

	// The batch confirms exactly once with the SAFE stop_all payload.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, "stop_all_safe", confirmer.calls[0].Kind)
}

// StopAll plans only running stacks: an already-stopped stack is skipped, not
// stopped, and is reported in AlreadyStopped. The confirmation names only the
// running app.
func TestStopAll_PlanFiltersToRunningOnly(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	client.addStack(base, "freshrss", freeLocalTCPPort(t), false)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{"uptime-kuma"}, stopAllAppIDs(result.Stopped))
	assert.Empty(t, result.Failed)
	assert.Equal(t, []string{"freshrss"}, stopAllAppIDs(result.AlreadyStopped))

	// Only the running stack was stopped.
	assert.Equal(t, []string{"wdm-uptime-kuma"}, client.stopCalls)

	// The confirmation names only the running app.
	require.Len(t, confirmer.calls, 1)
	assert.Contains(t, confirmer.calls[0].Message, "uptime-kuma")
	assert.NotContains(t, confirmer.calls[0].Message, "freshrss")
	assert.Contains(t, confirmer.calls[0].Message, "1 running app(s)")
}

// When no managed stack is running the confirmer is NOT consulted and StopAll
// returns a clean no-op with the skipped apps reported.
func TestStopAll_NoRunningAppsSkipsConfirmerAndExitsClean(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), false)
	client.addStack(base, "freshrss", freeLocalTCPPort(t), false)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, result.Stopped)
	assert.Empty(t, result.Failed)
	assert.Equal(t, []string{"freshrss", "uptime-kuma"}, stopAllAppIDs(result.AlreadyStopped))

	assert.Empty(t, client.stopCalls, "no running app means no docker compose stop")
	assert.Empty(t, confirmer.calls, "an empty plan must not consult the confirmer")
}

func TestStopAll_ContinuesOnPerStackFailure(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	client.addStack(base, "freshrss", freeLocalTCPPort(t), true)
	client.stopErr["wdm-freshrss"] = errors.New("daemon unreachable")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a per-stack failure is not a whole-operation error")
	require.NotNil(t, result)

	require.Len(t, result.Stopped, 1)
	require.Len(t, result.Failed, 1)
	assert.Equal(t, "uptime-kuma", result.Stopped[0].AppID)
	assert.Equal(t, "freshrss", result.Failed[0].AppID)
	assert.Contains(t, result.Failed[0].Error, "daemon unreachable")
}

func TestStopAll_EmptyManagedSetIsCleanNoOp(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	client := newStopAllDockerClient(t)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, result.Stopped)
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.AlreadyStopped)
	assert.Empty(t, client.stopCalls, "no managed stacks means no docker stop")
	assert.Empty(t, confirmer.calls, "an empty plan must not consult the confirmer")
}

func TestStopAll_NilConfirmerFailsClosed(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))
	assert.Empty(t, client.stopCalls, "a nil confirmer refuses before any docker stop")
}

// A nil confirmer with no running app is a clean no-op, not a refusal: the
// fail-closed check only fires when a stop is actually planned.
func TestStopAll_NilConfirmerWithNoRunningAppsIsCleanNoOp(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), false)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Stopped)
	assert.Equal(t, []string{"uptime-kuma"}, stopAllAppIDs(result.AlreadyStopped))
	assert.Empty(t, client.stopCalls)
}

func TestStopAll_DeclinedConfirmationCancelsWithNoSideEffects(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled))
	assert.Empty(t, client.stopCalls, "a decline runs no docker stop")
}

func TestStopAll_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	client := newStopAllDockerClient(t)
	client.addStack(base, "uptime-kuma", freeLocalTCPPort(t), true)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := eng.StopAll(ctx, types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Empty(t, client.stopCalls)
}
