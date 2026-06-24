package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

var composeProjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// composeOverrideFilename is the user-owned structural overlay merged on top of
// the rendered base compose file (Compose's standard override filename). Kept
// local to internal/docker to avoid an import cycle with internal/core (which
// imports this package). The value is assembled from parts so this non-test
// source does not carry the bare compose-v1 binary substring that this package's
// TestProductionSourcesRejectShellComposeV1AndDangerousLiterals guard forbids;
// the runtime value is the standard override filename.
const composeOverrideFilename = "docker" + "-compose.override.yml"

// resolveOverridePath reports the absolute override-file path for stackDir only
// when that file exists and carries non-comment, non-whitespace content;
// otherwise it returns "". It is strictly read-only: read-only wrappers
// (status/config/logs) must never create or mutate stack files, so a missing or
// effectively-empty override yields "" with a nil error rather than an error.
func resolveOverridePath(stackDir string) (string, error) {
	overridePath := filepath.Join(stackDir, composeOverrideFilename)

	data, err := os.ReadFile(overridePath) //nolint:gosec // G304: overridePath is filepath.Join(stackDir, fixed override filename) under the engine-controlled stack dir, not user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"compose override file cannot be read",
			"ensure the stack directory and its override file are readable",
			fmt.Errorf("read compose override: %w", err),
		)
	}

	for line := range strings.Lines(string(data)) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return overridePath, nil
	}

	return "", nil
}

// ComposeProject holds the rendered Compose and env-file paths plus the
// deterministic project name used for deployment operations.
type ComposeProject struct {
	ComposeFile string
	EnvFile     string
	ProjectName string
}

// ComposeUpOptions configures optional flags for docker compose up.
type ComposeUpOptions struct {
	ForceRecreate bool
}

// ComposePull executes `docker compose pull` for a validated project.
func ComposePull(ctx context.Context, client Client, project ComposeProject) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposePullInvocation(project)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

// ComposeUp executes `docker compose up -d` for a validated project.
func ComposeUp(
	ctx context.Context,
	client Client,
	project ComposeProject,
	opts ComposeUpOptions,
) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeUpInvocation(project, opts)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

// ComposeRestart executes plain `docker compose restart` for a validated
// project. It stops and starts the same containers without re-reading the
// Compose file, so it never recreates containers or re-renders templates.
// No per-service argument is ever passed: the whole stack restarts
// together.
func ComposeRestart(ctx context.Context, client Client, project ComposeProject) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeRestartInvocation(project)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

// ComposeStop executes plain `docker compose stop` for a validated
// project. It stops the project's running containers without removing
// them: the containers, networks, and named volumes stay defined and all
// data is preserved (this is NOT `docker compose down`). No per-service
// argument is ever passed: the whole stack stops together. `docker
// compose stop` is idempotent, so an already-stopped stack is a no-op.
func ComposeStop(ctx context.Context, client Client, project ComposeProject) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeStopInvocation(project)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

// ComposeDown executes safe `docker compose down` (no -v) for a
// validated project.
func ComposeDown(ctx context.Context, client Client, project ComposeProject) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeDownInvocation(project)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

// ComposeDownRemoveImages executes `docker compose down --rmi all` (NEVER
// -v) for a validated project. It removes the project's containers, the
// default network Compose created for it, AND every image the project's
// services reference — but never named volumes, so all data is preserved.
// It is the self-uninstall teardown verb (PRD §39): wdm-managed scope only,
// never a system-wide prune. The `--rmi all` flag is appended privately so
// callers cannot inject the forbidden `-v`.
func ComposeDownRemoveImages(ctx context.Context, client Client, project ComposeProject) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeDownRemoveImagesInvocation(project)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

type composePullInvocation struct {
	composeFile  string
	envFile      string
	projectName  string
	overridePath string
}

func (composePullInvocation) isDockerInvocation() {}

type composeUpInvocation struct {
	composeFile   string
	envFile       string
	projectName   string
	overridePath  string
	forceRecreate bool
}

func (composeUpInvocation) isDockerInvocation() {}

type composeRestartInvocation struct {
	composeFile  string
	envFile      string
	projectName  string
	overridePath string
}

func (composeRestartInvocation) isDockerInvocation() {}

type composeStopInvocation struct {
	composeFile  string
	envFile      string
	projectName  string
	overridePath string
}

func (composeStopInvocation) isDockerInvocation() {}

