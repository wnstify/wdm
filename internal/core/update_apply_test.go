package core_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// updateApplyFixture wires a fully renderable managed stack on disk for
// Engine.Update apply-path tests: a
// template-bearing catalog whose image pin is bumped past the manifest
// (so an update is available), an on-disk docker-compose.yml /.env /
// .wdm.lock / sidecar file, a stubbed secret generator, and the fake
// docker client so any Docker call would be observable (the rewrite
// makes none).
type updateApplyFixture struct {
	eng       *core.Engine
	stateDir  string
	stackPath string
	appID     string
	fake      *fakeDockerClient

	composePath  string
	envPath      string
	manifestPath string
	sidecarPath  string
	backupRoot   string

	generatedAPIToken string
}

const (
	// apiTokenInstallValue is the regenerable=true secret's pre-update
	// value; the rewrite must replace it with a fresh generated value.
	apiTokenInstallValue = "old-api-token-value"
	// dbPasswordInstallValue is the regenerable=false secret's value;
	// the rewrite must reuse it byte-for-byte from the existing .env.
	dbPasswordInstallValue = "install-time-db-password-keep-me"
	// siteNameInstallValue is a non-secret placeholder reused verbatim.
	siteNameInstallValue = "My Reused Site"
	// regeneratedAPIToken is what the stubbed generator returns for the
	// regenerable=true secret on update.
	regeneratedAPIToken = "freshly-generated-api-token"
)

// updateApplyApp returns the catalog entry the apply fixture installs:
// a bumped image pin (1.0.0 -> 2.0.0) drives "update available", two
// secrets split on regenerable, one non-secret placeholder, and one
// sidecar additional file so backup coverage is exercised. risks tags
// the candidate's risk classification.
func updateApplyApp(appID string, risks ...string) catalog.App {
	regenerableFalse := false
	app := appFixture(appID, 18080)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url", Regenerable: &regenerableFalse},
		{Name: "API_TOKEN", Type: "secret", Required: true, Encoding: "hex"},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.AdditionalFiles = []catalog.AdditionalFile{
		{
			Src:   "init-data.sh",
			Dest:  "init-data.sh",
			Mode:  "0755",
			Mount: "./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro",
		},
	}
	if len(risks) > 0 {
		app.RiskClassification = append([]string(nil), risks...)
	}
	return app
}

