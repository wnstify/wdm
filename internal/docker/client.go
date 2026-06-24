package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// ErrNilRedactor is returned when [WithRedactor] receives nil.
var ErrNilRedactor = errors.New("docker: WithRedactor requires non-nil redactor; pass security.NoopRedactor for no-op redaction")

// ErrNilCommandExecutor is returned when [WithCommandExecutor]
// receives nil.
var ErrNilCommandExecutor = errors.New("docker: WithCommandExecutor requires non-nil executor")

// Client is the mockable Docker command surface consumed by higher
// layers. It exposes only a closed typed invocation seam.
type Client interface {
	// Run executes one typed invocation. Raw argv is deliberately not
	// part of this API: execClient builds argv privately from invocation
	// types, so callers cannot inject command tokens.
	Run(ctx context.Context, inv Invocation) (CommandResult, error)
}

// Invocation is the closed typed input set accepted by [Client.Run]. The
// unexported method keeps the set closed to this package.
type Invocation interface {
	isDockerInvocation()
}

// VersionInvocation maps to `docker version`.
type VersionInvocation struct{}

func (VersionInvocation) isDockerInvocation() {}

// ComposeVersionInvocation maps to `docker compose version`.
type ComposeVersionInvocation struct{}

func (ComposeVersionInvocation) isDockerInvocation() {}

// CommandResult carries normalized process output.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CommandExecutor is the process-execution seam injected into
// [execClient]. Its command shape is package-private, keeping argv
// construction inside execClient.
type CommandExecutor func(ctx context.Context, cmd commandSpec) (CommandResult, error)

// Option mutates constructor config for [New].
type Option func(*config) error

type config struct {
	redactor security.Redactor
	execFn   CommandExecutor
	streamFn StreamExecutor
}

// WithRedactor overrides the redactor used to scrub stderr content.
// Nil is rejected to fail closed at construction time.
func WithRedactor(r security.Redactor) Option {
	return func(c *config) error {
		if r == nil {
			return ErrNilRedactor
		}
		c.redactor = r
		return nil
	}
}

// WithCommandExecutor overrides the command execution dependency. Nil
// is rejected to fail closed at construction time.
func WithCommandExecutor(fn CommandExecutor) Option {
	return func(c *config) error {
		if fn == nil {
			return ErrNilCommandExecutor
		}
		c.execFn = fn
		return nil
	}
}

// WithStreamExecutor overrides the streaming command execution
// dependency used by [execClient.StreamLogs]. Nil is rejected to fail
// closed at construction time.
func WithStreamExecutor(fn StreamExecutor) Option {
	return func(c *config) error {
		if fn == nil {
			return ErrNilStreamExecutor
		}
		c.streamFn = fn
		return nil
	}
}

// execClient is the production [Client] implementation. It also
// implements the optional [LogStreamer] capability for line-streaming
// invocations.
type execClient struct {
	redactor security.Redactor
	execFn   CommandExecutor
	streamFn StreamExecutor
}

type commandSpec struct {
	argv []string
}

const dockerCancelWaitDelay = 30 * time.Second

// Compile-time check that execClient satisfies [Client].
var _ Client = (*execClient)(nil)

// New constructs a production [Client] with safe defaults: active
// redaction and argv-only command execution via exec.CommandContext.
func New(opts ...Option) (Client, error) {
	cfg := &config{
		redactor: security.NewActiveRedactor(nil),
		execFn:   runCommandExecutor,
		streamFn: runStreamExecutor,
	}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("docker.New: %w", err)
		}
	}

	return &execClient{
		redactor: cfg.redactor,
		execFn:   cfg.execFn,
		streamFn: cfg.streamFn,
	}, nil
}

