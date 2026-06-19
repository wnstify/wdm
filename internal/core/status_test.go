package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

const statusTestContainerID = "0123456789abcdef0123456789abcdef"

// statusTestFixture wires one managed-stack-on-disk scenario for
// Engine.Status tests: a stack base under the test home, a written
// .wdm.lock manifest, and the fake docker client seam.
type statusTestFixture struct {
	eng       *core.Engine
	stateDir  string
	stackBase string
	stackPath string
	appID     string
	hostPort  int
	fake      *fakeDockerClient
}

// newStatusFixture builds the fixture for app "status-app" with a
// healthy default manifest (one "app" image pin, one local port, a
// non-nil last_successful_operation). mutateLock may adjust the
// manifest before it is written; a nil fake docker client factory is
// NOT installed here so refusal tests can assert zero docker calls
// with the default zero-value fake.
func newStatusFixture(t *testing.T, mutateLock func(*state.StackLock)) *statusTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	appID := "status-app"
	stackPath := filepath.Join(stackBase, appID)
	hostPort := freeLocalTCPPort(t)

	lock := statusStackLock(appID, stackPath, []int{hostPort})
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &statusTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     appID,
		hostPort:  hostPort,
		fake:      fake,
	}
}

func statusStackLock(appID, stackPath string, ports []int) state.StackLock {
	return state.StackLock{
		SchemaVersion:   1,
		AppID:           appID,
		TemplateName:    appID,
		TemplateVersion: "2026.06.01",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.06.01",
		StackPath:       stackPath,
		LocalPorts:      ports,
		ComposeProject:  "wdm-" + appID,
		ImagePins: []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		},
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}
}

func writeStatusStackLock(t *testing.T, stackBase, dirName string, lock state.StackLock) string {
	t.Helper()

	raw, err := json.MarshalIndent(lock, "", "  ")
	require.NoError(t, err)
	return writeCoreStackFixture(t, stackBase, dirName, string(raw))
}

// statusContainerInspectStdout fabricates the 8-line safe-field
// inspect output with caller-controlled runtime state, labeled as a
// wdm-managed container unless drifted labels are supplied.
func statusContainerInspectStdout(
	t *testing.T,
	service, appID string,
	hostPort int,
	containerStatus string,
	running, restarting bool,
	exitCode int,
	health string,
) string {
	t.Helper()

	return statusContainerInspectStdoutWithLabels(t, map[string]string{
		"com.docker.compose.service": service,
		"wdm.managed":                "true",
		"wdm.app":                    appID,
	}, hostPort, containerStatus, running, restarting, exitCode, health)
}

func statusContainerInspectStdoutWithLabels(
	t *testing.T,
	labels map[string]string,
	hostPort int,
	containerStatus string,
	running, restarting bool,
	exitCode int,
	health string,
) string {
	t.Helper()

	rawLabels, err := json.Marshal(labels)
	require.NoError(t, err)
	ports := fmt.Sprintf(`{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]}`, hostPort)
	return fmt.Sprintf(
		"%q\n%s\n%q\n%t\n%t\n%d\n%q\n%s\n",
		"/status-app-app-1", rawLabels, containerStatus, running, restarting, exitCode, health, ports,
	)
}

// scriptStatusDocker wires the fixture's fake to answer the Status
// invocation sequence by invocation type: container list, container
// inspect, compose config validation.
func scriptStatusDocker(f *statusTestFixture, listStdout, inspectStdout string, composeErr error) {
	f.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: listStdout}, nil
		case "docker.containerInspectInvocation":
			return docker.CommandResult{Stdout: inspectStdout}, nil
		case "docker.composeConfigInvocation":
			if composeErr != nil {
				return docker.CommandResult{ExitCode: 1}, composeErr
			}
			return docker.CommandResult{}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}
}

// holdFlockExclusive opens path on a fresh descriptor and holds
// LOCK_EX until the test ends, simulating another process's held
// lock (separate open file descriptions contend under flock even
// within one process).
func holdFlockExclusive(t *testing.T, path string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, state.LockExclusive(f))
}

