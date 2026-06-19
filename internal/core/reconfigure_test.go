package core_test

import (
	"context"
	"errors"
	"fmt"
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

// TestReconfigure_DeployFailureRestoresPreviousConfig drives a fault on
// the recreate (docker compose up) AFTER the rewrite exposed the new .env
// bytes and the config validation passed. The sad path must restore the
// snapshot byte-for-byte (the resource var returns to its install value),
// surface a typed error, and return no result.
func TestReconfigure_DeployFailureRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-deployfail-app"), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeUpInvocation" {
			return docker.CommandResult{ExitCode: 1, Stderr: "recreate failed"}, errors.New("compose up rejected")
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
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"the deploy must have been attempted before the restore")

	// The sad path restored the snapshot: the resource var is back to its
	// install value, byte-for-byte.
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP="+reconfigureInstallMemory,
		"a deploy failure restores the pre-change .env")
	assert.NotContains(t, string(envAfter), "MEMORY_LIMIT_APP=1g",
		"the exposed new value must be rolled back after the deploy fault")
	assert.Contains(t, string(envAfter), "DB_PASSWORD="+reconfigureSecretValue,
		"the secret survives the restore byte-for-byte")
}

// TestReconfigure_ConfirmerDeclinedAbortsAndRestores proves a declined
// recreate confirmation aborts before any Docker mutation and restores the
// rewritten .env to its pre-change bytes. The confirmation gate runs after
// the rewrite exposed the new bytes, so the decline routes through the same
// sad path as a deploy fault.
func TestReconfigure_ConfirmerDeclinedAbortsAndRestores(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-decline-app"), nil)
	decliner := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) { return false, nil },
	}

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, decliner)
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code,
		"a declined recreate maps to the user-canceled code")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a declined confirmation must not recreate the container")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP="+reconfigureInstallMemory,
		"a declined confirmation restores the pre-change .env")
}

// TestReconfigure_NilConfirmerRefusedAndRestores proves a nil confirmer is
// refused fail-closed with a usage error before any Docker mutation, and
// the rewritten .env is restored to its pre-change bytes.
func TestReconfigure_NilConfirmerRefusedAndRestores(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-nilconf-app"), nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envAfter), "MEMORY_LIMIT_APP="+reconfigureInstallMemory,
		"a nil-confirmer refusal restores the pre-change .env")
}

// TestReconfigure_BackupFailureAbortsBeforeRewrite proves a backup-creation
// failure aborts before the rewrite exposes any byte: docker-compose.yml is
// replaced with a directory so the backup's regular-file copy fails AFTER
// planning has read the (still-regular) .env. A typed generic error surfaces
// and no Docker call runs.
func TestReconfigure_BackupFailureAbortsBeforeRewrite(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-backupfail-app"), nil)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")
	require.NoError(t, os.Remove(composePath))
	require.NoError(t, os.Mkdir(composePath, 0o755)) // compose is now a directory

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.ErrorContains(t, err, "config backup could not be created")
	assert.Zero(t, fx.fake.calls, "a backup failure aborts before any Docker call")
}

// TestReconfigure_RewriteFailureAbortsBeforeDeploy proves a render fault in
// the rewrite (a poisoned compose template) aborts before any byte is
// written and before any Docker call: the backup ran, the on-disk .env is
// unchanged, and no recreate happens. rewriteReconfigureStack is pure, so
// the fault is pre-exposure and needs no restore.
func TestReconfigure_RewriteFailureAbortsBeforeDeploy(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-rewritefail-app")
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/" + app.AppID + "/docker-compose.yml.tmpl": "services:\n  app:\n    image: ${X} {{ .Broken\n",
		"templates/" + app.AppID + "/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n" +
			"MEMORY_LIMIT_APP={{ .MEMORY_LIMIT_APP }}\n" +
			"CPUS_LIMIT_APP={{ .CPUS_LIMIT_APP }}\n" +
			"PIDS_LIMIT_APP={{ .PIDS_LIMIT_APP }}\n",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		t.Fatalf("no secret may be generated on reconfigure")
		return "", nil
	})
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)
	lock := updateStackLockForApp(app, stackPath)
	lock.GeneratedFields = []string{"DB_PASSWORD"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)
	envBytes := []byte(renderReconfigureEnv(map[string]string{
		"DB_PASSWORD":      reconfigureSecretValue,
		"MEMORY_LIMIT_APP": reconfigureInstallMemory,
		"CPUS_LIMIT_APP":   reconfigureInstallCPUs,
		"PIDS_LIMIT_APP":   reconfigureInstallPIDs,
	}))
	envPath := filepath.Join(stackPath, ".env")
	require.NoError(t, os.WriteFile(envPath, envBytes, 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))

	res, err := eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   app.AppID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.Zero(t, fake.calls, "a render fault aborts before any Docker call")

	envAfter, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, envBytes, envAfter, "a pre-exposure rewrite fault must not touch .env")
}