// Run executes one typed invocation through the injected command
// executor. Streaming invocations are rejected: buffering a potentially
// unbounded stream (compose logs --follow never exits) into a
// [CommandResult] would break the bounded-memory contract, so they must
// go through [execClient.StreamLogs] instead.
func (c *execClient) Run(ctx context.Context, inv Invocation) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, types.WrapError(
			types.ErrCodeUserCanceled,
			"docker command canceled",
			"",
			err,
		)
	}
	if _, ok := inv.(composeLogsInvocation); ok {
		return CommandResult{}, usageValidationError(
			"invalid docker invocation",
			errors.New("compose logs is a streaming invocation; use ComposeLogs"),
		)
	}

	cmd, err := buildCommand(inv)
	if err != nil {
		return CommandResult{}, err
	}
	if err := validateCommandSpec(cmd); err != nil {
		return CommandResult{}, err
	}

	res, err := c.execFn(ctx, cmd)
	redacted := CommandResult{
		Stdout:   c.redactor.Redact(res.Stdout),
		Stderr:   c.redactor.Redact(res.Stderr),
		ExitCode: res.ExitCode,
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return redacted, types.WrapError(
				types.ErrCodeUserCanceled,
				"docker command canceled",
				"",
				decorateCauseWithStderr(errors.Join(err, ctxErr), redacted.Stderr),
			)
		}
		return redacted, mapCommandError(err, redacted.Stderr)
	}

	return redacted, nil
}

// buildComposeInvocationCommand handles the `docker compose ...` deployment
// and logs invocations, keeping the compose family out of buildCommand's main
// switch so its cyclomatic complexity stays bounded as verbs are added. It
// returns handled=false for any non-compose invocation so buildCommand falls
// through to the remaining cases.
func buildComposeInvocationCommand(inv Invocation) (cmd commandSpec, handled bool, err error) {
	switch typedInv := inv.(type) {
	case composeConfigInvocation:
		cmd, err = buildComposeConfigCommand(typedInv)
	case composePullInvocation:
		cmd, err = buildComposeProjectCommand(typedInv.composeFile, typedInv.envFile, typedInv.projectName, typedInv.overridePath, "pull")
	case composeUpInvocation:
		cmd, err = buildComposeUpCommand(typedInv)
	case composeRestartInvocation:
		cmd, err = buildComposeProjectCommand(typedInv.composeFile, typedInv.envFile, typedInv.projectName, typedInv.overridePath, "restart")
	case composeStopInvocation:
		cmd, err = buildComposeProjectCommand(typedInv.composeFile, typedInv.envFile, typedInv.projectName, typedInv.overridePath, "stop")
	case composeDownInvocation:
		cmd, err = buildComposeProjectCommand(typedInv.composeFile, typedInv.envFile, typedInv.projectName, typedInv.overridePath, "down")
	case composeDownRemoveImagesInvocation:
		cmd, err = buildComposeDownRemoveImagesCommand(typedInv)
	case composeLogsInvocation:
		cmd, err = buildComposeLogsCommand(typedInv)
	default:
		return commandSpec{}, false, nil
	}
	return cmd, true, err
}

func buildCommand(inv Invocation) (commandSpec, error) {
	if cmd, handled, err := buildComposeInvocationCommand(inv); handled {
		return cmd, err
	}

	switch typedInv := inv.(type) {
	case VersionInvocation:
		return commandSpec{argv: []string{"version"}}, nil
	case ComposeVersionInvocation:
		return commandSpec{argv: []string{"compose", "version"}}, nil
	case networkInspectInvocation:
		return buildNetworkInspectCommand(typedInv.name)
	case networkManagedLabelInvocation:
		return buildNetworkManagedLabelCommand(typedInv.name)
	case networkSubnetInvocation:
		return buildNetworkSubnetCommand(typedInv.name)
	case networkCreateInvocation:
		return buildNetworkCreateCommand(typedInv)
	case removeNetworkInvocation:
		return buildRemoveNetworkCommand(typedInv.name)
	case managedNetworkListInvocation:
		return buildManagedNetworkListCommand()
	case removeNamedVolumeInvocation:
		return buildRemoveNamedVolumeCommand(typedInv.name)
	case projectContainerListInvocation:
		return buildProjectContainerListCommand(typedInv.projectName)
	case containerInspectInvocation:
		return buildContainerInspectCommand(typedInv.id)
	case imageDigestInspectInvocation:
		return buildImageDigestInspectCommand(typedInv.imageRef)
	case projectVolumeListInvocation:
		return buildProjectVolumeListCommand(typedInv.projectName)
	case bindMountCleanupInvocation:
		return buildBindMountCleanupCommand(typedInv.targetPath)
	default:
		return commandSpec{}, types.WrapError(
			types.ErrCodeUsageValidation,
			"unsupported docker invocation",
			"",
			fmt.Errorf("unsupported invocation type %T", inv),
		)
	}
}