// updateApplyTemplates returns the catalog template files for app. The
// compose template carries the bumped image tag and a sidecar mount;
// .env.tmpl injects every value through {{.VAR }}; the sidecar template
// echoes the non-secret placeholder so a leaked secret would be visible
// in a non-0600 artifact.
func updateApplyTemplates(app catalog.App) map[string]string {
	dir := "templates/" + app.AppID + "/"
	return map[string]string{
		dir + "docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:2.0.0
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      API_TOKEN: ${API_TOKEN}
      SITE_NAME: ${SITE_NAME}
`,
		dir + ".env.tmpl":    "DB_PASSWORD={{ .DB_PASSWORD }}\nAPI_TOKEN={{ .API_TOKEN }}\nSITE_NAME={{ .SITE_NAME }}\n",
		dir + "init-data.sh": "echo {{ .SITE_NAME }}\n",
	}
}

// newUpdateApplyFixture installs the on-disk stack and returns the
// fixture. mutateApp and mutateTemplates let individual tests poison a
// template or rewind the catalog; mutateEnv lets them drop a key from
// the existing .env to exercise the missing-value refusals. upToDate,
// when true, leaves the manifest mirroring the catalog (no image-pin
// change) so the apply is a no-op rewrite.
func newUpdateApplyFixture(
	t *testing.T,
	app catalog.App,
	upToDate bool,
	mutateTemplates func(map[string]string),
	mutateEnv func(map[string]string),
) *updateApplyFixture {
	t.Helper()

	templates := updateApplyTemplates(app)
	if mutateTemplates != nil {
		mutateTemplates(templates)
	}
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	if !upToDate {
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		}
	}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	env := map[string]string{
		"DB_PASSWORD": dbPasswordInstallValue,
		"API_TOKEN":   apiTokenInstallValue,
		"SITE_NAME":   siteNameInstallValue,
	}
	if mutateEnv != nil {
		mutateEnv(env)
	}
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(renderEnvFixture(env)), 0o600))

	composeBytes := []byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n")
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "docker-compose.yml"), composeBytes, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))

	return &updateApplyFixture{
		eng:               eng,
		stateDir:          stateDir,
		stackPath:         stackPath,
		appID:             app.AppID,
		fake:              fake,
		composePath:       filepath.Join(stackPath, "docker-compose.yml"),
		envPath:           filepath.Join(stackPath, ".env"),
		manifestPath:      filepath.Join(stackPath, ".wdm.lock"),
		sidecarPath:       filepath.Join(stackPath, "init-data.sh"),
		backupRoot:        filepath.Join(stackPath, state.BackupDirName),
		generatedAPIToken: regeneratedAPIToken,
	}
}

// updateApplySecretGenerator returns the regenerable=true secret's fresh
// value (hex) and fails the test on any other encoding so a regenerated
// regenerable=false secret would be caught.
func updateApplySecretGenerator(t *testing.T) func(security.Encoding) (string, error) {
	t.Helper()
	return func(enc security.Encoding) (string, error) {
		require.Equal(t, security.EncodingHex, enc,
			"only the regenerable=true API_TOKEN (hex) may be regenerated on update")
		return regeneratedAPIToken, nil
	}
}

func renderEnvFixture(env map[string]string) string {
	var b strings.Builder
	for _, key := range []string{"DB_PASSWORD", "API_TOKEN", "SITE_NAME", "WEBHOOK_SECRET"} {
		value, ok := env[key]
		if !ok {
			continue
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(value)
		b.WriteByte('\n')
	}
	return b.String()
}

func snapshotDir(t *testing.T, backupRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(backupRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1, "expected exactly one backup snapshot")
	require.True(t, entries[0].IsDir())
	return filepath.Join(backupRoot, entries[0].Name())
}

// TestUpdate_ApplyBacksUpThenRewrites is the end-to-end happy path for
// the full apply: an available update backs up the
// pre-update config, reuses the regenerable=false secret and the
// non-secret placeholder byte-for-byte, regenerates the regenerable=true
// secret, rewrites compose + .env + sidecar atomically, validates,
// confirms the recreate, pulls, recreates, captures pins, commits the
// manifest, and returns a populated result.
func TestUpdate_ApplyBacksUpThenRewrites(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-rewrite-app"), false, nil, nil)

	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err, "the full apply path completes end to end")
	require.NotNil(t, res)

	assert.Contains(t, steps, types.StepUpdateBackup)
	assert.Contains(t, steps, types.StepUpdateRender)
	assert.Contains(t, steps, types.StepUpdateDeploy)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"the apply recreates the stack")

	// Backup exists and carries the pre-update bytes.
	snapshot := snapshotDir(t, fx.backupRoot)
	backupCompose, err := os.ReadFile(filepath.Join(snapshot, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, "services:\n  app:\n    image: docker.io/example/app:1.0.0\n", string(backupCompose),
		"the backup must hold the pre-update compose")
	backupEnv, err := os.ReadFile(filepath.Join(snapshot, ".env"))
	require.NoError(t, err)
	assert.Contains(t, string(backupEnv), "API_TOKEN="+apiTokenInstallValue,
		"the backup must hold the pre-update .env")
	assert.FileExists(t, filepath.Join(snapshot, ".wdm.lock"))
	assert.FileExists(t, filepath.Join(snapshot, "init-data.sh"))

	// Rewrite reused regenerable=false + non-secret, regenerated
	// regenerable=true.
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "DB_PASSWORD="+dbPasswordInstallValue,
		"the regenerable=false secret must be reused byte-for-byte")
	assert.Contains(t, string(envAfter), "SITE_NAME="+siteNameInstallValue,
		"the non-secret placeholder must be reused byte-for-byte")
	assert.Contains(t, string(envAfter), "API_TOKEN="+regeneratedAPIToken,
		"the regenerable=true secret must be regenerated")
	assert.NotContains(t, string(envAfter), apiTokenInstallValue,
		"the old regenerable=true value must be gone after the rewrite")

	// Compose was rewritten to the candidate tag and carries wdm labels.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "docker.io/example/app:2.0.0",
		"the rewrite must carry the candidate image tag")
	assert.Contains(t, string(composeAfter), "wdm.managed",
		"the rewrite must inject the managed labels")
	assert.NotContains(t, string(composeAfter), dbPasswordInstallValue,
		"a reused secret must not leak into the non-secret compose file")

	// Sidecar rewritten with the reused non-secret value.
	sidecarAfter, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)
	assert.Equal(t, "echo "+siteNameInstallValue+"\n", string(sidecarAfter))

	// .env keeps secret-file mode.
	assert.Equal(t, os.FileMode(0o600), fileModePerm(t, fx.envPath),
		".env must keep 0o600 after the rewrite")
}

// TestUpdate_ApplyReusesNonPlaceholderEnvKeys proves the rewrite reuses
// every install-written.env key that is neither a catalog placeholder
// nor a wdm built-in — resource-limit vars and
// any future built-in — verbatim from the existing .env, never
// re-planning or re-probing the host. The fixture's.env carries a
// MEMORY_LIMIT_APP value the new template still references; the rewrite
// must reproduce it unchanged.
func TestUpdate_ApplyReusesNonPlaceholderEnvKeys(t *testing.T) {
	t.Parallel()

	const appID = "apply-reuse-resvar-app"
	app := appFixture(appID, 18080)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/" + appID + "/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:2.0.0
    deploy:
      resources:
        limits:
          memory: ${MEMORY_LIMIT_APP}
`,
		"templates/" + appID + "/.env.tmpl": "SITE_NAME={{ .SITE_NAME }}\nMEMORY_LIMIT_APP={{ .MEMORY_LIMIT_APP }}\n",
	}, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)
	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	writeStatusStackLock(t, stackBase, appID, lock)
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte("SITE_NAME=Reused Site\nMEMORY_LIMIT_APP=512m\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "docker-compose.yml"), []byte("old\n"), 0o644))

	_, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	envAfter, err := os.ReadFile(filepath.Join(stackPath, ".env"))
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP=512m",
		"a non-placeholder resource-limit var must be reused verbatim into .env")
	composeAfter, err := os.ReadFile(filepath.Join(stackPath, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "${MEMORY_LIMIT_APP}",
		"compose keeps the ${VAR} reference for Compose runtime substitution")
	assert.Contains(t, fake.invocationTypes, "docker.composeUpInvocation")
}