// writeHeldRuntimeLock writes runtime.lock holder metadata under the
// fixture's state dir and holds its flock until the test ends.
func writeHeldRuntimeLock(t *testing.T, stateDir string, info state.RuntimeLockInfo) {
	t.Helper()

	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	raw, err := json.MarshalIndent(info, "", "  ")
	require.NoError(t, err)
	lockPath := filepath.Join(stateDir, "runtime.lock")
	require.NoError(t, os.WriteFile(lockPath, raw, 0o600))
	holdFlockExclusive(t, lockPath)
}

func TestStatus_RunningHappyPath(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "healthy")
	scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)

	manifestBefore, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, status)

	assert.Equal(t, fixture.appID, status.AppID)
	assert.Equal(t, "running", status.State)
	assert.Equal(t, "all managed services are running", status.Message)
	assert.Equal(t, "wdm-"+fixture.appID, status.ComposeProject)
	assert.Equal(t, fixture.stackPath, status.StackPath)
	assert.False(t, status.NeedsAttention)
	assert.Empty(t, status.AttentionReasons)
	require.NotNil(t, status.UpdatedAt)

	require.Len(t, status.Services, 1)
	service := status.Services[0]
	assert.Equal(t, "app", service.Service)
	assert.Equal(t, "status-app-app-1", service.ContainerName)
	assert.Equal(t, "running", service.State)
	assert.Equal(t, "healthy", service.Health)
	assert.False(t, service.NeedsAttention)

	require.Len(t, status.LocalPorts, 1)
	assert.Equal(t, fixture.hostPort, status.LocalPorts[0].HostPort)
	assert.Equal(t, "app", status.LocalPorts[0].Service)

	// Managed-only verification order: list by Compose project, then
	// per-container inspect, then compose-config validation — and
	// nothing else.
	assert.Equal(t, []string{
		"docker.projectContainerListInvocation",
		"docker.containerInspectInvocation",
		"docker.composeConfigInvocation",
	}, fixture.fake.invocationTypes)

	// Read-only discipline: no runtime.lock appears, the manifest is
	// byte-identical, and the stack dir gained no files.
	_, statErr := os.Stat(filepath.Join(fixture.stateDir, "runtime.lock"))
	assert.True(t, os.IsNotExist(statErr), "Status must not create runtime.lock")
	manifestAfter, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter, "Status must not rewrite the stack manifest")
	entries, err := os.ReadDir(fixture.stackPath)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ".wdm.lock", entries[0].Name())
}