func buildComposeConfigCommand(inv composeConfigInvocation) (commandSpec, error) {
	projectDir, composeFile, err := validateComposeConfigPaths(
		inv.projectDir,
		inv.composeFile,
	)
	if err != nil {
		return commandSpec{}, err
	}

	argv := []string{
		"compose",
		"--project-directory",
		projectDir,
		"-f",
		composeFile,
	}
	argv = append(argv, overrideFileArgs(inv.overridePath)...)
	argv = append(argv, "config", "--quiet")

	return commandSpec{argv: argv}, nil
}

func buildComposeProjectCommand(
	composeFile, envFile, projectName, overridePath, command string,
) (commandSpec, error) {
	project, err := validateComposeProject(ComposeProject{
		ComposeFile: composeFile,
		EnvFile:     envFile,
		ProjectName: projectName,
	})
	if err != nil {
		return commandSpec{}, err
	}
	argv := []string{
		"compose",
		"-f",
		project.ComposeFile,
		"--env-file",
		project.EnvFile,
		"--project-name",
		project.ProjectName,
	}
	argv = append(argv, overrideFileArgs(overridePath)...)
	argv = append(argv, command)
	return commandSpec{argv: argv}, nil
}

// overrideFileArgs returns the `-f <override>` pair for a non-empty content-gated
// override path, or nil. Appending the override after the base `-f` keeps native
// last-write-wins merge order while leaving the base argv prefix byte-identical
// when no override is present.
func overrideFileArgs(overridePath string) []string {
	if overridePath == "" {
		return nil
	}
	return []string{"-f", overridePath}
}

func buildComposeUpCommand(inv composeUpInvocation) (commandSpec, error) {
	cmd, err := buildComposeProjectCommand(
		inv.composeFile,
		inv.envFile,
		inv.projectName,
		inv.overridePath,
		"up",
	)
	if err != nil {
		return commandSpec{}, err
	}
	cmd.argv = append(cmd.argv, "-d")
	if inv.forceRecreate {
		cmd.argv = append(cmd.argv, "--force-recreate")
	}
	return cmd, nil
}

// buildComposeDownRemoveImagesCommand builds `docker compose ... down --rmi
// all` for the self-uninstall teardown (PRD §39). It reuses the validated
// down base and appends only `--rmi all` — never `-v` — so the forbidden
// volume-removal flag can never reach the argv. The allowlist re-checks the
// exact shape before exec.
func buildComposeDownRemoveImagesCommand(inv composeDownRemoveImagesInvocation) (commandSpec, error) {
	cmd, err := buildComposeProjectCommand(
		inv.composeFile,
		inv.envFile,
		inv.projectName,
		inv.overridePath,
		"down",
	)
	if err != nil {
		return commandSpec{}, err
	}
	cmd.argv = append(cmd.argv, "--rmi", "all")
	return cmd, nil
}

func buildComposeLogsCommand(inv composeLogsInvocation) (commandSpec, error) {
	cmd, err := buildComposeProjectCommand(
		inv.composeFile,
		inv.envFile,
		inv.projectName,
		inv.overridePath,
		"logs",
	)
	if err != nil {
		return commandSpec{}, err
	}
	opts, err := validateComposeLogsOptions(ComposeLogsOptions{
		Follow:   inv.follow,
		Tail:     inv.tail,
		Services: inv.services,
	})
	if err != nil {
		return commandSpec{}, err
	}

	cmd.argv = append(cmd.argv, "--no-color", "--timestamps")
	if opts.Tail > 0 {
		cmd.argv = append(cmd.argv, "--tail", strconv.Itoa(opts.Tail))
	}
	if opts.Follow {
		cmd.argv = append(cmd.argv, "--follow")
	}
	cmd.argv = append(cmd.argv, opts.Services...)
	return cmd, nil
}

