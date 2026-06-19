package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// emptyCatalogFS is a catalog FS with no stable/catalog.yaml, so
// loadInstallCatalog fails — used to exercise the opportunistic
// catalog-load degradation in the remaining-network read.
func emptyCatalogFS() fs.FS {
	return fstest.MapFS{}
}

// removeTestFixture wires one managed-stack-on-disk scenario for
// Engine.Remove safe-removal planning tests: a stack base under the
// test home, a written.wdm.lock manifest, a catalog FS carrying the
// candidate entry (so remaining-network reporting has a source), and
// the fake docker client seam so every Docker call is observable — the
// managed-only refusals must make none, and the only call on the happy
// path is the named-volume listing.
type removeTestFixture struct {
	eng       *core.Engine
	stateDir  string
	stackBase string
	stackPath string
	appID     string
	fake      *fakeDockerClient
}

// newRemoveFixture builds the fixture with a catalog containing exactly
// app and a stack manifest mirroring the catalog entry.
func newRemoveFixture(t *testing.T, app catalog.App, mutateLock func(*state.StackLock)) *removeTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := removeStackLockForApp(app, stackPath)
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &removeTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// newRemoveFixtureWithCatalogFS builds the fixture against a custom
// catalog engine option (used by the network-reporting tests that need
// the catalog to lack the app) while the stack manifest still names
// app, so the managed-only gate succeeds and only the remaining-network
// catalog read diverges.
func newRemoveFixtureWithCatalogFS(t *testing.T, opt core.Option, app catalog.App) *removeTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, opt)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), removeStackLockForApp(app, stackPath))

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &removeTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// removeStackLockForApp returns a manifest that mirrors the catalog
// entry so the managed-stack resolution succeeds and the Compose
// project drives the named-volume listing.
func removeStackLockForApp(app catalog.App, stackPath string) state.StackLock {
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

// volumeListResult mimics `docker volume ls` newline-delimited output
// so ListProjectNamedVolumes returns the named set.
func volumeListResult(names ...string) docker.CommandResult {
	return docker.CommandResult{Stdout: strings.Join(names, "\n") + "\n"}
}

// scriptRemoveHappyPath wires the fake to answer the full safe-remove
// execution sequence by invocation type so an end-to-end Remove succeeds:
// the planning and post-down named-volume listings both return volumes,
// `docker compose down` succeeds, and the post-down container list is
// empty (every container removed). It records the down invocation so a
// caller can assert the exact ComposeDown shape and the absence of -v.
func scriptRemoveHappyPath(fx *removeTestFixture, volumes ...string) {
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectVolumeListInvocation":
			return volumeListResult(volumes...), nil
		case "docker.projectContainerListInvocation":
			// Empty stdout → no remaining containers → clean removal.
			return docker.CommandResult{}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}
}

// removeContainerInspectStdout builds one `docker inspect`-shaped record
// for a container that survived `docker compose down`, in the strict
// 8-field order internal/docker.parseContainerInspectOutput expects.
func removeContainerInspectStdout(t *testing.T, service, appID string) string {
	t.Helper()

	labels := map[string]string{
		"wdm.managed":                "true",
		"wdm.app":                    appID,
		"com.docker.compose.service": service,
		"com.docker.compose.project": "wdm-" + appID,
	}
	rawLabels, err := json.Marshal(labels)
	require.NoError(t, err)
	return fmt.Sprintf(
		"%q\n%s\n%q\n%t\n%t\n%d\n%q\n%s\n",
		"/wdm-"+appID+"-"+service+"-1", rawLabels, "running", true, false, 0, "", "{}",
	)
}