// TestStatus_FusesEachAttentionCondition drives every PRD §18
// needs-attention condition individually and asserts the exact
// machine-readable reason ID set for each.
func TestStatus_FusesEachAttentionCondition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		mutateLock      func(*state.StackLock)
		setup           func(t *testing.T, fixture *statusTestFixture)
		script          func(t *testing.T, fixture *statusTestFixture)
		expectedReasons []string
	}{
		{
			name:       "managed container missing",
			mutateLock: func(lock *state.StackLock) { lock.LocalPorts = nil },
			script: func(_ *testing.T, fixture *statusTestFixture) {
				scriptStatusDocker(fixture, "", "", nil)
			},
			expectedReasons: []string{"container_missing"},
		},
		{
			name:       "managed container in restart loop",
			mutateLock: func(lock *state.StackLock) { lock.LocalPorts = nil },
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "restarting", false, true, 1, "")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)
			},
			expectedReasons: []string{"restart_loop"},
		},
		{
			name:       "healthcheck reports unhealthy",
			mutateLock: func(lock *state.StackLock) { lock.LocalPorts = nil },
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "unhealthy")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)
			},
			expectedReasons: []string{"unhealthy"},
		},
		{
			name: "compose validation fails",
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, errors.New("compose config rejected"))
			},
			expectedReasons: []string{"compose_validation_failed"},
		},
		{
			name: "local ports no longer match lock file",
			mutateLock: func(lock *state.StackLock) {
				lock.LocalPorts = []int{lock.LocalPorts[0] + 1}
			},
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)
			},
			expectedReasons: []string{"port_mismatch"},
		},
		{
			name: "last wdm operation failed",
			mutateLock: func(lock *state.StackLock) {
				lock.LastSuccessfulOperation = nil
			},
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)
			},
			expectedReasons: []string{"last_operation_failed"},
		},
		{
			name: "stale runtime lock affects the app",
			setup: func(t *testing.T, fixture *statusTestFixture) {
				writeHeldRuntimeLock(t, fixture.stateDir, state.RuntimeLockInfo{
					SchemaVersion: 1,
					PID:           1 << 30, // no such process on Linux or macOS
					Command:       "update",
					StartedAt:     time.Now().UTC().Add(-time.Minute),
					WDMVersion:    "0.1.0",
				})
			},
			script: func(t *testing.T, fixture *statusTestFixture) {
				inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
				scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)
			},
			expectedReasons: []string{"stale_runtime_lock"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newStatusFixture(t, tt.mutateLock)
			if tt.setup != nil {
				tt.setup(t, fixture)
			}
			tt.script(t, fixture)

			status, err := fixture.eng.Status(t.Context(), fixture.appID)
			require.NoError(t, err)
			require.NotNil(t, status)

			assert.Equal(t, "needs_attention", status.State)
			assert.True(t, status.NeedsAttention)
			assert.Equal(t, tt.expectedReasons, status.AttentionReasons)
			assert.Equal(t, "status checks found issues that need attention", status.Message)
		})
	}
}

// TestStatus_CompletedServiceExemption drives the completed-service
// fusion: a service listed in completed_services that ran to a clean
// exit reports "completed" with no attention, while every abnormal exit
// shape (nonzero exit, a down state that is not "exited", a restart
// loop, or a missing container) still surfaces its real attention
// reason. The single image-pin service "app" is the completed one;
// LocalPorts is cleared so the unpublished port never adds a separate
// port_mismatch reason to the cases under test.
func TestStatus_CompletedServiceExemption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		containerStatus string
		running         bool
		restarting      bool
		exitCode        int
		listStdout      string
		expectedState   string
		expectedReasons []string
		expectAttention bool
	}{
		{
			name:            "exited cleanly is completed",
			containerStatus: "exited",
			exitCode:        0,
			listStdout:      statusTestContainerID + "\n",
			expectedState:   "completed",
			expectedReasons: nil,
			expectAttention: false,
		},
		{
			name:            "exited nonzero still needs attention",
			containerStatus: "exited",
			exitCode:        1,
			listStdout:      statusTestContainerID + "\n",
			expectedState:   "exited",
			expectedReasons: []string{"container_exited"},
			expectAttention: true,
		},
		{
			name:            "dead with exit zero still needs attention",
			containerStatus: "dead",
			exitCode:        0,
			listStdout:      statusTestContainerID + "\n",
			expectedState:   "dead",
			expectedReasons: []string{"container_exited"},
			expectAttention: true,
		},
		{
			name:            "created with exit zero still needs attention",
			containerStatus: "created",
			exitCode:        0,
			listStdout:      statusTestContainerID + "\n",
			expectedState:   "created",
			expectedReasons: []string{"container_exited"},
			expectAttention: true,
		},
		{
			name:            "restarting wins over completed",
			containerStatus: "restarting",
			restarting:      true,
			exitCode:        0,
			listStdout:      statusTestContainerID + "\n",
			expectedState:   "restarting",
			expectedReasons: []string{"restart_loop"},
			expectAttention: true,
		},
		{
			name:            "missing container still needs attention",
			listStdout:      "",
			expectedState:   "missing",
			expectedReasons: []string{"container_missing"},
			expectAttention: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newStatusFixture(t, func(lock *state.StackLock) {
				lock.LocalPorts = nil
				lock.CompletedServices = []string{"app"}
			})
			inspect := ""
			if tt.listStdout != "" {
				inspect = statusContainerInspectStdout(
					t,
					"app",
					fixture.appID,
					fixture.hostPort,
					tt.containerStatus,
					tt.running,
					tt.restarting,
					tt.exitCode,
					"",
				)
			}
			scriptStatusDocker(fixture, tt.listStdout, inspect, nil)

			status, err := fixture.eng.Status(t.Context(), fixture.appID)
			require.NoError(t, err)
			require.NotNil(t, status)

			require.Len(t, status.Services, 1)
			service := status.Services[0]
			assert.Equal(t, tt.expectedState, service.State)
			assert.Equal(t, tt.expectAttention, service.NeedsAttention)

			assert.Equal(t, tt.expectAttention, status.NeedsAttention)
			assert.Equal(t, tt.expectedReasons, status.AttentionReasons)
			if !tt.expectAttention {
				assert.Equal(t, "running", status.State)
				assert.NotContains(t, status.AttentionReasons, "container_exited")
			} else {
				assert.Equal(t, "needs_attention", status.State)
			}
		})
	}
}