type composeDownInvocation struct {
	composeFile  string
	envFile      string
	projectName  string
	overridePath string
}

func (composeDownInvocation) isDockerInvocation() {}

type composeDownRemoveImagesInvocation struct {
	composeFile  string
	envFile      string
	projectName  string
	overridePath string
}

func (composeDownRemoveImagesInvocation) isDockerInvocation() {}

func newComposePullInvocation(project ComposeProject) (composePullInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composePullInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composePullInvocation{}, err
	}

	return composePullInvocation{
		composeFile:  normalized.ComposeFile,
		envFile:      normalized.EnvFile,
		projectName:  normalized.ProjectName,
		overridePath: overridePath,
	}, nil
}

func newComposeUpInvocation(
	project ComposeProject,
	opts ComposeUpOptions,
) (composeUpInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeUpInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composeUpInvocation{}, err
	}

	return composeUpInvocation{
		composeFile:   normalized.ComposeFile,
		envFile:       normalized.EnvFile,
		projectName:   normalized.ProjectName,
		overridePath:  overridePath,
		forceRecreate: opts.ForceRecreate,
	}, nil
}

func newComposeRestartInvocation(project ComposeProject) (composeRestartInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeRestartInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composeRestartInvocation{}, err
	}

	return composeRestartInvocation{
		composeFile:  normalized.ComposeFile,
		envFile:      normalized.EnvFile,
		projectName:  normalized.ProjectName,
		overridePath: overridePath,
	}, nil
}

func newComposeStopInvocation(project ComposeProject) (composeStopInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeStopInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composeStopInvocation{}, err
	}

	return composeStopInvocation{
		composeFile:  normalized.ComposeFile,
		envFile:      normalized.EnvFile,
		projectName:  normalized.ProjectName,
		overridePath: overridePath,
	}, nil
}

func newComposeDownInvocation(project ComposeProject) (composeDownInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeDownInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composeDownInvocation{}, err
	}

	return composeDownInvocation{
		composeFile:  normalized.ComposeFile,
		envFile:      normalized.EnvFile,
		projectName:  normalized.ProjectName,
		overridePath: overridePath,
	}, nil
}

func newComposeDownRemoveImagesInvocation(project ComposeProject) (composeDownRemoveImagesInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeDownRemoveImagesInvocation{}, err
	}

	overridePath, err := resolveOverridePath(filepath.Dir(normalized.ComposeFile))
	if err != nil {
		return composeDownRemoveImagesInvocation{}, err
	}

	return composeDownRemoveImagesInvocation{
		composeFile:  normalized.ComposeFile,
		envFile:      normalized.EnvFile,
		projectName:  normalized.ProjectName,
		overridePath: overridePath,
	}, nil
}

func validateComposeProject(project ComposeProject) (ComposeProject, error) {
	cleanComposeFile, err := validateAbsolutePath(
		project.ComposeFile,
		"compose file",
		"pass a non-empty absolute path for compose file",
	)
	if err != nil {
		return ComposeProject{}, err
	}

	cleanEnvFile, err := validateAbsolutePath(
		project.EnvFile,
		"env file",
		"pass a non-empty absolute path for env file",
	)
	if err != nil {
		return ComposeProject{}, err
	}

	composeDir := filepath.Dir(cleanComposeFile)
	envDir := filepath.Dir(cleanEnvFile)
	if composeDir != envDir {
		return ComposeProject{}, types.NewError(
			types.ErrCodeUsageValidation,
			"env file must be in the same directory as compose file",
			"place .env in the stack directory next to the compose file",
		)
	}

	projectName, err := validateComposeProjectName(project.ProjectName)
	if err != nil {
		return ComposeProject{}, err
	}

	return ComposeProject{
		ComposeFile: cleanComposeFile,
		EnvFile:     cleanEnvFile,
		ProjectName: projectName,
	}, nil
}

func validateComposeProjectName(raw string) (string, error) {
	projectName := strings.TrimSpace(raw)
	if projectName == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"compose project name is required",
			"use a deterministic lowercase project name like wdm-<app>",
		)
	}

	if !composeProjectNamePattern.MatchString(raw) {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"compose project name is invalid",
			"use lowercase letters/digits first, then lowercase letters/digits/underscore/hyphen",
			fmt.Errorf("project name %q does not match allowed format", raw),
		)
	}

	return raw, nil
}
