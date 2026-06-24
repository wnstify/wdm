package docker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

type validateComposeConfigFakeClient struct {
	runFn    func(context.Context, Invocation) (CommandResult, error)
	runCalls int
}

func (f *validateComposeConfigFakeClient) Run(ctx context.Context, inv Invocation) (CommandResult, error) {
	f.runCalls++
	if f.runFn != nil {
		return f.runFn(ctx, inv)
	}
	return CommandResult{}, nil
}

func TestValidateComposeConfig_RejectsNilClient(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	err := ValidateComposeConfig(t.Context(), nil, projectDir, composeFile)
	requireUsageValidationError(t, err)
}

func TestValidateComposeConfig_RejectsInvalidPathsWithoutRunningClient(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	outsideDir := t.TempDir()

	tests := []struct {
		name        string
		projectDir  string
		composeFile string
	}{
		{
			name:        "blank project directory",
			projectDir:  "   ",
			composeFile: filepath.Join(projectDir, "docker-compose.yml"),
		},
		{
			name:        "relative project directory",
			projectDir:  "relative/project",
			composeFile: filepath.Join(projectDir, "docker-compose.yml"),
		},
		{
			name:        "blank compose file",
			projectDir:  projectDir,
			composeFile: "   ",
		},
		{
			name:        "relative compose file",
			projectDir:  projectDir,
			composeFile: "docker-compose.yml",
		},
		{
			name:        "compose file outside project directory",
			projectDir:  projectDir,
			composeFile: filepath.Join(outsideDir, "docker-compose.yml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &validateComposeConfigFakeClient{}
			err := ValidateComposeConfig(t.Context(), fake, tt.projectDir, tt.composeFile)
			requireUsageValidationError(t, err)
			require.Zero(t, fake.runCalls)
		})
	}
}

func TestValidateComposeConfig_SuccessUsesComposeConfigInvocationAndDiscardsStdout(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	fake := &validateComposeConfigFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			cfgInv, ok := inv.(composeConfigInvocation)
			require.True(t, ok, "wrapper must use private compose config invocation")
			require.Equal(t, projectDir, cfgInv.projectDir)
			require.Equal(t, composeFile, cfgInv.composeFile)
			return CommandResult{Stdout: "normalized content should stay private"}, nil
		},
	}

	err := ValidateComposeConfig(t.Context(), fake, projectDir, composeFile)
	require.NoError(t, err)
	require.Equal(t, 1, fake.runCalls)
}

func TestValidateComposeConfig_ReturnsClientRunErrorUnchanged(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	wantErr := errors.New("client run failed")
	fake := &validateComposeConfigFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(composeConfigInvocation)
			require.True(t, ok)
			return CommandResult{Stdout: "ignored"}, wantErr
		},
	}

	err := ValidateComposeConfig(t.Context(), fake, projectDir, composeFile)
	require.Same(t, wantErr, err)
}

func TestRun_ComposeConfigInvocationBuildsExactArgv(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(
			t,
			[]string{
				"compose",
				"--project-directory",
				projectDir,
				"-f",
				composeFile,
				"config",
				"--quiet",
			},
			cmd.argv,
		)
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(
		t.Context(),
		composeConfigInvocation{projectDir: projectDir, composeFile: composeFile},
	)
	require.NoError(t, err)
	require.True(t, invoked)
}

func TestRun_ComposeConfigInvocationWithOverrideBuildsExactArgv(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	overridePath := filepath.Join(projectDir, "docker-compose.override.yml")

	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(
			t,
			[]string{
				"compose",
				"--project-directory",
				projectDir,
				"-f",
				composeFile,
				"-f",
				overridePath,
				"config",
				"--quiet",
			},
			cmd.argv,
		)
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(
		t.Context(),
		composeConfigInvocation{
			projectDir:   projectDir,
			composeFile:  composeFile,
			overridePath: overridePath,
		},
	)
	require.NoError(t, err)
	require.True(t, invoked)
}

func TestRun_DefaultExecutorComposeConfigInvocationUsesExpectedArgv(t *testing.T) {
	fakeDocker := `#!/bin/sh
printf 'argv='
for arg in "$@"; do
  printf '[%s]' "$arg"
done
printf '\n'
`
	useFakeDocker(t, fakeDocker)

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	client, err := New()
	require.NoError(t, err)

	got, err := client.Run(
		t.Context(),
		composeConfigInvocation{projectDir: projectDir, composeFile: composeFile},
	)
	require.NoError(t, err)
	require.Contains(
		t,
		got.Stdout,
		"argv=[compose][--project-directory]["+projectDir+"][-f]["+composeFile+"][config][--quiet]",
	)
}

func TestValidateCommandSpec_AllowsComposeConfigShape(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")

	err := validateCommandSpec(commandSpec{
		argv: []string{
			"compose",
			"--project-directory",
			projectDir,
			"-f",
			composeFile,
			"config",
			"--quiet",
		},
	})
	require.NoError(t, err)
}

func TestValidateCommandSpec_RejectsUnsafeComposeConfigShapes(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	composeFile := filepath.Join(projectDir, "docker-compose.yml")
	outsideDir := t.TempDir()
	outsideCompose := filepath.Join(outsideDir, "docker-compose.yml")

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "missing project directory value",
			argv: []string{"compose", "--project-directory", "-f", composeFile, "config", "--quiet"},
		},
		{
			name: "relative project directory",
			argv: []string{"compose", "--project-directory", "relative/project", "-f", composeFile, "config", "--quiet"},
		},
		{
			name: "missing compose file value",
			argv: []string{"compose", "--project-directory", projectDir, "-f", "config", "--quiet"},
		},
		{
			name: "relative compose file",
			argv: []string{"compose", "--project-directory", projectDir, "-f", "docker-compose.yml", "config", "--quiet"},
		},
		{
			name: "compose file outside project directory",
			argv: []string{"compose", "--project-directory", projectDir, "-f", outsideCompose, "config", "--quiet"},
		},
		{
			name: "wrong flag order",
			argv: []string{"compose", "-f", composeFile, "--project-directory", projectDir, "config", "--quiet"},
		},
		{
			name: "missing quiet flag",
			argv: []string{"compose", "--project-directory", projectDir, "-f", composeFile, "config"},
		},
		{
			name: "extra arg after quiet",
			argv: []string{"compose", "--project-directory", projectDir, "-f", composeFile, "config", "--quiet", "--no-interpolate"},
		},
		{
			name: "wrong terminal verb",
			argv: []string{"compose", "--project-directory", projectDir, "-f", composeFile, "up", "--quiet"},
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

func requireUsageValidationError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}