// TestReconfigure_MissingEnvResourceVarRefused proves readServiceResourceValues
// refuses fail-closed when the targeted service's resource var is absent from
// the stack .env: a corrupt/incomplete stack is a usage error, not a silent
// default, and no backup or Docker call happens.
func TestReconfigure_MissingEnvResourceVarRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-missingvar-app"), func(env map[string]string) {
		delete(env, "PIDS_LIMIT_APP")
	})

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "pids limit")
	assert.Zero(t, fx.fake.calls)
	assert.NoDirExists(t, fx.backupRoot, "a missing-var refusal aborts before the backup")
}

// TestReconfigure_NonIntegerEnvPidsRefused proves readServiceResourceValues
// treats a non-integer PIDS_LIMIT in the .env as a corrupt-stack refusal
// rather than a silent default.
func TestReconfigure_NonIntegerEnvPidsRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-badpids-app"), func(env map[string]string) {
		env["PIDS_LIMIT_APP"] = "not-a-number"
	})

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "invalid pids limit")
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_ServiceNotInCatalogRefused proves a --service the catalog
// declares no resource band for is refused fail-closed before any change.
func TestReconfigure_ServiceNotInCatalogRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-nosvc-app"), nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "missing-service",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "service does not declare resource limits")
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_StackPathMismatchRefused proves a StackPath that does not
// resolve to the managed stack for the app is refused fail-closed before any
// change, guarding against reconfiguring an unexpected directory.
func TestReconfigure_StackPathMismatchRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-pathmismatch-app"), nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:     fx.appID,
		Service:   "app",
		Memory:    strPtr("1g"),
		StackPath: "/not/the/managed/stack",
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "stack path does not match")
	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_ClosedEngineRefused proves Reconfigure and
// ResourceSettings both return ErrClosed after the engine is closed,
// before any lock acquisition or disk access.
func TestReconfigure_ClosedEngineRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-closed-app"), nil)
	require.NoError(t, fx.eng.Close())

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.ErrorIs(t, err, core.ErrClosed)

	settings, err := fx.eng.ResourceSettings(t.Context(), fx.appID)
	require.Error(t, err)
	assert.Nil(t, settings)
	assert.ErrorIs(t, err, core.ErrClosed)

	assert.Zero(t, fx.fake.calls)
}

