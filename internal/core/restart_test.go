package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// TestRestart_ClosedEngineReturnsErrClosed keeps the closed-engine arm:
// a closed engine returns ErrClosed (it takes precedence over every other
// outcome) with a nil result.
func TestRestart_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.Restart(t.Context(), types.RestartRequest{AppID: "uptime-kuma"}, nil, nil)
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

// restartTestFixture wires one managed-stack-on-disk scenario for
// Engine.Restart tests: a stack base under the test home, a written
// .wdm.lock manifest, a catalog FS carrying the candidate entry, and the
// fake docker client seam so every Docker call is observable — the
// managed-only refusals must make none, and the happy path makes exactly
// the restart plus the status inspect.
type restartTestFixture struct {
	eng       *core.Engine
	stateDir  string
	stackBase string
	stackPath string
	appID     string
	fake      *fakeDockerClient
}

// newRestartFixture builds the fixture with a catalog containing exactly
// app and a stack manifest mirroring the catalog entry. mutateLock may
// adjust the manifest before it is written.
func newRestartFixture(t *testing.T, app catalog.App, mutateLock func(*state.StackLock)) *restartTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := restartStackLockForApp(app, stackPath)
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &restartTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// restartStackLockForApp returns a manifest that mirrors the catalog
// entry so the managed-stack resolution succeeds and the Compose project
// drives the restart. It seeds non-empty provenance collections
// (local_ports, generated_fields, backup_history, recommended_resources)
// so the byte-identity test proves the restart rewrites NONE of them.
func restartStackLockForApp(app catalog.App, stackPath string) state.StackLock {
	pins := make([]state.ImagePin, 0, len(app.ImagePins))
	for _, pin := range app.ImagePins {
		pins = append(pins, state.ImagePin{
			Service: pin.Service,
			Image:   pin.Image,
			Tag:     pin.Tag,
		})
	}
	return state.StackLock{
		SchemaVersion:   1,
		AppID:           app.AppID,
		TemplateName:    app.TemplateName,
		TemplateVersion: app.TemplateVersion,
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.05.29",
		StackPath:       stackPath,
		ComposeProject:  "wdm-" + app.AppID,
		ImagePins:       pins,
		LocalPorts:      []int{18080},
		GeneratedFields: []string{"DB_PASSWORD"},
		BackupHistory: []json.RawMessage{
			json.RawMessage(`{"path":".wdm-backups/1747752731487293841-update","operation":"update","at":"2026-06-01T12:00:00Z"}`),
		},
		RecommendedResources: &state.RecommendedResources{
			MemoryBytes: 536870912,
			CPUs:        0.5,
		},
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}
}

// scriptRestartHappyPath wires the fake to answer the full restart
// execution sequence by invocation type so an end-to-end Restart
// succeeds: `docker compose restart` succeeds and the post-restart
// container list is empty (no lingering inspect needed; the status fuses
// container_missing for the expected service, but the restart still
// succeeds). For a running-status happy path use scriptRestartRunning.
func scriptRestartHappyPath(fx *restartTestFixture) {
	fx.fake.runFn = func(_ int, _ docker.Invocation) (docker.CommandResult, error) {
		return docker.CommandResult{}, nil
	}
}

// restartContainerInspectStdout builds one `docker inspect`-shaped record
// for a running managed container, in the strict 8-field order
// internal/docker.parseContainerInspectOutput expects, so the
// post-restart status fuses a clean running state.
func restartContainerInspectStdout(t *testing.T, service, appID string) string {
	t.Helper()

	labels := map[string]string{
		"wdm.managed":                "true",
		"wdm.app":                    appID,
		"com.docker.compose.service": service,
		"com.docker.compose.project": "wdm-" + appID,
	}
	rawLabels, err := json.Marshal(labels)
	require.NoError(t, err)
	ports := `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"18080"}]}`
	return fmt.Sprintf(
		"%q\n%s\n%q\n%t\n%t\n%d\n%q\n%s\n",
		"/wdm-"+appID+"-"+service+"-1", rawLabels, "running", true, false, 0, "healthy", ports,
	)
}