// TestRemove_HappyPathRunsConfirmDownManifestStatus is the end-to-end
// arc: planning → confirm →
// `docker compose down` → manifest commit recording
// last_successful_operation kind="remove" through the held fd
// #8) → post-down status verify (no containers remain) → populated
// RemoveResult. It proves the step stream, the confirm payload contents,
// the down invocation type, the manifest mutation with prior fields
// preserved, and every RemoveResult field.
func TestRemove_HappyPathRunsConfirmDownManifestStatus(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-happy-app", 18080)
	app.Networks = []catalog.Network{{Name: "wdm_back", Internal: true}}
	fx := newRemoveFixture(t, app, nil)
	scriptRemoveHappyPath(fx, "wdm-remove-happy-app_data")

	manifestBefore, err := os.ReadFile(filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.NoError(t, err, "the safe removal runs to completion")
	require.NotNil(t, res)

	// The frozen remove step stream fires in order.
	assert.Equal(t, []string{
		types.StepRemovePlanning,
		types.StepRemovePlanning,
		types.StepRemoveConfirm,
		types.StepRemoveComposeDown,
		types.StepRemoveLockUpdate,
		types.StepRemoveStatus,
	}, steps)

	// The confirm payload: app name, stack path, the
	// stop/remove-but-keep statement, and the preserved sets.
	require.Len(t, confirmer.calls, 1, "the confirmer is asked exactly once before down")
	payload := confirmer.calls[0]
	assert.Equal(t, "remove_safe", payload.Kind)
	assert.Contains(t, payload.Title, fx.appID)
	assert.Contains(t, payload.Message, "app: "+fx.appID)
	assert.Contains(t, payload.Message, "stack path: "+fx.stackPath)
	assert.Contains(t, payload.Message, "this stops and removes the stack's containers")
	assert.Contains(t, payload.Message, "files and data are kept")
	assert.Contains(t, payload.Message, "keeps "+fx.stackPath)
	assert.Contains(t, payload.Message, "keeps named volume wdm-remove-happy-app_data")
	assert.Contains(t, payload.Message, "keeps docker network wdm_back")

	// The down invocation ran (the safe compose down type — no down-with-v
	// invocation exists in internal/docker at all).
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")
	for _, inv := range fx.fake.invocationTypes {
		assert.NotContains(t, inv, "down -v",
			"no -v down invocation type may ever appear (row 31)")
	}
	// Regression: safe Remove NEVER removes networks (issue #33 only adds
	// delete-time network removal). Remove preserves them and reports them in
	// RemainingNetworks, so no network rm invocation may run.
	assert.NotContains(t, fx.fake.invocationTypes, "docker.removeNetworkInvocation",
		"safe remove must never remove networks; it preserves them")

	// Manifest commit point: last_successful_operation becomes
	// kind="remove" while every other field is preserved byte-equivalent.
	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "remove", committed.LastSuccessfulOperation.Kind)

	before := decodeRemoveStackLock(t, manifestBefore)
	assert.Equal(t, before.AppID, committed.AppID)
	assert.Equal(t, before.ComposeProject, committed.ComposeProject)
	assert.Equal(t, before.TemplateName, committed.TemplateName)
	assert.Equal(t, before.TemplateVersion, committed.TemplateVersion)
	assert.Equal(t, before.CatalogChannel, committed.CatalogChannel)
	assert.Equal(t, before.CatalogVersion, committed.CatalogVersion)
	assert.Equal(t, before.StackPath, committed.StackPath)
	assert.Equal(t, before.ImagePins, committed.ImagePins)
	// Non-empty collections survive the remove commit byte-verbatim: the
	// removal mutates only last_successful_operation, so the
	// provenance a later reinstall or status read relies on is preserved.
	assert.Equal(t, before.LocalPorts, committed.LocalPorts)
	assert.Equal(t, before.GeneratedFields, committed.GeneratedFields)
	assert.Equal(t, before.BackupHistory, committed.BackupHistory)
	assert.Equal(t, before.RecommendedResources, committed.RecommendedResources)
	require.Len(t, committed.GeneratedFields, 1,
		"the seeded generated field survives the remove commit")
	require.Len(t, committed.BackupHistory, 1,
		"the seeded backup-history entry survives the remove commit")
	assert.Equal(t, "install", before.LastSuccessfulOperation.Kind,
		"the pre-remove manifest recorded the install op")

	// Post-down status: no managed container remains → State "removed".
	require.NotNil(t, res.Status)
	assert.Equal(t, "removed", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.Status.AttentionReasons)

	// The structured result carries every field.
	assert.Equal(t, fx.appID, res.AppID)
	assert.Equal(t, fx.stackPath, res.StackPath)
	assert.Equal(t, "wdm-"+fx.appID, res.ComposeProject)
	assert.Equal(t, []string{fx.stackPath}, res.PreservedPaths)
	assert.Equal(t, []string{"wdm-remove-happy-app_data"}, res.RemainingNamedVolumes)
	assert.Equal(t, []string{"wdm_back"}, res.RemainingNetworks)
}

