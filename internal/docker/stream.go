package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/wnstify/wdm/pkg/types"
)

// Stream labels attached to every line delivered by a [RawLogSink] and
// carried into [ComposeLogEntry.Stream]. They mirror the docker process's
// source pipe: container stdout on the command's stdout, container stderr
// on its stderr.
const (
	LogStreamStdout = "stdout"
	LogStreamStderr = "stderr"
)

// ErrNilStreamExecutor is returned when [WithStreamExecutor] receives
// nil.
var ErrNilStreamExecutor = errors.New("docker: WithStreamExecutor requires non-nil executor")

// streamStderrDiagnosticLimit caps how much redacted stderr a streaming
// command retains for error mapping. Streaming commands may run
// indefinitely (`docker compose logs --follow`), so unlike
// [execClient.Run] the stream path must never buffer output without
// bound. Docker's failure diagnostics appear in the first few lines, so a
// small head capture suffices for [mapCommandError]'s indicator scan.
const streamStderrDiagnosticLimit = 4096

// Line-scanning buffer bounds for the default stream executor. Container
// log lines may exceed bufio.Scanner's 64 KiB default token size; the
// 1 MiB ceiling keeps memory bounded while tolerating realistically long
// application log lines.
const (
	streamLineInitialBufferBytes = 64 * 1024
	streamLineMaxBytes           = 1024 * 1024
)

// RawLogSink consumes one redacted raw output line from a streaming
// docker command. stream is [LogStreamStdout] or [LogStreamStderr]; line
// carries the text without its trailing newline. Implementations may
// block: the executor applies back-pressure by not reading further output
// until the sink returns. Calls are serialized, so a sink never runs
// concurrently with itself.
type RawLogSink func(stream string, line string)

// LogStreamer is the optional [Client] capability for docker commands
// whose output is consumed line by line instead of buffered into a
// [CommandResult]. The production client from [New] implements it; test
// fakes opt in by implementing the method. Wrappers that need streaming
// (currently [ComposeLogs]) type-assert the capability and fail closed
// with a typed usage-validation error when it is absent, so a
// buffered-only client can never silently absorb an unbounded stream.
type LogStreamer interface {
	// StreamLogs executes one typed streaming invocation, delivering each
	// redacted output line to sink until the upstream closes, ctx is
	// canceled, or the command fails. Raw argv stays private to the
	// implementation, exactly like [Client.Run].
	StreamLogs(ctx context.Context, inv Invocation, sink RawLogSink) error
}

// StreamExecutor is the streaming process-execution seam injected into
// execClient for line-oriented docker commands. Its command shape is
// package-private, keeping argv construction inside execClient.
// Implementations MUST serialize emit calls and MUST NOT invoke emit
// after returning.
type StreamExecutor func(ctx context.Context, cmd commandSpec, emit RawLogSink) error

// Compile-time check that execClient also satisfies [LogStreamer].
var _ LogStreamer = (*execClient)(nil)

// StreamLogs executes one typed streaming invocation through the injected
// stream executor. Every line is scrubbed by the client's redactor before
// it reaches sink, mirroring [execClient.Run]'s redact-before-return
// discipline (PRD §12, §24). A bounded head of redacted stderr feeds the
// same typed error mapping as Run, so a failed stream surfaces
// [types.ErrCodeDockerUnavailable], [types.ErrCodeNetworkFailure], or
// [types.ErrCodeGeneric] with raw stderr confined to the internal Cause.
// Context cancellation maps to [types.ErrCodeUserCanceled], the ordinary
// teardown path for follow-mode streams. Buffered invocations are
// rejected: only compose logs is consumed line by line, so every other
// invocation must go through [execClient.Run] instead.
func (c *execClient) StreamLogs(ctx context.Context, inv Invocation, sink RawLogSink) error {
	if sink == nil {
		return usageValidationError(
			"invalid docker log stream",
			errors.New("nil log stream sink"),
		)
	}
	if err := ctx.Err(); err != nil {
		return types.WrapError(
			types.ErrCodeUserCanceled,
			"docker command canceled",
			"",
			err,
		)
	}
	if _, ok := inv.(composeLogsInvocation); !ok {
		return usageValidationError(
			"invalid docker invocation",
			fmt.Errorf("%T is not a streaming invocation; use Run", inv),
		)
	}

	cmd, err := buildCommand(inv)
	if err != nil {
		return err
	}
	if err := validateCommandSpec(cmd); err != nil {
		return err
	}

	var diagnostics stderrDiagnostics
	streamErr := c.streamFn(ctx, cmd, func(stream, raw string) {
		line := c.redactor.Redact(raw)
		if stream == LogStreamStderr {
			diagnostics.append(line)
		}
		sink(stream, line)
	})
	if streamErr == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return types.WrapError(
			types.ErrCodeUserCanceled,
			"docker command canceled",
			"",
			decorateCauseWithStderr(errors.Join(streamErr, ctxErr), diagnostics.String()),
		)
	}
	return mapCommandError(streamErr, diagnostics.String())
}