func buildNetworkInspectCommand(name string) (commandSpec, error) {
	networkName, err := validateNetworkName(name)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"network",
			"inspect",
			"--format",
			"{{.Internal}}",
			networkName,
		},
	}, nil
}

// networkManagedLabelFormat prints a network's `wdm.managed` label value, or
// empty when the label is absent, so the ownership gate can compare it to
// "true" before a compose-derived removal (PRD §10).
const networkManagedLabelFormat = `{{index .Labels "wdm.managed"}}`

func buildNetworkManagedLabelCommand(name string) (commandSpec, error) {
	networkName, err := validateNetworkName(name)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"network",
			"inspect",
			"--format",
			networkManagedLabelFormat,
			networkName,
		},
	}, nil
}

// networkSubnetInspectFormat reads the first configured subnet from a network's
// IPAM config. Networks wdm creates pin a single subnet, so the first entry is
// the one [EnsureNetworkReport] reconciles against the requested spec.
const networkSubnetInspectFormat = `{{range .IPAM.Config}}{{.Subnet}}{{end}}`

func buildNetworkSubnetCommand(name string) (commandSpec, error) {
	networkName, err := validateNetworkName(name)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"network",
			"inspect",
			"--format",
			networkSubnetInspectFormat,
			networkName,
		},
	}, nil
}

func buildNetworkCreateCommand(inv networkCreateInvocation) (commandSpec, error) {
	networkName, err := validateNetworkName(inv.name)
	if err != nil {
		return commandSpec{}, err
	}

	argv := []string{"network", "create"}
	if inv.internal {
		argv = append(argv, "--internal")
	}
	if inv.subnet != "" {
		argv = append(argv, "--subnet", inv.subnet)
	}
	if inv.gateway != "" {
		argv = append(argv, "--gateway", inv.gateway)
	}
	// PRD §10 ownership labels, in fixed order so the argv is deterministic and
	// the last-gate validator can match the pair positionally. Stamped only
	// when the spec carries an app ID; the validator re-checks the charset.
	if inv.appID != "" {
		argv = append(
			argv,
			"--label", managedNetworkLabel,
			"--label", appNetworkLabelPrefix+inv.appID,
		)
	}
	argv = append(argv, networkName)
	return commandSpec{argv: argv}, nil
}

// buildManagedNetworkListCommand builds the fixed label-filtered managed-network
// list argv: `network ls --filter label=wdm.managed=true --format {{.Name}}`.
// Every token is a constant, so no caller input reaches the argv; the matching
// validator arm accepts only this exact shape (PRD §39).
func buildManagedNetworkListCommand() (commandSpec, error) {
	return commandSpec{
		argv: []string{
			"network",
			"ls",
			"--filter",
			managedNetworkLabelFilter,
			"--format",
			managedNetworkListFormat,
		},
	}, nil
}

func buildRemoveNetworkCommand(name string) (commandSpec, error) {
	networkName, err := validateNetworkName(name)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{"network", "rm", networkName},
	}, nil
}

func buildProjectContainerListCommand(projectName string) (commandSpec, error) {
	projectName, err := validateComposeProjectName(projectName)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"container",
			"ls",
			"--all",
			"--filter",
			composeProjectLabelFilterPrefix + projectName,
			"--format",
			containerListFormat,
		},
	}, nil
}

func buildContainerInspectCommand(containerID string) (commandSpec, error) {
	id, err := validateContainerID(containerID)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"container",
			"inspect",
			"--format",
			containerInspectFormat,
			id,
		},
	}, nil
}

func buildImageDigestInspectCommand(ref string) (commandSpec, error) {
	imageRef, err := validateImageRef(ref)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"image",
			"inspect",
			"--format",
			imageDigestInspectFormat,
			imageRef,
		},
	}, nil
}

