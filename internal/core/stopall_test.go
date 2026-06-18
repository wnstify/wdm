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
// and the per-stack flock reconfirms a Compose project. The compose and
// .env files are not required on disk: ComposeStop validates the project
// paths but the fake docker client never reads them.
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

func TestStopAll_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

func TestStopAll_StopsEveryManagedStack(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	stopAllManagedStack(t, base, "uptime-kuma")
	stopAllManagedStack(t, base, "freshrss")

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Len(t, result.Stopped, 2)
	assert.Empty(t, result.Failed)
	assert.Equal(t, []string{"wdm-freshrss", "wdm-uptime-kuma"}, stoppedProjectNames(result))

	// Exactly one docker compose stop per managed stack, no other docker call.
	assert.Equal(t, 2, fake.calls)
	for _, invType := range fake.invocationTypes {
		assert.Contains(t, invType, "composeStopInvocation")
	}

	// The batch confirms exactly once with the SAFE stop_all payload.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, "stop_all_safe", confirmer.calls[0].Kind)
}

func TestStopAll_ContinuesOnPerStackFailure(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	base := stopAllStackBase(stateDir)
	stopAllManagedStack(t, base, "uptime-kuma")
	stopAllManagedStack(t, base, "freshrss")

	wantErr := errors.New("daemon unreachable")
	fake := &fakeDockerClient{
		runFn: func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			// The invocation type is unexported in internal/docker, so the
			// project name is matched against the %+v rendering. Fail only the
			// freshrss stack; uptime-kuma must still stop.
			if strings.Contains(fmt.Sprintf("%+v", inv), "wdm-freshrss") {
				return docker.CommandResult{}, wantErr
			}
			return docker.CommandResult{}, nil
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a per-stack failure is not a whole-operation error")
	require.NotNil(t, result)

	// Both stacks were attempted: one stopped, one failed.
	require.Len(t, result.Stopped, 1)
	require.Len(t, result.Failed, 1)
	assert.Equal(t, "uptime-kuma", result.Stopped[0].AppID)
	assert.Equal(t, "freshrss", result.Failed[0].AppID)
	assert.Contains(t, result.Failed[0].Error, "daemon unreachable")
	assert.Equal(t, 2, fake.calls)
}

func TestStopAll_EmptyManagedSetIsCleanNoOp(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Empty(t, result.Stopped)
	assert.Empty(t, result.Failed)
	assert.Zero(t, fake.calls, "no managed stacks means no docker call")
	// The confirmer is still consulted (fail-closed contract is uniform).
	require.Len(t, confirmer.calls, 1)
}

func TestStopAll_NilConfirmerFailsClosed(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stopAllManagedStack(t, stopAllStackBase(stateDir), "uptime-kuma")

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))
	assert.Zero(t, fake.calls, "a nil confirmer refuses before any docker call")
}

func TestStopAll_DeclinedConfirmationCancelsWithNoSideEffects(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stopAllManagedStack(t, stopAllStackBase(stateDir), "uptime-kuma")

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
	result, err := eng.StopAll(t.Context(), types.StopAllRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled))
	assert.Zero(t, fake.calls, "a decline runs no docker call")
}

func TestStopAll_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stopAllManagedStack(t, stopAllStackBase(stateDir), "uptime-kuma")

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := eng.StopAll(ctx, types.StopAllRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Zero(t, fake.calls)
}
