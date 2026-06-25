package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// stagingProbeClient is a fake docker.Client whose Run inspects the
// hermetic compose-config project directory on disk at the moment
// validation calls it. It locates that directory via the single
// wdm-compose-validate-* entry under the test-isolated TMPDIR, then
// records whether the rendered config artifact was staged alongside the
// compose file. The probe runs synchronously inside
// validateRenderedComposeConfig, before its deferred cleanup, so the
// staged tree is still present.
type stagingProbeClient struct {
	tmpRoot      string
	t            *testing.T
	runCalls     int
	sawArtifact  bool
	sawEnvFile   bool
	sawEnvUser   bool
	artifactMode os.FileMode
	envUserMode  os.FileMode
}

func (c *stagingProbeClient) Run(_ context.Context, _ docker.Invocation) (docker.CommandResult, error) {
	c.t.Helper()
	c.runCalls++

	matches, err := filepath.Glob(filepath.Join(c.tmpRoot, "wdm-compose-validate-*"))
	require.NoError(c.t, err)
	require.Len(c.t, matches, 1, "exactly one hermetic validation workspace must exist during Run")
	projectDir := matches[0]

	if info, statErr := os.Stat(filepath.Join(projectDir, "secrets.env")); statErr == nil {
		c.sawArtifact = true
		c.artifactMode = info.Mode().Perm()
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, ".env")); statErr == nil {
		c.sawEnvFile = true
	}
	if info, statErr := os.Stat(filepath.Join(projectDir, ".env.user")); statErr == nil {
		c.sawEnvUser = true
		c.envUserMode = info.Mode().Perm()
	}
	return docker.CommandResult{}, nil
}

// TestValidateRenderedComposeConfig_StagesConfigArtifacts proves the
// pre-deploy compose-config validation stages the COMPLETE deployed
// artifact set — not just compose + .env — into its hermetic project
// dir, so a compose that points an env_file at a rendered
// config_generation artifact validates against the exact layout that
// will deploy. Before the fix the validation staged only compose + .env,
// so secrets.env was absent and `docker compose config` would fail
// closed; the probe asserts the artifact is present on disk in the
// project dir at the moment validation invokes the docker client.
func TestValidateRenderedComposeConfig_StagesConfigArtifacts(t *testing.T) {
	// t.Setenv pins TMPDIR so os.MkdirTemp roots the workspace under a
	// directory this test owns; it forbids t.Parallel, which keeps the
	// single-workspace glob in the probe unambiguous.
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	rendered := &render.RenderedStack{
		ComposeBytes: []byte("services:\n" +
			"  app:\n" +
			"    image: docker.io/example/app:1.0.0\n" +
			"    env_file: secrets.env\n"),
		EnvBytes: []byte("APP_KEY=value\n"),
		ConfigArtifacts: []render.RenderedFile{
			{
				Dest:  "secrets.env",
				Mode:  "0600",
				Bytes: []byte("SECRET_TOKEN=staged-value\n"),
			},
		},
	}

	client := &stagingProbeClient{tmpRoot: tmpRoot, t: t}

	err := validateRenderedComposeConfig(t.Context(), client, rendered)
	require.NoError(t, err)

	require.Equal(t, 1, client.runCalls, "validation must invoke the docker client exactly once")
	require.True(t, client.sawEnvFile, ".env must be staged in the validation workspace")
	require.True(
		t,
		client.sawArtifact,
		"the rendered config artifact must be staged in the validation workspace; "+
			"without full-set staging secrets.env is absent and env_file resolution fails closed",
	)
	require.Equal(
		t,
		security.SecretFileMode,
		client.artifactMode,
		"a 0600 config artifact must stay 0600 in the hermetic workspace",
	)
}

// TestValidateRenderedComposeConfig_StagesEmptyEnvUser proves the
// pre-deploy validation stages an empty .env.user (0600) into its
// hermetic project dir even though .env.user is user-owned, not a
// rendered artifact. A template that lists env_file: [.env.user] would
// otherwise fail `docker compose config` fail-closed because the file is
// absent at validation time. The probe asserts it is present on disk in
// the project dir at the moment validation invokes the docker client.
func TestValidateRenderedComposeConfig_StagesEmptyEnvUser(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	rendered := &render.RenderedStack{
		ComposeBytes: []byte("services:\n" +
			"  app:\n" +
			"    image: docker.io/example/app:1.0.0\n" +
			"    env_file:\n" +
			"      - .env.user\n"),
		EnvBytes: []byte("APP_KEY=value\n"),
	}

	client := &stagingProbeClient{tmpRoot: tmpRoot, t: t}

	err := validateRenderedComposeConfig(t.Context(), client, rendered)
	require.NoError(t, err)

	require.Equal(t, 1, client.runCalls, "validation must invoke the docker client exactly once")
	require.True(
		t,
		client.sawEnvUser,
		"an empty .env.user must be staged so env_file: [.env.user] resolves during compose config",
	)
	require.Equal(
		t,
		security.SecretFileMode,
		client.envUserMode,
		"the staged .env.user must be 0600",
	)
}

// TestValidateRenderedComposeConfig_NilRenderedFailsClosed proves the
// validate phase refuses an unrendered plan: a nil rendered stack returns a
// usage-validation error and never invokes the docker client, so a future
// edit that reaches validation before render is caught at the seam.
func TestValidateRenderedComposeConfig_NilRenderedFailsClosed(t *testing.T) {
	t.Parallel()

	client := &stagingProbeClient{t: t}
	err := validateRenderedComposeConfig(t.Context(), client, nil)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	require.Equal(t, 0, client.runCalls, "validation must not invoke the docker client for a nil rendered stack")
}

// TestValidateRenderedComposeConfig_RemovesStagedArtifactsOnReturn proves
// the hermetic workspace — including the secret-bearing copies — never
// outlives the call: the deferred cleanup removes the whole tree before
// validateRenderedComposeConfig returns.
func TestValidateRenderedComposeConfig_RemovesStagedArtifactsOnReturn(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	rendered := &render.RenderedStack{
		ComposeBytes: []byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		EnvBytes:     []byte("APP_KEY=value\n"),
		ConfigArtifacts: []render.RenderedFile{
			{Dest: "secrets.env", Mode: "0600", Bytes: []byte("SECRET_TOKEN=staged-value\n")},
		},
	}

	client := &stagingProbeClient{tmpRoot: tmpRoot, t: t}
	require.NoError(t, validateRenderedComposeConfig(t.Context(), client, rendered))

	matches, err := filepath.Glob(filepath.Join(tmpRoot, "wdm-compose-validate-*"))
	require.NoError(t, err)
	require.Empty(t, matches, "the hermetic workspace and its secret-bearing copies must be removed on return")
}
