package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// rewireFixture wires a managed stack whose CURRENT catalog template carries
// the env_file: [.env.user] overlay, while the on-disk compose is the
// pre-feature shape WITHOUT it. RewireStack must detect the gap, re-render
// from this template, and inject the overlay without changing the image or
// the on-disk secrets.
type rewireFixture struct {
	eng        *core.Engine
	stackPath  string
	appID      string
	fake       *fakeDockerClient
	composeStr string
	envPath    string
}

const rewireSecretValue = "rewire-install-secret-keep-me"

// rewireTemplates returns a catalog template that carries env_file:
// [.env.user] on every service — the post-feature template shape. The image
// matches the image pin so the install-arc guards pass on re-render.
func rewireTemplates(appID string) map[string]string {
	dir := "templates/" + appID + "/"
	return map[string]string{
		dir + "docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    env_file:
      - .env.user
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
`,
		dir + ".env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}
}

// rewireApp returns a catalog entry with a single regenerable=false secret
// and a matching image pin; no resource bands are needed for rewire.
func rewireApp(appID string) catalog.App {
	regenerableFalse := false
	app := appFixture(appID, 18081)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	app.Resources = []catalog.ResourceProfile{}
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url", Regenerable: &regenerableFalse},
	}
	return app
}

// newRewireFixture builds the engine + on-disk stack. onDiskCompose is the
// compose written to disk (the pre-feature shape unless a test overrides it).
func newRewireFixture(t *testing.T, app catalog.App, onDiskCompose string) *rewireFixture {
	t.Helper()

	catalogFS := catalogFixtureFSWithFiles(t, rewireTemplates(app.AppID), app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		t.Fatalf("no secret may be generated on rewire")
		return "", nil
	})
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	lock.GeneratedFields = []string{"DB_PASSWORD"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte("DB_PASSWORD="+rewireSecretValue+"\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte(onDiskCompose),
		0o644,
	))

	return &rewireFixture{
		eng:        eng,
		stackPath:  stackPath,
		appID:      app.AppID,
		fake:       fake,
		composeStr: onDiskCompose,
		envPath:    filepath.Join(stackPath, ".env"),
	}
}

// preFeatureCompose is the on-disk compose of a stack installed before the
// env_file overlay landed: same image, no env_file entry.
const preFeatureCompose = `services:
  app:
    image: docker.io/example/app:1.0.0
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    labels:
      wdm.managed: "true"
      wdm.app: rewire-app
`

// TestRewireStack_PreFeatureStackInjectsOverlayImagesAndSecretsUnchanged is
// the headline: a pre-feature stack (no env_file) gets re-rendered so the
// compose now references .env.user, while the image stays byte-identical and
// the on-disk .env secret is never touched. It exercises the REAL render path.
func TestRewireStack_PreFeatureStackInjectsOverlayImagesAndSecretsUnchanged(t *testing.T) {
	t.Parallel()

	fx := newRewireFixture(t, rewireApp("rewire-app"), preFeatureCompose)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")

	envBefore, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)

	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, &fakeConfirmer{})
	require.NoError(t, err)
	assert.True(t, rewired, "a pre-feature stack must be rewired")
	assert.Equal(t, filepath.Join(fx.stackPath, ".env.user"), path)

	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "env_file:", "compose must now carry an env_file entry")
	assert.Contains(t, string(composeAfter), ".env.user", "compose must now reference .env.user")
	assert.Contains(t, string(composeAfter), "docker.io/example/app:1.0.0",
		"the image reference must be byte-identical after rewire")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, envBefore, envAfter, ".env (and its secret) must be byte-identical after rewire")
	assert.Contains(t, string(envAfter), "DB_PASSWORD="+rewireSecretValue)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), ".env.user must be seeded 0600")

	assert.Contains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"the stack must restart so the overlay takes effect")
}

// TestRewireStack_AlreadyWiredIsNoOp proves an already-wired stack is a
// no-op: nothing is rewritten, no restart runs, and the confirmer is not
// consulted.
func TestRewireStack_AlreadyWiredIsNoOp(t *testing.T) {
	t.Parallel()

	const wiredCompose = `services:
  app:
    image: docker.io/example/app:1.0.0
    env_file:
      - .env.user
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    labels:
      wdm.managed: "true"
      wdm.app: rewire-app
`
	fx := newRewireFixture(t, rewireApp("rewire-app"), wiredCompose)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")

	confirmer := &fakeConfirmer{}
	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, confirmer)
	require.NoError(t, err)
	assert.False(t, rewired, "an already-wired stack must be a no-op")
	assert.Empty(t, path)
	assert.Empty(t, confirmer.calls, "a no-op must not prompt for confirmation")

	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, wiredCompose, string(composeAfter), "an already-wired compose must not be rewritten")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a no-op must not restart the stack")
}

// TestRewireStack_ConfirmerDeclinedWritesNothing proves a declined confirmer
// aborts before any byte change: the compose is untouched and no restart runs.
func TestRewireStack_ConfirmerDeclinedWritesNothing(t *testing.T) {
	t.Parallel()

	fx := newRewireFixture(t, rewireApp("rewire-app"), preFeatureCompose)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")

	confirmer := &fakeConfirmer{
		confirmFn: func(_ context.Context, _ types.Confirmation) (bool, error) { return false, nil },
	}
	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, confirmer)
	require.Error(t, err)
	assert.False(t, rewired)
	assert.Empty(t, path)

	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, preFeatureCompose, string(composeAfter), "a declined rewire must write nothing")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a declined rewire must not restart the stack")

	_, statErr := os.Stat(filepath.Join(fx.stackPath, ".env.user"))
	assert.True(t, os.IsNotExist(statErr), "a declined rewire must not seed .env.user")
}