// decodeRemoveStackLock unmarshals a raw.wdm.lock for field-preservation
// assertions.
func decodeRemoveStackLock(t *testing.T, raw []byte) state.StackLock {
	t.Helper()

	var lock state.StackLock
	require.NoError(t, json.Unmarshal(raw, &lock))
	return lock
}

// TestRemove_HoldsAndReleasesRuntimeLock is the lock-posture proof for
// the live full-path removal (PRD §26): the
// global runtime.lock is held — attributed to the "remove" command —
// while the removal runs end-to-end (planning → confirm → down → manifest
// commit → status), the operation completes without error, and the lock
// is released when Remove returns so a later acquisition succeeds.
func TestRemove_HoldsAndReleasesRuntimeLock(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-lock-posture-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-lock-posture-app_data")
	lockPath := filepath.Join(fx.stateDir, "runtime.lock")

	contended := false
	onProgress := func(step string, _ float64, _ string) {
		if step != types.StepRemovePlanning || contended {
			return
		}
		contended = true
		_, err := state.AcquireRuntimeLock(
			t.Context(),
			lockPath,
			state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
		)
		require.Error(t, err, "runtime.lock must be held during safe-remove planning")
		var held *state.LockHeldError
		require.ErrorAs(t, err, &held)
		assert.Equal(t, "remove", held.Holder.Command,
			"runtime.lock metadata must attribute the hold to the remove command")
	}

	_, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, onProgress, &fakeConfirmer{})
	require.NoError(t, err)
	require.True(t, contended, "the planning progress event must have fired")

	probe, err := state.AcquireRuntimeLock(
		t.Context(),
		lockPath,
		state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
	)
	require.NoError(t, err, "Remove must release runtime.lock on return")
	require.NoError(t, probe.Release())
}

// TestRemove_RefusesUnmanagedMissingAndEmptyAppIDs covers the
// managed-only refusals (PRD §9, §10, §19): empty app
// ids, uninstalled apps, and unmanaged directories all surface
// usage-validation errors without any Docker call. An empty app id
// additionally refuses before the first progress event, matching the
// install/update validate-first contract.
func TestRemove_RefusesUnmanagedMissingAndEmptyAppIDs(t *testing.T) {
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

			eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, appFixture("remove-refusal-app", 18080))))
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

			res, err := eng.Remove(t.Context(), types.RemoveRequest{AppID: tt.appID}, onProgress, &fakeConfirmer{})
			require.Error(t, err)
			assert.NotErrorIs(t, err, types.ErrNotImplemented,
				"a managed-only refusal must precede the execution boundary")
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

// TestRemove_CanceledContextEmitsNoEvents proves the validate-first
// contract's ctx arm: a pre-canceled context refuses before the first
// StepRemovePlanning emission and before any Docker call.
func TestRemove_CanceledContextEmitsNoEvents(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-cancel-app", 18080), nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var events int
	res, err := fx.eng.Remove(ctx, types.RemoveRequest{AppID: fx.appID}, func(string, float64, string) {
		events++
	}, &fakeConfirmer{})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context must propagate as context.Canceled")
	assert.Nil(t, res)
	assert.Zero(t, events, "a canceled request must refuse before the first progress event")
	assert.Zero(t, fx.fake.calls, "a canceled request must refuse before any docker command")
}

// TestRemove_RefusesBusyStackWithoutBlocking proves the non-blocking
// read posture: a stack whose .wdm.lock flock is held by another
// operation refuses with ErrCodeRuntimeLockHeld instead of stalling
// behind the writer (PRD §26), before any Docker call.
func TestRemove_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-busy-app", 18080), nil)
	holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
	assert.Zero(t, fx.fake.calls, "a busy stack must refuse before any docker command")
}

