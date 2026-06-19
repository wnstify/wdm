package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComposeStop_RejectsNilClient(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	err := ComposeStop(t.Context(), nil, project)
	requireUsageValidationError(t, err)
}

func TestComposeStop_RejectsInvalidProjectBeforeRunningClient(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)

	fake := &composeDeploymentFakeClient{}
	err := ComposeStop(t.Context(), fake, ComposeProject{
		ComposeFile: "docker-compose.yml", // relative path is rejected
		EnvFile:     project.EnvFile,
		ProjectName: project.ProjectName,
	})
	requireUsageValidationError(t, err)
	require.Zero(t, fake.runCalls)
}

func TestComposeStop_SendsPrivateInvocationAndReturnsRunError(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	wantErr := errors.New("run failed")

	fake := &composeDeploymentFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			stopInv, ok := inv.(composeStopInvocation)
			require.True(t, ok)
			require.Equal(t, project.ComposeFile, stopInv.composeFile)
			require.Equal(t, project.EnvFile, stopInv.envFile)
			require.Equal(t, project.ProjectName, stopInv.projectName)
			return CommandResult{Stdout: "ignored"}, wantErr
		},
	}

	err := ComposeStop(t.Context(), fake, project)
	require.Same(t, wantErr, err)
	require.Equal(t, 1, fake.runCalls)
}

func TestRun_ComposeStopInvocationBuildsExactArgv(t *testing.T) {
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
		"stop",
	}

	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(t, wantArg, cmd.argv)
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(t.Context(), composeStopInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	require.NoError(t, err)
	require.True(t, invoked)
}

func TestValidateCommandSpec_AllowsComposeStopShape(t *testing.T) {
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
			"stop",
		},
	}))
}

func TestValidateCommandSpec_RejectsUnsafeComposeStopShapes(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "bare compose stop is not allowlisted",
			argv: []string{"compose", "stop"},
		},
		{
			name: "trailing service name forbidden",
			argv: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"stop",
				"web",
			},
		},
		{
			name: "timeout flag forbidden",
			argv: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"stop",
				"-t",
				"0",
			},
		},
		{
			name: "metacharacter in project name position",
			argv: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				"wdm-app; rm -rf /",
				"stop",
			},
		},
		{
			name: "shell wrapper around stop",
			argv: []string{"sh", "-c", "docker compose stop"},
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
