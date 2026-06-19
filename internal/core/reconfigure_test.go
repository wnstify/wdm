package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// reconfigureFixture wires a fully renderable resource-bearing managed
// stack on disk for Engine.Reconfigure tests: a template-bearing catalog
// declaring one overridable resource band, an on-disk
// docker-compose.yml / .env / .wdm.lock carrying the install-time
// resource vars and a secret, a stubbed secret generator, and the fake
// docker client so any Docker call is observable.
type reconfigureFixture struct {
	eng        *core.Engine
	stackPath  string
	appID      string
	fake       *fakeDockerClient
	envPath    string
	backupRoot string
}

const (
	reconfigureSecretValue   = "install-time-secret-keep-me"
	reconfigureInstallMemory = "512m"
	reconfigureInstallCPUs   = "1.0"
	reconfigureInstallPIDs   = "200"
)

// reconfigureApp returns the catalog entry: one overridable "app"
// service with a memory/cpu/pids band, a regenerable=false secret, and a
// compose template that references the three resource vars plus the
// secret so a leak would be visible.
func reconfigureApp(appID string) catalog.App {
	regenerableFalse := false
	app := appFixture(appID, 18080)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url", Regenerable: &regenerableFalse},
	}
	app.Resources = []catalog.ResourceProfile{
		{
			Service:       "app",
			Memory:        catalog.MemoryBand{Min: "256m", Recommended: "512m", Max: "1g"},
			CPUs:          catalog.CPUBand{Min: "0.25", Recommended: "1.0", Max: "2.0"},
			PIDs:          catalog.PIDsBand{Default: 200, Max: 500},
			AllowOverride: true,
		},
	}
	return app
}

