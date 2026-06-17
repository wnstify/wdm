package docker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type composeDeploymentFakeClient struct {
	runFn    func(context.Context, Invocation) (CommandResult, error)
	runCalls int
}

func (f *composeDeploymentFakeClient) Run(ctx context.Context, inv Invocation) (CommandResult, error) {
	f.runCalls++
	if f.runFn != nil {
		return f.runFn(ctx, inv)
	}
	return CommandResult{}, nil
}

func TestComposeDeploymentWrappers_RejectNilClient(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "pull",
			call: func(ctx context.Context) error {
				return ComposePull(ctx, nil, project)
			},
		},
		{
			name: "up",
			call: func(ctx context.Context) error {
				return ComposeUp(ctx, nil, project, ComposeUpOptions{})
			},
		},
		{
			name: "down",
			call: func(ctx context.Context) error {
				return ComposeDown(ctx, nil, project)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call(t.Context())
			requireUsageValidationError(t, err)
		})
	}
}

func TestComposeDeploymentWrappers_RejectInvalidProjectBeforeRunningClient(t *testing.T) {
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
		{
			name: "slash in project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "wdm/app",
			},
		},
		{
			name: "backslash in project name",
			project: ComposeProject{
				ComposeFile: validProject.ComposeFile,
				EnvFile:     validProject.EnvFile,
				ProjectName: "wdm\\app",
			},
		},
	}

	wrapperCalls := []struct {
		name string
		call func(context.Context, Client, ComposeProject) error
	}{
		{
			name: "pull",
			call: func(ctx context.Context, client Client, project ComposeProject) error {
				return ComposePull(ctx, client, project)
			},
		},
		{
			name: "up",
			call: func(ctx context.Context, client Client, project ComposeProject) error {
				return ComposeUp(ctx, client, project, ComposeUpOptions{})
			},
		},
		{
			name: "down",
			call: func(ctx context.Context, client Client, project ComposeProject) error {
				return ComposeDown(ctx, client, project)
			},
		},
	}

	for _, wc := range wrapperCalls {
		t.Run(wc.name, func(t *testing.T) {
			t.Parallel()

			for _, pc := range projectCases {
				t.Run(pc.name, func(t *testing.T) {
					t.Parallel()

					fake := &composeDeploymentFakeClient{}
					err := wc.call(t.Context(), fake, pc.project)
					requireUsageValidationError(t, err)
					require.Zero(t, fake.runCalls)
				})
			}
		})
	}
}

func TestComposeDeploymentWrappers_SendPrivateInvocationsAndReturnRunErrors(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	wantErr := errors.New("run failed")

	t.Run("pull", func(t *testing.T) {
		t.Parallel()

		fake := &composeDeploymentFakeClient{
			runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
				pullInv, ok := inv.(composePullInvocation)
				require.True(t, ok)
				require.Equal(t, project.ComposeFile, pullInv.composeFile)
				require.Equal(t, project.EnvFile, pullInv.envFile)
				require.Equal(t, project.ProjectName, pullInv.projectName)
				return CommandResult{Stdout: "ignored"}, wantErr
			},
		}

		err := ComposePull(t.Context(), fake, project)
		require.Same(t, wantErr, err)
		require.Equal(t, 1, fake.runCalls)
	})

	t.Run("up", func(t *testing.T) {
		t.Parallel()

		fake := &composeDeploymentFakeClient{
			runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
				upInv, ok := inv.(composeUpInvocation)
				require.True(t, ok)
				require.Equal(t, project.ComposeFile, upInv.composeFile)
				require.Equal(t, project.EnvFile, upInv.envFile)
				require.Equal(t, project.ProjectName, upInv.projectName)
				require.True(t, upInv.forceRecreate)
				return CommandResult{Stdout: "ignored"}, wantErr
			},
		}

		err := ComposeUp(
			t.Context(),
			fake,
			project,
			ComposeUpOptions{ForceRecreate: true},
		)
		require.Same(t, wantErr, err)
		require.Equal(t, 1, fake.runCalls)
	})

	t.Run("down", func(t *testing.T) {
		t.Parallel()

		fake := &composeDeploymentFakeClient{
			runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
				downInv, ok := inv.(composeDownInvocation)
				require.True(t, ok)
				require.Equal(t, project.ComposeFile, downInv.composeFile)
				require.Equal(t, project.EnvFile, downInv.envFile)
				require.Equal(t, project.ProjectName, downInv.projectName)
				return CommandResult{Stdout: "ignored"}, wantErr
			},
		}

		err := ComposeDown(t.Context(), fake, project)
		require.Same(t, wantErr, err)
		require.Equal(t, 1, fake.runCalls)
	})
}

