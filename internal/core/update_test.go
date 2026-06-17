package core_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// updateTestFixture wires one managed-stack-on-disk scenario for
// Engine.Update check-planning tests: a stack base under the test
// home, a written.wdm.lock manifest, a catalog FS carrying the
// candidate entry, and the fake docker client seam so any Docker call
// would be observable (the check stage must make none).
type updateTestFixture struct {
	eng       *core.Engine
	stateDir  string
	stackBase string
	stackPath string
	appID     string
	fake      *fakeDockerClient
}

// newUpdateFixture builds the fixture with a catalog containing
// exactly app and a stack manifest that mirrors the catalog entry
// (up to date) unless mutateLock rewinds it.
func newUpdateFixture(t *testing.T, app catalog.App, mutateLock func(*state.StackLock)) *updateTestFixture {
	t.Helper()

	return newUpdateFixtureWithCatalog(t, catalogFixtureFS(t, app), app, mutateLock)
}

// newUpdateFixtureWithCatalog is the variant for catalog-failure
// scenarios where the catalog FS deliberately diverges from the
// stack's manifest.
func newUpdateFixtureWithCatalog(
	t *testing.T,
	catalogFS fs.FS,
	app catalog.App,
	mutateLock func(*state.StackLock),
) *updateTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &updateTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// updateStackLockForApp returns a manifest that mirrors the catalog
// entry exactly — same template version, same image pins — so the
// default fixture state is "already up to date" and tests rewind
// individual fields to create candidate updates.
func updateStackLockForApp(app catalog.App, stackPath string) state.StackLock {
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
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}
}

// TestUpdate_DryRunCheckHoldsAndReleasesRuntimeLock is the lock
// posture proof for the check stage (PRD §26, protocol
// step 1): the global runtime.lock is held — attributed to the
// "update" command — while planning runs, and released when Update
// returns.
func TestUpdate_DryRunCheckHoldsAndReleasesRuntimeLock(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, appFixture("lock-posture-app", 18080), nil)
	lockPath := filepath.Join(fx.stateDir, "runtime.lock")

	contended := false
	onProgress := func(step string, _ float64, _ string) {
		if step != types.StepUpdatePlanning || contended {
			return
		}
		contended = true
		_, err := state.AcquireRuntimeLock(
			t.Context(),
			lockPath,
			state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
		)
		require.Error(t, err, "runtime.lock must be held during the update check")
		var held *state.LockHeldError
		require.ErrorAs(t, err, &held)
		assert.Equal(t, "update", held.Holder.Command,
			"runtime.lock metadata must attribute the hold to the update command")
	}

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, onProgress, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, contended, "the planning progress event must have fired")

	probe, err := state.AcquireRuntimeLock(
		t.Context(),
		lockPath,
		state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
	)
	require.NoError(t, err, "Update must release runtime.lock on return")
	require.NoError(t, probe.Release())
}

// TestUpdate_DryRunGroupsRiskPerCatalogClass proves the PRD §20 risk
// grouping over catalog metadata for each risk class, including a
// multi-class stack whose catalog array is carried verbatim in
// catalog order.
func TestUpdate_DryRunGroupsRiskPerCatalogClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		risks []string
	}{
		{name: "safe", risks: []string{"safe"}},
		{name: "major", risks: []string{"major"}},
		{name: "database", risks: []string{"database"}},
		{name: "complex", risks: []string{"complex"}},
		{name: "multi-class stack keeps catalog order", risks: []string{"database", "complex"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := appFixture("risk-app", 18080)
			app.TemplateVersion = "2026.06.10"
			app.ImagePins = []catalog.ImagePin{
				{Service: "app", Image: "docker.io/example/app", Tag: "1.1.0"},
			}
			app.RiskClassification = append([]string(nil), tt.risks...)
			fx := newUpdateFixture(t, app, func(lock *state.StackLock) {
				lock.TemplateVersion = "2026.05.29"
				lock.ImagePins = []state.ImagePin{
					{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
				}
			})

			res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, nil)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.Equal(t, fx.appID, res.AppID)
			assert.Equal(t, "2026.05.29", res.PreviousTemplateVersion)
			assert.Equal(t, "2026.06.10", res.NewTemplateVersion)
			assert.Equal(t, []string{"app"}, res.UpdatedServices)
			assert.Equal(t, tt.risks, res.RiskClassifications,
				"risk grouping must carry the catalog risk_classification array verbatim")
			assert.Empty(t, res.BackupPath, "no backup is taken during a check")
			assert.Nil(t, res.Status, "no deployment status exists during a check")
			assert.Zero(t, fx.fake.calls, "the check stage must not run docker commands")
		})
	}
}