// TestUpdate_ApplyBackupPrecedesRewrite proves the load-bearing ordering
// (PRD §20 step 7 before step 8): when the rewrite fails
// the backup already exists, so the snapshot captured the pre-update
// config before any byte changed. A poisoned compose template forces the
// render to fail after the backup.
func TestUpdate_ApplyBackupPrecedesRewrite(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-order-app"), false, func(templates map[string]string) {
		// An unterminated template action fails the compose render.
		templates["templates/apply-order-app/docker-compose.yml.tmpl"] = "services:\n  app:\n    image: ${X} {{ .Broken\n"
	}, nil)

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)

	// The backup ran before the failing rewrite.
	snapshot := snapshotDir(t, fx.backupRoot)
	backupCompose, err := os.ReadFile(filepath.Join(snapshot, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, composeBefore, backupCompose,
		"the backup must capture the pre-update compose before the rewrite fails")

	// The on-disk compose is unchanged — the failing render never wrote.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, composeBefore, composeAfter,
		"a render failure must not touch the stack files")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyMissingRegenerableFalseSecretRefuses proves the
// regenerable=false secret fails with the locked hint. The backup runs
// first (it captures whatever is on disk), but no file is rewritten and
// no Docker call is made.
func TestUpdate_ApplyMissingRegenerableFalseSecretRefuses(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-missing-secret-app"), false, nil, func(env map[string]string) {
		delete(env, "DB_PASSWORD")
	})

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
	assert.Equal(t, "regenerable=false secret missing from existing .env", typed.Hint,
		"the pinned hint must be byte-identical")

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, composeBefore, composeAfter, "no file may be rewritten when a required secret is missing")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyMissingNonSecretValueRefuses proves a non-secret
// placeholder absent from the existing .env also refuses fail-closed
// before any rewrite — the rewrite cannot invent a value the running
// app already depends on.
func TestUpdate_ApplyMissingNonSecretValueRefuses(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-missing-nonsecret-app"), false, nil, func(env map[string]string) {
		delete(env, "SITE_NAME")
	})

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "existing .env is missing a required value")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyMissingEnvFileRefuses proves a stack whose .env is
// gone entirely refuses through state.ReadStackEnv before any rewrite.
// The backup still ran (compose + lock exist), but no file is rewritten.
func TestUpdate_ApplyMissingEnvFileRefuses(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-no-env-app"), false, nil, nil)
	require.NoError(t, os.Remove(fx.envPath))

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "existing stack env file is missing")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_NoOpApplyStillBacksUpAndRewrites is the proof
// plus the no-op-deploy decision: an apply against an unchanged template
// is a no-op (no image-pin change) but still creates a backup snapshot,
// re-renders the stack deterministically, AND redeploys so the rewritten
// files become live (the rewrite regenerated regenerable=true secrets, so
// the running containers must be recreated to pick them up — otherwise
// disk and runtime would silently diverge). The result reports equal
// previous/new template versions and no changed services.
func TestUpdate_NoOpApplyStillBacksUpAndRewrites(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-noop-app"), true, nil, nil)

	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, res.PreviousTemplateVersion, res.NewTemplateVersion,
		"a no-op apply reports no version change")
	assert.Empty(t, res.UpdatedServices, "a no-op apply changes no services")

	assert.Contains(t, steps, types.StepUpdateBackup, "a no-op apply still backs up")
	assert.Contains(t, steps, types.StepUpdateRender, "a no-op apply still re-renders the stack")
	assert.Contains(t, steps, types.StepUpdateDeploy, "a no-op apply still redeploys to make the rewrite live")
	assert.DirExists(t, fx.backupRoot, "a no-op apply must create a backup snapshot")

	// The rewrite is deterministic: reused values are byte-identical.
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "DB_PASSWORD="+dbPasswordInstallValue)
	assert.Contains(t, string(envAfter), "SITE_NAME="+siteNameInstallValue)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
}

