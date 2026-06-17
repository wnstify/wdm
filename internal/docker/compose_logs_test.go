package docker

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// composeLogsRunOnlyClient implements Client without the LogStreamer
// capability, proving the wrapper fails closed on buffered-only
// clients.
type composeLogsRunOnlyClient struct {
	runCalls int
}

func (f *composeLogsRunOnlyClient) Run(_ context.Context, _ Invocation) (CommandResult, error) {
	f.runCalls++
	return CommandResult{}, nil
}

// composeLogsFakeStreamClient implements both Client and LogStreamer
// so wrapper tests can intercept the typed streaming invocation and
// feed raw lines through the real parsing layer.
type composeLogsFakeStreamClient struct {
	streamFn    func(context.Context, Invocation, RawLogSink) error
	runCalls    int
	streamCalls int
}

func (f *composeLogsFakeStreamClient) Run(_ context.Context, _ Invocation) (CommandResult, error) {
	f.runCalls++
	return CommandResult{}, nil
}

func (f *composeLogsFakeStreamClient) StreamLogs(
	ctx context.Context,
	inv Invocation,
	sink RawLogSink,
) error {
	f.streamCalls++
	if f.streamFn != nil {
		return f.streamFn(ctx, inv, sink)
	}
	return nil
}

func collectComposeLogEntries(entries *[]ComposeLogEntry) ComposeLogSink {
	return func(entry ComposeLogEntry) {
		*entries = append(*entries, entry)
	}
}

func TestComposeLogs_RejectsNilClientAndNilSink(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)

	err := ComposeLogs(t.Context(), nil, project, ComposeLogsOptions{}, func(ComposeLogEntry) {})
	requireUsageValidationError(t, err)

	fake := &composeLogsFakeStreamClient{}
	err = ComposeLogs(t.Context(), fake, project, ComposeLogsOptions{}, nil)
	requireUsageValidationError(t, err)
	require.Zero(t, fake.streamCalls)
	require.Zero(t, fake.runCalls)
}