func buildProjectVolumeListCommand(projectName string) (commandSpec, error) {
	projectName, err := validateComposeProjectName(projectName)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"volume",
			"ls",
			"--filter",
			composeProjectLabelFilterPrefix + projectName,
			"--format",
			volumeListFormat,
		},
	}, nil
}

func buildRemoveNamedVolumeCommand(name string) (commandSpec, error) {
	volumeName, err := validateVolumeName(name)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{"volume", "rm", volumeName},
	}, nil
}

func buildBindMountCleanupCommand(path string) (commandSpec, error) {
	targetPath, err := validateBindCleanupPath(path)
	if err != nil {
		return commandSpec{}, err
	}

	return commandSpec{
		argv: []string{
			"run",
			"--rm",
			"--pull=never",
			"--network",
			"none",
			"--mount",
			bindCleanupMountArg(targetPath),
			bindCleanupImage,
			"find",
			bindCleanupTarget,
			"-xdev",
			"-depth",
			"-mindepth",
			"1",
			"-delete",
		},
	}, nil
}

func validateCommandSpec(cmd commandSpec) error {
	if len(cmd.argv) == 0 {
		return usageValidationError("invalid docker command", errors.New("empty docker argv"))
	}
	for i, token := range cmd.argv {
		if strings.TrimSpace(token) == "" {
			return usageValidationError(
				"invalid docker command",
				fmt.Errorf("docker argv[%d] is empty", i),
			)
		}
	}

	switch cmd.argv[0] {
	case "version":
		return validateDockerVersionArgv(cmd.argv)
	case "compose":
		return validateComposeArgv(cmd.argv)
	case "network":
		return validateNetworkArgv(cmd.argv)
	case "container":
		return validateContainerArgv(cmd.argv)
	case "image":
		return validateImageArgv(cmd.argv)
	case "volume":
		return validateVolumeArgv(cmd.argv)
	case "run":
		return validateRunArgv(cmd.argv)
	default:
		return unsupportedDockerCommand(cmd)
	}
}

func validateDockerVersionArgv(argv []string) error {
	if len(argv) == 1 && argv[0] == "version" {
		return nil
	}
	return unsupportedDockerArgv(argv)
}

func validateComposeArgv(argv []string) error {
	argv, err := stripValidatedComposeOverride(argv)
	if err != nil {
		return err
	}

	switch {
	case len(argv) == 2 && argv[1] == "version":
		return nil
	case isComposeConfigArgv(argv):
		_, _, err := validateComposeConfigPaths(argv[2], argv[4])
		return err
	case isComposeProjectArgv(argv):
		return validateComposeProjectArgv(argv)
	case isComposeLogsArgv(argv):
		return validateComposeLogsArgvSuffix(argv)
	default:
		return unsupportedDockerArgv(argv)
	}
}

// stripValidatedComposeOverride detects an optional content-gated override
// (`-f <override>`) inserted right after the base `-f <compose>` pair, validates
// the override path, and returns argv with the pair removed so the fixed-position
// allowlist checks see the same shape they do without an override. The base
// compose `-f` always precedes the override `-f`, preserving last-write-wins
// merge order. Returns argv unchanged when no override pair is present.
func stripValidatedComposeOverride(argv []string) ([]string, error) {
	// Project shape: compose -f <c> --env-file <e> --project-name <p>
	// [-f <override>] <cmd>... ; override pair at indices 7,8.
	if len(argv) >= 10 &&
		argv[0] == "compose" &&
		argv[1] == "-f" &&
		argv[3] == "--env-file" &&
		argv[5] == "--project-name" &&
		argv[7] == "-f" {
		if _, err := validateAbsolutePath(
			argv[8],
			"compose override file",
			"pass a non-empty absolute path for the compose override file",
		); err != nil {
			return nil, err
		}
		return slices.Concat(argv[:7], argv[9:]), nil
	}

	// Config shape: compose --project-directory <d> -f <c> [-f <override>]
	// config --quiet ; override pair at indices 5,6.
	if len(argv) >= 9 &&
		argv[0] == "compose" &&
		argv[1] == "--project-directory" &&
		argv[3] == "-f" &&
		argv[5] == "-f" {
		if _, err := validateAbsolutePath(
			argv[6],
			"compose override file",
			"pass a non-empty absolute path for the compose override file",
		); err != nil {
			return nil, err
		}
		return slices.Concat(argv[:5], argv[7:]), nil
	}

	return argv, nil
}