// TestRemove_CorruptManifestSurfacesStaleState proves the fail-closed
// posture on corrupt stack state: planning refuses with a wrapped
// types.ErrStaleState before any Docker call.
func TestRemove_CorruptManifestSurfacesStaleState(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-corrupt-app", 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, app.AppID, "{not json")
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Remove(t.Context(), types.RemoveRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, types.ErrStaleState)
	assert.Zero(t, fake.calls, "a corrupt manifest must refuse before any docker command")
}

// TestRemove_ListsNamedVolumesAfterManagedCheck proves the named-volume
// listing runs against the manifest's Compose project
// strictly after the managed-only gate (it is the first Docker call) and
// that the full safe-remove Docker sequence is volume-list (plan) →
// compose-down → container-list (status) → volume-list (post-down
// re-list for the result).
func TestRemove_ListsNamedVolumesAfterManagedCheck(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-volumes-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-volumes-app_db", "wdm-remove-volumes-app_cache")

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"docker.projectVolumeListInvocation",
		"docker.composeDownInvocation",
		"docker.projectContainerListInvocation",
		"docker.projectVolumeListInvocation",
	}, fx.fake.invocationTypes,
		"the named-volume listing is the first Docker call (after the managed gate)")
	// ListProjectNamedVolumes returns sorted unique names (c24).
	assert.Equal(t, []string{
		"wdm-remove-volumes-app_cache",
		"wdm-remove-volumes-app_db",
	}, res.RemainingNamedVolumes,
		"the post-down re-list surfaces the surviving named volumes")
}

// TestRemove_OpportunisticVolumeListFailureDoesNotBlock proves an
// ordinary named-volume inspect failure is opportunistic
// the removal still runs `docker compose down` and commits rather than
// failing the removal of a managed stack, and the unavailable re-list
// surfaces as an empty volume set (never a confident zero on a new
// surface) rather than a fault.
func TestRemove_OpportunisticVolumeListFailureDoesNotBlock(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-vol-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectVolumeListInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err,
		"an opportunistic volume-list failure must not block the removal")
	require.NotNil(t, res)
	assert.Empty(t, res.RemainingNamedVolumes,
		"an unavailable post-down re-list yields an empty set, not a fault")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"the removal still runs down after the opportunistic volume-list failure")
}

// TestRemove_DaemonUnavailableDuringVolumeListPropagates proves the
// hard carve-out: an unreachable daemon (ErrCodeDockerUnavailable) from
// the volume listing propagates unchanged rather than being swallowed
// as an empty list, matching the read-only Status discipline.
func TestRemove_DaemonUnavailableDuringVolumeListPropagates(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-daemon-down-app", 18080), nil)
	fx.fake.runFn = func(int, docker.Invocation) (docker.CommandResult, error) {
		return docker.CommandResult{}, types.NewError(
			types.ErrCodeDockerUnavailable,
			"docker daemon is not reachable",
			"start the docker service and retry",
		)
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, types.ErrNotImplemented,
		"a dead daemon must surface as a typed error, not the execution boundary")
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typed.Code)
}

