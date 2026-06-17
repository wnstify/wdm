package docker

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wnstify/wdm/pkg/types"
)

// composeServiceNamePattern is the strict shape for Compose service names
// accepted as trailing `docker compose logs` argv tokens. The required
// leading lowercase-alphanumeric character makes flag injection (`-f`,
// `--until`,...) structurally impossible, mirroring
// [composeProjectNamePattern]; no whitespace is trimmed into validity.
var composeServiceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// composeLogPrefixPattern matches the `<container-name> | ` prefix
// Compose V2 writes before every container log line when color output
// is disabled: the container name (no whitespace, no pipe), right-
// padding spaces, a pipe, and a single space.
var composeLogPrefixPattern = regexp.MustCompile(`^([^\s|]+) +\| `)

// composeLogsFixedArgvLen is the length of the fixed allowlisted prefix
// `compose -f <file> --env-file <file> --project-name <name> logs
// --no-color --timestamps`, before any optional tail/follow flags and
// trailing service names.
const composeLogsFixedArgvLen = 10

// ComposeLogsOptions configures optional `docker compose logs` flags.
// The zero value streams the full history of every service without
// following.
type ComposeLogsOptions struct {
	// Follow keeps the stream open for new lines (`--follow`).
	Follow bool

	// Tail limits the initial output to the last N lines per service
	// (`--tail N`). Zero streams all available history; negative
	// values are rejected.
	Tail int

	// Services restricts streaming to the named Compose services.
	// Empty means every service in the project.
	Services []string
}

// ComposeLogEntry is one parsed container log line from
// `docker compose logs`. Text has already passed through the client's
// redactor and had the container-name prefix and timestamp stripped.
type ComposeLogEntry struct {
	// ContainerName is the parsed container-name prefix
	// (e.g. "uptime-kuma-mariadb" or "wdm-app-app-1").
	ContainerName string

	// Timestamp is the log-driver emission time parsed from the
	// `--timestamps` token; zero when the token was absent or
	// unparsable.
	Timestamp time.Time

	// Stream is [LogStreamStdout] or [LogStreamStderr].
	Stream string

	// Text is the redacted log message after prefix and timestamp
	// stripping.
	Text string
}

// ComposeLogSink consumes parsed log entries during streaming. Calls
// are serialized; implementations may block to apply back-pressure.
type ComposeLogSink func(entry ComposeLogEntry)

type composeLogsInvocation struct {
	composeFile string
	envFile     string
	projectName string
	follow      bool
	tail        int
	services    []string
}

func (composeLogsInvocation) isDockerInvocation() {}

// ComposeLogs streams `docker compose logs` output for a validated
// project through sink until the upstream closes, ctx is canceled, or the
// command fails. The argv shape is
//
//	compose -f <file> --env-file <file> --project-name <name>
//	    logs --no-color --timestamps [--tail N] [--follow] [service...]
//
// `--no-color` keeps the per-line container-name prefix parseable;
// `--timestamps` carries the emission time the [ComposeLogEntry] contract
// strips into Timestamp. Lines without a container-name prefix (Compose's
// own diagnostics) are dropped from the sink, but on failure still reach
// the typed error's internal Cause via the client's bounded stderr
// capture.
// The client must implement [LogStreamer]; a buffered-only client is
// rejected with a typed usage-validation error, so an unbounded stream
// can never be silently buffered through [Client.Run].
func ComposeLogs(
	ctx context.Context,
	client Client,
	project ComposeProject,
	opts ComposeLogsOptions,
	sink ComposeLogSink,
) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}
	if sink == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"log sink is required",
			"pass a non-nil log sink to receive streamed lines",
		)
	}

	inv, err := newComposeLogsInvocation(project, opts)
	if err != nil {
		return err
	}

	streamer, ok := client.(LogStreamer)
	if !ok {
		return types.WrapError(
			types.ErrCodeUsageValidation,
			"docker client does not support log streaming",
			"use a client constructed by docker.New or a fake implementing LogStreamer",
			fmt.Errorf("client type %T does not implement StreamLogs", client),
		)
	}

	return streamer.StreamLogs(ctx, inv, func(stream, line string) {
		entry, ok := parseComposeLogLine(stream, line)
		if !ok {
			return
		}
		sink(entry)
	})
}

