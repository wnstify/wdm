package docker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeUserEnvStack creates a stack dir with a base .env and, when withUserEnv
// is set, a 0600 .env.user overlay, returning a ComposeProject so the real
// argv-build seam stats the on-disk overlay rather than a hand-set field.
func writeUserEnvStack(t *testing.T, withUserEnv bool) ComposeProject {
	t.Helper()

	stackDir := t.TempDir()
	composeFile := filepath.Join(stackDir, "docker-compose.yml")
	envFile := filepath.Join(stackDir, ".env")
	require.NoError(t, os.WriteFile(composeFile, []byte("services: {}\n"), 0o644))
	require.NoError(t, os.WriteFile(envFile, []byte("SMTP_HOST=base\n"), 0o644))
	if withUserEnv {
		userEnvFile := filepath.Join(stackDir, ".env.user")
		require.NoError(t, os.WriteFile(userEnvFile, []byte("SMTP_HOST=user\n"), 0o600))
	}

	return ComposeProject{
		ComposeFile: composeFile,
		EnvFile:     envFile,
		ProjectName: "wdm-vaultwarden",
	}
}

// TestComposeUserEnvFileIsInterpolationSource is the regression guard for the
// ".env.user is attached as a service env_file but is not a Compose
// interpolation source" finding: when .env.user exists the built argv must
// carry a second `--env-file <.env.user>` immediately after the base
// `--env-file <.env>` (user last → last-wins), for the project (up) and config
// builders, and the strict allowlist must accept the shape.
func TestComposeUserEnvFileIsInterpolationSource(t *testing.T) {
	t.Parallel()

	project := writeUserEnvStack(t, true)
	stackDir := filepath.Dir(project.ComposeFile)
	userEnvFile := filepath.Join(stackDir, ".env.user")
	wantPair := []string{"--env-file", project.EnvFile, "--env-file", userEnvFile}

	projectInv, err := newComposeUpInvocation(project, ComposeUpOptions{})
	require.NoError(t, err)
	configInv, err := newComposeConfigInvocation(stackDir, project.ComposeFile)
	require.NoError(t, err)

	for name, inv := range map[string]Invocation{"project": projectInv, "config": configInv} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd, err := buildCommand(inv)
			require.NoError(t, err)
			require.GreaterOrEqualf(
				t,
				indexOfSubslice(cmd.argv, wantPair),
				0,
				"argv must carry base then user --env-file (user last): %v",
				cmd.argv,
			)
			require.NoError(t, validateCommandSpec(cmd))
		})
	}
}

// TestComposeUserEnvFileAbsentArgvUnchanged proves that without a .env.user
// overlay the argv keeps its single-env-file shape: the project builder emits
// exactly one `--env-file` and the config builder emits none (auto-discovery),
// with no reference to .env.user.
func TestComposeUserEnvFileAbsentArgvUnchanged(t *testing.T) {
	t.Parallel()

	project := writeUserEnvStack(t, false)
	stackDir := filepath.Dir(project.ComposeFile)
	userEnvFile := filepath.Join(stackDir, ".env.user")

	projectInv, err := newComposeUpInvocation(project, ComposeUpOptions{})
	require.NoError(t, err)
	projectCmd, err := buildCommand(projectInv)
	require.NoError(t, err)
	require.Equal(t, 1, countToken(projectCmd.argv, "--env-file"), projectCmd.argv)
	require.NotContains(t, projectCmd.argv, userEnvFile)
	require.NoError(t, validateCommandSpec(projectCmd))

	configInv, err := newComposeConfigInvocation(stackDir, project.ComposeFile)
	require.NoError(t, err)
	configCmd, err := buildCommand(configInv)
	require.NoError(t, err)
	require.NotContains(t, configCmd.argv, "--env-file")
	require.NoError(t, validateCommandSpec(configCmd))
}

func indexOfSubslice(haystack, needle []string) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func countToken(argv []string, token string) int {
	n := 0
	for _, a := range argv {
		if a == token {
			n++
		}
	}
	return n
}