// scriptRestartRunning wires the fake so the restart succeeds and the
// post-restart status verify reports a single healthy running "app"
// service (matching the appFixture image pin), so the result status is
// State "running" with no attention reasons.
func scriptRestartRunning(fx *restartTestFixture, t *testing.T) {
	t.Helper()

	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: statusTestContainerID + "\n"}, nil
		case "docker.containerInspectInvocation":
			return docker.CommandResult{Stdout: restartContainerInspectStdout(t, "app", fx.appID)}, nil
		default:
			// `docker compose restart` succeeds.
			return docker.CommandResult{}, nil
		}
	}
}

// TestRestart_HappyPathRunsConfirmRestartStatus is the end-to-end arc:
// planning → confirm (immediately before the restart) → `docker compose
// restart` → post-restart status verify → populated RestartResult. It
// proves the step stream, the confirm payload contents, the restart
// invocation type, the sorted RestartedServices, and the fused running
// status — plus that the runtime.lock is released (a second Restart
// succeeds).
func TestRestart_HappyPathRunsConfirmRestartStatus(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-happy-app", 18080), nil)
	scriptRestartRunning(fx, t)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.NoError(t, err, "the restart runs to completion")
	require.NotNil(t, res)

	// The frozen restart step stream fires in order.
	assert.Equal(t, []string{
		types.StepRestartPlanning,
		types.StepRestartPlanning,
		types.StepRestartConfirm,
		types.StepRestartExecute,
		types.StepRestartStatus,
	}, steps)

	// The confirm payload: the SAFE restart_safe kind, app name, stack
	// path, Compose project, the stop/start statement, and the per-service
	// restart lines.
	require.Len(t, confirmer.calls, 1, "the confirmer is asked exactly once before the restart")
	payload := confirmer.calls[0]
	assert.Equal(t, "restart_safe", payload.Kind)
	assert.Contains(t, payload.Title, fx.appID)
	assert.Contains(t, payload.Message, "app: "+fx.appID)
	assert.Contains(t, payload.Message, "stack path: "+fx.stackPath)
	assert.Contains(t, payload.Message, "compose project: wdm-"+fx.appID)
	assert.Contains(t, payload.Message, "this stops and starts the stack's containers")
	assert.Contains(t, payload.Message, "no data loss")
	assert.Contains(t, payload.Message, "restarts service app")

	// The restart invocation ran (the plain compose restart type).
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation")

	// The structured result carries the sorted services and the fused
	// running status.
	assert.Equal(t, fx.appID, res.AppID)
	assert.Equal(t, "wdm-"+fx.appID, res.ComposeProject)
	assert.Equal(t, []string{"app"}, res.RestartedServices)
	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.Status.AttentionReasons)

	// The runtime.lock was released: a second restart on the same engine
	// succeeds.
	res2, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "Restart must release runtime.lock so a second call succeeds")
	require.NotNil(t, res2)
}

// TestRestart_SortsRestartedServices proves RestartedServices is the
// manifest's image-pin service set, sorted (the whole stack — restart is
// whole-stack only). A multi-service manifest with out-of-order pins
// surfaces in sorted order.
func TestRestart_SortsRestartedServices(t *testing.T) {
	t.Parallel()

	app := appFixture("restart-multi-svc-app", 18080)
	app.ImagePins = []catalog.ImagePin{
		{Service: "web", Image: "docker.io/example/web", Tag: "1.0.0"},
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		{Service: "db", Image: "docker.io/example/db", Tag: "1.0.0"},
	}
	fx := newRestartFixture(t, app, nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"app", "db", "web"}, res.RestartedServices,
		"RestartedServices is the manifest image-pin services, sorted")
}

// snapshotStackDir hashes every file under root recursively, keyed by the
// relative path, so a test can prove a directory is byte-identical before
// and after an operation.
func snapshotRestartStackDir(t *testing.T, root string) map[string]string {
	t.Helper()

	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		snap[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
		return nil
	})
	require.NoError(t, err)
	return snap
}

