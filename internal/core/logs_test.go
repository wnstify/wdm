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
	"github.com/wnstify/wdm/pkg/types"
)

const logsSecondTestContainerID = "fedcba9876543210fedcba9876543210"

// logsContainerInspectStdout fabricates the 8-line safe-field inspect
// output for a running container with caller-controlled name and
// labels, so Logs tests can map arbitrary container names (including
// container_name overrides) onto services.
func logsContainerInspectStdout(t *testing.T, containerName string, labels map[string]string) string {
	t.Helper()

	rawLabels, err := json.Marshal(labels)
	require.NoError(t, err)
	return fmt.Sprintf(
		"%q\n%s\n%q\n%t\n%t\n%d\n%q\n%s\n",
		"/"+containerName, rawLabels, "running", true, false, 0, "healthy", "{}",
	)
}

// scriptLogsDocker wires the fixture's fake to answer the Logs
// invocation sequence: one container list, one inspect per listed ID
// (answered in order), then the streaming call.
func scriptLogsDocker(
	t *testing.T,
	f *statusTestFixture,
	listStdout string,
	inspectStdouts []string,
	streamFn func(ctx context.Context, inv docker.Invocation, sink docker.RawLogSink) error,
) {
	t.Helper()

	inspectCalls := 0
	f.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: listStdout}, nil
		case "docker.containerInspectInvocation":
			require.Less(t, inspectCalls, len(inspectStdouts), "unexpected extra container inspect call")
			out := inspectStdouts[inspectCalls]
			inspectCalls++
			return docker.CommandResult{Stdout: out}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}
	f.fake.streamFn = streamFn
}

func managedLogsLabels(service, appID string) map[string]string {
	return map[string]string{
		"com.docker.compose.service": service,
		"wdm.managed":                "true",
		"wdm.app":                    appID,
	}
}

func TestLogs_StreamsManagedContainerLines(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	managedName := "status-app-app-1"
	intruderName := "intruder-1"
	scriptLogsDocker(
		t,
		fixture,
		statusTestContainerID+"\n"+logsSecondTestContainerID+"\n",
		[]string{
			logsContainerInspectStdout(t, managedName, managedLogsLabels("app", fixture.appID)),
			// Same Compose project, but drifted wdm labels: its lines
			// must never reach the callback (PRD §10).
			logsContainerInspectStdout(t, intruderName, map[string]string{
				"com.docker.compose.service": "intruder",
				"wdm.managed":                "true",
				"wdm.app":                    "other-app",
			}),
		},
		func(_ context.Context, _ docker.Invocation, sink docker.RawLogSink) error {
			sink("stdout", managedName+"  | 2026-06-10T08:15:30.123456789Z ready for connections")
			sink("stderr", managedName+"  | 2026-06-10T08:15:31.000000000Z [warn] slow query")
			sink("stdout", intruderName+"  | 2026-06-10T08:15:32.000000000Z unmanaged content")
			sink("stderr", "level=warning msg=\"compose diagnostic without prefix\"")
			return nil
		},
	)

	var lines []types.LogLine
	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(line types.LogLine) {
		lines = append(lines, line)
	})
	require.NoError(t, err)
	require.Len(t, lines, 2,
		"unmanaged-container lines and unprefixed diagnostics must be dropped")

	first := lines[0]
	assert.Equal(t, fixture.appID, first.AppID)
	assert.Equal(t, "wdm-"+fixture.appID, first.ComposeProject)
	assert.Equal(t, managedName, first.ContainerName)
	assert.Equal(t, "app", first.Service)
	assert.Equal(t, "stdout", first.Stream)
	assert.Equal(t, "ready for connections", first.Text)
	assert.True(
		t,
		first.Timestamp.Equal(time.Date(2026, time.June, 10, 8, 15, 30, 123456789, time.UTC)),
		"timestamp must come from the --timestamps token",
	)

	second := lines[1]
	assert.Equal(t, "stderr", second.Stream)
	assert.Equal(t, "[warn] slow query", second.Text)
	assert.Equal(t, "app", second.Service)

	assert.Equal(t, 1, fixture.fake.streamCalls)
	assert.Contains(t, fixture.fake.invocationTypes, "docker.composeLogsInvocation")
}

func TestLogs_RefusalsBeforeDocker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(t *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn)
	}{
		{
			name: "nil callback",
			prepare: func(_ *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{AppID: fixture.appID}, nil
			},
		},
		{
			name: "empty app id",
			prepare: func(_ *testing.T, _ *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{}, func(types.LogLine) {}
			},
		},
		{
			name: "negative tail",
			prepare: func(_ *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{AppID: fixture.appID, Tail: -1}, func(types.LogLine) {}
			},
		},
		{
			name: "app not installed",
			prepare: func(_ *testing.T, _ *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{AppID: "ghost"}, func(types.LogLine) {}
			},
		},
		{
			name: "unmanaged stack directory",
			prepare: func(t *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(fixture.stackBase, "plaindir"), 0o755))
				return types.LogsRequest{AppID: "plaindir"}, func(types.LogLine) {}
			},
		},
		{
			name: "service not in manifest",
			prepare: func(_ *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{
					AppID:    fixture.appID,
					Services: []string{"nope"},
				}, func(types.LogLine) {}
			},
		},
		{
			name: "empty service name",
			prepare: func(_ *testing.T, fixture *statusTestFixture) (types.LogsRequest, types.LogLineFn) {
				return types.LogsRequest{
					AppID:    fixture.appID,
					Services: []string{""},
				}, func(types.LogLine) {}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newStatusFixture(t, nil)
			req, onLine := tt.prepare(t, fixture)

			err := fixture.eng.Logs(t.Context(), req, onLine)
			require.Error(t, err)
			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
			assert.Zero(t, fixture.fake.calls, "refusal must run zero docker commands")
			assert.Zero(t, fixture.fake.streamCalls, "refusal must never open a stream")
		})
	}
}