// TestRemove_ContextCanceledDuringVolumeListPropagates proves the
// other hard carve-out in the opportunistic volume listing: a context
// canceled while the listing runs propagates unchanged rather than
// being swallowed as an empty list (the read-only Status discipline).
func TestRemove_ContextCanceledDuringVolumeListPropagates(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-list-cancel-app", 18080), nil)
	ctx, cancel := context.WithCancel(t.Context())
	fx.fake.runFn = func(int, docker.Invocation) (docker.CommandResult, error) {
		cancel()
		return docker.CommandResult{}, context.Canceled
	}

	res, err := fx.eng.Remove(ctx, types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a cancellation during the volume listing must propagate")
	assert.NotErrorIs(t, err, types.ErrNotImplemented)
	assert.Nil(t, res)
}

// TestRemove_ContextCanceledDuringNetworkPlanningPropagates proves the
// network-planning carve-out: the volume listing succeeds but the
// context is canceled before the catalog read for remaining-network
// reporting runs (loadInstallCatalog checks ctx at entry), so the
// cancellation must propagate as context.Canceled rather than degrading
// to a false "catalog unavailable" WARN, an empty network list, and the
// 15% summary event followed by the execution boundary.
func TestRemove_ContextCanceledDuringNetworkPlanningPropagates(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-net-cancel-app", 18080), nil)
	ctx, cancel := context.WithCancel(t.Context())
	fx.fake.runFn = func(int, docker.Invocation) (docker.CommandResult, error) {
		// The volume listing succeeds, then cancels the request ctx so the
		// subsequent network-planning catalog read sees a canceled context.
		cancel()
		return volumeListResult("wdm-remove-net-cancel-app_data"), nil
	}

	var percents []float64
	res, err := fx.eng.Remove(ctx, types.RemoveRequest{AppID: fx.appID}, func(_ string, percent float64, _ string) {
		percents = append(percents, percent)
	}, &fakeConfirmer{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a cancellation during network planning must propagate")
	assert.NotErrorIs(t, err, types.ErrNotImplemented,
		"a canceled network read must not reach the execution boundary")
	assert.Nil(t, res)
	assert.Equal(t, 1, fx.fake.calls,
		"only the volume listing runs before the cancellation surfaces")
	assert.NotContains(t, percents, float64(15),
		"the 15% plan summary must not emit after the cancellation")
}

// TestRemove_MismatchedStackPathRefusesBeforeDocker proves the
// fail-closed StackPath cross-check: a provided
// req.StackPath that does not match the AppID-resolved managed stack
// refuses with ErrCodeUsageValidation before any Docker call — the
// check sits ahead of the named-volume listing.
func TestRemove_MismatchedStackPathRefusesBeforeDocker(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-stackpath-mismatch-app", 18080), nil)

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{
		AppID:     fx.appID,
		StackPath: filepath.Join(fx.stackBase, "some-other-path"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotErrorIs(t, err, types.ErrNotImplemented,
		"a stack-path mismatch must refuse before the execution boundary")
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "stack path does not match the managed stack")
	assert.Zero(t, fx.fake.calls,
		"the stack-path cross-check must refuse before any docker command")
}

// TestRemove_MatchingStackPathProceeds proves a req.StackPath that
// names the resolved managed stack (here with a trailing separator,
// normalized away by filepath.Clean) passes the cross-check and the
// removal proceeds to completion.
func TestRemove_MatchingStackPathProceeds(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-stackpath-match-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-stackpath-match-app_data")

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{
		AppID:     fx.appID,
		StackPath: fx.stackPath + string(filepath.Separator),
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a matching stack path proceeds to completion")
	require.NotNil(t, res)
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")
}

// TestRemove_OpportunisticUnreadableCatalogOmitsNetworks proves the
// remaining-network read is opportunistic at the catalog-load layer
// too: a catalog FS that cannot be read at all WARN-logs and yields an
// empty network set without blocking removal of a managed stack.
func TestRemove_OpportunisticUnreadableCatalogOmitsNetworks(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-bad-catalog-app", 18080)
	// An empty catalog FS has no stable/catalog.yaml, so loadInstallCatalog
	// fails — the network read must degrade, not fail the removal.
	fx := newRemoveFixtureWithCatalogFS(t, core.WithCatalog(emptyCatalogFS()), app)
	scriptRemoveHappyPath(fx, "wdm-remove-bad-catalog-app_data")

	var messages []string
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, func(_ string, _ float64, msg string) {
		messages = append(messages, msg)
	}, &fakeConfirmer{})
	require.NoError(t, err,
		"an unreadable catalog must not block removal of a managed stack")
	assert.Contains(t, strings.Join(messages, "\n"), "0 network(s) will be preserved")
	assert.Empty(t, res.RemainingNetworks,
		"an unreadable catalog yields an empty remaining-network set in the result")
}

// TestRemove_ReportsCatalogDeclaredNetworksInPlan proves the
// remaining-network source: the
// catalog-declared networks surface on the StepRemovePlanning summary
// count, sorted and deduplicated. The.wdm.lock carries no networks, so
// the catalog is the authoritative source.
func TestRemove_ReportsCatalogDeclaredNetworksInPlan(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-networks-app", 18080)
	app.Networks = []catalog.Network{
		{Name: "wdm_front", Internal: false},
		{Name: "wdm_back", Internal: true},
		{Name: "wdm_front", Internal: false},
	}
	fx := newRemoveFixture(t, app, nil)
	scriptRemoveHappyPath(fx, "wdm-remove-networks-app_data")

	var messages []string
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, func(_ string, _ float64, msg string) {
		messages = append(messages, msg)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	joined := strings.Join(messages, "\n")
	assert.Contains(t, joined, "2 network(s) will be preserved",
		"the deduplicated catalog network set (wdm_front, wdm_back) must surface in the plan summary")
	assert.Contains(t, joined, "1 named volume(s)")
	assert.Equal(t, []string{"wdm_back", "wdm_front"}, res.RemainingNetworks,
		"the sorted, deduplicated catalog networks surface on the result")
}

// TestRemove_OpportunisticMissingCatalogAppOmitsNetworks proves the
// network reporting is opportunistic: a catalog that does not carry the
// app yields an empty remaining-network set without blocking removal of
// a stack wdm already manages (managed = valid lock + labels, not
// catalog availability).
func TestRemove_OpportunisticMissingCatalogAppOmitsNetworks(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-no-catalog-app", 18080)
	// The catalog FS carries a DIFFERENT app, so selectCatalogApp misses.
	other := appFixture("some-other-app", 18081)
	fx := newRemoveFixtureWithCatalogFS(t, core.WithCatalog(catalogFixtureFS(t, other)), app)
	scriptRemoveHappyPath(fx, "wdm-remove-no-catalog-app_data")

	var messages []string
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, func(_ string, _ float64, msg string) {
		messages = append(messages, msg)
	}, &fakeConfirmer{})
	require.NoError(t, err,
		"a missing catalog app must not block removal of a managed stack")

	joined := strings.Join(messages, "\n")
	assert.Contains(t, joined, "0 network(s) will be preserved",
		"an app absent from the catalog yields an empty remaining-network set")
	assert.Empty(t, res.RemainingNetworks)
}

// TestRemove_EmitsOnlyRemovePrefixedStepIDs is the whole-stream guard
// for the row-37 frozen remove progress API: every emitted step ID must
// carry the step_remove_ prefix — no step_install_ or step_update_ leak
// (mirrors the c37 update guard). Pinning the whole stream — not just
// the planning event — catches any future cross-path leak as the
// execution slice lands.
func TestRemove_EmitsOnlyRemovePrefixedStepIDs(t *testing.T) {
	t.Parallel()

	app := appFixture("remove-step-guard-app", 18080)
	app.Networks = []catalog.Network{{Name: "wdm_back", Internal: false}}
	fx := newRemoveFixture(t, app, nil)
	scriptRemoveHappyPath(fx, "wdm-remove-step-guard-app_data")

	var steps []string
	_, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, steps, "the removal must emit progress steps")
	for _, step := range steps {
		assert.True(t, strings.HasPrefix(step, "step_remove_"),
			"the remove progress stream must only carry step_remove_* IDs, got %q", step)
	}
}

// TestRemove_DeclineCancelsWithZeroMutation proves the confirm gate
// a decline maps to ErrCodeUserCanceled, runs no
// `docker compose down`, and leaves the manifest byte-identical — the
// confirm precedes both the down and the manifest write, so a decline
// leaves zero trace (PRD §25 "preserve any completed safe state").
func TestRemove_DeclineCancelsWithZeroMutation(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-decline-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-decline-app_data")

	manifestBefore, err := os.ReadFile(filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, nil
	}}
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code)

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a decline must run no docker compose down")
	manifestAfter, err := os.ReadFile(filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter,
		"a decline must leave the manifest byte-identical")
}

// TestRemove_NilConfirmerRefuses proves a nil confirmer refuses with
// ErrCodeUsageValidation per the pkg/engine contract and runs no down.
func TestRemove_NilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-nil-confirmer-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-nil-confirmer-app_data")

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a nil confirmer must refuse before any down")
}