// TestStatus_OrdinaryServiceExitStillNeedsAttention is the regression
// guard for the nil-completed sibling path: an ordinary service (NOT in
// completed_services) that exited 0 while a sibling is still running surfaces
// container_exited, so the completed exemption never leaks to ordinary
// services and a partial-up stack is not misread as cleanly stopped. The
// running sibling keeps this a partial-up case rather than the all-down
// stopped case.
func TestStatus_OrdinaryServiceExitStillNeedsAttention(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, func(lock *state.StackLock) {
		lock.LocalPorts = nil
		lock.CompletedServices = nil
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "worker", Image: "docker.io/example/worker", Tag: "1.0.0"},
		}
	})
	appUp := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
	workerDown := statusContainerInspectStdout(t, "worker", fixture.appID, fixture.hostPort, "exited", false, false, 0, "")
	calls := 0
	fixture.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: statusTestContainerID + "\nabcdefabcdef\n"}, nil
		case "docker.containerInspectInvocation":
			calls++
			if calls == 1 {
				return docker.CommandResult{Stdout: appUp}, nil
			}
			return docker.CommandResult{Stdout: workerDown}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, status)

	assert.Equal(t, "needs_attention", status.State)
	assert.True(t, status.NeedsAttention)
	assert.Equal(t, []string{"container_exited"}, status.AttentionReasons)
	require.Len(t, status.Services, 2)
	worker := status.Services[1]
	assert.Equal(t, "worker", worker.Service)
	assert.Equal(t, "exited", worker.State)
	assert.True(t, worker.NeedsAttention)
}

// TestStatus_StoppedState drives the stopped classification through the full
// Status path: a single managed service whose container is present but not
// running reports the calm "stopped" state with NeedsAttention false and no
// port_mismatch/container_exited reason, while a running container reports
// "running" and a missing container reports "removed"-style needs-attention
// (container_missing). The manifest keeps its local port so the suppression
// of port_mismatch for a stopped app is exercised.
func TestStatus_StoppedState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		listStdout      string
		containerStatus string
		running         bool
		expectedState   string
		expectAttention bool
		forbidReasons   []string
	}{
		{
			name:            "all containers present but stopped is stopped",
			listStdout:      statusTestContainerID + "\n",
			containerStatus: "exited",
			running:         false,
			expectedState:   "stopped",
			expectAttention: false,
			forbidReasons:   []string{"container_exited", "port_mismatch"},
		},
		{
			name:            "running container is running",
			listStdout:      statusTestContainerID + "\n",
			containerStatus: "running",
			running:         true,
			expectedState:   "running",
			expectAttention: false,
		},
		{
			name:            "missing container needs attention",
			listStdout:      "",
			expectedState:   "needs_attention",
			expectAttention: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newStatusFixture(t, nil)
			inspect := ""
			if tt.listStdout != "" {
				inspect = statusContainerInspectStdout(
					t, "app", fixture.appID, fixture.hostPort,
					tt.containerStatus, tt.running, false, 0, "",
				)
			}
			scriptStatusDocker(fixture, tt.listStdout, inspect, nil)

			status, err := fixture.eng.Status(t.Context(), fixture.appID)
			require.NoError(t, err)
			require.NotNil(t, status)

			assert.Equal(t, tt.expectedState, status.State)
			assert.Equal(t, tt.expectAttention, status.NeedsAttention)
			for _, reason := range tt.forbidReasons {
				assert.NotContains(t, status.AttentionReasons, reason)
			}
			if tt.expectedState == "stopped" {
				assert.Empty(t, status.AttentionReasons)
			}
		})
	}
}

