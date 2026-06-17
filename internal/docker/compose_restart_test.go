package docker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComposeRestart_RejectsNilClient(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	err := ComposeRestart(t.Context(), nil, project)
	requireUsageValidationError(t, err)
}

func TestComposeRestart_RejectsInvalidProjectBeforeRunningClient(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	outsideDir := t.TempDir()
	validProject := ComposeProject{
		ComposeFile: filepath.Join(stackDir, "docker-compose.yml"),
		EnvFile:     filepath.Join(stackDir, ".env"),
		ProjectName: "wdm-freshrss",
	}

	projectCases := []struct {
		name    string
		project ComposeProject
	}{
		{
			name: "blank compose file",
			project: ComposeProject{
				ComposeFile: "  ",
				EnvFile:     validProject.EnvFile,
				ProjectName: validProject.ProjectName,
			},
		},
		{
			name: "relative compose file",
			project: ComposeProject{
				ComposeFile: "docker-compose.yml",
				EnvFile:     validProject.EnvFile,
				ProjectName: validProject.ProjectName,
			},
		},
		{
			name: "blank env file",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     " ",
				ProjectName: validProject.ProjectName,
			},
		},
		{
			name: "relative env file",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     ".env",
				ProjectName: validProject.ProjectName,
			},
		},
		{
			name: "env file outside compose directory",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     filepath.Join(outsideDir, ".env"),
				ProjectName: validProject.ProjectName,
			},
		},
		{
			name: "blank project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: " ",
			},
		},
		{
			name: "leading dash project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "-wdm-app",
			},
		},
		{
			name: "uppercase project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "Wdm-App",
			},
		},
		{
			name: "space in project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "wdm app",
			},
		},
		{
			name: "leading whitespace in project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: " wdm-app",
			},
		},
		{
			name: "trailing whitespace in project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "wdm-app ",
			},
		},
	}

	for _, pc := range projectCases {
		t.Run(pc.name, func(t *testing.T) {
			t.Parallel()

			fake := &composeDeploymentFakeClient{}
			err := ComposeRestart(t.Context(), fake, pc.project)
			requireUsageValidationError(t, err)
			require.Zero(t, fake.runCalls)
		})
	}
}

func TestComposeRestart_SendsPrivateInvocationAndReturnsRunError(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	wantErr := errors.New("run failed")

	fake := &composeDeploymentFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			restartInv, ok := inv.(composeRestartInvocation)
			require.True(t, ok)
			require.Equal(t, project.ComposeFile, restartInv.composeFile)
			require.Equal(t, project.EnvFile, restartInv.envFile)
			require.Equal(t, project.ProjectName, restartInv.projectName)
			return CommandResult{Stdout: "ignored"}, wantErr
		},
	}

	err := ComposeRestart(t.Context(), fake, project)
	require.Same(t, wantErr, err)
	require.Equal(t, 1, fake.runCalls)
}

func TestRun_ComposeRestartInvocationBuildsExactArgv(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	wantArg := []string{
		"compose",
		"-f",
		project.ComposeFile,
		"--env-file",
		project.EnvFile,
		"--project-name",
		project.ProjectName,
		"restart",
	}

	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(t, wantArg, cmd.argv)
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(t.Context(), composeRestartInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	require.NoError(t, err)
	require.True(t, invoked)
}

func TestRun_DefaultExecutorComposeRestartInvocationUsesExpectedArgv(t *testing.T) {
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

	restartRes, err := client.Run(t.Context(), composeRestartInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	require.NoError(t, err)
	require.Contains(
		t,
		restartRes.Stdout,
		"argv=[compose][-f]["+project.ComposeFile+"][--env-file]["+project.EnvFile+"][--project-name]["+project.ProjectName+"][restart]",
	)
	// Whole-stack only: no service token is ever appended to the argv.
	require.NotContains(t, restartRes.Stdout, "[--force-recreate]")
	require.NotContains(t, restartRes.Stdout, "[-t]")
}

func TestValidateCommandSpec_AllowsComposeRestartShape(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"compose",
			"-f",
			project.ComposeFile,
			"--env-file",
			project.EnvFile,
			"--project-name",
			project.ProjectName,
			"restart",
		},
	}))
}

func TestValidateCommandSpec_RejectsUnsafeComposeRestartShapes(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	outsideDir := t.TempDir()
	composeFile := filepath.Join(stackDir, "docker-compose.yml")
	envFile := filepath.Join(stackDir, ".env")
	outsideEnvFile := filepath.Join(outsideDir, ".env")

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "bare compose restart is not allowlisted",
			argv: []string{"compose", "restart"},
		},
		{
			name: "trailing service name forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"restart",
				"web",
			},
		},
		{
			name: "timeout flag forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"restart",
				"-t",
				"0",
			},
		},
		{
			name: "force recreate on restart forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"restart",
				"--force-recreate",
			},
		},
		{
			name: "no-deps flag forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"restart",
				"--no-deps",
			},
		},
		{
			name: "invalid project name",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"WDM-App",
				"restart",
			},
		},
		{
			name: "leading whitespace project name",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				" wdm-app",
				"restart",
			},
		},
		{
			name: "trailing whitespace project name",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app ",
				"restart",
			},
		},
		{
			name: "env file outside compose directory",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				outsideEnvFile,
				"--project-name",
				"wdm-app",
				"restart",
			},
		},
		{
			name: "metacharacter in project name position",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app; ls",
				"restart",
			},
		},
		{
			name: "shell wrapper around restart",
			argv: []string{"sh", "-c", "docker compose restart"},
		},
		{
			name: "compose v1 restart with chained command",
			argv: []string{"compose", "restart", "; rm -rf /"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCommandSpec(commandSpec{argv: tt.argv})
			requireUsageValidationError(t, err)
		})
	}
}