// TestUpdate_DryRunRecordsOldToNewImageReferences proves the PRD §20
// old → new surface across a multi-service stack: retagged, added,
// and removed services appear sorted in UpdatedServices and named
// with their image references on the StepUpdatePlanning progress
// stream, while unchanged services stay silent. The manifest also
// carries a duplicate, an empty-service, and a tag-less pin to pin
// the diff's defensive handling of degraded lock content.
func TestUpdate_DryRunRecordsOldToNewImageReferences(t *testing.T) {
	t.Parallel()

	app := appFixture("tags-app", 18080)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
		{Service: "db", Image: "docker.io/example/db", Tag: "11.4"},
		{Service: "worker", Image: "docker.io/example/worker", Tag: "2.0.0"},
	}
	fx := newUpdateFixture(t, app, func(lock *state.StackLock) {
		lock.ImagePins = []state.ImagePin{
			{Service: "legacy", Image: "docker.io/example/legacy", Tag: "0.9.0"},
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "app", Image: "docker.io/example/app", Tag: "9.9.9"},
			{Service: "", Image: "docker.io/example/ghost", Tag: "1.0.0"},
			{Service: "digest-only", Image: "docker.io/example/digest", Tag: ""},
			{Service: "db", Image: "docker.io/example/db", Tag: "11.4"},
		}
	})

	var steps []string
	var messages []string
	onProgress := func(step string, _ float64, message string) {
		steps = append(steps, step)
		messages = append(messages, message)
	}

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, onProgress, nil)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, []string{"app", "digest-only", "legacy", "worker"}, res.UpdatedServices,
		"changed services must be sorted and unchanged services excluded")

	for _, step := range steps {
		assert.Equal(t, types.StepUpdatePlanning, step,
			"the check stage must emit only planning step events")
	}
	joined := strings.Join(messages, "\n")
	assert.Contains(t, joined, "service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0")
	assert.Contains(t, joined, "service worker: new service docker.io/example/worker:2.0.0")
	assert.Contains(t, joined, "service legacy: removed from template (was docker.io/example/legacy:0.9.0)")
	assert.Contains(t, joined, "service digest-only: removed from template (was docker.io/example/digest)",
		"a tag-less pin must surface its bare image reference")
	assert.NotContains(t, joined, "docker.io/example/db",
		"unchanged services must not be reported as changes")
	assert.NotContains(t, joined, "9.9.9",
		"a duplicate lock pin must keep its first occurrence")
	assert.NotContains(t, joined, "docker.io/example/ghost",
		"an empty-service lock pin must be ignored")
	assert.Contains(t, joined, "update available", "the summary event must name the outcome")
	assert.Zero(t, fx.fake.calls, "the check stage must not run docker commands")
}

// TestUpdate_DryRunUpToDateIsNoUpdateOutcome proves the no-update
// outcome: a manifest that mirrors the catalog yields no changed
// services, no risk grouping, equal versions, and the up-to-date
// summary message.
func TestUpdate_DryRunUpToDateIsNoUpdateOutcome(t *testing.T) {
	t.Parallel()

	app := appFixture("current-app", 18080)
	fx := newUpdateFixture(t, app, nil)

	var messages []string
	onProgress := func(_ string, _ float64, message string) {
		messages = append(messages, message)
	}

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, onProgress, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, app.TemplateVersion, res.PreviousTemplateVersion)
	assert.Equal(t, app.TemplateVersion, res.NewTemplateVersion)
	assert.Empty(t, res.UpdatedServices)
	assert.Empty(t, res.RiskClassifications,
		"an up-to-date stack has no candidate update to risk-group")
	assert.Empty(t, res.BackupPath)
	assert.Nil(t, res.Status)
	assert.Contains(t, strings.Join(messages, "\n"),
		"already up to date at template version "+app.TemplateVersion)
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_RefusesUnmanagedMissingAndEmptyAppIDs covers the
// managed-only refusals (PRD §9, §20 step 1):
// empty app ids, uninstalled apps, and unmanaged directories all
// surface usage-validation errors without any Docker call. An
// invalid request additionally refuses before the first progress
// event, matching install's validate-first contract.
func TestUpdate_RefusesUnmanagedMissingAndEmptyAppIDs(t *testing.T) {
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

			eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, appFixture("refusal-app", 18080))))
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

			res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: tt.appID, DryRun: true}, onProgress, nil)
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

// TestUpdate_RefusesBusyStackWithoutBlocking proves the non-blocking
// read posture: a stack whose .wdm.lock flock is held by another
// operation refuses with ErrCodeRuntimeLockHeld instead of stalling
// behind the writer (PRD §26).
func TestUpdate_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, appFixture("busy-app", 18080), nil)
	holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
	assert.Zero(t, fx.fake.calls)
}