// TestStatus_PartialUpStaysNeedsAttention proves a mixed app — one service
// running, one exited — is NOT classified stopped: a partial-up stack stays
// needs_attention because something that should be running isn't.
func TestStatus_PartialUpStaysNeedsAttention(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, func(lock *state.StackLock) {
		lock.LocalPorts = nil
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "db", Image: "docker.io/example/db", Tag: "1.0.0"},
		}
	})

	appUp := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
	dbDown := statusContainerInspectStdout(t, "db", fixture.appID, fixture.hostPort, "exited", false, false, 1, "")
	listStdout := statusTestContainerID + "\n" + "abcdefabcdef\n"
	calls := 0
	fixture.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: listStdout}, nil
		case "docker.containerInspectInvocation":
			calls++
			if calls == 1 {
				return docker.CommandResult{Stdout: appUp}, nil
			}
			return docker.CommandResult{Stdout: dbDown}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.NoError(t, err)
	assert.Equal(t, "needs_attention", status.State)
	assert.True(t, status.NeedsAttention)
	assert.Contains(t, status.AttentionReasons, "container_exited")
}

// TestStatus_WedgedLiveHolderRuntimeLockIsStale covers the second
// staleness arm: the holder process is alive but has held the lock longer
// than 24h, applied to the recorded start time.
func TestStatus_WedgedLiveHolderRuntimeLockIsStale(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	writeHeldRuntimeLock(t, fixture.stateDir, state.RuntimeLockInfo{
		SchemaVersion: 1,
		PID:           os.Getpid(),
		Command:       "update",
		StartedAt:     time.Now().UTC().Add(-25 * time.Hour),
		WDMVersion:    "0.1.0",
	})
	inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
	scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.NoError(t, err)
	assert.True(t, status.NeedsAttention)
	assert.Equal(t, []string{"stale_runtime_lock"}, status.AttentionReasons)
}

// TestStatus_ActiveRuntimeLockHolderIsNotStale proves a live, recent
// holder (an operation legitimately in flight on ANOTHER stack) does
// not mark this app stale, and a released lock's leftover file never
// counts as held.
func TestStatus_ActiveRuntimeLockHolderIsNotStale(t *testing.T) {
	t.Parallel()

	t.Run("live recent holder", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		writeHeldRuntimeLock(t, fixture.stateDir, state.RuntimeLockInfo{
			SchemaVersion: 1,
			PID:           os.Getpid(),
			Command:       "install",
			StartedAt:     time.Now().UTC().Add(-time.Minute),
			WDMVersion:    "0.1.0",
		})
		inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
		scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)

		status, err := fixture.eng.Status(t.Context(), fixture.appID)
		require.NoError(t, err)
		assert.Equal(t, "running", status.State)
		assert.False(t, status.NeedsAttention)
	})

	t.Run("released leftover file", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		require.NoError(t, os.MkdirAll(fixture.stateDir, 0o700))
		raw, err := json.Marshal(state.RuntimeLockInfo{
			SchemaVersion: 1,
			PID:           1 << 30,
			Command:       "install",
			StartedAt:     time.Now().UTC().Add(-48 * time.Hour),
			WDMVersion:    "0.1.0",
		})
		require.NoError(t, err)
		// File present, flock NOT held: the normal post-release state.
		require.NoError(t, os.WriteFile(filepath.Join(fixture.stateDir, "runtime.lock"), raw, 0o600))
		inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
		scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)

		status, err := fixture.eng.Status(t.Context(), fixture.appID)
		require.NoError(t, err)
		assert.Equal(t, "running", status.State)
		assert.False(t, status.NeedsAttention)
	})
}

