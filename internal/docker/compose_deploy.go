package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

var composeProjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

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

type composePullInvocation struct {
	composeFile string
	envFile     string
	projectName string
}

func (composePullInvocation) isDockerInvocation() {}

type composeUpInvocation struct {
	composeFile   string
	envFile       string
	projectName   string
	forceRecreate bool
}

func (composeUpInvocation) isDockerInvocation() {}

type composeRestartInvocation struct {
	composeFile string
	envFile     string
	projectName string
}

func (composeRestartInvocation) isDockerInvocation() {}

type composeDownInvocation struct {
	composeFile string
	envFile     string
	projectName string
}

func (composeDownInvocation) isDockerInvocation() {}

func newComposePullInvocation(project ComposeProject) (composePullInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composePullInvocation{}, err
	}

	return composePullInvocation{
		composeFile: normalized.ComposeFile,
		envFile:     normalized.EnvFile,
		projectName: normalized.ProjectName,
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

	return composeUpInvocation{
		composeFile:   normalized.ComposeFile,
		envFile:       normalized.EnvFile,
		projectName:   normalized.ProjectName,
		forceRecreate: opts.ForceRecreate,
	}, nil
}

func newComposeRestartInvocation(project ComposeProject) (composeRestartInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeRestartInvocation{}, err
	}

	return composeRestartInvocation{
		composeFile: normalized.ComposeFile,
		envFile:     normalized.EnvFile,
		projectName: normalized.ProjectName,
	}, nil
}

func newComposeDownInvocation(project ComposeProject) (composeDownInvocation, error) {
	normalized, err := validateComposeProject(project)
	if err != nil {
		return composeDownInvocation{}, err
	}

	return composeDownInvocation{
		composeFile: normalized.ComposeFile,
		envFile:     normalized.EnvFile,
		projectName: normalized.ProjectName,
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