func TestComposeLogs_RejectsInvalidProjectAndOptionsBeforeStreaming(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	tests := []struct {
		name    string
		project ComposeProject
		opts    ComposeLogsOptions
	}{
		{
			name: "relative compose file",
			project: ComposeProject{
				ComposeFile: "docker-compose.yml",
				EnvFile:     project.EnvFile,
				ProjectName: project.ProjectName,
			},
		},
		{
			name: "blank env file",
			project: ComposeProject{
				ComposeFile: project.ComposeFile,
				EnvFile:     " ",
				ProjectName: project.ProjectName,
			},
		},
		{
			name: "uppercase project name",
			project: ComposeProject{
				ComposeFile: project.ComposeFile,
				EnvFile:     project.EnvFile,
				ProjectName: "Wdm-App",
			},
		},
		{
			name:    "negative tail",
			project: project,
			opts:    ComposeLogsOptions{Tail: -1},
		},
		{
			name:    "empty service name",
			project: project,
			opts:    ComposeLogsOptions{Services: []string{""}},
		},
		{
			name:    "leading dash service name",
			project: project,
			opts:    ComposeLogsOptions{Services: []string{"-evil"}},
		},
		{
			name:    "uppercase service name",
			project: project,
			opts:    ComposeLogsOptions{Services: []string{"App"}},
		},
		{
			name:    "whitespace service name",
			project: project,
			opts:    ComposeLogsOptions{Services: []string{" app"}},
		},
		{
			name:    "slash service name",
			project: project,
			opts:    ComposeLogsOptions{Services: []string{"a/b"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &composeLogsFakeStreamClient{}
			err := ComposeLogs(t.Context(), fake, tt.project, tt.opts, func(ComposeLogEntry) {})
			requireUsageValidationError(t, err)
			require.Zero(t, fake.streamCalls)
			require.Zero(t, fake.runCalls)
		})
	}
}

func TestComposeLogs_RejectsBufferedOnlyClient(t *testing.T) {
	t.Parallel()

	fake := &composeLogsRunOnlyClient{}
	err := ComposeLogs(
		t.Context(),
		fake,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		func(ComposeLogEntry) {},
	)
	requireUsageValidationError(t, err)
	require.Zero(t, fake.runCalls, "capability refusal must not fall back to buffered Run")
}

func TestComposeLogs_SendsPrivateInvocationAndReturnsStreamErrorUnchanged(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	wantErr := errors.New("stream failed")

	fake := &composeLogsFakeStreamClient{
		streamFn: func(_ context.Context, inv Invocation, _ RawLogSink) error {
			logsInv, ok := inv.(composeLogsInvocation)
			require.True(t, ok, "wrapper must send the private compose-logs invocation")
			require.Equal(t, project.ComposeFile, logsInv.composeFile)
			require.Equal(t, project.EnvFile, logsInv.envFile)
			require.Equal(t, project.ProjectName, logsInv.projectName)
			require.True(t, logsInv.follow)
			require.Equal(t, 25, logsInv.tail)
			require.Equal(t, []string{"app", "db"}, logsInv.services)
			return wantErr
		},
	}

	err := ComposeLogs(
		t.Context(),
		fake,
		project,
		ComposeLogsOptions{Follow: true, Tail: 25, Services: []string{"app", "db"}},
		func(ComposeLogEntry) {},
	)
	require.Same(t, wantErr, err, "stream errors must propagate unchanged")
	require.Equal(t, 1, fake.streamCalls)
}

func TestComposeLogs_ParsesPrefixedLinesAndDropsDiagnostics(t *testing.T) {
	t.Parallel()

	fake := &composeLogsFakeStreamClient{
		streamFn: func(_ context.Context, _ Invocation, sink RawLogSink) error {
			sink(LogStreamStdout, "uptime-kuma-mariadb  | 2026-06-10T08:15:30.123456789Z ready for connections")
			sink(LogStreamStderr, "uptime-kuma  | 2026-06-10T08:15:31.000000000Z [warn] slow query")
			sink(LogStreamStderr, "level=warning msg=\"compose diagnostic without prefix\"")
			sink(LogStreamStdout, "uptime-kuma  | timestampless text survives")
			sink(LogStreamStdout, "uptime-kuma  | 2026-06-10T08:15:32.000000000Z ")
			return nil
		},
	}

	var entries []ComposeLogEntry
	err := ComposeLogs(
		t.Context(),
		fake,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		collectComposeLogEntries(&entries),
	)
	require.NoError(t, err)
	require.Len(t, entries, 4, "unprefixed compose diagnostics must be dropped")

	first := entries[0]
	require.Equal(t, "uptime-kuma-mariadb", first.ContainerName)
	require.Equal(t, LogStreamStdout, first.Stream)
	require.Equal(t, "ready for connections", first.Text)
	require.True(
		t,
		first.Timestamp.Equal(time.Date(2026, time.June, 10, 8, 15, 30, 123456789, time.UTC)),
		"timestamp must parse from the --timestamps token",
	)

	second := entries[1]
	require.Equal(t, "uptime-kuma", second.ContainerName)
	require.Equal(t, LogStreamStderr, second.Stream)
	require.Equal(t, "[warn] slow query", second.Text)

	third := entries[2]
	require.Equal(t, "uptime-kuma", third.ContainerName)
	require.True(t, third.Timestamp.IsZero(), "unparsable timestamp must stay zero")
	require.Equal(t, "timestampless text survives", third.Text)

	fourth := entries[3]
	require.Equal(t, "", fourth.Text, "empty message after timestamp must survive as empty text")
	require.False(t, fourth.Timestamp.IsZero())
}

func TestComposeLogs_PreservesEmissionOrder(t *testing.T) {
	t.Parallel()

	fake := &composeLogsFakeStreamClient{
		streamFn: func(_ context.Context, _ Invocation, sink RawLogSink) error {
			sink(LogStreamStdout, "app-1  | 2026-06-10T08:15:30.000000001Z line one")
			sink(LogStreamStderr, "app-1  | 2026-06-10T08:15:30.000000002Z line two")
			sink(LogStreamStdout, "app-1  | 2026-06-10T08:15:30.000000003Z line three")
			return nil
		},
	}

	var entries []ComposeLogEntry
	err := ComposeLogs(
		t.Context(),
		fake,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		collectComposeLogEntries(&entries),
	)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, "line one", entries[0].Text)
	require.Equal(t, "line two", entries[1].Text)
	require.Equal(t, "line three", entries[2].Text)
}