// TestStatus_VerifiesWdmLabelsNotJustProject is the PRD §10 / §18
// managed-only proof: a container inside the Compose project that
// lacks the wdm labels is NOT counted as the managed service — the
// service surfaces as missing instead.
func TestStatus_VerifiesWdmLabelsNotJustProject(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, func(lock *state.StackLock) { lock.LocalPorts = nil })
	drifted := statusContainerInspectStdoutWithLabels(t, map[string]string{
		"com.docker.compose.service": "app",
	}, fixture.hostPort, "running", true, false, 0, "")
	scriptStatusDocker(fixture, statusTestContainerID+"\n", drifted, nil)

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.NoError(t, err)
	assert.True(t, status.NeedsAttention)
	assert.Equal(t, []string{"container_missing"}, status.AttentionReasons)
	require.Len(t, status.Services, 1)
	assert.Equal(t, "missing", status.Services[0].State)
}

func TestStatus_RefusesMissingAndUnmanagedStacks(t *testing.T) {
	t.Parallel()

	t.Run("app not installed", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		status, err := eng.Status(t.Context(), "ghost-app")
		require.Error(t, err)
		assert.Nil(t, status)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "app is not installed")
		assert.Zero(t, fake.calls, "managed-only refusal must precede any docker call")
	})

	t.Run("directory exists but is unmanaged", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
		require.NoError(t, os.MkdirAll(filepath.Join(stackBase, "user-dir"), 0o755))
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		status, err := eng.Status(t.Context(), "user-dir")
		require.Error(t, err)
		assert.Nil(t, status)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "stack directory is not managed by wdm")
		assert.Zero(t, fake.calls, "managed-only refusal must precede any docker call")
	})

	t.Run("empty app id", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		status, err := eng.Status(t.Context(), "")
		require.Error(t, err)
		assert.Nil(t, status)
		assertUsageValidation(t, err)
	})
}

// TestStatus_RefusesBusyStackWithoutBlocking proves the chosen
// PRD §26 read-only lock posture: while a (simulated) state-changing
// operation holds the per-stack flock, Status returns the typed
// runtime-lock-held refusal promptly instead of stalling behind the
// writer — and issues no docker command.
func TestStatus_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	holdFlockExclusive(t, filepath.Join(fixture.stackPath, ".wdm.lock"))

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, status)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typedErr.Code)
	require.ErrorIs(t, err, state.ErrStackLockBusy)
	assert.Zero(t, fixture.fake.calls)
}

func TestStatus_CorruptManifestSurfacesStaleState(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, "broken-app", `{not-json`)
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	status, err := eng.Status(t.Context(), "broken-app")
	require.Error(t, err)
	assert.Nil(t, status)
	require.ErrorIs(t, err, types.ErrStaleState)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeGeneric, typedErr.Code)
	assert.Zero(t, fake.calls)
}

func TestStatus_DockerClientFactoryFailures(t *testing.T) {
	t.Parallel()

	t.Run("nil factory", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		core.SetInstallDockerClientFactoryForTest(fixture.eng, nil)

		status, err := fixture.eng.Status(t.Context(), fixture.appID)
		require.Error(t, err)
		assert.Nil(t, status)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "docker client factory is required")
	})

	t.Run("factory error", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		factoryErr := errors.New("docker socket vanished")
		core.SetInstallDockerClientFactoryForTest(fixture.eng, func(security.Redactor) (docker.Client, error) {
			return nil, factoryErr
		})

		status, err := fixture.eng.Status(t.Context(), fixture.appID)
		require.Error(t, err)
		assert.Nil(t, status)
		require.ErrorIs(t, err, factoryErr)
	})

	t.Run("factory returns nil client", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		core.SetInstallDockerClientFactoryForTest(fixture.eng, func(security.Redactor) (docker.Client, error) {
			return nil, nil
		})

		status, err := fixture.eng.Status(t.Context(), fixture.appID)
		require.Error(t, err)
		assert.Nil(t, status)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "docker client factory returned no client")
	})
}