func TestLogs_UnknownServiceHintNamesKnownSet(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	err := fixture.eng.Logs(
		t.Context(),
		types.LogsRequest{AppID: fixture.appID, Services: []string{"nope"}},
		func(types.LogLine) {},
	)
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	assert.Contains(t, typedErr.Hint, "app", "hint must name the manifest's known services")
}

func TestLogs_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	holdFlockExclusive(t, filepath.Join(fixture.stackPath, ".wdm.lock"))

	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(types.LogLine) {})
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typedErr.Code)
	assert.Zero(t, fixture.fake.calls)
	assert.Zero(t, fixture.fake.streamCalls)
}

func TestLogs_PropagatesInspectErrorUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	wantErr := types.WrapError(
		types.ErrCodeDockerUnavailable,
		"docker is unavailable",
		"",
		errors.New("daemon down"),
	)
	fixture.fake.runFn = func(_ int, _ docker.Invocation) (docker.CommandResult, error) {
		return docker.CommandResult{}, wantErr
	}

	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(types.LogLine) {})
	require.Same(t, wantErr, err, "docker-layer errors must propagate unchanged")
	assert.Zero(t, fixture.fake.streamCalls, "stream must not open after a failed inspection")
}

func TestLogs_PassesKnownServiceRestrictionThrough(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	scriptLogsDocker(
		t,
		fixture,
		statusTestContainerID+"\n",
		[]string{logsContainerInspectStdout(t, "status-app-app-1", managedLogsLabels("app", fixture.appID))},
		func(_ context.Context, _ docker.Invocation, _ docker.RawLogSink) error {
			return nil
		},
	)

	err := fixture.eng.Logs(
		t.Context(),
		types.LogsRequest{AppID: fixture.appID, Follow: true, Tail: 50, Services: []string{"app", "app"}},
		func(types.LogLine) {},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, fixture.fake.streamCalls)
	assert.Contains(t, fixture.fake.invocationTypes, "docker.composeLogsInvocation")
}

func TestLogs_StreamErrorPropagatesAfterDeliveredLines(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	wantErr := errors.New("stream broke")
	scriptLogsDocker(
		t,
		fixture,
		statusTestContainerID+"\n",
		[]string{logsContainerInspectStdout(t, "status-app-app-1", managedLogsLabels("app", fixture.appID))},
		func(_ context.Context, _ docker.Invocation, sink docker.RawLogSink) error {
			sink("stdout", "status-app-app-1  | 2026-06-10T08:15:30.000000000Z before the fault")
			return wantErr
		},
	)

	var lines []types.LogLine
	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(line types.LogLine) {
		lines = append(lines, line)
	})
	require.Same(t, wantErr, err)
	require.Len(t, lines, 1, "lines delivered before the fault must reach the callback")
	assert.Equal(t, "before the fault", lines[0].Text)
}

func TestLogs_ContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("canceled before any docker call", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := fixture.eng.Logs(ctx, types.LogsRequest{AppID: fixture.appID}, func(types.LogLine) {})
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, fixture.fake.calls)
		assert.Zero(t, fixture.fake.streamCalls)
	})

	t.Run("canceled mid-stream propagates typed error", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		canceled := types.WrapError(
			types.ErrCodeUserCanceled,
			"docker command canceled",
			"",
			context.Canceled,
		)
		scriptLogsDocker(
			t,
			fixture,
			statusTestContainerID+"\n",
			[]string{logsContainerInspectStdout(t, "status-app-app-1", managedLogsLabels("app", fixture.appID))},
			func(_ context.Context, _ docker.Invocation, sink docker.RawLogSink) error {
				sink("stdout", "status-app-app-1  | 2026-06-10T08:15:30.000000000Z first line")
				cancel()
				return canceled
			},
		)

		var lines []types.LogLine
		err := fixture.eng.Logs(ctx, types.LogsRequest{AppID: fixture.appID, Follow: true}, func(line types.LogLine) {
			lines = append(lines, line)
		})
		require.Same(t, canceled, err, "the docker layer's typed cancel error must propagate unchanged")
		require.Len(t, lines, 1)
	})
}

func TestLogs_WiresActiveRedactorIntoDockerClientFactory(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	scriptLogsDocker(
		t,
		fixture,
		statusTestContainerID+"\n",
		[]string{logsContainerInspectStdout(t, "status-app-app-1", managedLogsLabels("app", fixture.appID))},
		nil,
	)

	var captured security.Redactor
	core.SetInstallDockerClientFactoryForTest(fixture.eng, func(r security.Redactor) (docker.Client, error) {
		captured = r
		return fixture.fake, nil
	})

	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(types.LogLine) {})
	require.NoError(t, err)
	require.NotNil(t, captured, "Logs must construct its docker client with a redactor")
	assert.Equal(t, "PASSWORD=[REDACTED]", captured.Redact("PASSWORD=hunter2"),
		"the wired redactor must scrub secret-shaped content")
}

func TestLogs_HonorsClosed(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	require.NoError(t, fixture.eng.Close())

	err := fixture.eng.Logs(t.Context(), types.LogsRequest{AppID: fixture.appID}, func(types.LogLine) {})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Zero(t, fixture.fake.calls)
	assert.Zero(t, fixture.fake.streamCalls)
}