// TestRemove_ConfirmerErrorPropagatesWrapped proves a confirmer backend
// error propagates wrapped (matching install/update posture) and runs no
// down.
func TestRemove_ConfirmerErrorPropagatesWrapped(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-confirmer-err-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-confirmer-err-app_data")

	sentinel := errors.New("confirmer backend down")
	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, sentinel
	}}
	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "the confirmer error must remain reachable in the chain")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a confirmer error must abort before any down")
}

// TestRemove_DownFailureLeavesManifestUnmarked proves a `docker compose
// down` failure surfaces a typed error, does NOT record the remove
// (the commit point is the later manifest write), and leaves the
// manifest's last_successful_operation as the pre-remove install op.
func TestRemove_DownFailureLeavesManifestUnmarked(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-down-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeDownInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return volumeListResult("wdm-remove-down-fail-app_data"), nil
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"a down failure must abort before the status verify")

	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "install", committed.LastSuccessfulOperation.Kind,
		"a failed down must leave the manifest unmarked (no remove recorded)")
}

// TestRemove_PostDownInspectFailureMarksNeedsAttention proves the
// durability boundary (mirroring verifyUpdateStatus): the manifest is
// committed (kind="remove") and an inspect failure during the post-down
// status verify marks the RemoveResult needs-attention with
// status_check_failed rather than failing the durable removal.
func TestRemove_PostDownInspectFailureMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-status-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectContainerListInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return volumeListResult("wdm-remove-status-fail-app_data"), nil
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a post-down inspect failure must not fail the durable removal")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)

	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "remove", committed.LastSuccessfulOperation.Kind,
		"the removal still committed (kind=remove) despite the status-check trouble")
}