// TestUpdate_ApplySecretLeakIntoComposeFailsRedacted proves the install
// secret-leak verification fires on the update path too: a poisoned
// compose template that inlines a regenerated secret literal into the
// non-secret compose file fails with a verification error whose chain
// never leaks the secret value (mirrors install's poisoned-template
// test).
func TestUpdate_ApplySecretLeakIntoComposeFailsRedacted(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-leak-app"), false, func(templates map[string]string) {
		templates["templates/apply-leak-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:2.0.0
    environment:
      API_TOKEN: "{{ .API_TOKEN }}"
`
	}, nil)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assertErrorChainDoesNotContain(t, err, regeneratedAPIToken)
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyHostNetworkModeRefused proves the public-bind scan's host
// networking refusal fires on the UPDATE re-render path, not only at install:
// a tampered candidate template that renders network_mode: host (which would
// publish every container port outside the scanned ports list) is refused with
// the redacted verification error before any Docker call. The sidecar mount is
// kept so the render passes mount verification and the host-network refusal —
// not an earlier render error — is the cause.
func TestUpdate_ApplyHostNetworkModeRefused(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-host-net-app"), false, func(templates map[string]string) {
		templates["templates/apply-host-net-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:2.0.0
    network_mode: host
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
`
	}, nil)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "network_mode")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyUndeclaredPublicBindRefused proves the public-bind scan is
// wired into the UPDATE re-render path (rewriteUpdateStack), not only at
// install: a tampered candidate template that binds an undeclared port on
// 0.0.0.0 — the fixture app declares no public:true port — is refused with the
// redacted verification error before any Docker call. The sidecar mount is kept
// so the render passes mount verification and the public-bind refusal is the
// cause.
func TestUpdate_ApplyUndeclaredPublicBindRefused(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-public-bind-app"), false, func(templates map[string]string) {
		templates["templates/apply-public-bind-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:2.0.0
    ports:
      - "0.0.0.0:8088:8088"
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
`
	}, nil)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "tcp/8088")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyReusedSecretLeakIntoComposeFailsRedacted is the
// BLOCKING-fix proof: a reused regenerable=false secret (DB_PASSWORD)
// spliced into the non-secret compose file must be caught and redacted
// exactly like a generated one. The shipping catalog's six secrets are
// ALL regenerable=false, so this provenance — not the generated one — is
// the realistic curated-update leak vector. A candidate compose template
// that inlines the reused literal fails the non-secret leak check, the
// reused value never appears in the full error chain, the backup is
// intact, and the on-disk stack files are untouched by the failed
// rewrite.
func TestUpdate_ApplyReusedSecretLeakIntoComposeFailsRedacted(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-reused-leak-app"), false, func(templates map[string]string) {
		// The sidecar mount stays so the render passes mount
		// verification; the leak is the reused DB_PASSWORD inlined into
		// the non-secret compose environment.
		templates["templates/apply-reused-leak-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:2.0.0
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    environment:
      DB_PASSWORD: "{{ .DB_PASSWORD }}"
`
	}, nil)

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	envBefore, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	sidecarBefore, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assertErrorChainDoesNotContain(t, err, dbPasswordInstallValue)
	assert.Zero(t, fx.fake.calls)

	// The backup ran before the failing rewrite and holds the pre-update
	// config.
	snapshot := snapshotDir(t, fx.backupRoot)
	backupCompose, err := os.ReadFile(filepath.Join(snapshot, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, composeBefore, backupCompose,
		"the backup must capture the pre-update compose before the rewrite fails")

	// The leak check fires before any write, so the on-disk stack files
	// are byte-identical to their pre-update contents.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, composeBefore, composeAfter,
		"a verification failure must not touch the stack compose")
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, envBefore, envAfter,
		"a verification failure must not touch the stack .env")
	sidecarAfter, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)
	assert.Equal(t, sidecarBefore, sidecarAfter,
		"a verification failure must not touch the stack sidecar")
}