func TestStreamLogs_BuildsExactArgv(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	baseArgv := []string{
		"compose",
		"-f",
		project.ComposeFile,
		"--env-file",
		project.EnvFile,
		"--project-name",
		project.ProjectName,
		"logs",
		"--no-color",
		"--timestamps",
	}

	tests := []struct {
		name    string
		opts    ComposeLogsOptions
		wantArg []string
	}{
		{
			name:    "history only",
			opts:    ComposeLogsOptions{},
			wantArg: baseArgv,
		},
		{
			name:    "tail",
			opts:    ComposeLogsOptions{Tail: 250},
			wantArg: append(append([]string{}, baseArgv...), "--tail", "250"),
		},
		{
			name:    "follow",
			opts:    ComposeLogsOptions{Follow: true},
			wantArg: append(append([]string{}, baseArgv...), "--follow"),
		},
		{
			name: "tail follow and services",
			opts: ComposeLogsOptions{Follow: true, Tail: 10, Services: []string{"app", "db"}},
			wantArg: append(
				append([]string{}, baseArgv...),
				"--tail", "10", "--follow", "app", "db",
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			invoked := false
			streamFn := func(_ context.Context, cmd commandSpec, _ RawLogSink) error {
				invoked = true
				require.Equal(t, tt.wantArg, cmd.argv)
				return nil
			}

			client, err := New(WithStreamExecutor(streamFn))
			require.NoError(t, err)

			err = ComposeLogs(t.Context(), client, project, tt.opts, func(ComposeLogEntry) {})
			require.NoError(t, err)
			require.True(t, invoked)
		})
	}
}

func TestStreamLogs_RedactsLinesBeforeSink(t *testing.T) {
	t.Parallel()

	secret := "hunter2-super-secret-value"
	streamFn := func(_ context.Context, _ commandSpec, emit RawLogSink) error {
		emit(LogStreamStdout, "app-1  | 2026-06-10T08:15:30.000000000Z DB_PASSWORD="+secret)
		return nil
	}

	client, err := New(WithStreamExecutor(streamFn))
	require.NoError(t, err)

	var entries []ComposeLogEntry
	err = ComposeLogs(
		t.Context(),
		client,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		collectComposeLogEntries(&entries),
	)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "DB_PASSWORD=[REDACTED]", entries[0].Text,
		"secret-shaped content must be redacted before the sink sees it")
	require.NotContains(t, entries[0].Text, secret)
}

func TestStreamLogs_RejectsNilSinkAndCanceledContext(t *testing.T) {
	t.Parallel()

	client, err := New(WithStreamExecutor(func(context.Context, commandSpec, RawLogSink) error {
		return nil
	}))
	require.NoError(t, err)

	streamer, ok := client.(LogStreamer)
	require.True(t, ok, "production client must implement LogStreamer")

	project := validComposeProjectForDeployTests(t)
	inv, err := newComposeLogsInvocation(project, ComposeLogsOptions{})
	require.NoError(t, err)

	err = streamer.StreamLogs(t.Context(), inv, nil)
	requireUsageValidationError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = streamer.StreamLogs(ctx, inv, func(string, string) {})
	requireTypedCode(t, err, types.ErrCodeUserCanceled)
}