// TestRemove_PostCommitDaemonDownDuringStatusMarksNeedsAttention proves
// the post-commit durability boundary for a DAEMON-DOWN inspect failure:
// unlike the read-only pre-commit Status path (and unlike the pre-commit
// planning volume listing, which propagates ErrCodeDockerUnavailable),
// once the manifest commit is durable a daemon-down status inspect must
// NOT fail the removal — it fuses needs-attention with status_check_failed
// exactly like any other post-commit inspect failure,
// matching verifyUpdateStatus/verifyInstallStatus which carve out ctx.Err
// only. The manifest still records kind="remove" (the commit stuck).
func TestRemove_PostCommitDaemonDownDuringStatusMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-postcommit-daemon-status-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectVolumeListInvocation":
			// Planning volume list succeeds, so the removal reaches and
			// passes the commit point before the status inspect runs.
			return volumeListResult("wdm-remove-postcommit-daemon-status-app_data"), nil
		case "docker.projectContainerListInvocation":
			// The post-down status inspect hits a dead daemon AFTER the
			// commit point — it must fuse needs-attention, not propagate.
			return docker.CommandResult{}, types.NewError(
				types.ErrCodeDockerUnavailable,
				"docker daemon is not reachable",
				"start the docker service and retry",
			)
		default:
			// `docker compose down` succeeds.
			return docker.CommandResult{}, nil
		}
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err,
		"a daemon-down status inspect after the commit point must not fail the durable removal")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)

	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "remove", committed.LastSuccessfulOperation.Kind,
		"the removal still committed (kind=remove) despite the daemon-down status check")
}

// TestRemove_PostCommitDaemonDownDuringVolumeRelistYieldsEmpty proves the
// post-commit re-list (Engine.relistRemoveNamedVolumesPostCommit) has no
// daemon carve-out: with the status inspect clean (zero containers) the
// removal is durably committed, so a daemon-down on the post-down volume
// re-list WARN-logs and yields an EMPTY RemainingNamedVolumes rather than
// failing the removal. The removal SUCCEEDS, the
// status is State "removed", and the manifest records kind="remove".
func TestRemove_PostCommitDaemonDownDuringVolumeRelistYieldsEmpty(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-postcommit-daemon-relist-app", 18080), nil)
	volumeListCalls := 0
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectVolumeListInvocation":
			volumeListCalls++
			if volumeListCalls == 1 {
				// Planning volume list succeeds; the removal reaches and
				// passes the commit point.
				return volumeListResult("wdm-remove-postcommit-daemon-relist-app_data"), nil
			}
			// The post-down re-list hits a dead daemon AFTER the commit
			// point — it must degrade to an empty list, not propagate.
			return docker.CommandResult{}, types.NewError(
				types.ErrCodeDockerUnavailable,
				"docker daemon is not reachable",
				"start the docker service and retry",
			)
		case "docker.projectContainerListInvocation":
			// Empty stdout → zero containers → clean removal, so the
			// status verify succeeds and the re-list is reached.
			return docker.CommandResult{}, nil
		default:
			// `docker compose down` succeeds.
			return docker.CommandResult{}, nil
		}
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err,
		"a daemon-down post-down volume re-list must not fail the durable removal")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.Equal(t, "removed", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.RemainingNamedVolumes,
		"a daemon-down re-list after the commit point yields an empty volume set, not a fault")
	require.Equal(t, 2, volumeListCalls,
		"both the planning list and the post-down re-list ran")

	committed, err := state.ReadStackLock(t.Context(), filepath.Join(fx.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, committed.LastSuccessfulOperation)
	assert.Equal(t, "remove", committed.LastSuccessfulOperation.Kind,
		"the removal still committed (kind=remove) despite the daemon-down re-list")
}