// TestUpdate_ApplySensitiveValueLeakIntoComposeFailsRedacted covers the
// update-path sensitive append (resolveUpdatePlaceholder, update_apply.go:458):
// a sensitive non-secret placeholder reused from the on-disk .env enters
// reusedSecretValues, so it both seeds the redactor and is forbidden from
// non-secret artifacts. The compose inlines the resolved sensitive value as a
// BARE token (no KEY= context), so only value-registration can catch it. The
// update must fail the non-secret leak check before any write, and the bare
// token must never appear in the error chain — parity with a reused secret.
func TestUpdate_ApplySensitiveValueLeakIntoComposeFailsRedacted(t *testing.T) {
	t.Parallel()

	const sensitiveValue = "Wb7Hk2Pq9Zr4Tn6Vc1Xd8Fy3Gj5Ms0"

	app := updateApplyApp("apply-sensitive-leak-app")
	app.Placeholders = append(app.Placeholders,
		catalog.Placeholder{Name: "WEBHOOK_SECRET", Type: "string", Required: true, Sensitive: true},
	)

	fx := newUpdateApplyFixture(t, app, false, func(templates map[string]string) {
		// Inline the resolved sensitive value as a bare label token — no
		// WEBHOOK_SECRET= context, so the structural name-pattern redactor
		// cannot catch it; only value-registration via reusedSecretValues can.
		templates["templates/apply-sensitive-leak-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:2.0.0
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    labels:
      wdm.test: "{{ .WEBHOOK_SECRET }}"
`
		// Reference WEBHOOK_SECRET in .env.tmpl so it renders into the
		// (secret-bearing) .env normally; the leak is the same value
		// inlined into the non-secret compose label above.
		templates["templates/apply-sensitive-leak-app/.env.tmpl"] =
			"DB_PASSWORD={{ .DB_PASSWORD }}\nAPI_TOKEN={{ .API_TOKEN }}\n" +
				"SITE_NAME={{ .SITE_NAME }}\nWEBHOOK_SECRET={{ .WEBHOOK_SECRET }}\n"
	}, func(env map[string]string) {
		env["WEBHOOK_SECRET"] = sensitiveValue
	})

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assertErrorChainDoesNotContain(t, err, sensitiveValue)
	assert.Zero(t, fx.fake.calls)

	// The leak check fires before any write, so the on-disk compose is
	// byte-identical to its pre-update contents.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, composeBefore, composeAfter,
		"a verification failure must not touch the stack compose")
}

// TestUpdate_ApplyReleasesStackFlock proves the per-stack exclusive flock
// and the runtime lock are released on a successful apply: a second apply
// acquires the stack lock cleanly (it would block/refuse busy otherwise)
// and runs to a durable success, and a fresh runtime-lock acquisition and
// stack-lock acquisition both succeed afterward. The second apply is an
// up-to-date no-op (the first committed the update) that still deploys.
func TestUpdate_ApplyReleasesStackFlock(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-flock-app"), false, nil, nil)

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	// A second apply must not stall on the stack flock and must complete,
	// proving both locks released.
	_, err = fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	probe, err := state.AcquireRuntimeLock(
		t.Context(),
		filepath.Join(fx.stateDir, "runtime.lock"),
		state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
	)
	require.NoError(t, err)
	require.NoError(t, probe.Release())

	handle, err := state.AcquireStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err, "the per-stack flock must be released after the apply")
	require.NoError(t, handle.Release())
}

// TestUpdate_ApplyDryRunTakesNoBackup re-pins the DryRun contract under
// the new apply slice: a dry-run reports the candidate without taking a
// backup, rewriting files, or touching Docker.
func TestUpdate_ApplyDryRunTakesNoBackup(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-dryrun-app"), false, nil, nil)

	envBefore, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.NoDirExists(t, fx.backupRoot, "DryRun must not take a backup")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, envBefore, envAfter, "DryRun must not rewrite .env")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyPrunesBackupsAfterCommit pins the retention behavior
// pruning
// runs AFTER the manifest commit is durable, with this run's snapshot
// pinned. Seeding 10 stale snapshots and running an apply that adds the
// 11th drives the cap to exactly 10 with the oldest seeded snapshot
// evicted by mtime and this run's just-created snapshot retained — the
// snapshot the run created can never be the one pruned.
func TestUpdate_ApplyPrunesBackupsAfterCommit(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-prune-app"), false, nil, nil)

	// Seed 10 stale snapshots so this run's 11th drives eviction. The
	// oldest by mtime must be the one dropped.
	oldest := strconv.Itoa(1_000_000_000_000_000_000) + "-update"
	for i := range 10 {
		name := strconv.Itoa(1_000_000_000_000_000_000+i) + "-update"
		dir := filepath.Join(fx.backupRoot, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("x"), 0o600))
		mtime := time.Date(2026, time.June, 1, 0, 0, i, 0, time.UTC)
		require.NoError(t, os.Chtimes(dir, mtime, mtime))
	}

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	entries, err := os.ReadDir(fx.backupRoot)
	require.NoError(t, err)
	assert.Len(t, entries, state.BackupRetentionLimit,
		"retention prunes to the cap after the durable commit")

	// The oldest seeded snapshot was evicted; this run's snapshot (the
	// pinned most-recent-successful) survives.
	assert.NoDirExists(t, filepath.Join(fx.backupRoot, oldest),
		"the oldest snapshot by mtime is evicted")
	require.NotEmpty(t, res.BackupPath, "the result names this run's backup")
	assert.DirExists(t, res.BackupPath,
		"this run's snapshot is pinned and never pruned")
}

// TestUpdate_ApplyContextCancellationBeforeRewrite proves ctx.Err
// discipline at the apply boundary: a context canceled on the planning
// summary aborts before the exclusive flock, backup, or rewrite.
func TestUpdate_ApplyContextCancellationBeforeRewrite(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-cancel-app"), false, nil, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	onProgress := func(step string, progress float64, _ string) {
		if step == types.StepUpdatePlanning && progress == 15 {
			cancel()
		}
	}

	res, err := fx.eng.Update(ctx, types.UpdateRequest{AppID: fx.appID}, onProgress, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.NoDirExists(t, fx.backupRoot, "a canceled apply must not take a backup")
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_ApplyNonRegularEnvFailsAtBackup proves the backup step
// catches a malformed (non-regular).env before the rewrite: the backup
// copies regular files only, so a directory named.env surfaces a typed
// generic backup error and no rewrite runs. This documents that the
// backup is the first reader of the stack files, ahead of
// state.ReadStackEnv.
func TestUpdate_ApplyNonRegularEnvFailsAtBackup(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-env-type-app"), false, nil, nil)

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	sidecarBefore, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)

	require.NoError(t, os.Remove(fx.envPath))
	require.NoError(t, os.Mkdir(fx.envPath, 0o755)) // .env is now a directory, not a regular file

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.ErrorContains(t, err, "config backup could not be created")
	assert.NoFileExists(t, fx.composePath+".tmp", "no rewrite temp file may linger")
	assert.Zero(t, fx.fake.calls)

	// A backup failure aborts before the rewrite, so the surviving
	// stack files are byte-identical to their pre-update contents.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, composeBefore, composeAfter,
		"a backup failure must leave docker-compose.yml untouched")
	sidecarAfter, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)
	assert.Equal(t, sidecarBefore, sidecarAfter,
		"a backup failure must leave the sidecar untouched")
}

// TestUpdate_ApplyWriteLoopFaultRestoresPreviousConfig drives a fault inside
// writeUpdateFiles' write loop AFTER docker-compose.yml and .env were
// already atomically replaced. The sidecar dest is conf/extra.txt; "conf"
// is a real directory so both the backup's and the write loop's
// read-only ancestry checks pass, but conf/extra.txt.tmp is pre-created
// as a directory so the atomic writer's O_EXCL temp-file create for the
// sidecar fails (state.WriteFileAtomic, inside writeUpdateFiles' loop).
// The collision deliberately targets the temp path, not the parent dir:
// a regular-file parent or a non-regular leaf would trip
// state.CreateConfigBackup's identical ancestry/regular-file checks at
// the backup step FIRST (validateAdditionalPathAncestors /
// collectBackupCandidate), so the fault would never reach the rewrite.
// The backup never touches <dest>.tmp, so it snapshots compose/.env/.lock
// cleanly and the write loop is the first code to fault.
// This proves a mid-write fault (compose +.env atomically replaced, then
// the sidecar write fails) is a step-4 byte-exposing fault, so the
// sad path restores the step-3 snapshot byte-for-byte: compose
// and .env go back to the pre-update bytes, the surfaced error is
// ErrCodeGeneric with the original write fault reachable, the backup
// survives, and no *.tmp FILE residue lingers.
func TestUpdate_ApplyWriteLoopFaultRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	const appID = "apply-writeloop-app"
	regenerableFalse := false
	app := appFixture(appID, 18080)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url", Regenerable: &regenerableFalse},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.AdditionalFiles = []catalog.AdditionalFile{
		{
			Src:   "extra-src.txt",
			Dest:  "conf/extra.txt",
			Mode:  "0644",
			Mount: "./conf/extra.txt:/etc/app/extra.txt:ro",
		},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/" + appID + "/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:2.0.0
    volumes:
      - ./conf/extra.txt:/etc/app/extra.txt:ro
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      SITE_NAME: ${SITE_NAME}
`,
		"templates/" + appID + "/.env.tmpl":     "DB_PASSWORD={{ .DB_PASSWORD }}\nSITE_NAME={{ .SITE_NAME }}\n",
		"templates/" + appID + "/extra-src.txt": "echo {{ .SITE_NAME }}\n",
	}, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)
	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD"}
	writeStatusStackLock(t, stackBase, appID, lock)

	composeBefore := []byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n")
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"),
		[]byte("DB_PASSWORD="+dbPasswordInstallValue+"\nSITE_NAME="+siteNameInstallValue+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "docker-compose.yml"), composeBefore, 0o644))
	// "conf" is a real directory (read-only ancestry checks pass), but
	// conf/extra.txt.tmp is a pre-existing directory so the sidecar's
	// O_EXCL temp-file create fails inside the write loop.
	require.NoError(t, os.MkdirAll(filepath.Join(stackPath, "conf", "extra.txt.tmp"), 0o755))

	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	// The atomic write fault is a byte-exposing step-4 fault, so the sad
	// path restores and re-wraps as a generic error; the original write
	// fault stays reachable in the cause chain.
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.ErrorContains(t, err, "updated stack files could not be written")
	assert.Contains(t, typed.Hint, state.BackupDirName, "the failure hint names the restored backup path")
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback")
	assert.Zero(t, fake.calls)

	// failed) is restored to the pre-update bytes from the step-3 snapshot.
	composeAfter, err := os.ReadFile(filepath.Join(stackPath, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, string(composeBefore), string(composeAfter),
		"compose is restored to the pre-update bytes after the write-loop fault")
	envAfter, err := os.ReadFile(filepath.Join(stackPath, ".env"))
	require.NoError(t, err)
	assert.Equal(t, "DB_PASSWORD="+dbPasswordInstallValue+"\nSITE_NAME="+siteNameInstallValue+"\n", string(envAfter),
		".env is restored to the pre-update bytes after the write-loop fault")

	// The step-3 backup is intact and holds the pre-update bytes.
	backupRoot := filepath.Join(stackPath, state.BackupDirName)
	snapshot := snapshotDir(t, backupRoot)
	backupCompose, err := os.ReadFile(filepath.Join(snapshot, "docker-compose.yml"))
	require.NoError(t, err)
	assert.Equal(t, composeBefore, backupCompose,
		"the backup must hold the pre-update compose after a write-loop fault")
	backupEnv, err := os.ReadFile(filepath.Join(snapshot, ".env"))
	require.NoError(t, err)
	assert.Contains(t, string(backupEnv), "DB_PASSWORD="+dbPasswordInstallValue,
		"the backup must hold the pre-update .env after a write-loop fault")

	// The sidecar dest was never created and no temp FILE lingers (the
	// pre-created conf/extra.txt.tmp is the fixture's obstruction dir,
	// not writer residue).
	assert.NoFileExists(t, filepath.Join(stackPath, "conf", "extra.txt"),
		"the sidecar dest must be absent after the write-loop fault")
	assertNoTempResidue(t, stackPath)
}