// stderrDiagnostics retains a bounded head of redacted stderr lines so
// stream failures can reuse [mapCommandError] without unbounded buffering.
// No mutex is needed: stream executors serialize emit calls per the
// [StreamExecutor] contract.
type stderrDiagnostics struct {
	builder strings.Builder
}

func (d *stderrDiagnostics) append(line string) {
	if d.builder.Len() >= streamStderrDiagnosticLimit {
		return
	}
	if d.builder.Len() > 0 {
		d.builder.WriteString("\n")
	}
	if remaining := streamStderrDiagnosticLimit - d.builder.Len(); len(line) > remaining {
		line = line[:remaining]
	}
	d.builder.WriteString(line)
}

func (d *stderrDiagnostics) String() string {
	return d.builder.String()
}

// runStreamExecutor is the production [StreamExecutor]: it launches docker
// via the same SIGTERM-on-cancel command construction as
// [runCommandExecutor], scans stdout and stderr line by line, and
// serializes emit calls. Teardown is bounded: when ctx is canceled and
// the process tree still holds the pipes open after
// [dockerCancelWaitDelay], the parent-side pipe readers are force-closed
// so the scanners unblock instead of waiting on an orphaned grandchild.
func runStreamExecutor(ctx context.Context, cmd commandSpec, emit RawLogSink) error {
	if err := validateCommandSpec(cmd); err != nil {
		return err
	}
	if emit == nil {
		return usageValidationError(
			"invalid docker log stream",
			errors.New("nil log stream sink"),
		)
	}

	execCmd := buildDockerExecCmd(ctx, cmd.argv)
	stdoutPipe, err := execCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("docker: opening stdout pipe: %w", err)
	}
	stderrPipe, err := execCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("docker: opening stderr pipe: %w", err)
	}
	if err := execCmd.Start(); err != nil {
		return err
	}

	var emitMu sync.Mutex
	serialEmit := func(stream, line string) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emit(stream, line)
	}

	scanDone := make(chan struct{})
	go forceClosePipesAfterCancel(ctx, scanDone, stdoutPipe, stderrPipe)

	var wg sync.WaitGroup
	scanErrs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanErrs[0] = scanStreamLines(stdoutPipe, LogStreamStdout, serialEmit)
	}()
	go func() {
		defer wg.Done()
		scanErrs[1] = scanStreamLines(stderrPipe, LogStreamStderr, serialEmit)
	}()
	wg.Wait()
	close(scanDone)

	return errors.Join(execCmd.Wait(), scanErrs[0], scanErrs[1])
}

// forceClosePipesAfterCancel closes the parent-side pipe readers when ctx
// has been canceled and the scanners are still running after
// [dockerCancelWaitDelay]. This bounds stream teardown even when an
// orphaned process keeps the write ends open after the direct child was
// signaled; the scanners observe [fs.ErrClosed] and finish.
func forceClosePipesAfterCancel(ctx context.Context, scanDone <-chan struct{}, pipes ...io.Closer) {
	select {
	case <-scanDone:
		return
	case <-ctx.Done():
	}

	grace := time.NewTimer(dockerCancelWaitDelay)
	defer grace.Stop()
	select {
	case <-scanDone:
	case <-grace.C:
		for _, pipe := range pipes {
			_ = pipe.Close() //nolint:errcheck // best-effort teardown unblocking the scanners
		}
	}
}

// scanStreamLines delivers each line read from r to emit, labeled with
// stream. A read failure from the bounded-teardown pipe close is benign;
// any other scan failure (including a line exceeding [streamLineMaxBytes])
// is returned after the rest of the stream is discarded, so the child
// process never blocks on a full pipe waiting for a reader that gave up.
func scanStreamLines(r io.Reader, stream string, emit RawLogSink) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, streamLineInitialBufferBytes), streamLineMaxBytes)
	for scanner.Scan() {
		emit(stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, fs.ErrClosed) {
		drainRemainder(r)
		return fmt.Errorf("docker: scanning %s stream: %w", stream, err)
	}
	return nil
}

// drainRemainder discards the unread rest of a faulted stream without
// buffering it, unblocking the writing child so [runStreamExecutor]'s
// Wait can reap it.
func drainRemainder(r io.Reader) {
	_, _ = io.Copy(io.Discard, r) //nolint:errcheck // best-effort drain; the caller's scan error is the primary failure
}