// TestStatus_PropagatesInspectErrorUnchanged proves docker-layer failures
// from container inspection surface unchanged and stop the pipeline before compose
// validation.
func TestStatus_PropagatesInspectErrorUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	inspectErr := errors.New("docker daemon unreachable")
	fixture.fake.runFn = func(_ int, _ docker.Invocation) (docker.CommandResult, error) {
		return docker.CommandResult{ExitCode: 1}, inspectErr
	}

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, status)
	require.ErrorIs(t, err, inspectErr)
	assert.NotContains(t, fixture.fake.invocationTypes, "docker.composeConfigInvocation")
}

// TestStatus_PropagatesComposeDockerUnavailableUnchanged proves a
// daemon that dies between container inspection and compose
// validation surfaces as the hard [types.ErrCodeDockerUnavailable]
// error unchanged — never downgraded to the
// compose_validation_failed condition.
func TestStatus_PropagatesComposeDockerUnavailableUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
	unavailableErr := types.WrapError(
		types.ErrCodeDockerUnavailable,
		"docker is unavailable",
		"",
		errors.New("cannot connect to the docker daemon"),
	)
	scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, unavailableErr)

	status, err := fixture.eng.Status(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, status)
	require.ErrorIs(t, err, unavailableErr)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	assert.Contains(t, fixture.fake.invocationTypes, "docker.composeConfigInvocation")
}

func TestStatus_ContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("pre-canceled context", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		status, err := fixture.eng.Status(ctx, fixture.appID)
		require.Error(t, err)
		assert.Nil(t, status)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, fixture.fake.calls)
	})

	t.Run("canceled during compose validation propagates as error not condition", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		inspect := statusContainerInspectStdout(t, "app", fixture.appID, fixture.hostPort, "running", true, false, 0, "")
		fixture.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			switch fmt.Sprintf("%T", inv) {
			case "docker.projectContainerListInvocation":
				return docker.CommandResult{Stdout: statusTestContainerID + "\n"}, nil
			case "docker.containerInspectInvocation":
				return docker.CommandResult{Stdout: inspect}, nil
			default:
				cancel()
				return docker.CommandResult{}, context.Canceled
			}
		}

		status, err := fixture.eng.Status(ctx, fixture.appID)
		require.Error(t, err)
		assert.Nil(t, status)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestStatus_FindsStackUnderCustomDirectoryName covers the scan
// fallback: a stack installed under a directory name that differs
// from its app_id stays reachable by the identifier List reports.
func TestStatus_FindsStackUnderCustomDirectoryName(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	appID := "scan-app"
	stackPath := filepath.Join(stackBase, "custom-dir")
	hostPort := freeLocalTCPPort(t)
	writeStatusStackLock(t, stackBase, "custom-dir", statusStackLock(appID, stackPath, []int{hostPort}))

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))
	fixture := &statusTestFixture{eng: eng, appID: appID, hostPort: hostPort, fake: fake}
	inspect := statusContainerInspectStdout(t, "app", appID, hostPort, "running", true, false, 0, "")
	scriptStatusDocker(fixture, statusTestContainerID+"\n", inspect, nil)

	status, err := eng.Status(t.Context(), appID)
	require.NoError(t, err)
	assert.Equal(t, "running", status.State)
	assert.Equal(t, stackPath, status.StackPath)
	assert.Equal(t, "wdm-"+appID, status.ComposeProject)
}

func TestStatus_HonorsClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	status, err := eng.Status(t.Context(), "any-app")
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, status)
}