func isComposeConfigArgv(argv []string) bool {
	return len(argv) == 7 &&
		argv[1] == "--project-directory" &&
		argv[3] == "-f" &&
		argv[5] == "config" &&
		argv[6] == "--quiet"
}

func isComposeProjectArgv(argv []string) bool {
	if len(argv) < 8 ||
		argv[1] != "-f" ||
		argv[3] != "--env-file" ||
		argv[5] != "--project-name" {
		return false
	}

	switch argv[7] {
	case "pull", "restart", "stop":
		return len(argv) == 8
	case "down":
		// Plain safe down (no -v), or the self-uninstall down --rmi all
		// teardown (PRD §39). -v is never allowlisted in either shape.
		return len(argv) == 8 ||
			len(argv) == 10 && argv[8] == "--rmi" && argv[9] == "all"
	case "up":
		return len(argv) == 9 && argv[8] == "-d" ||
			len(argv) == 10 && argv[8] == "-d" && argv[9] == "--force-recreate"
	default:
		return false
	}
}

func validateComposeProjectArgv(argv []string) error {
	_, err := validateComposeProject(ComposeProject{
		ComposeFile: argv[2],
		EnvFile:     argv[4],
		ProjectName: argv[6],
	})
	return err
}

func validateNetworkArgv(argv []string) error {
	switch {
	case len(argv) == 5 &&
		argv[1] == "inspect" &&
		argv[2] == "--format" &&
		(argv[3] == "{{.Internal}}" ||
			argv[3] == networkSubnetInspectFormat ||
			argv[3] == networkManagedLabelFormat):
		_, err := validateNetworkName(argv[4])
		return err
	case len(argv) >= 3 && argv[1] == "create":
		return validateNetworkCreateArgv(argv)
	case len(argv) == 3 && argv[1] == "rm":
		_, err := validateNetworkName(argv[2])
		return err
	case len(argv) == 6 &&
		argv[1] == "ls" &&
		argv[2] == "--filter" &&
		argv[3] == managedNetworkLabelFilter &&
		argv[4] == "--format" &&
		argv[5] == managedNetworkListFormat:
		// The managed-network sweep list (PRD §39): the ONLY allowlisted
		// `network ls` shape. Every token is a fixed literal, so any other
		// `network ls` invocation still falls through to the default refusal.
		return nil
	default:
		return unsupportedDockerArgv(argv)
	}
}

// validateNetworkCreateArgv enforces the fixed `network create` argv grammar:
// an optional `--internal` flag, an optional `--subnet <cidr>` pair, an optional
// `--gateway <ip>` pair, then the optional ordered ownership-label pair
// (`--label wdm.managed=true --label wdm.app=<appID>`, PRD §10), in that order,
// and the network name as the trailing token. A `--gateway` clause is rejected
// unless a `--subnet` clause preceded it, matching Docker's own constraint. Each
// flag value is re-validated here — the allow-list is the last gate before exec,
// so it never trusts the builder (PRD §12).
func validateNetworkCreateArgv(argv []string) error {
	rest := argv[2:]
	if len(rest) > 0 && rest[0] == "--internal" {
		rest = rest[1:]
	}
	sawSubnet := false
	if len(rest) >= 2 && rest[0] == "--subnet" {
		if err := validateNetworkSubnetArg(rest[1]); err != nil {
			return err
		}
		sawSubnet = true
		rest = rest[2:]
	}
	if len(rest) >= 2 && rest[0] == "--gateway" {
		if !sawSubnet {
			return unsupportedDockerArgv(argv)
		}
		if err := validateNetworkGatewayArg(rest[1]); err != nil {
			return err
		}
		rest = rest[2:]
	}
	var err error
	rest, err = consumeNetworkLabelArgs(argv, rest)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return unsupportedDockerArgv(argv)
	}
	_, err = validateNetworkName(rest[0])
	return err
}