// TestRestart_ChangesNothingOnDisk is THE the invariant pin: after a
// successful restart the stack directory is byte-identical (every file,
// including.wdm.lock, hashes the same) and no.wdm-backups directory
// appears. The restart runs `docker compose restart` ONLY — it never
// re-renders templates, never writes config files, never creates a
// backup, and never updates the manifest (no commit point, no
// last_successful_operation change). The byte-identity of.wdm.lock IS
// the proof that no manifest write occurred.
func TestRestart_ChangesNothingOnDisk(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-no-write-app", 18080), nil)
	scriptRestartRunning(fx, t)

	// Seed the stack with the config files a real stack carries so the
	// snapshot has compose/.env content to compare, not just.wdm.lock.
	envPath := filepath.Join(fx.stackPath, ".env")
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")
	require.NoError(t, os.WriteFile(envPath, []byte("DB_PASSWORD=keepme\n"), 0o600))
	require.NoError(t, os.WriteFile(composePath, []byte("services:\n  app:\n    image: x\n"), 0o644))

	before := snapshotRestartStackDir(t, fx.stackPath)
	require.NotEmpty(t, before[".wdm.lock"], "the manifest must be in the snapshot")

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"the restart must actually run docker compose restart")

	after := snapshotRestartStackDir(t, fx.stackPath)
	assert.Equal(t, before, after,
		"a successful restart must leave the entire stack directory byte-identical (decision #46)")

	// No backup directory is created — restart takes no config snapshot.
	_, statErr := os.Stat(filepath.Join(fx.stackPath, ".wdm-backups"))
	assert.True(t, errors.Is(statErr, fs.ErrNotExist),
		"a restart must not create a .wdm-backups directory (decision #46)")

	// The manifest's last_successful_operation is unchanged: still the
	// pre-restart install op, never a "restart" marker.
	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "install", committed.LastSuccessfulOperation.Kind,
		"a restart must not record a commit point in last_successful_operation")
}

// TestRestart_HoldsAndReleasesRuntimeLock is the lock-posture proof for
// the live restart (PRD §26): the global runtime.lock is held —
// attributed to the "restart" command — while the restart runs end-to-end
// (planning → confirm → restart → status), and the lock is released when
// Restart returns so a later acquisition succeeds.
func TestRestart_HoldsAndReleasesRuntimeLock(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-lock-posture-app", 18080), nil)
	scriptRestartHappyPath(fx)
	lockPath := filepath.Join(fx.stateDir, "runtime.lock")

	contended := false
	onProgress := func(step string, _ float64, _ string) {
		if step != types.StepRestartPlanning || contended {
			return
		}
		contended = true
		_, err := state.AcquireRuntimeLock(
			t.Context(),
			lockPath,
			state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
		)
		require.Error(t, err, "runtime.lock must be held during restart planning")
		var held *state.LockHeldError
		require.ErrorAs(t, err, &held)
		assert.Equal(t, "restart", held.Holder.Command,
			"runtime.lock metadata must attribute the hold to the restart command")
	}

	_, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, onProgress, &fakeConfirmer{})
	require.NoError(t, err)
	require.True(t, contended, "the planning progress event must have fired")

	probe, err := state.AcquireRuntimeLock(
		t.Context(),
		lockPath,
		state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
	)
	require.NoError(t, err, "Restart must release runtime.lock on return")
	require.NoError(t, probe.Release())
}