// TestUpdate_CorruptManifestSurfacesStaleState proves the fail-closed
// posture on corrupt stack state: the check refuses with a wrapped
// types.ErrStaleState before any Docker call.
func TestUpdate_CorruptManifestSurfacesStaleState(t *testing.T) {
	t.Parallel()

	app := appFixture("corrupt-app", 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, app.AppID, "{not json")
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID, DryRun: true}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, types.ErrStaleState)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.Zero(t, fake.calls)
}

// TestUpdate_TargetTemplateVersionMustMatchCatalog pins the explicit
// target semantics under the catalog-metadata-only candidate source
// the only reachable target is the
// catalog's current template version.
func TestUpdate_TargetTemplateVersionMustMatchCatalog(t *testing.T) {
	t.Parallel()

	newFixture := func(t *testing.T) *updateTestFixture {
		t.Helper()
		app := appFixture("target-app", 18080)
		app.TemplateVersion = "2026.06.10"
		return newUpdateFixture(t, app, func(lock *state.StackLock) {
			lock.TemplateVersion = "2026.05.29"
		})
	}

	t.Run("matching target succeeds", func(t *testing.T) {
		t.Parallel()

		fx := newFixture(t)
		res, err := fx.eng.Update(t.Context(), types.UpdateRequest{
			AppID:                 fx.appID,
			TargetTemplateVersion: "2026.06.10",
			DryRun:                true,
		}, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "2026.06.10", res.NewTemplateVersion)
	})

	t.Run("unavailable target refuses", func(t *testing.T) {
		t.Parallel()

		fx := newFixture(t)
		res, err := fx.eng.Update(t.Context(), types.UpdateRequest{
			AppID:                 fx.appID,
			TargetTemplateVersion: "2099.01.01",
			DryRun:                true,
		}, nil, nil)
		require.Error(t, err)
		assert.Nil(t, res)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "not available in the selected catalog")
	})
}

// TestUpdate_CatalogFailuresSurfaceTyped covers the catalog side of
// the check: missing and malformed catalogs map to verification
// failures, and an app absent from the catalog refuses with usage
// validation — all after the managed-stack resolution succeeded.
func TestUpdate_CatalogFailuresSurfaceTyped(t *testing.T) {
	t.Parallel()

	app := appFixture("catalog-app", 18080)

	tests := []struct {
		name      string
		catalogFS fs.FS
		check     func(t *testing.T, err error)
	}{
		{
			name:      "missing catalog file",
			catalogFS: fstest.MapFS{},
			check: func(t *testing.T, err error) {
				t.Helper()
				assertVerificationFailed(t, err)
				assert.ErrorContains(t, err, "catalog could not be read")
			},
		},
		{
			name: "malformed catalog",
			catalogFS: fstest.MapFS{
				"stable/catalog.yaml": &fstest.MapFile{Data: []byte("apps: [")},
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				assertVerificationFailed(t, err)
				assert.ErrorContains(t, err, "catalog could not be verified")
			},
		},
		{
			name:      "app absent from catalog",
			catalogFS: catalogFixtureFS(t, appFixture("other-app", 18081)),
			check: func(t *testing.T, err error) {
				t.Helper()
				assertUsageValidation(t, err)
				assert.ErrorContains(t, err, "not available in the selected catalog")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newUpdateFixtureWithCatalog(t, tt.catalogFS, app, nil)
			res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, nil)
			require.Error(t, err)
			assert.Nil(t, res)
			tt.check(t, err)
			assert.Zero(t, fx.fake.calls)
		})
	}
}