// consumeNetworkLabelArgs validates the optional ownership-label pair stamped by
// the create builder and returns the remaining tokens. The pair is all-or-
// nothing and positionally fixed: `--label wdm.managed=true` immediately
// followed by `--label wdm.app=<appID>`, where <appID> matches the catalog
// app_id charset. A lone `--label`, a wrong key, a wrong managed value, or an
// out-of-charset app id is rejected — this is the last gate before exec, so a
// label value carrying shell metacharacters or extra tokens never reaches the
// daemon (PRD §10, §12).
func consumeNetworkLabelArgs(argv, rest []string) ([]string, error) {
	if len(rest) == 0 || rest[0] != "--label" {
		return rest, nil
	}
	if len(rest) < 4 || rest[2] != "--label" {
		return nil, unsupportedDockerArgv(argv)
	}
	if rest[1] != managedNetworkLabel {
		return nil, unsupportedDockerArgv(argv)
	}
	appID, ok := strings.CutPrefix(rest[3], appNetworkLabelPrefix)
	if !ok {
		return nil, unsupportedDockerArgv(argv)
	}
	if !networkAppIDPattern.MatchString(appID) {
		return nil, usageValidationError(
			"network app label is invalid",
			fmt.Errorf("network app label value %q does not match allowed app id format", appID),
		)
	}
	return rest[4:], nil
}

func validateNetworkSubnetArg(subnet string) error {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() {
		return usageValidationError(
			"network subnet is invalid",
			fmt.Errorf("subnet %q is not a valid IPv4 CIDR", subnet),
		)
	}
	return nil
}

func validateNetworkGatewayArg(gateway string) error {
	addr, err := netip.ParseAddr(gateway)
	if err != nil || !addr.Is4() {
		return usageValidationError(
			"network gateway is invalid",
			fmt.Errorf("gateway %q is not a valid IPv4 address", gateway),
		)
	}
	return nil
}

func validateContainerArgv(argv []string) error {
	switch {
	case len(argv) == 7 &&
		argv[1] == "ls" &&
		argv[2] == "--all" &&
		argv[3] == "--filter" &&
		argv[5] == "--format" &&
		argv[6] == containerListFormat:
		return validateComposeProjectLabelFilter(argv[4])
	case len(argv) == 5 &&
		argv[1] == "inspect" &&
		argv[2] == "--format" &&
		argv[3] == containerInspectFormat:
		_, err := validateContainerID(argv[4])
		return err
	default:
		return unsupportedDockerArgv(argv)
	}
}

func validateImageArgv(argv []string) error {
	if len(argv) == 5 &&
		argv[1] == "inspect" &&
		argv[2] == "--format" &&
		argv[3] == imageDigestInspectFormat {
		_, err := validateImageRef(argv[4])
		return err
	}
	return unsupportedDockerArgv(argv)
}

func validateVolumeArgv(argv []string) error {
	switch {
	case len(argv) == 6 &&
		argv[1] == "ls" &&
		argv[2] == "--filter" &&
		argv[4] == "--format" &&
		argv[5] == volumeListFormat:
		return validateComposeProjectLabelFilter(argv[3])
	case len(argv) == 3 && argv[1] == "rm":
		_, err := validateVolumeName(argv[2])
		return err
	default:
		return unsupportedDockerArgv(argv)
	}
}

func validateRunArgv(argv []string) error {
	if len(argv) == 15 &&
		argv[1] == "--rm" &&
		argv[2] == "--pull=never" &&
		argv[3] == "--network" &&
		argv[4] == "none" &&
		argv[5] == "--mount" &&
		argv[7] == bindCleanupImage &&
		argv[8] == "find" &&
		argv[9] == bindCleanupTarget &&
		argv[10] == "-xdev" &&
		argv[11] == "-depth" &&
		argv[12] == "-mindepth" &&
		argv[13] == "1" &&
		argv[14] == "-delete" {
		return validateBindCleanupMountArg(argv[6])
	}
	return unsupportedDockerArgv(argv)
}

func unsupportedDockerCommand(cmd commandSpec) error {
	return unsupportedDockerArgv(cmd.argv)
}