// TestRestart_RefusesUnmanagedMissingAndEmptyAppIDs covers the
// managed-only refusals (PRD §9, §10): empty app ids, uninstalled apps,
// and unmanaged directories all surface usage-validation errors without
// any Docker call. An empty app id additionally refuses before the first
// progress event, matching the install/update/remove validate-first
// contract.
func TestRestart_RefusesUnmanagedMissingAndEmptyAppIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		appID          string
		setup          func(t *testing.T, stackBase string)
		wantContains   string
		wantZeroEvents bool
	}{
		{
			name:           "empty app id",
			appID:          "",
			wantContains:   "app id is required",
			wantZeroEvents: true,
		},
		{
			name:         "app not installed",
			appID:        "ghost-app",
			wantContains: "app is not installed",
		},
		{
			name:  "unmanaged directory",
			appID: "unmanaged-app",
			setup: func(t *testing.T, stackBase string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(stackBase, "unmanaged-app"), 0o755))
			},
			wantContains: "not managed by wdm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, appFixture("restart-refusal-app", 18080))))
			stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
			if tt.setup != nil {
				tt.setup(t, stackBase)
			}
			fake := &fakeDockerClient{}
			core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

			var events int
			onProgress := func(string, float64, string) {
				events++
			}

			res, err := eng.Restart(t.Context(), types.RestartRequest{AppID: tt.appID}, onProgress, &fakeConfirmer{})
			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, tt.wantContains)
			assert.Zero(t, fake.calls, "refusals must happen before any docker command")
			if tt.wantZeroEvents {
				assert.Zero(t, events, "request validation must refuse before the first progress event")
			}
		})
	}
}

// TestRestart_CanceledContextEmitsNoEvents proves the validate-first
// contract's ctx arm: a pre-canceled context refuses before the first
// StepRestartPlanning emission and before any Docker call.
func TestRestart_CanceledContextEmitsNoEvents(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-cancel-app", 18080), nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var events int
	res, err := fx.eng.Restart(ctx, types.RestartRequest{AppID: fx.appID}, func(string, float64, string) {
		events++
	}, &fakeConfirmer{})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context must propagate as context.Canceled")
	assert.Nil(t, res)
	assert.Zero(t, events, "a canceled request must refuse before the first progress event")
	assert.Zero(t, fx.fake.calls, "a canceled request must refuse before any docker command")
}

// TestRestart_RefusesBusyStackWithoutBlocking proves the non-blocking
// read posture: a stack whose .wdm.lock flock is held by another
// operation refuses with ErrCodeRuntimeLockHeld instead of stalling
// behind the writer (PRD §26), before any Docker call.
func TestRestart_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-busy-app", 18080), nil)
	holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
	assert.Zero(t, fx.fake.calls, "a busy stack must refuse before any docker command")
}

// TestRestart_CorruptManifestSurfacesStaleState proves the fail-closed
// posture on corrupt stack state: planning refuses with a wrapped
// types.ErrStaleState before any Docker call.
func TestRestart_CorruptManifestSurfacesStaleState(t *testing.T) {
	t.Parallel()

	app := appFixture("restart-corrupt-app", 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, app.AppID, "{not json")
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Restart(t.Context(), types.RestartRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, types.ErrStaleState)
	assert.Zero(t, fake.calls, "a corrupt manifest must refuse before any docker command")
}

// TestRestart_EmptyComposeProjectRefuses proves the corrupt-manifest
// guard: a managed lock missing its compose project refuses with
// ErrCodeUsageValidation naming the corrupt manifest, before any Docker
// call.
func TestRestart_EmptyComposeProjectRefuses(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-empty-project-app", 18080), func(lock *state.StackLock) {
		lock.ComposeProject = ""
	})
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "missing its compose project")
	assert.Zero(t, fx.fake.calls,
		"a corrupt manifest must refuse before any docker command")
}

// TestRestart_MismatchedStackPathRefusesBeforeDocker proves the
// fail-closed StackPath cross-check: a provided req.StackPath that does
// not match the AppID-resolved managed stack refuses with
// ErrCodeUsageValidation before any Docker call — the check sits ahead of
// the restart.
func TestRestart_MismatchedStackPathRefusesBeforeDocker(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-stackpath-mismatch-app", 18080), nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{
		AppID:     fx.appID,
		StackPath: filepath.Join(fx.stackBase, "some-other-path"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "stack path does not match the managed stack")
	assert.Zero(t, fx.fake.calls,
		"the stack-path cross-check must refuse before any docker command")
}

