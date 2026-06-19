package core_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// listStatusDockerClient is a thread-safe docker.Client double for the
// concurrent ListStatus fan-out. The shared install fakeDockerClient races
// its call counters under the errgroup, so this double answers
// independently per request: a project's container-list reply is keyed by
// Compose project, and a container's inspect reply is keyed by the
// container id the list reply names. That keeps each stack's inspection
// fully independent and the race detector quiet.
type listStatusDockerClient struct {
	mu               sync.Mutex
	listByProject    map[string]string
	inspectByID      map[string]string
	projectListCalls map[string]int
}

func (c *listStatusDockerClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch fmt.Sprintf("%T", inv) {
	case "docker.projectContainerListInvocation":
		project := invocationField(inv, "projectName:")
		c.projectListCalls[project]++
		return docker.CommandResult{Stdout: c.listByProject[project]}, nil
	case "docker.containerInspectInvocation":
		id := invocationField(inv, "id:")
		return docker.CommandResult{Stdout: c.inspectByID[id]}, nil
	default:
		return docker.CommandResult{}, nil
	}
}

func (c *listStatusDockerClient) StreamLogs(context.Context, docker.Invocation, docker.RawLogSink) error {
	return nil
}

// invocationField extracts a single field value from the %+v rendering of a
// project- or container-scoped invocation. The struct fields are unexported,
// so the test parses them from the formatted representation — the same %+v
// seam the install tests use to assert invocation targets.
func invocationField(inv docker.Invocation, key string) string {
	rendered := fmt.Sprintf("%+v", inv)
	idx := strings.Index(rendered, key)
	if idx < 0 {
		return ""
	}
	rest := rendered[idx+len(key):]
	if end := strings.IndexAny(rest, " }"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// listStatusContainerID builds a distinct 64-hex container id per app so the
// double can key inspect replies by id under concurrency. The id must match
// the docker layer's ^[a-f0-9]{12,64}$ id pattern.
func listStatusContainerID(seed byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed), 32)
}

// addRunningStack writes a managed stack manifest and wires a single
// running, healthy managed container for it.
func (c *listStatusDockerClient) addRunningStack(t *testing.T, stackBase, appID string, hostPort int, seed byte) {
	t.Helper()

	writeListStatusStack(t, stackBase, appID, hostPort)
	id := listStatusContainerID(seed)
	c.listByProject["wdm-"+appID] = id + "\n"
	c.inspectByID[id] = statusContainerInspectStdout(
		t, "app", appID, hostPort, "running", true, false, 0, "healthy",
	)
}

func writeListStatusStack(t *testing.T, stackBase, appID string, hostPort int) {
	t.Helper()

	stackPath := filepath.Join(stackBase, appID)
	lock := statusStackLock(appID, stackPath, []int{hostPort})
	writeStatusStackLock(t, stackBase, appID, lock)
}

func newListStatusEngine(t *testing.T, client docker.Client) (*core.Engine, string) {
	t.Helper()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(client))
	return eng, stackBase
}

func newListStatusClient() *listStatusDockerClient {
	return &listStatusDockerClient{
		listByProject:    map[string]string{},
		inspectByID:      map[string]string{},
		projectListCalls: map[string]int{},
	}
}

func TestListStatus_EmptyReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	eng, _ := newListStatusEngine(t, newListStatusClient())

	statuses, err := eng.ListStatus(t.Context())
	require.NoError(t, err)
	assert.Empty(t, statuses)
	assert.NotNil(t, statuses, "an empty system must return a non-nil empty slice")
}