func TestRun_RejectsComposeLogsStreamingInvocation(t *testing.T) {
	t.Parallel()

	client, err := New(WithCommandExecutor(func(context.Context, commandSpec) (CommandResult, error) {
		t.Error("buffered executor must not run for a streaming invocation")
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	project := validComposeProjectForDeployTests(t)
	inv, err := newComposeLogsInvocation(project, ComposeLogsOptions{Follow: true})
	require.NoError(t, err)

	_, err = client.Run(t.Context(), inv)
	requireUsageValidationError(t, err)
}

func TestRun_CanceledContextPrecedesComposeLogsStreamingRejection(t *testing.T) {
	t.Parallel()

	client, err := New(WithCommandExecutor(func(context.Context, commandSpec) (CommandResult, error) {
		t.Error("buffered executor must not run for a canceled context")
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	project := validComposeProjectForDeployTests(t)
	inv, err := newComposeLogsInvocation(project, ComposeLogsOptions{Follow: true})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = client.Run(ctx, inv)
	requireTypedCode(t, err, types.ErrCodeUserCanceled)
}

func TestStreamLogs_RejectsBufferedInvocation(t *testing.T) {
	t.Parallel()

	client, err := New(WithStreamExecutor(func(context.Context, commandSpec, RawLogSink) error {
		t.Error("stream executor must not run for a buffered invocation")
		return nil
	}))
	require.NoError(t, err)

	streamer, ok := client.(LogStreamer)
	require.True(t, ok, "production client must implement LogStreamer")

	inv, err := newComposeDownInvocation(validComposeProjectForDeployTests(t))
	require.NoError(t, err)

	err = streamer.StreamLogs(t.Context(), inv, func(string, string) {})
	requireUsageValidationError(t, err)
}

func TestNew_RejectsNilStreamExecutor(t *testing.T) {
	t.Parallel()

	_, err := New(WithStreamExecutor(nil))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNilStreamExecutor)
}

func TestStreamLogs_DefaultExecutorStreamsBothPipesAndRedacts(t *testing.T) {
	fakeDocker := `#!/bin/sh
printf 'web-1  | 2026-06-10T08:15:30.000000000Z hello stdout\n'
printf 'db-1   | 2026-06-10T08:15:31.000000000Z POSTGRES_PASSWORD=fake-pg-pass-for-test\n' >&2
printf 'web-1  | 2026-06-10T08:15:32.000000000Z goodbye stdout\n'
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	var entries []ComposeLogEntry
	err = ComposeLogs(
		t.Context(),
		client,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{Tail: 5},
		collectComposeLogEntries(&entries),
	)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	var stdoutTexts []string
	for _, entry := range entries {
		switch entry.Stream {
		case LogStreamStdout:
			require.Equal(t, "web-1", entry.ContainerName)
			stdoutTexts = append(stdoutTexts, entry.Text)
		case LogStreamStderr:
			require.Equal(t, "db-1", entry.ContainerName)
			require.Equal(t, "POSTGRES_PASSWORD=[REDACTED]", entry.Text,
				"stderr content must be redacted before the sink")
			require.NotContains(t, entry.Text, "fake-pg-pass-for-test")
		default:
			t.Fatalf("unexpected stream %q", entry.Stream)
		}
	}
	require.Equal(t, []string{"hello stdout", "goodbye stdout"}, stdoutTexts,
		"per-pipe ordering must be preserved")
}

func TestStreamLogs_DefaultExecutorRunsExpectedArgv(t *testing.T) {
	fakeDocker := `#!/bin/sh
printf 'argv='
for arg in "$@"; do
  printf '[%s]' "$arg"
done
printf '\n'
`
	useFakeDocker(t, fakeDocker)

	project := validComposeProjectForDeployTests(t)
	client, err := New()
	require.NoError(t, err)

	streamer, ok := client.(LogStreamer)
	require.True(t, ok)

	inv, err := newComposeLogsInvocation(project, ComposeLogsOptions{
		Follow:   true,
		Tail:     7,
		Services: []string{"app"},
	})
	require.NoError(t, err)

	var rawLines []string
	err = streamer.StreamLogs(t.Context(), inv, func(stream, line string) {
		if stream == LogStreamStdout {
			rawLines = append(rawLines, line)
		}
	})
	require.NoError(t, err)
	require.Len(t, rawLines, 1)
	require.Equal(
		t,
		"argv=[compose][-f]["+project.ComposeFile+"][--env-file]["+project.EnvFile+
			"][--project-name]["+project.ProjectName+
			"][logs][--no-color][--timestamps][--tail][7][--follow][app]",
		rawLines[0],
	)
}

func TestStreamLogs_DefaultExecutorContextCancellationMidStream(t *testing.T) {
	// exec replaces the shell so SIGTERM reaches the long-running
	// process directly, modeling docker's signal-respecting teardown;
	// a plain `sleep` child would orphan the inherited pipes and push
	// the test onto the 30s bounded-teardown watchdog path instead.
	fakeDocker := `#!/bin/sh
printf 'web-1  | 2026-06-10T08:15:30.000000000Z first line\n'
exec sleep 60
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lineCh := make(chan ComposeLogEntry, 16)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ComposeLogs(
			ctx,
			client,
			validComposeProjectForDeployTests(t),
			ComposeLogsOptions{Follow: true},
			func(entry ComposeLogEntry) { lineCh <- entry },
		)
	}()

	select {
	case entry := <-lineCh:
		require.Equal(t, "web-1", entry.ContainerName)
		require.Equal(t, "first line", entry.Text)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the first streamed line")
	}

	cancel()

	select {
	case streamErr := <-errCh:
		requireTypedCode(t, streamErr, types.ErrCodeUserCanceled)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for stream teardown after cancellation")
	}
}

func TestStreamLogs_DefaultExecutorMapsDaemonUnreachableAndRedactsCause(t *testing.T) {
	fakeDocker := `#!/bin/sh
echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. DB_PASSWORD=fake-daemon-secret" >&2
exit 1
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	err = ComposeLogs(
		t.Context(),
		client,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		func(ComposeLogEntry) {},
	)
	requireTypedCode(t, err, types.ErrCodeDockerUnavailable)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.NotNil(t, typedErr.Cause)
	require.NotContains(t, typedErr.Cause.Error(), "fake-daemon-secret",
		"stderr captured for error mapping must be redacted")
}

func TestStreamLogs_DefaultExecutorMapsMissingDockerBinaryToDockerUnavailable(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	client, err := New()
	require.NoError(t, err)

	err = ComposeLogs(
		t.Context(),
		client,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		func(ComposeLogEntry) {},
	)
	requireTypedCode(t, err, types.ErrCodeDockerUnavailable)
}

func TestStreamLogs_DefaultExecutorBoundsOverlongLinesWithoutHanging(t *testing.T) {
	// 1.1 MB without a newline exceeds streamLineMaxBytes; the scanner
	// must fail typed instead of buffering without bound, and the
	// post-fault drain must keep the writing child from blocking on a
	// full pipe so Wait still reaps it.
	fakeDocker := `#!/bin/sh
head -c 1100000 /dev/zero | tr '\0' 'a'
echo
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	err = ComposeLogs(
		t.Context(),
		client,
		validComposeProjectForDeployTests(t),
		ComposeLogsOptions{},
		func(ComposeLogEntry) {},
	)
	requireTypedCode(t, err, types.ErrCodeGeneric)
	require.ErrorIs(t, err, bufio.ErrTooLong,
		"the bounded-line failure must stay inspectable in the chain")
}

func TestStderrDiagnostics_CapsCapturedBytes(t *testing.T) {
	t.Parallel()

	var full stderrDiagnostics
	full.append(strings.Repeat("a", streamStderrDiagnosticLimit))
	full.append("overflow line")
	require.Len(t, full.String(), streamStderrDiagnosticLimit)
	require.NotContains(t, full.String(), "overflow")

	var truncated stderrDiagnostics
	truncated.append("head")
	truncated.append(strings.Repeat("b", streamStderrDiagnosticLimit))
	require.Len(t, truncated.String(), streamStderrDiagnosticLimit,
		"capture must stay capped even across the joining newline")
}

func TestValidateCommandSpec_AllowsComposeLogsShapes(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	prefix := []string{
		"compose",
		"-f",
		project.ComposeFile,
		"--env-file",
		project.EnvFile,
		"--project-name",
		project.ProjectName,
		"logs",
		"--no-color",
		"--timestamps",
	}

	tests := []struct {
		name   string
		suffix []string
	}{
		{name: "base", suffix: nil},
		{name: "tail", suffix: []string{"--tail", "100"}},
		{name: "follow", suffix: []string{"--follow"}},
		{name: "tail and follow", suffix: []string{"--tail", "1", "--follow"}},
		{name: "services", suffix: []string{"app", "db"}},
		{name: "full combination", suffix: []string{"--tail", "10", "--follow", "app", "db"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			argv := append(append([]string{}, prefix...), tt.suffix...)
			require.NoError(t, validateCommandSpec(commandSpec{argv: argv}))
		})
	}
}

func TestValidateCommandSpec_RejectsUnsafeComposeLogsShapes(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	prefix := []string{
		"compose",
		"-f",
		project.ComposeFile,
		"--env-file",
		project.EnvFile,
		"--project-name",
		project.ProjectName,
		"logs",
		"--no-color",
		"--timestamps",
	}

	withSuffix := func(suffix ...string) []string {
		return append(append([]string{}, prefix...), suffix...)
	}

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "missing no-color",
			argv: []string{
				"compose", "-f", project.ComposeFile,
				"--env-file", project.EnvFile,
				"--project-name", project.ProjectName,
				"logs", "--timestamps",
			},
		},
		{
			name: "missing timestamps",
			argv: []string{
				"compose", "-f", project.ComposeFile,
				"--env-file", project.EnvFile,
				"--project-name", project.ProjectName,
				"logs", "--no-color",
			},
		},
		{name: "zero tail", argv: withSuffix("--tail", "0")},
		{name: "negative tail", argv: withSuffix("--tail", "-5")},
		{name: "non-numeric tail", argv: withSuffix("--tail", "all")},
		{name: "non-canonical tail", argv: withSuffix("--tail", "05")},
		{name: "signed tail", argv: withSuffix("--tail", "+5")},
		{name: "tail missing value", argv: withSuffix("--tail")},
		{name: "follow before tail", argv: withSuffix("--follow", "--tail", "5")},
		{name: "duplicate follow", argv: withSuffix("--follow", "--follow")},
		{name: "unknown flag", argv: withSuffix("--until=1h")},
		{name: "flag-shaped service", argv: withSuffix("-evil")},
		{name: "uppercase service", argv: withSuffix("App")},
		{name: "whitespace service", argv: withSuffix(" app")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCommandSpec(commandSpec{argv: tt.argv})
			requireUsageValidationError(t, err)
		})
	}
}