// TestRestart_MatchingStackPathProceeds proves a req.StackPath that names
// the resolved managed stack (here with a trailing separator, normalized
// away by filepath.Clean) passes the cross-check and the restart proceeds
// to completion.
func TestRestart_MatchingStackPathProceeds(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-stackpath-match-app", 18080), nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{
		AppID:     fx.appID,
		StackPath: fx.stackPath + string(filepath.Separator),
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a matching stack path proceeds to completion")
	require.NotNil(t, res)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation")
}

// TestRestart_NilConfirmerRefuses proves a nil confirmer refuses with
// ErrCodeUsageValidation per the pkg/engine contract and runs no restart.
func TestRestart_NilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-nil-confirmer-app", 18080), nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a nil confirmer must refuse before any restart")
}

// TestRestart_DeclineCancelsWithZeroMutation proves the confirm gate: a
// decline maps to ErrCodeUserCanceled, runs no `docker compose restart`,
// and leaves the manifest byte-identical — the confirm precedes the
// restart, so a decline leaves zero trace.
func TestRestart_DeclineCancelsWithZeroMutation(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-decline-app", 18080), nil)
	scriptRestartHappyPath(fx)

	manifestBefore, err := os.ReadFile(filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, nil
	}}
	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code)

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a decline must run no docker compose restart")
	manifestAfter, err := os.ReadFile(filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter,
		"a decline must leave the manifest byte-identical")
}

// TestRestart_ConfirmerErrorPropagatesWrapped proves a confirmer backend
// error propagates wrapped (matching the install/update/remove posture)
// and runs no restart.
func TestRestart_ConfirmerErrorPropagatesWrapped(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-confirmer-err-app", 18080), nil)
	scriptRestartHappyPath(fx)

	sentinel := errors.New("confirmer backend down")
	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, sentinel
	}}
	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "the confirmer error must remain reachable in the chain")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a confirmer error must abort before any restart")
}

// TestRestart_EmitsOnlyRestartPrefixedStepIDs is the whole-stream guard
// for the frozen restart progress API: every emitted step ID must carry
// the step_restart_ prefix — no step_install_, step_update_, or
// step_remove_ leak (mirrors the remove guard). Pinning the whole stream
// catches any cross-path leak.
func TestRestart_EmitsOnlyRestartPrefixedStepIDs(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-step-guard-app", 18080), nil)
	scriptRestartRunning(fx, t)

	var steps []string
	_, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, steps, "the restart must emit progress steps")
	for _, step := range steps {
		assert.True(t, strings.HasPrefix(step, "step_restart_"),
			"the restart progress stream must only carry step_restart_* IDs, got %q", step)
	}
}

// TestRestart_DaemonUnavailableDuringRestartPropagates proves the daemon
// carve-out for the restart itself: an unreachable daemon
// (ErrCodeDockerUnavailable) returned by `docker compose restart`
// propagates unchanged rather than being swallowed — the restart could
// not run, so it is a typed error (PRD §27 exit 5).
func TestRestart_DaemonUnavailableDuringRestartPropagates(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-daemon-down-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeRestartInvocation" {
			return docker.CommandResult{}, types.NewError(
				types.ErrCodeDockerUnavailable,
				"docker daemon is not reachable",
				"start the docker service and retry",
			)
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typed.Code)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"a restart failure must abort before the status verify")
}

// TestRestart_PostRestartInspectFailureMarksNeedsAttention proves the
// post-restart status verify never fails the operation: the restart ran,
// so an inspect failure during the status verify marks the RestartResult
// needs-attention with status_check_failed rather than failing the
// restart (mirroring verifyUpdateStatus / verifyInstallStatus).
func TestRestart_PostRestartInspectFailureMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-status-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectContainerListInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a post-restart inspect failure must not fail the restart")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)
}

// TestRestart_PostRestartDaemonDownDuringStatusMarksNeedsAttention proves
// the daemon-down inspect failure during the post-restart status verify
// fuses needs-attention rather than propagating: unlike the restart
// itself (which propagates ErrCodeDockerUnavailable), once the restart
// already ran a daemon-down status inspect must NOT fail the operation —
// it marks the result needs-attention with status_check_failed
func TestRestart_PostRestartDaemonDownDuringStatusMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-postrestart-daemon-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectContainerListInvocation" {
			return docker.CommandResult{}, types.NewError(
				types.ErrCodeDockerUnavailable,
				"docker daemon is not reachable",
				"start the docker service and retry",
			)
		}
		// `docker compose restart` succeeds.
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err,
		"a daemon-down status inspect after the restart must not fail the operation")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)
}