func newComposeLogsInvocation(
	project ComposeProject,
	opts ComposeLogsOptions,
) (composeLogsInvocation, error) {
	normalizedProject, err := validateComposeProject(project)
	if err != nil {
		return composeLogsInvocation{}, err
	}
	normalizedOpts, err := validateComposeLogsOptions(opts)
	if err != nil {
		return composeLogsInvocation{}, err
	}

	return composeLogsInvocation{
		composeFile: normalizedProject.ComposeFile,
		envFile:     normalizedProject.EnvFile,
		projectName: normalizedProject.ProjectName,
		follow:      normalizedOpts.Follow,
		tail:        normalizedOpts.Tail,
		services:    normalizedOpts.Services,
	}, nil
}

func validateComposeLogsOptions(opts ComposeLogsOptions) (ComposeLogsOptions, error) {
	if opts.Tail < 0 {
		return ComposeLogsOptions{}, types.WrapError(
			types.ErrCodeUsageValidation,
			"log tail must be zero or positive",
			"pass 0 to stream all history or a positive line count",
			fmt.Errorf("tail %d is negative", opts.Tail),
		)
	}

	services := make([]string, 0, len(opts.Services))
	for _, raw := range opts.Services {
		service, err := validateComposeServiceName(raw)
		if err != nil {
			return ComposeLogsOptions{}, err
		}
		services = append(services, service)
	}

	return ComposeLogsOptions{
		Follow:   opts.Follow,
		Tail:     opts.Tail,
		Services: services,
	}, nil
}

func validateComposeServiceName(raw string) (string, error) {
	if !composeServiceNamePattern.MatchString(raw) {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"compose service name is invalid",
			"use lowercase letters/digits first, then lowercase letters/digits/dot/underscore/hyphen",
			fmt.Errorf("service name %q does not match allowed format", raw),
		)
	}
	return raw, nil
}

// validateComposeLogsArgvSuffix verifies the variable-length tokens of an
// allowlisted compose-logs argv after the fixed prefix: an optional
// `--tail <n>` pair (n a canonical positive integer), an optional
// `--follow`, then zero or more validated service names. Service-name
// validation rejects any leading dash, so no other flag can hide in the
// trailing positions.
func validateComposeLogsArgvSuffix(argv []string) error {
	if _, err := validateComposeProject(ComposeProject{
		ComposeFile: argv[2],
		EnvFile:     argv[4],
		ProjectName: argv[6],
	}); err != nil {
		return err
	}

	rest := argv[composeLogsFixedArgvLen:]
	if len(rest) >= 2 && rest[0] == "--tail" {
		if err := validateComposeLogsTailToken(rest[1]); err != nil {
			return err
		}
		rest = rest[2:]
	}
	if len(rest) >= 1 && rest[0] == "--follow" {
		rest = rest[1:]
	}
	for _, service := range rest {
		if _, err := validateComposeServiceName(service); err != nil {
			return err
		}
	}
	return nil
}

func validateComposeLogsTailToken(token string) error {
	tail, err := strconv.Atoi(token)
	if err != nil || tail < 1 || token != strconv.Itoa(tail) {
		return usageValidationError(
			"invalid docker command",
			fmt.Errorf("compose logs tail token %q is not a canonical positive integer", token),
		)
	}
	return nil
}

// isComposeLogsArgv reports whether argv carries the fixed allowlisted
// compose-logs prefix; the variable-length suffix is validated
// separately by [validateComposeLogsArgvSuffix].
func isComposeLogsArgv(argv []string) bool {
	return len(argv) >= composeLogsFixedArgvLen &&
		argv[0] == "compose" &&
		argv[1] == "-f" &&
		argv[3] == "--env-file" &&
		argv[5] == "--project-name" &&
		argv[7] == "logs" &&
		argv[8] == "--no-color" &&
		argv[9] == "--timestamps"
}

// parseComposeLogLine splits one redacted raw line into its container
// identity, emission timestamp, and message text. Lines without the
// Compose container-name prefix report ok=false: they are Compose's own
// diagnostics, not container log content. A prefixed line whose timestamp
// token is missing or unparsable keeps its full text with a zero
// Timestamp rather than being dropped.
func parseComposeLogLine(stream, line string) (ComposeLogEntry, bool) {
	loc := composeLogPrefixPattern.FindStringSubmatchIndex(line)
	if loc == nil {
		return ComposeLogEntry{}, false
	}

	entry := ComposeLogEntry{
		ContainerName: line[loc[2]:loc[3]],
		Stream:        stream,
		Text:          line[loc[1]:],
	}
	if ts, remainder, ok := cutComposeLogTimestamp(entry.Text); ok {
		entry.Timestamp = ts
		entry.Text = remainder
	}
	return entry, true
}

func cutComposeLogTimestamp(text string) (time.Time, string, bool) {
	token, remainder, _ := strings.Cut(text, " ")
	ts, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		return time.Time{}, "", false
	}
	return ts, remainder, true
}