// TestReconfigure_TamperedPublicBindRefused proves the install-arc
// public-bind scan is wired into the reconfigure re-render path: a tampered
// catalog template that binds an undeclared port on 0.0.0.0 is refused with
// the redacted verification error before any byte is written or recreate
// runs. The backup precedes the rewrite, but no Docker call happens.
func TestReconfigure_TamperedPublicBindRefused(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-publicbind-app")
	templates := reconfigureTemplates(app.AppID)
	templates["templates/"+app.AppID+"/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:1.0.0
    ports:
      - "0.0.0.0:8099:8099"
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
    deploy:
      resources:
        limits:
          memory: ${MEMORY_LIMIT_APP}
          cpus: ${CPUS_LIMIT_APP}
        reservations:
          pids: ${PIDS_LIMIT_APP}
`
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		t.Fatalf("no secret may be generated on reconfigure")
		return "", nil
	})
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)
	lock := updateStackLockForApp(app, stackPath)
	lock.GeneratedFields = []string{"DB_PASSWORD"}
	writeStatusStackLock(t, stackBase, app.AppID, lock)
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(renderReconfigureEnv(map[string]string{
		"DB_PASSWORD":      reconfigureSecretValue,
		"MEMORY_LIMIT_APP": reconfigureInstallMemory,
		"CPUS_LIMIT_APP":   reconfigureInstallCPUs,
		"PIDS_LIMIT_APP":   reconfigureInstallPIDs,
	})), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))

	res, err := eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   app.AppID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.ErrorContains(t, err, "tcp/8099")
	assert.Zero(t, fake.calls, "a public-bind refusal aborts before any Docker call")
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

// TestResourceSettings_EmptyAppIDRefused proves the read-only view refuses
// an empty app id with a usage error before touching the catalog or disk.
func TestResourceSettings_EmptyAppIDRefused(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-emptyid-app"), nil)

	settings, err := fx.eng.ResourceSettings(t.Context(), "")
	require.Error(t, err)
	assert.Nil(t, settings)
	assertUsageValidation(t, err)
}

// TestResourceSettings_AppNotInstalledRefused proves the read-only view
// refuses an uninstalled app: resolveManagedStack fails before any catalog
// lookup or env read.
func TestResourceSettings_AppNotInstalledRefused(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-view-absent-app")
	catalogFS := catalogFixtureFSWithFiles(t, reconfigureTemplates(app.AppID), app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))

	settings, err := eng.ResourceSettings(t.Context(), app.AppID)
	require.Error(t, err)
	assert.Nil(t, settings)
	assertUsageValidation(t, err)
}

// TestResourceSettings_MissingEnvRendersBandsWithEmptyCurrent proves the
// read-only view tolerates a stack whose .env lacks a service's resource
// vars: the catalog bands still render and the current values come back
// empty/zero rather than failing (the view is informational).
func TestResourceSettings_MissingEnvRendersBandsWithEmptyCurrent(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-view-noenv-app"), func(env map[string]string) {
		delete(env, "MEMORY_LIMIT_APP")
		delete(env, "CPUS_LIMIT_APP")
		delete(env, "PIDS_LIMIT_APP")
	})

	settings, err := fx.eng.ResourceSettings(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, settings)
	require.Len(t, settings.Services, 1)

	svc := settings.Services[0]
	assert.Empty(t, svc.CurrentMemory, "an absent .env resource var renders empty current")
	assert.Empty(t, svc.CurrentCPUs)
	assert.Zero(t, svc.CurrentPIDs)
	assert.Equal(t, "256m", svc.MemoryMin, "the catalog band still renders")
	assert.Equal(t, 500, svc.PIDsMax)
}

// TestReconfigure_DeployFailureEmitsRestoreProgress proves the sad path
// emits its restore progress event when a progress callback is wired: a
// deploy fault routes through restoreReconfigureOnFailure, which emits the
// config-restore step before delegating to the shared restore.
func TestReconfigure_DeployFailureEmitsRestoreProgress(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("reconf-restoreprog-app"), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeUpInvocation" {
			return docker.CommandResult{ExitCode: 1}, errors.New("compose up rejected")
		}
		return docker.CommandResult{}, nil
	}

	var steps []string
	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, steps, types.StepReconfigureConfigRestore,
		"the sad path must emit the config-restore progress step")
}

// TestReconfigure_DuplicatePlaceholderRefused proves the rewrite's
// placeholder-resolution guard rejects a catalog that declares the same
// placeholder twice, fail-closed before any byte is written. The backup
// runs first (it precedes the rewrite), but no Docker call happens.
func TestReconfigure_DuplicatePlaceholderRefused(t *testing.T) {
	t.Parallel()

	app := reconfigureApp("reconf-dupph-app")
	regenerableFalse := false
	app.Placeholders = append(app.Placeholders, catalog.Placeholder{
		Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url", Regenerable: &regenerableFalse,
	})
	fx := newReconfigureFixture(t, app, nil)

	res, err := fx.eng.Reconfigure(t.Context(), types.ReconfigureRequest{
		AppID:   fx.appID,
		Service: "app",
		Memory:  strPtr("1g"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.ErrorContains(t, err, "duplicate placeholder")
	assert.Zero(t, fx.fake.calls, "a catalog-shape fault aborts before any Docker call")
}