// TestRestart_RunsOnlyRestartAndStatusDockerCalls proves the exact Docker
// sequence: the restart runs `docker compose restart` (the FIRST and only
// mutating Docker call), then the post-restart status verify lists
// containers. No volume listing, no compose down, no pull, no up — restart
// neither removes nor recreates anything.
func TestRestart_RunsOnlyRestartAndStatusDockerCalls(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("restart-docker-seq-app", 18080), nil)
	scriptRestartRunning(fx, t)

	_, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	// The first Docker call is the restart, then the status container list
	// (plus an inspect for the one observed container).
	require.NotEmpty(t, fx.fake.invocationTypes)
	assert.Equal(t, "docker.composeRestartInvocation", fx.fake.invocationTypes[0],
		"the restart must be the first Docker call")
	assert.Contains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"the post-restart status verify lists containers")
	for _, inv := range fx.fake.invocationTypes {
		assert.NotContains(t, inv, "composeDownInvocation",
			"a restart must never run docker compose down")
		assert.NotContains(t, inv, "composePullInvocation",
			"a restart must never pull images")
		assert.NotContains(t, inv, "composeUpInvocation",
			"a restart must never run docker compose up")
		assert.NotContains(t, inv, "projectVolumeListInvocation",
			"a restart must never list volumes")
	}
}

// TestRestart_RestartArgvIsPlainRestart is the core-level
// real-docker-binary proof: a REAL internal/docker client over a fake
// `docker` binary that logs argv runs `docker compose... restart` with no
// per-service argument and no recreate/down/pull flags. Cannot run in
// parallel: it mutates PATH.
func TestRestart_RestartArgvIsPlainRestart(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	installFakeDockerOnPath(t, `#!/bin/sh
{
  printf 'argv='
  for arg in "$@"; do printf '[%s]' "$arg"; done
  printf '\n'
} >> "$WDM_ARGV_LOG"
exit 0
`)
	t.Setenv("WDM_ARGV_LOG", argvLog)

	fx := newRestartFixture(t, appFixture("restart-argv-app", 18080), nil)
	core.SetInstallDockerClientFactoryForTest(fx.eng, realDockerClientFactory(t))

	_, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the restart completes against the fake docker binary")

	logged, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	logText := string(logged)
	t.Logf("captured docker argv:\n%s", logText)

	assert.Contains(t, logText, "[restart]",
		"the restart must run docker compose restart")
	assert.NotContains(t, logText, "[down]",
		"a restart must never run docker compose down")
	assert.NotContains(t, logText, "[--force-recreate]",
		"a restart must never force-recreate containers")
	assert.NotContains(t, logText, "[pull]",
		"a restart must never pull images")
}

// sortedServices is a local helper for asserting the whole-stack service
// derivation independent of map iteration order.
func sortedServices(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestRestart_RestartedServicesMatchManifestPins is an explicit guard
// that RestartedServices equals the manifest's image-pin service set
// (deduplicated, sorted), independent of catalog port shape — restart is
// whole-stack and derives its service set from the lock, not the request.
func TestRestart_RestartedServicesMatchManifestPins(t *testing.T) {
	t.Parallel()

	app := appFixture("restart-pins-app", 18080)
	app.ImagePins = []catalog.ImagePin{
		{Service: "beta", Image: "docker.io/example/beta", Tag: "1.0.0"},
		{Service: "alpha", Image: "docker.io/example/alpha", Tag: "1.0.0"},
		{Service: "beta", Image: "docker.io/example/beta", Tag: "1.0.0"},
	}
	fx := newRestartFixture(t, app, nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.Restart(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, sortedServices([]string{"alpha", "beta"}), res.RestartedServices,
		"RestartedServices is the deduplicated, sorted manifest pin services")
}