func unsupportedDockerArgv(argv []string) error {
	return usageValidationError(
		"unsupported docker command",
		fmt.Errorf("docker argv not allowlisted: %q", argv),
	)
}

func usageValidationError(message string, cause error) error {
	return types.WrapError(
		types.ErrCodeUsageValidation,
		message,
		"",
		cause,
	)
}

func runCommandExecutor(ctx context.Context, cmd commandSpec) (CommandResult, error) {
	if err := validateCommandSpec(cmd); err != nil {
		return CommandResult{}, err
	}

	execCmd := buildDockerExecCmd(ctx, cmd.argv)
	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	runErr := execCmd.Run()
	result := CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: commandExitCode(execCmd, runErr),
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func buildDockerExecCmd(ctx context.Context, argv []string) *exec.Cmd {
	//nolint:gosec // argv is constrained by validateCommandSpec allowlist before this function is called.
	execCmd := exec.CommandContext(ctx, "docker", argv...)
	execCmd.WaitDelay = dockerCancelWaitDelay
	execCmd.Cancel = func() error {
		if execCmd.Process == nil {
			return nil
		}
		err := execCmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return execCmd
}

func commandExitCode(execCmd *exec.Cmd, runErr error) int {
	if runErr == nil {
		if execCmd.ProcessState != nil {
			return execCmd.ProcessState.ExitCode()
		}
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode()
	}
	if execCmd.ProcessState != nil {
		return execCmd.ProcessState.ExitCode()
	}
	return -1
}

func mapCommandError(cause error, redactedStderr string) error {
	if cause == nil {
		return nil
	}
	var typedErr *types.Error
	if errors.As(cause, &typedErr) {
		return cause
	}

	causeWithStderr := decorateCauseWithStderr(cause, redactedStderr)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return types.WrapError(
			types.ErrCodeUserCanceled,
			"docker command canceled",
			"",
			causeWithStderr,
		)
	}

	lowerStderr := strings.ToLower(redactedStderr)
	if isDockerUnavailable(cause, lowerStderr) {
		return types.WrapError(
			types.ErrCodeDockerUnavailable,
			"docker is unavailable",
			"",
			causeWithStderr,
		)
	}
	if isRuntimeLockHeld(lowerStderr) {
		return types.WrapError(
			types.ErrCodeRuntimeLockHeld,
			"docker runtime lock is held",
			"",
			causeWithStderr,
		)
	}
	if isNetworkFailure(lowerStderr) {
		return types.WrapError(
			types.ErrCodeNetworkFailure,
			"docker command failed due to network error",
			"",
			causeWithStderr,
		)
	}

	return types.WrapError(
		types.ErrCodeGeneric,
		"docker command failed",
		"",
		causeWithStderr,
	)
}

func decorateCauseWithStderr(cause error, redactedStderr string) error {
	if redactedStderr == "" {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("docker stderr: %s", redactedStderr))
}

func isDockerUnavailable(cause error, lowerStderr string) bool {
	if errors.Is(cause, exec.ErrNotFound) {
		return true
	}

	var execErr *exec.Error
	if errors.As(cause, &execErr) {
		return true
	}

	indicators := []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"error during connect",
		"permission denied while trying to connect to the docker daemon socket",
	}
	for _, indicator := range indicators {
		if strings.Contains(lowerStderr, indicator) {
			return true
		}
	}
	return false
}

func isNetworkFailure(lowerStderr string) bool {
	indicators := []string{
		"no such host",
		"temporary failure in name resolution",
		"network is unreachable",
		"tls handshake timeout",
		"i/o timeout",
		"connection reset by peer",
		"dial tcp",
	}
	for _, indicator := range indicators {
		if strings.Contains(lowerStderr, indicator) {
			return true
		}
	}
	return false
}

func isRuntimeLockHeld(lowerStderr string) bool {
	indicators := []string{
		"another operation is already in progress",
		"resource busy",
	}
	for _, indicator := range indicators {
		if strings.Contains(lowerStderr, indicator) {
			return true
		}
	}
	return false
}