func TestListStatus_RunningStoppedAndNeedsAttention(t *testing.T) {
	t.Parallel()

	client := newListStatusClient()
	eng, stackBase := newListStatusEngine(t, client)

	runningPort := freeLocalTCPPort(t)
	client.addRunningStack(t, stackBase, "alpha", runningPort, 0x1a)

	// bravo: manifest present, but no managed container — stopped/missing.
	writeListStatusStack(t, stackBase, "bravo", freeLocalTCPPort(t))
	client.listByProject["wdm-bravo"] = ""

	// charlie: the only managed container is present but exited — all
	// expected containers down -> the calm stopped state.
	charliePort := freeLocalTCPPort(t)
	writeListStatusStack(t, stackBase, "charlie", charliePort)
	charlieID := listStatusContainerID(0x3c)
	client.listByProject["wdm-charlie"] = charlieID + "\n"
	client.inspectByID[charlieID] = statusContainerInspectStdout(
		t, "app", "charlie", charliePort, "exited", false, false, 1, "",
	)

	statuses, err := eng.ListStatus(t.Context())
	require.NoError(t, err)
	require.Len(t, statuses, 3)

	alpha := statusByID(statuses, "alpha")
	assert.Equal(t, "running", alpha.State)
	assert.False(t, alpha.NeedsAttention)
	assert.Empty(t, alpha.AttentionReasons)

	bravo := statusByID(statuses, "bravo")
	assert.Equal(t, "needs_attention", bravo.State, "a stack with no managed container needs attention")
	assert.True(t, bravo.NeedsAttention)
	assert.Contains(t, bravo.AttentionReasons, "container_missing")

	charlie := statusByID(statuses, "charlie")
	assert.Equal(t, "stopped", charlie.State, "all containers present but down is stopped")
	assert.False(t, charlie.NeedsAttention)
	assert.Empty(t, charlie.AttentionReasons)
}

// A stack whose expected managed container exists but is not running is
// reported with the calm "stopped" state, NeedsAttention false, and no
// alarmist reasons — through the same ListStatus surface that backs the
// dashboard. This mirrors the Status-path stopped classification so the
// detail and list views agree.
func TestListStatus_StoppedStateIsCalm(t *testing.T) {
	t.Parallel()

	client := newListStatusClient()
	eng, stackBase := newListStatusEngine(t, client)

	port := freeLocalTCPPort(t)
	writeListStatusStack(t, stackBase, "delta", port)
	id := listStatusContainerID(0x4d)
	client.listByProject["wdm-delta"] = id + "\n"
	// Present but exited cleanly: all expected containers down -> stopped.
	client.inspectByID[id] = statusContainerInspectStdout(
		t, "app", "delta", port, "exited", false, false, 0, "",
	)

	statuses, err := eng.ListStatus(t.Context())
	require.NoError(t, err)
	require.Len(t, statuses, 1)

	delta := statusByID(statuses, "delta")
	assert.Equal(t, "stopped", delta.State)
	assert.False(t, delta.NeedsAttention)
	assert.Empty(t, delta.AttentionReasons)
}

func TestListStatus_OutputSortedByAppID(t *testing.T) {
	t.Parallel()

	client := newListStatusClient()
	eng, stackBase := newListStatusEngine(t, client)

	seeds := map[string]byte{"zeta": 0x10, "mike": 0x20, "alpha": 0x30, "tango": 0x40}
	for appID, seed := range seeds {
		client.addRunningStack(t, stackBase, appID, freeLocalTCPPort(t), seed)
	}

	statuses, err := eng.ListStatus(t.Context())
	require.NoError(t, err)
	require.Len(t, statuses, 4)

	ids := make([]string, len(statuses))
	for i, s := range statuses {
		ids[i] = s.AppID
	}
	expected := append([]string(nil), ids...)
	sort.Strings(expected)
	assert.Equal(t, expected, ids, "output must be sorted by app id regardless of completion order")
}

func TestListStatus_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	client := newListStatusClient()
	eng, stackBase := newListStatusEngine(t, client)
	client.addRunningStack(t, stackBase, "alpha", freeLocalTCPPort(t), 0x1a)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	statuses, err := eng.ListStatus(ctx)
	require.Error(t, err)
	assert.Nil(t, statuses)
}

func statusByID(statuses []types.AppRuntimeStatus, appID string) types.AppRuntimeStatus {
	for _, s := range statuses {
		if s.AppID == appID {
			return s
		}
	}
	return types.AppRuntimeStatus{}
}