func TestRun_ComposeDeploymentInvocationsBuildExactArgv(t *testing.T) {
	t.Parallel()

	project := validComposeProjectForDeployTests(t)
	tests := []struct {
		name    string
		inv     Invocation
		wantArg []string
	}{
		{
			name: "pull",
			inv: composePullInvocation{
				composeFile: project.ComposeFile,
				envFile:     project.EnvFile,
				projectName: project.ProjectName,
			},
			wantArg: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"pull",
			},
		},
		{
			name: "up",
			inv: composeUpInvocation{
				composeFile: project.ComposeFile,
				envFile:     project.EnvFile,
				projectName: project.ProjectName,
			},
			wantArg: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"up",
				"-d",
			},
		},
		{
			name: "up force recreate",
			inv: composeUpInvocation{
				composeFile:   project.ComposeFile,
				envFile:       project.EnvFile,
				projectName:   project.ProjectName,
				forceRecreate: true,
			},
			wantArg: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"up",
				"-d",
				"--force-recreate",
			},
		},
		{
			name: "down",
			inv: composeDownInvocation{
				composeFile: project.ComposeFile,
				envFile:     project.EnvFile,
				projectName: project.ProjectName,
			},
			wantArg: []string{
				"compose",
				"-f",
				project.ComposeFile,
				"--env-file",
				project.EnvFile,
				"--project-name",
				project.ProjectName,
				"down",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			invoked := false
			execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
				invoked = true
				require.Equal(t, tt.wantArg, cmd.argv)
				return CommandResult{}, nil
			}

			client, err := New(WithCommandExecutor(execFn))
			require.NoError(t, err)

			_, err = client.Run(t.Context(), tt.inv)
			require.NoError(t, err)
			require.True(t, invoked)
		})
	}
}

func TestRun_DefaultExecutorComposeDeploymentInvocationsUseExpectedArgv(t *testing.T) {
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

	pullRes, err := client.Run(t.Context(), composePullInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	require.NoError(t, err)
	require.Contains(
		t,
		pullRes.Stdout,
		"argv=[compose][-f]["+project.ComposeFile+"][--env-file]["+project.EnvFile+"][--project-name]["+project.ProjectName+"][pull]",
	)

	upRes, err := client.Run(t.Context(), composeUpInvocation{
		composeFile:   project.ComposeFile,
		envFile:       project.EnvFile,
		projectName:   project.ProjectName,
		forceRecreate: true,
	})
	require.NoError(t, err)
	require.Contains(
		t,
		upRes.Stdout,
		"argv=[compose][-f]["+project.ComposeFile+"][--env-file]["+project.EnvFile+"][--project-name]["+project.ProjectName+"][up][-d][--force-recreate]",
	)

	downRes, err := client.Run(t.Context(), composeDownInvocation{
		composeFile: project.ComposeFile,
		envFile:     project.EnvFile,
		projectName: project.ProjectName,
	})
	require.NoError(t, err)
	require.Contains(
		t,
		downRes.Stdout,
		"argv=[compose][-f]["+project.ComposeFile+"][--env-file]["+project.EnvFile+"][--project-name]["+project.ProjectName+"][down]",
	)
	require.NotContains(t, downRes.Stdout, "[-v]")
}

func TestValidateCommandSpec_AllowsComposeDeploymentShapes(t *testing.T) {
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
			"pull",
		},
	}))

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"compose",
			"-f",
			project.ComposeFile,
			"--env-file",
			project.EnvFile,
			"--project-name",
			project.ProjectName,
			"up",
			"-d",
		},
	}))

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"compose",
			"-f",
			project.ComposeFile,
			"--env-file",
			project.EnvFile,
			"--project-name",
			project.ProjectName,
			"up",
			"-d",
			"--force-recreate",
		},
	}))

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"compose",
			"-f",
			project.ComposeFile,
			"--env-file",
			project.EnvFile,
			"--project-name",
			project.ProjectName,
			"down",
		},
	}))
}

func TestValidateCommandSpec_RejectsUnsafeComposeDeploymentShapes(t *testing.T) {
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
			name: "bare compose down is no longer allowlisted",
			argv: []string{"compose", "down"},
		},
		{
			name: "down with -v forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"down",
				"-v",
			},
		},
		{
			name: "up missing -d",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"up",
			},
		},
		{
			name: "pull with extra flag",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"pull",
				"--quiet",
			},
		},
		{
			name: "force recreate on pull forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"pull",
				"--force-recreate",
			},
		},
		{
			name: "force recreate on down forbidden",
			argv: []string{
				"compose",
				"-f",
				composeFile,
				"--env-file",
				envFile,
				"--project-name",
				"wdm-app",
				"down",
				"--force-recreate",
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
				"pull",
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
				"pull",
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
				"pull",
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
				"pull",
			},
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

func validComposeProjectForDeployTests(t *testing.T) ComposeProject {
	t.Helper()

	stackDir := t.TempDir()
	return ComposeProject{
		ComposeFile: filepath.Join(stackDir, "docker-compose.yml"),
		EnvFile:     filepath.Join(stackDir, ".env"),
		ProjectName: "wdm-freshrss",
	}
}