func reconfigureTemplates(appID string) map[string]string {
	dir := "templates/" + appID + "/"
	return map[string]string{
		dir + "docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    deploy:
      resources:
        limits:
          memory: ${MEMORY_LIMIT_APP}
          cpus: ${CPUS_LIMIT_APP}
        reservations:
          pids: ${PIDS_LIMIT_APP}
`,
		dir + ".env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n" +
			"MEMORY_LIMIT_APP={{ .MEMORY_LIMIT_APP }}\n" +
			"CPUS_LIMIT_APP={{ .CPUS_LIMIT_APP }}\n" +
			"PIDS_LIMIT_APP={{ .PIDS_LIMIT_APP }}\n",
	}
}

func newReconfigureFixture(t *testing.T, app catalog.App, mutateEnv func(map[string]string)) *reconfigureFixture {
	t.Helper()

	catalogFS := catalogFixtureFSWithFiles(t, reconfigureTemplates(app.AppID), app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		t.Fatalf("no regenerable=true secret may be generated on reconfigure")
		return "", nil
	})
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	lock.GeneratedFields = []string{"DB_PASSWORD"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	env := map[string]string{
		"DB_PASSWORD":      reconfigureSecretValue,
		"MEMORY_LIMIT_APP": reconfigureInstallMemory,
		"CPUS_LIMIT_APP":   reconfigureInstallCPUs,
		"PIDS_LIMIT_APP":   reconfigureInstallPIDs,
	}
	if mutateEnv != nil {
		mutateEnv(env)
	}
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(renderReconfigureEnv(env)), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))

	return &reconfigureFixture{
		eng:        eng,
		stackPath:  stackPath,
		appID:      app.AppID,
		fake:       fake,
		envPath:    filepath.Join(stackPath, ".env"),
		backupRoot: filepath.Join(stackPath, state.BackupDirName),
	}
}

func renderReconfigureEnv(env map[string]string) string {
	var out string
	for _, key := range []string{"DB_PASSWORD", "MEMORY_LIMIT_APP", "CPUS_LIMIT_APP", "PIDS_LIMIT_APP"} {
		if value, ok := env[key]; ok {
			out += key + "=" + value + "\n"
		}
	}
	return out
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// TestReconfigure_HappyPathRewritesResourceVarsAndRecreates is the
// end-to-end happy path: a request changing memory and cpus rewrites
// ONLY those resource vars in .env, preserves the secret and the
// untouched pids var, re-renders, validates, confirms, recreates, and
// commits the manifest with a reconfigure operation kind.
func TestReconfigure_HappyPathRewritesResourceVarsAndRecreates(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-happy-app"), nil)

	var steps []string
	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
		CPUs:    strPtr("2.0"),
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, "1g", res.Memory)
	assert.Equal(t, "2.0", res.CPUs)
	assert.Equal(t, 200, res.PIDs, "an unchanged pids limit keeps its installed value")

	assert.Contains(t, steps, types.StepReconfigureBackup)
	assert.Contains(t, steps, types.StepReconfigureRender)
	assert.Contains(t, steps, types.StepReconfigureDeploy)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composePullInvocation",
		"reconfigure changes no image, so it never pulls")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP=1g")
	assert.Contains(t, string(envAfter), "CPUS_LIMIT_APP=2.0")
	assert.Contains(t, string(envAfter), "PIDS_LIMIT_APP=200",
		"an unchanged resource var is preserved verbatim")
	assert.Contains(t, string(envAfter), "DB_PASSWORD="+reconfigureSecretValue,
		"the install-time secret is preserved byte-for-byte")
	assert.Equal(t, os.FileMode(0o600), fileModePerm(t, fx.envPath),
		".env keeps secret-file mode after the rewrite")
}

// TestReconfigure_OutOfBandRejectedBeforeAnyChange proves an over-max
// memory value is refused fail-closed with a usage error and no backup,
// rewrite, or Docker call happens.
func TestReconfigure_OutOfBandRejectedBeforeAnyChange(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-band-app"), nil)
	envBefore, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("4g"), // band max is 1g
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.Zero(t, fx.fake.calls, "no Docker call on an out-of-band refusal")
	assert.NoDirExists(t, fx.backupRoot, "no backup is taken before the band check")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, envBefore, envAfter, "an out-of-band refusal must not touch .env")
}

// TestReconfigure_NotAdjustableServiceRejected proves a service whose
// catalog band forbids overrides is refused fail-closed.
func TestReconfigure_NotAdjustableServiceRejected(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-locked-app")
	app.Resources[0].AllowOverride = false
	fx := newReconfigureFixture(t, app, nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_AppNotInstalledRejected proves an uninstalled app is
// refused with a usage error.
func TestReconfigure_AppNotInstalledRejected(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-absent-app")
	catalogFS := catalogFixtureFSWithFiles(t, reconfigureTemplates(app.AppID), app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))

	res, err := eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   app.AppID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
}

// TestReconfigure_NoLimitsRequestedRejected proves a request with all
// three limits nil is refused: callers use the read-only view instead.
func TestReconfigure_NoLimitsRequestedRejected(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-empty-app"), nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_SetButInvalidLimitsRejectedBeforeAnyChange proves a
// non-nil-but-empty/zero limit is a user error, not "leave unchanged":
// an explicit empty memory, empty cpus, or sub-1 pids is refused
// fail-closed with a usage error, and no backup, .env rewrite, or Docker
// call happens. Without this guard the install-time sentinel semantics
// in applyResourceLimitOverride would silently retain the current value.
func TestReconfigure_SetButInvalidLimitsRejectedBeforeAnyChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  func(appID string) types.ReconfigureRequest
	}{
		{
			name: "explicit empty memory",
			req: func(appID string) types.ReconfigureRequest {
				return types.ReconfigureRequest{AppID: appID, Service: "app", Memory: strPtr("")}
			},
		},
		{
			name: "explicit empty cpus",
			req: func(appID string) types.ReconfigureRequest {
				return types.ReconfigureRequest{AppID: appID, Service: "app", CPUs: strPtr("")}
			},
		},
		{
			name: "pids set to zero",
			req: func(appID string) types.ReconfigureRequest {
				return types.ReconfigureRequest{AppID: appID, Service: "app", PIDs: intPtr(0)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newReconfigureFixture(t, reconfigureApp("reconf-invalid-app"), nil)
			envBefore, err := os.ReadFile(fx.envPath)
			require.NoError(t, err)

			res, err := fx.eng.Reconfigure(t.Context(), tt.req(fx.appID), nil, &fakeConfirmer{})
			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.Zero(t, fx.fake.calls, "no Docker call on a set-but-invalid refusal")
			assert.NoDirExists(t, fx.backupRoot, "no backup is taken before the value check")

			envAfter, err := os.ReadFile(fx.envPath)
			require.NoError(t, err)
			assert.Equal(t, envBefore, envAfter, "a set-but-invalid refusal must not touch .env")
		})
	}
}

// TestReconfigure_ComposeConfigFailAbortsBeforeRecreate proves a
// failing `docker compose config` validation aborts before the recreate
// and restores the previous config (the .env is rewritten then restored,
// so the resource var returns to its install value and no compose up
// runs).
func TestReconfigure_ComposeConfigFailAbortsBeforeRecreate(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-cfgfail-app"), nil)
	// The first Docker call on the apply path is the compose-config
	// validation; fail it so the recreate never runs.
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 1 {
			return docker.CommandResult{ExitCode: 1, Stderr: "config invalid"}, errors.New("compose config rejected")
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a config-validation failure must abort before the recreate")

	// The previous config is restored: the resource var is back to its
	// install value.
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP="+reconfigureInstallMemory,
		"a pre-recreate failure restores the previous .env")
}

// TestReconfigure_BackupPrecedesRewrite proves the load-bearing
// ordering: a backup snapshot exists carrying the pre-change .env.
func TestReconfigure_BackupPrecedesRewrite(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-backup-app"), nil)

	_, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		PIDs:    intPtr(300),
	}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	snapshot := snapshotDir(t, fx.backupRoot)
	backupEnv, err := os.ReadFile(filepath.Join(snapshot, ".env"))
	require.NoError(t, err)
	assert.Contains(t, string(backupEnv), "MEMORY_LIMIT_APP="+reconfigureInstallMemory,
		"the backup holds the pre-change .env")
	assert.Contains(t, string(backupEnv), "PIDS_LIMIT_APP="+reconfigureInstallPIDs,
		"the backup holds the pre-change pids value")
}

// TestResourceSettings_ReportsCurrentValuesAndBands proves the read-only
// view surfaces the .env current values alongside the catalog bands.
func TestResourceSettings_ReportsCurrentValuesAndBands(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-view-app"), nil)

	settings, err := fx.eng.ResourceSettings(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, settings)
	require.Len(t, settings.Services, 1)

	svc := settings.Services[0]
	assert.Equal(t, "app", svc.Service)
	assert.True(t, svc.Adjustable)
	assert.Equal(t, reconfigureInstallMemory, svc.CurrentMemory)
	assert.Equal(t, reconfigureInstallCPUs, svc.CurrentCPUs)
	assert.Equal(t, 200, svc.CurrentPIDs)
	assert.Equal(t, "256m", svc.MemoryMin)
	assert.Equal(t, "1g", svc.MemoryMax)
	assert.Equal(t, "2.0", svc.CPUsMax)
	assert.Equal(t, 500, svc.PIDsMax)
}