// TestUpdate_ApplyDeploysAndCommitsManifest is the canonical end-to-end
// apply proof (PRD §20 steps 8-11, protocol step 6 commit point): an
// available update validates, confirms the recreate, pulls, recreates,
// captures the digest, and commits the manifest with the new template
// version, the bumped image pin plus its opportunistic digest, the
// update-kind last_successful_operation, and the backup snapshot appended
// to backup_history. Fields the update preserves — Compose project,
// schema version — survive the commit. The result mirrors the manifest.
func TestUpdate_ApplyDeploysAndCommitsManifest(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("b", 64)
	const containerID = "0123456789ab"
	fx := newUpdateApplyFixture(t, updateApplyApp("apply-commit-app"), false, nil, nil)
	// Call order (no catalog networks): validate(1), pull(2), up(3),
	// image-digest inspect(4), container-id list(5), container inspect(6).
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		switch call {
		case 4: // opportunistic image digest inspect after deploy
			return docker.CommandResult{Stdout: "docker.io/example/app@" + digest + "\n"}, nil
		case 5: // status container-id listing by project label
			return docker.CommandResult{Stdout: containerID + "\n"}, nil
		case 6: // status container inspection
			return docker.CommandResult{
				Stdout: managedContainerInspectStdout(t, "wdm-app-1", "app", fx.appID, 18080),
			}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}

	before := time.Now().UTC()
	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	// Step ordering: validate → confirm → pull → deploy → lock → status.
	validateIdx := stepIndex(t, steps, types.StepUpdateComposeValidate)
	confirmIdx := stepIndex(t, steps, types.StepUpdateConfirm)
	pullIdx := stepIndex(t, steps, types.StepUpdatePull)
	deployIdx := stepIndex(t, steps, types.StepUpdateDeploy)
	lockIdx := stepIndex(t, steps, types.StepUpdateLockUpdate)
	statusIdx := stepIndex(t, steps, types.StepUpdateStatus)
	assert.Less(t, validateIdx, confirmIdx)
	assert.Less(t, confirmIdx, pullIdx)
	assert.Less(t, pullIdx, deployIdx)
	assert.Less(t, deployIdx, lockIdx)
	assert.Less(t, lockIdx, statusIdx)

	// Update pulls then recreates — the install path does neither.
	assert.Contains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "docker.io/example/app:2.0.0",
		"the rewrite carries the candidate image tag")

	// The.wdm.lock manifest committed the update.
	lock, err := state.ReadStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err)
	assert.Equal(t, 1, lock.SchemaVersion)
	assert.Equal(t, fx.appID, lock.AppID)
	assert.Equal(t, "2026.05.29", lock.TemplateVersion, "template version advanced to the candidate")
	assert.Equal(t, "wdm-"+fx.appID, lock.ComposeProject)
	require.Len(t, lock.ImagePins, 1)
	assert.Equal(t, "app", lock.ImagePins[0].Service)
	assert.Equal(t, "2.0.0", lock.ImagePins[0].Tag, "the image pin advanced to the candidate tag")
	assert.Equal(t, digest, lock.ImagePins[0].Digest, "the opportunistic digest is captured")
	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "update", lock.LastSuccessfulOperation.Kind)
	assert.WithinDuration(t, before, lock.LastSuccessfulOperation.At, time.Minute)
	require.Len(t, lock.BackupHistory, 1, "the step-3 snapshot is appended to backup_history")
	assert.Contains(t, string(lock.BackupHistory[0]), state.BackupDirName,
		"the backup_history entry records the snapshot path")

	// The result mirrors the committed manifest.
	assert.Equal(t, fx.appID, res.AppID)
	assert.Equal(t, []string{"app"}, res.UpdatedServices)
	assert.Equal(t, "2026.05.29", res.NewTemplateVersion)
	require.NotEmpty(t, res.BackupPath)
	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)
}

// TestUpdate_ContextCancellation covers the ctx.Err discipline: a
// pre-canceled context refuses before the runtime.lock MkdirAll side
// effect, and cancellation mid-planning propagates as
// context.Canceled through the wrap chain.
func TestUpdate_ContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("pre-canceled context", func(t *testing.T) {
		t.Parallel()

		fx := newUpdateFixture(t, appFixture("cancel-app", 18080), nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		res, err := fx.eng.Update(ctx, types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, nil)
		require.Error(t, err)
		assert.Nil(t, res)
		require.ErrorIs(t, err, context.Canceled)
		_, statErr := os.Stat(fx.stateDir)
		require.True(t, os.IsNotExist(statErr),
			"a pre-canceled context must not create the state dir")
		assert.Zero(t, fx.fake.calls)
	})

	t.Run("canceled during planning", func(t *testing.T) {
		t.Parallel()

		fx := newUpdateFixture(t, appFixture("cancel-mid-app", 18080), nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		onProgress := func(string, float64, string) {
			cancel()
		}

		res, err := fx.eng.Update(ctx, types.UpdateRequest{AppID: fx.appID, DryRun: true}, onProgress, nil)
		require.Error(t, err)
		assert.Nil(t, res)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, fx.fake.calls)
	})
}

// TestUpdate_FindsStackUnderCustomDirectoryName covers the scan
// fallback shared with Status: a stack installed under a directory
// name that differs from its app_id stays update-checkable by the
// identifier List reports.
func TestUpdate_FindsStackUnderCustomDirectoryName(t *testing.T) {
	t.Parallel()

	app := appFixture("custom-name-app", 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, "renamed-dir")
	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "0.9.0"},
	}
	writeStatusStackLock(t, stackBase, "renamed-dir", lock)
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID, DryRun: true}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"app"}, res.UpdatedServices)
	assert.Zero(t, fake.calls)
}