// assertNoTempResidue walks the stack tree and fails if any *.tmp file
// remains, proving the atomic writer left no partial artifact after a
// mid-write fault.
func assertNoTempResidue(t *testing.T, root string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			t.Errorf("unexpected temp residue at %q", path)
		}
		return nil
	})
	require.NoError(t, err)
}

// argon2idUpdatePHCEscaped is the $$-doubled argon2id PHC seeded into the
// existing .env for the update round-trip test. A regenerable:false
// argon2id secret must survive read -> rewrite byte-identical: no single-$
// normalization, no regeneration, no re-surfacing.
const argon2idUpdatePHCEscaped = "$$argon2id$$v=19$$m=65536,t=3,p=4$$c2FsdHNhbHRzYWx0c2E$$aGFzaC9oYXNoaGFzaA"

// TestUpdate_Argon2idRegenerableFalsePHCRoundTripsByteIdentical proves a
// regenerable:false argon2id secret is reused verbatim across an update:
// the $$-doubled PHC read from the existing .env is rewritten byte-for-byte
// with no normalization, nothing is regenerated (the generator seam is
// never invoked for it), and no plaintext is re-surfaced on the result.
func TestUpdate_Argon2idRegenerableFalsePHCRoundTripsByteIdentical(t *testing.T) {
	t.Parallel()

	regenerableFalse := false
	app := appFixture("argon2id-update-app", 18080)
	app.Name = "Vaultwarden"
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/argon2id-update/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/argon2id-update/.env.tmpl"
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	app.Placeholders = []catalog.Placeholder{
		{Name: "ADMIN_TOKEN", Type: "secret", Required: true, Encoding: "argon2id", Regenerable: &regenerableFalse},
	}

	templates := map[string]string{
		"templates/argon2id-update/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:2.0.0
    env_file:
      - .env
`,
		"templates/argon2id-update/.env.tmpl": "ADMIN_TOKEN={{ .ADMIN_TOKEN }}\n",
	}
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	// Fail the test if either secret seam is ever invoked: a
	// regenerable:false argon2id secret must be reused, never regenerated.
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		t.Fatal("the base64url/hex generator must not run for a regenerable:false argon2id secret")
		return "", nil
	})
	core.SetInstallArgon2idGeneratorForTest(eng, func() (string, string, error) {
		t.Fatal("the argon2id generator must not run on update for a regenerable:false secret")
		return "", "", nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	lock.GeneratedFields = []string{"ADMIN_TOKEN"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	envBefore := "ADMIN_TOKEN=" + argon2idUpdatePHCEscaped + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(envBefore), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))

	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	envAfter, err := os.ReadFile(filepath.Join(stackPath, ".env"))
	require.NoError(t, err)
	// The PHC line round-trips byte-identical: the $$ bytes survive intact,
	// with no single-$ normalization.
	assert.Equal(t, "ADMIN_TOKEN="+argon2idUpdatePHCEscaped, strings.TrimRight(string(envAfter), "\n"),
		"the $$-doubled PHC must round-trip byte-identical through read->rewrite")
	assert.Contains(t, string(envAfter), "$$argon2id$$",
		"the $$ escaping must not be collapsed to single-$ on rewrite")

	// The update result has no guidance/credentials surface at all
	// (UpdateResult carries no PostInstallGuidance), so a one-time
	// credential can never be re-surfaced on update — the t.Fatal guards on
	// both generator seams above already prove nothing was minted.
	_ = res
}

// TestUpdate_ApplyRewritesConfigArtifact proves the apply path renders and
// writes a catalog config_generation artifact to disk through the same
// writer arc that rewrites docker-compose.yml, .env, and additional_files
// (PRD §17, §20, F6).
func TestUpdate_ApplyRewritesConfigArtifact(t *testing.T) {
	t.Parallel()

	app := updateApplyApp("apply-config-gen-app")
	app.ConfigGeneration = []catalog.ConfigGenerationArtifact{
		{Template: "config/app.toml.tmpl", Dest: "config/app.toml", Mode: "0640"},
	}
	templates := updateApplyTemplates(app)
	templates["templates/"+app.AppID+"/config/app.toml.tmpl"] = "site = \"{{ .SITE_NAME }}\"\n"
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(renderEnvFixture(map[string]string{
		"DB_PASSWORD": dbPasswordInstallValue,
		"API_TOKEN":   apiTokenInstallValue,
		"SITE_NAME":   siteNameInstallValue,
	})), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))
	configDir := filepath.Join(stackPath, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "app.toml"), []byte("site = \"stale\"\n"), 0o640))

	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	artifactPath := filepath.Join(configDir, "app.toml")
	artifactBytes, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	assert.Equal(t, "site = \""+siteNameInstallValue+"\"\n", string(artifactBytes),
		"the config artifact must be rewritten from the reused placeholder map")
	assert.Equal(t, os.FileMode(0o640), fileModePerm(t, artifactPath))

	// The pre-update backup captured the stale artifact so a rollback can
	// restore it.
	snapshot := snapshotDir(t, filepath.Join(stackPath, state.BackupDirName))
	backupArtifact, err := os.ReadFile(filepath.Join(snapshot, "config", "app.toml"))
	require.NoError(t, err)
	assert.Equal(t, "site = \"stale\"\n", string(backupArtifact),
		"the backup must hold the pre-update config artifact")
}

// TestUpdateBackupArtifactPaths_IncludesConfigGeneration proves the
// pre-update backup-path projection covers both additional_files and
// config_generation destinations (skipping empties) so a rollback can
// restore every artifact the rewrite may overwrite.
func TestUpdateBackupArtifactPaths_IncludesConfigGeneration(t *testing.T) {
	t.Parallel()

	app := updateApplyApp("backup-paths-app")
	app.ConfigGeneration = []catalog.ConfigGenerationArtifact{
		{Template: "config/app.toml.tmpl", Dest: "config/app.toml", Mode: "0640"},
		{Template: "config/skip.tmpl", Dest: "", Mode: "0640"},
	}

	paths := core.UpdateBackupArtifactPathsForTest(app)
	assert.Equal(t, []string{"init-data.sh", "config/app.toml"}, paths,
		"backup paths must list additional_files then config_generation dests, skipping empties")
}