// TestRemove_LingeringContainerMarksNeedsAttention proves that a managed
// container surviving `docker compose down` marks the result
// needs-attention with status_check_failed and surfaces the lingering
// container in the status services — the removal still committed.
func TestRemove_LingeringContainerMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-lingering-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: statusTestContainerID + "\n"}, nil
		case "docker.containerInspectInvocation":
			return docker.CommandResult{Stdout: removeContainerInspectStdout(t, "app", fx.appID)}, nil
		default:
			return volumeListResult("wdm-remove-lingering-app_data"), nil
		}
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)
	require.Len(t, res.Status.Services, 1)
	assert.Equal(t, "app", res.Status.Services[0].Service)
	assert.True(t, res.Status.Services[0].NeedsAttention)
}

// TestRemove_PreservesStackFiles is the row-31 / core
// proof: safe removal stops containers but deletes NO files. After a
// successful remove,.env, the compose file, the .wdm.lock, and the
// .wdm-backups/ directory all remain on disk untouched.
func TestRemove_PreservesStackFiles(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-preserve-app", 18080), nil)
	scriptRemoveHappyPath(fx, "wdm-remove-preserve-app_data")

	// Seed the stack with the files safe removal must preserve.
	envPath := filepath.Join(fx.stackPath, ".env")
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")
	backupDir := filepath.Join(fx.stackPath, ".wdm-backups", "1747752731487293841-update")
	backupFile := filepath.Join(backupDir, "docker-compose.yml")
	composeContent := []byte("services:\n  app:\n    image: x\n")
	require.NoError(t, os.WriteFile(envPath, []byte("SECRET=keepme\n"), 0o600))
	require.NoError(t, os.WriteFile(composePath, composeContent, 0o644))
	require.NoError(t, os.MkdirAll(backupDir, 0o755))
	require.NoError(t, os.WriteFile(backupFile, []byte("services: {}\n"), 0o644))

	_, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	for _, path := range []string{envPath, composePath, backupFile, filepath.Join(fx.stackPath, ".wdm.lock")} {
		_, statErr := os.Stat(path)
		assert.NoErrorf(t, statErr, "safe removal must preserve %s", path)
	}
	// The.env content is untouched, and its secret-mode 0o600 perms survive.
	envAfter, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, "SECRET=keepme\n", string(envAfter),
		"safe removal must not rewrite preserved files")
	envInfo, err := os.Stat(envPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), envInfo.Mode().Perm(),
		"safe removal must preserve the secret-mode .env perms")
	// The compose file content survives byte-for-byte.
	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, composeContent, composeAfter,
		"safe removal must preserve the compose file byte-for-byte")
}

// TestRemove_EmptyComposeProjectRefuses proves the corrupt-manifest
// guard: a managed lock missing its compose project refuses
// with ErrCodeUsageValidation naming the corrupt manifest, before any
// Docker call — closing the c39 reviewer's late-generic-refusal gap.
func TestRemove_EmptyComposeProjectRefuses(t *testing.T) {
	t.Parallel()

	fx := newRemoveFixture(t, appFixture("remove-empty-project-app", 18080), func(lock *state.StackLock) {
		lock.ComposeProject = ""
	})
	fx.fake.runFn = func(int, docker.Invocation) (docker.CommandResult, error) {
		return volumeListResult("should-not-be-listed"), nil
	}

	res, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "missing its compose project")
	assert.Zero(t, fx.fake.calls,
		"a corrupt manifest must refuse before any docker command")
}

// TestRemove_DownArgvHasNoDashV is the core-level real-docker-binary
// proof for: a REAL internal/docker client over a fake
// `docker` binary that logs argv runs `docker compose... down` and the
// down argv NEVER contains -v. Cannot run in parallel: it mutates PATH.
func TestRemove_DownArgvHasNoDashV(t *testing.T) {
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

	fx := newRemoveFixture(t, appFixture("remove-argv-app", 18080), nil)
	core.SetInstallDockerClientFactoryForTest(fx.eng, realDockerClientFactory(t))

	_, err := fx.eng.Remove(t.Context(), types.RemoveRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the removal completes against the fake docker binary")

	logged, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	logText := string(logged)
	t.Logf("captured docker argv:\n%s", logText)

	assert.Contains(t, logText, "[down]",
		"the safe removal must run docker compose down")
	assert.NotContains(t, logText, "[-v]",
		"safe removal must NEVER pass -v to docker compose down (row 31)")
	assert.NotContains(t, logText, "[--volumes]",
		"safe removal must NEVER pass --volumes to docker compose down (row 31)")
}
