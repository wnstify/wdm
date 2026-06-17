package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// deleteTestFixture wires one managed-stack-on-disk scenario for
// Engine.DeleteApp destructive-deletion tests: a stack base under the
// test home, a written.wdm.lock manifest plus seeded stack files, a
// catalog FS carrying the candidate entry (so remaining-network reporting
// has a source), and the fake docker client seam so every Docker call is
// observable. It reuses the remove fixture's lock/catalog helpers because
// the managed-only resolution posture is shared.
type deleteTestFixture struct {
	eng       *core.Engine
	stateDir  string
	stackBase string
	stackPath string
	appID     string
	fake      *fakeDockerClient
}

// newDeleteFixture builds the fixture with a catalog containing exactly
// app and a stack manifest mirroring the catalog entry, then seeds the
// stack directory with the files a real install would write so the
// §19:449 file list and the os.RemoveAll have something to enumerate and
// remove.
func newDeleteFixture(t *testing.T, app catalog.App, mutateLock func(*state.StackLock)) *deleteTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := removeStackLockForApp(app, stackPath)
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)
	seedDeleteStackFiles(t, stackPath)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &deleteTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// seedDeleteStackFiles writes the files a real install commits — the
// compose file, the .env, and two.wdm-backups/ snapshots — so the file
// list is realistic and the snapshot count is exercised. The.wdm.lock
// is written separately by writeStatusStackLock.
func seedDeleteStackFiles(t *testing.T, stackPath string) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte("DB_PASSWORD=super-secret\n"),
		0o600,
	))
	for _, snap := range []string{"1747752731487293841-install", "1747752731487293842-update"} {
		dir := filepath.Join(stackPath, ".wdm-backups", snap)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "docker-compose.yml"),
			[]byte("services: {}\n"),
			0o644,
		))
	}
}

// scriptDeleteHappyPath wires the fake to answer the destructive-delete
// Docker sequence by invocation type: the planning named-volume listing
// returns the surviving volumes and `docker compose down` succeeds. Delete
// has no post-down inspection (DeleteResult carries no Status), so the
// container-list invocation never runs.
func scriptDeleteHappyPath(fx *deleteTestFixture, volumes ...string) {
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectVolumeListInvocation" {
			return volumeListResult(volumes...), nil
		}
		return docker.CommandResult{}, nil
	}
}

// TestDeleteApp_HappyPathConfirmsDownDeletesAndReports is the end-to-end
// arc: planning → confirm (with the §19:449 file list, the §19:450
// permanence warning, and the §19:454 remaining-volume notice) → `docker
// compose down` (no -v per §19:453) → path-contained os.RemoveAll → a
// populated DeleteResult. It proves the step stream, the confirm payload
// contents (including .wdm-backups/ with its snapshot count), and
// every DeleteResult field.
func TestDeleteApp_HappyPathConfirmsDownDeletesAndReports(t *testing.T) {
	t.Parallel()

	app := appFixture("delete-happy-app", 18080)
	app.Networks = []catalog.Network{{Name: "wdm_back", Internal: true}}
	fx := newDeleteFixture(t, app, nil)
	scriptDeleteHappyPath(fx, "wdm-delete-happy-app_data")

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.NoError(t, err, "the destructive deletion runs to completion")
	require.NotNil(t, res)

	// The frozen delete step stream fires in order — only the four
	// step_delete_* IDs (no manifest update, no status verify).
	assert.Equal(t, []string{
		types.StepDeletePlanning,
		types.StepDeletePlanning,
		types.StepDeleteConfirm,
		types.StepDeleteComposeDown,
		types.StepDeleteFiles,
	}, steps)

	// The confirm payload (§19:449-455): the destructive kind, app/stack
	// identity, the permanence warning, the file list, the .wdm-backups
	// snapshot count, and the surviving volumes/networks.
	require.Len(t, confirmer.calls, 1, "the confirmer is asked exactly once before any mutation")
	payload := confirmer.calls[0]
	assert.Equal(t, types.ConfirmationKindDeleteDestructive, payload.Kind)
	assert.Equal(t, "delete_destructive", payload.Kind,
		"the exported kind must be the byte-stable delete_destructive literal")
	assert.Contains(t, payload.Title, fx.appID)
	assert.Contains(t, payload.Message, "app: "+fx.appID)
	assert.Contains(t, payload.Message, "stack path: "+fx.stackPath)
	assert.Contains(t, payload.Message, "compose project: wdm-"+fx.appID)
	assert.Contains(t, strings.ToUpper(payload.Message), "PERMANENT",
		"the permanence warning (§19:450) must be in the payload")
	assert.Contains(t, payload.Message, "cannot be undone")
	assert.Contains(t, payload.Message, "deletes .env")
	assert.Contains(t, payload.Message, "deletes docker-compose.yml")
	assert.Contains(t, payload.Message, "deletes .wdm.lock")
	assert.Contains(t, payload.Message, "deletes .wdm-backups"+string(filepath.Separator))
	assert.Contains(t, payload.Message, "deletes 2 backup snapshot(s) under .wdm-backups/",
		"the §19 list must name .wdm-backups/ with its snapshot count")
	assert.Contains(t, payload.Message, "named volumes are NOT deleted")
	assert.Contains(t, payload.Message, "keeps named volume wdm-delete-happy-app_data")
	assert.Contains(t, payload.Message, "keeps docker network wdm_back")
	// The.env content (a secret) must NEVER reach the confirm payload.
	assert.NotContains(t, payload.Message, "super-secret",
		"the confirmation payload must list filenames only, never .env content")

	// The down invocation ran (the safe compose down type — no -v).
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")
	for _, inv := range fx.fake.invocationTypes {
		assert.NotContains(t, inv, "down -v",
			"no -v down invocation type may ever appear (§19:453)")
	}
	// Delete runs no post-down container inspection — there is no Status.
	assert.NotContains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"destructive delete performs no post-down status verification")

	// The stack directory is gone — files, .env, .wdm.lock, and backups all.
	_, statErr := os.Stat(fx.stackPath)
	assert.True(t, os.IsNotExist(statErr),
		"the stack directory must be removed entirely")

	// The structured result carries every field.
	assert.Equal(t, fx.appID, res.AppID)
	assert.Contains(t, res.DeletedPaths, fx.stackPath,
		"DeletedPaths must include the stack directory")
	assert.Contains(t, res.DeletedPaths, ".env")
	assert.Contains(t, res.DeletedPaths, ".wdm.lock")
	assert.Contains(t, res.DeletedPaths, ".wdm-backups"+string(filepath.Separator),
		"the backups directory is listed as a deleted path with a trailing separator")
	assert.Equal(t, []string{"wdm-delete-happy-app_data"}, res.RemainingNamedVolumes)
	assert.Equal(t, []string{"wdm_back"}, res.RemainingNetworks)
}

// TestDeleteApp_HoldsAndReleasesRuntimeLock is the lock-posture proof: the
// global runtime.lock is held — attributed to the "delete" command —
// while the deletion runs end-to-end, and is released when DeleteApp
// returns so a later acquisition succeeds.
func TestDeleteApp_HoldsAndReleasesRuntimeLock(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-lock-posture-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-lock-posture-app_data")
	lockPath := filepath.Join(fx.stateDir, "runtime.lock")

	contended := false
	onProgress := func(step string, _ float64, _ string) {
		if step != types.StepDeletePlanning || contended {
			return
		}
		contended = true
		_, err := state.AcquireRuntimeLock(
			t.Context(),
			lockPath,
			state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
		)
		require.Error(t, err, "runtime.lock must be held during destructive-delete planning")
		var held *state.LockHeldError
		require.ErrorAs(t, err, &held)
		assert.Equal(t, "delete", held.Holder.Command,
			"runtime.lock metadata must attribute the hold to the delete command")
	}

	_, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, onProgress, &fakeConfirmer{})
	require.NoError(t, err)
	require.True(t, contended, "the planning progress event must have fired")

	probe, err := state.AcquireRuntimeLock(
		t.Context(),
		lockPath,
		state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
	)
	require.NoError(t, err, "DeleteApp must release runtime.lock on return")
	require.NoError(t, probe.Release())
}

// TestDeleteApp_ClosedEngineRefuses proves a closed engine refuses with
// ErrClosed before any validation or I/O.
func TestDeleteApp_ClosedEngineRefuses(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	res, err := eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            "uptime-kuma",
		ConfirmationName: "uptime-kuma",
	}, nil, nil)
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, res)
}

// TestDeleteApp_ValidateFirstRefusalsEmitNoEvents covers the
// validate-first refusal table (the install/update/remove
// zero-events-on-invalid contract): an empty app id, a
// DeleteNamedVolumes:true request, and a ConfirmationName mismatch all
// refuse with ErrCodeUsageValidation BEFORE the first progress event,
// before the runtime.lock, and before any Docker call.
func TestDeleteApp_ValidateFirstRefusalsEmitNoEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		req          types.DeleteRequest
		wantContains string
	}{
		{
			name:         "empty app id",
			req:          types.DeleteRequest{AppID: "", ConfirmationName: ""},
			wantContains: "app id is required",
		},
		{
			name: "delete named volumes refused",
			req: types.DeleteRequest{
				AppID:              "delete-validate-app",
				ConfirmationName:   "delete-validate-app",
				DeleteNamedVolumes: true,
			},
			wantContains: "named-volume deletion is not supported",
		},
		{
			name: "confirmation name mismatch",
			req: types.DeleteRequest{
				AppID:            "delete-validate-app",
				ConfirmationName: "wrong-name",
			},
			wantContains: "confirmation name does not match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newDeleteFixture(t, appFixture("delete-validate-app", 18080), nil)
			lockPath := filepath.Join(fx.stateDir, "runtime.lock")

			var events int
			res, err := fx.eng.DeleteApp(t.Context(), tt.req, func(string, float64, string) {
				events++
			}, &fakeConfirmer{})
			require.Error(t, err)
			assert.Nil(t, res)
			assert.NotErrorIs(t, err, types.ErrNotImplemented,
				"a validate-first refusal must precede the execution boundary")
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, tt.wantContains)
			assert.Zero(t, events, "request validation must refuse before the first progress event")
			assert.Zero(t, fx.fake.calls, "validation must refuse before any docker command")
			_, statErr := os.Stat(lockPath)
			assert.True(t, os.IsNotExist(statErr),
				"a validate-first refusal must not acquire the runtime.lock")
			// Nothing on disk was touched.
			_, statErr = os.Stat(fx.stackPath)
			assert.NoError(t, statErr, "a validate-first refusal must not delete anything")
		})
	}
}

// TestDeleteApp_DeleteNamedVolumesHintNamesSemantics proves the
// DeleteNamedVolumes:true refusal hint explains the §19:453 contract
// (v1 never deletes named volumes) so the user understands why.
func TestDeleteApp_DeleteNamedVolumesHintNamesSemantics(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-vol-flag-app", 18080), nil)

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:              fx.appID,
		ConfirmationName:   fx.appID,
		DeleteNamedVolumes: true,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
	assert.Contains(t, typed.Hint, "never deletes named volumes",
		"the hint must name the §19:453 v1 contract")
	assert.Zero(t, fx.fake.calls)
}

// TestDeleteApp_TypedNameMismatchTouchesNothing proves the the invariant
// engine-side re-verification: a ConfirmationName that does not equal
// AppID refuses with zero progress events, zero docker calls, and nothing
// touched on disk — the belt-and-suspenders gate that runs independent of
// the Confirmer (here a confirmer that would happily accept).
func TestDeleteApp_TypedNameMismatchTouchesNothing(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-name-mismatch-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-name-mismatch-app_data")

	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		t.Fatal("the confirmer must never be consulted on a typed-name mismatch")
		return true, nil
	}}

	var events int
	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID + "-typo",
	}, func(string, float64, string) {
		events++
	}, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmation name does not match")
	assert.Zero(t, events)
	assert.Zero(t, fx.fake.calls)
	assert.Empty(t, confirmer.calls, "the typed-name gate precedes the confirmer entirely")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a typed-name mismatch must delete nothing")
}

// TestDeleteApp_DeclineLeavesEverythingIntact proves the confirm gate: a
// decline maps to ErrCodeUserCanceled, runs no `docker compose down`,
// deletes no files, and leaves the .wdm.lock byte-identical — the confirm
// precedes both the down and the file deletion, so a decline leaves zero
// trace.
func TestDeleteApp_DeclineLeavesEverythingIntact(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-decline-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-decline-app_data")

	lockPath := filepath.Join(fx.stackPath, ".wdm.lock")
	manifestBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, nil
	}}
	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code)

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a decline must run no docker compose down")
	manifestAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter,
		"a decline must leave the manifest byte-identical")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a decline must leave the stack directory intact")
}

// TestDeleteApp_NilConfirmerRefuses proves a nil confirmer refuses with
// ErrCodeUsageValidation per the pkg/engine contract and runs no down,
// deletes nothing.
func TestDeleteApp_NilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-nil-confirmer-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-nil-confirmer-app_data")

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a nil confirmer must refuse before any down")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a nil confirmer must delete nothing")
}

// TestDeleteApp_ConfirmerErrorPropagatesWrapped proves a confirmer
// backend error propagates wrapped and deletes nothing.
func TestDeleteApp_ConfirmerErrorPropagatesWrapped(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-confirmer-err-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-confirmer-err-app_data")

	sentinel := errors.New("confirmer backend down")
	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, sentinel
	}}
	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "the confirmer error must remain reachable in the chain")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"a confirmer error must abort before any down")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a confirmer error must delete nothing")
}

// TestDeleteApp_DownFailureLeavesFilesIntact proves a `docker compose
// down` failure surfaces a typed error and leaves the stack files
// byte-identical — the file deletion is the later step, so a down failure
// removes nothing (no restore owed because nothing was rewritten).
func TestDeleteApp_DownFailureLeavesFilesIntact(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-down-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeDownInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return volumeListResult("wdm-delete-down-fail-app_data"), nil
	}

	lockPath := filepath.Join(fx.stackPath, ".wdm.lock")
	manifestBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)

	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a down failure must leave the stack directory intact")
	manifestAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter,
		"a down failure must leave the manifest byte-identical")
}

// TestDeleteApp_HelperUnavailableStopsBeforeComposeDownAndFiles proves the
// local-only cleanup-helper preflight runs before any delete mutation. If the
// digest-pinned helper image is not already present locally, DeleteApp must
// refuse with a typed generic error, run no compose down, and leave the stack
// byte-identical for a later retry after the operator pre-pulls the helper.
func TestDeleteApp_HelperUnavailableStopsBeforeComposeDownAndFiles(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-helper-missing-app", 18080), nil)
	extraPath := filepath.Join(fx.stackPath, "docker", "uptime-kuma", "db", "ib_buffer_pool")
	require.NoError(t, os.MkdirAll(filepath.Dir(extraPath), 0o755))
	require.NoError(t, os.WriteFile(extraPath, []byte("container-owned db metadata"), 0o644))

	lockPath := filepath.Join(fx.stackPath, ".wdm.lock")
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")
	envPath := filepath.Join(fx.stackPath, ".env")
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	composeBefore, err := os.ReadFile(composePath)
	require.NoError(t, err)
	envBefore, err := os.ReadFile(envPath)
	require.NoError(t, err)
	extraBefore, err := os.ReadFile(extraPath)
	require.NoError(t, err)

	helperMissing := errors.New("No such image: docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662")
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectVolumeListInvocation":
			return volumeListResult("wdm-delete-helper-missing-app_data"), nil
		case "docker.imageDigestInspectInvocation":
			return docker.CommandResult{Stderr: helperMissing.Error(), ExitCode: 1}, helperMissing
		default:
			return docker.CommandResult{}, nil
		}
	}

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeGeneric, typedErr.Code)
	assert.Contains(t, typedErr.Message, "delete cleanup helper image is unavailable")
	assert.Contains(t, typedErr.Hint, "docker pull docker.io/library/busybox@sha256")

	assert.Contains(t, fx.fake.invocationTypes, "docker.imageDigestInspectInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"helper preflight must refuse before compose down mutates containers")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.bindMountCleanupInvocation",
		"helper preflight failure must not run the cleanup helper")

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	composeAfter, err := os.ReadFile(composePath)
	require.NoError(t, err)
	envAfter, err := os.ReadFile(envPath)
	require.NoError(t, err)
	extraAfter, err := os.ReadFile(extraPath)
	require.NoError(t, err)
	assert.Equal(t, lockBefore, lockAfter)
	assert.Equal(t, composeBefore, composeAfter)
	assert.Equal(t, envBefore, envAfter)
	assert.Equal(t, extraBefore, extraAfter,
		"helper preflight failure must not partially delete bind-mounted files")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "the stack directory must remain for retry")
}

// TestDeleteApp_PermissionDeniedBindFilesUsePathContainedDockerCleanup
// reproduces the VM smoke failure from uptime-kuma: compose down succeeds,
// but container-owned bind-mounted DB files under the managed stack
// directory leave a parent directory the normal user cannot mutate, so the
// first os.RemoveAll gets EACCES. DeleteApp should recover by running one
// bounded Docker cleanup over the already containment-proven stack path,
// then remove the now-empty stack directory without deleting named volumes.
func TestDeleteApp_PermissionDeniedBindFilesUsePathContainedDockerCleanup(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are required to reproduce the bind-file deletion failure")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can delete the protected fixture directly, so the fallback would not be exercised")
	}

	fx := newDeleteFixture(t, appFixture("delete-bind-perms-app", 18080), nil)
	protectedDir := filepath.Join(fx.stackPath, "docker", "uptime-kuma", "db")
	protectedFile := filepath.Join(protectedDir, "ib_buffer_pool")
	require.NoError(t, os.MkdirAll(protectedDir, 0o755))
	require.NoError(t, os.WriteFile(protectedFile, []byte("container-owned db metadata"), 0o644))
	require.NoError(t, os.Chmod(protectedDir, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(protectedDir, 0o755)
		_ = os.RemoveAll(filepath.Join(fx.stackPath, "docker"))
	})

	var helperCalls int
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectVolumeListInvocation":
			return volumeListResult("wdm-delete-bind-perms-app_data"), nil
		case "docker.composeDownInvocation":
			return docker.CommandResult{}, nil
		case "docker.bindMountCleanupInvocation":
			helperCalls++
			require.NoError(t, os.Chmod(protectedDir, 0o755))
			require.NoError(t, os.RemoveAll(filepath.Join(fx.stackPath, "docker")))
			return docker.CommandResult{}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, 1, helperCalls,
		"permission-denied bind files should trigger exactly one Docker cleanup helper")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")
	assert.Contains(t, fx.fake.invocationTypes, "docker.bindMountCleanupInvocation")
	assert.Equal(t, []string{"wdm-delete-bind-perms-app_data"}, res.RemainingNamedVolumes)
	_, statErr := os.Stat(fx.stackPath)
	assert.True(t, os.IsNotExist(statErr), "the stack directory should be removed after helper cleanup")
}

// TestDeleteApp_SymlinkStackDirRefusesWithNothingDeleted is the
// end-to-end §19:452 containment proof for the direct-symlink case: a
// managed stack directory that is itself a symlink pointing outside the
// engine's stack base refuses the deletion with a typed usage-validation
// error, and the symlink target's sentinel file survives untouched. NOTE
// (finding for the reviewer): the resolution layer's
// installStackPathExists already rejects a symlinked stack directory
// ("stack path is a symlink", ErrCodeUsageValidation) BEFORE the
// containment check or any Docker call runs — a stronger, earlier guard.
// This test pins the user-visible refusal-and-zero-deletion outcome; the
// resolveDeleteTarget containment logic for the cases resolution does not
// pre-empt is unit-tested directly in
// TestResolveDeleteTarget_ContainmentArms.
func TestDeleteApp_SymlinkStackDirRefusesWithNothingDeleted(t *testing.T) {
	t.Parallel()

	// An "outside" directory with a sentinel file the deletion must NOT
	// touch, placed under a sibling temp root not under the stack base.
	outside := t.TempDir()
	sentinelPath := filepath.Join(outside, "do-not-delete.txt")
	require.NoError(t, os.WriteFile(sentinelPath, []byte("keep me"), 0o644))

	app := appFixture("delete-symlink-escape-app", 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	// Make the stack path a SYMLINK to the outside directory, then write
	// the managed manifest through the symlink so the only thing standing
	// between the deletion and the outside dir is the containment defense.
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	require.NoError(t, os.Symlink(outside, stackPath))
	lock := removeStackLockForApp(app, stackPath)
	rawLock, err := json.MarshalIndent(lock, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".wdm.lock"), rawLock, 0o600))

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            app.AppID,
		ConfirmationName: app.AppID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.Zero(t, fake.calls,
		"a symlinked stack directory refuses before any docker command")

	// The escape target and its sentinel file survive untouched.
	content, err := os.ReadFile(sentinelPath)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(content),
		"the symlink-escape target must NOT be deleted (§19:452)")
}

// TestResolveDeleteTarget_ContainmentArms drives the §19:452 containment
// check directly through the export seam so every arm is exercised
// independent of the resolution layer's earlier symlinked-stack refusal:
// a happy in-base path resolves; an escaping path, the base itself, a
// shallow path, and a symlink that resolves outside the base all refuse
// with ErrCodeUsageValidation; and a symlinked ANCESTOR that still
// resolves inside the base is permitted (the resolved real path is what
// matters). The function never deletes anything — it only adjudicates.
func TestResolveDeleteTarget_ContainmentArms(t *testing.T) {
	t.Parallel()

	t.Run("in-base path resolves to its real target", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		stack := filepath.Join(base, "app")
		require.NoError(t, os.MkdirAll(stack, 0o755))

		resolved, err := core.ResolveDeleteTargetForTest(base, stack)
		require.NoError(t, err)
		realStack, err := filepath.EvalSymlinks(stack)
		require.NoError(t, err)
		assert.Equal(t, realStack, resolved,
			"a contained path resolves to its symlink-free real target")
	})

	t.Run("symlinked stack escaping the base refuses", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		outside := t.TempDir()
		stack := filepath.Join(base, "app")
		require.NoError(t, os.Symlink(outside, stack))

		_, err := core.ResolveDeleteTargetForTest(base, stack)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "outside the managed stack base")
	})

	t.Run("the stack base itself refuses", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()

		_, err := core.ResolveDeleteTargetForTest(base, base)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "stack base itself")
	})

	t.Run("symlinked ancestor still inside the base is permitted", func(t *testing.T) {
		t.Parallel()

		// realBase/link -> realBase/inner, with the stack under the link.
		// The resolved path is realBase/inner/app, still inside the base.
		realBase := t.TempDir()
		inner := filepath.Join(realBase, "inner")
		require.NoError(t, os.MkdirAll(inner, 0o755))
		link := filepath.Join(realBase, "link")
		require.NoError(t, os.Symlink(inner, link))
		stack := filepath.Join(link, "app")
		require.NoError(t, os.MkdirAll(filepath.Join(inner, "app"), 0o755))

		resolved, err := core.ResolveDeleteTargetForTest(realBase, stack)
		require.NoError(t, err,
			"a symlinked ancestor that still resolves inside the base is permitted")
		realInnerApp, err := filepath.EvalSymlinks(filepath.Join(inner, "app"))
		require.NoError(t, err)
		assert.Equal(t, realInnerApp, resolved,
			"the resolved real path (symlink-free) is what is permitted")
	})
}

// TestIsSuspiciouslyShallowPath pins the lexical shallow-path backstop:
// the filesystem root and any single top-level component are shallow
// (refused), while a path at least two levels deep is not (a legitimate
// managed stack lives at e.g. /home/<user>/docker/<app>). This is the
// §19:452 defense-in-depth arm guarding against a stack base that ever
// resolves near root.
func TestIsSuspiciouslyShallowPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path    string
		shallow bool
	}{
		{"/", true},
		{"/etc", true},
		{"/home", true},
		{"/private", true},
		{"/home/user", false},
		{"/home/user/docker/app", false},
		{"/private/var/folders/xx/stacks/app", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.shallow, core.IsSuspiciouslyShallowPathForTest(tt.path),
				"isSuspiciouslyShallowPath(%q)", tt.path)
		})
	}
}

// TestDeleteApp_RefusesUnmanagedMissingAndCorrupt covers the managed-only
// refusals shared with Remove (PRD §9, §10, §19):
// uninstalled apps, unmanaged directories, and corrupt manifests all
// surface typed errors without any Docker call or deletion.
func TestDeleteApp_RefusesUnmanagedMissingAndCorrupt(t *testing.T) {
	t.Parallel()

	t.Run("app not installed", func(t *testing.T) {
		t.Parallel()

		fx := newDeleteFixture(t, appFixture("delete-refusal-app", 18080), nil)
		res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
			AppID:            "ghost-app",
			ConfirmationName: "ghost-app",
		}, nil, &fakeConfirmer{})
		require.Error(t, err)
		assert.Nil(t, res)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "app is not installed")
		assert.Zero(t, fx.fake.calls)
	})

	t.Run("unmanaged directory", func(t *testing.T) {
		t.Parallel()

		app := appFixture("delete-unmanaged-app", 18080)
		eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
		stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
		require.NoError(t, os.MkdirAll(filepath.Join(stackBase, "unmanaged-app"), 0o755))
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		res, err := eng.DeleteApp(t.Context(), types.DeleteRequest{
			AppID:            "unmanaged-app",
			ConfirmationName: "unmanaged-app",
		}, nil, &fakeConfirmer{})
		require.Error(t, err)
		assert.Nil(t, res)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "not managed by wdm")
		assert.Zero(t, fake.calls)
	})

	t.Run("corrupt manifest", func(t *testing.T) {
		t.Parallel()

		app := appFixture("delete-corrupt-app", 18080)
		eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
		stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
		writeCoreStackFixture(t, stackBase, app.AppID, "{not json")
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		res, err := eng.DeleteApp(t.Context(), types.DeleteRequest{
			AppID:            app.AppID,
			ConfirmationName: app.AppID,
		}, nil, &fakeConfirmer{})
		require.Error(t, err)
		assert.Nil(t, res)
		require.ErrorIs(t, err, types.ErrStaleState)
		assert.Zero(t, fake.calls)
	})

	t.Run("empty compose project", func(t *testing.T) {
		t.Parallel()

		fx := newDeleteFixture(t, appFixture("delete-empty-project-app", 18080), func(lock *state.StackLock) {
			lock.ComposeProject = ""
		})
		res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
			AppID:            fx.appID,
			ConfirmationName: fx.appID,
		}, nil, &fakeConfirmer{})
		require.Error(t, err)
		assert.Nil(t, res)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "missing its compose project")
		assert.Zero(t, fx.fake.calls,
			"a corrupt manifest must refuse before any docker command")
	})
}

// TestDeleteApp_RefusesBusyStackWithoutBlocking proves the non-blocking
// read posture: a stack whose .wdm.lock flock is held by another operation
// refuses with ErrCodeRuntimeLockHeld instead of stalling, before any
// Docker call.
func TestDeleteApp_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-busy-app", 18080), nil)
	holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
	assert.Zero(t, fx.fake.calls, "a busy stack must refuse before any docker command")
}

// TestDeleteApp_CanceledContextEmitsNoEvents proves the validate-first
// ctx arm: a pre-canceled context refuses before the first
// StepDeletePlanning emission and before any Docker call. DeleteApp checks
// ctx first — immediately after the closed-engine guard and before the
// request-shape validation — so the canceled error propagates as
// context.Canceled.
func TestDeleteApp_CanceledContextEmitsNoEvents(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-cancel-app", 18080), nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var events int
	res, err := fx.eng.DeleteApp(ctx, types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, func(string, float64, string) {
		events++
	}, &fakeConfirmer{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, res)
	assert.Zero(t, events, "a canceled request must refuse before the first progress event")
	assert.Zero(t, fx.fake.calls, "a canceled request must refuse before any docker command")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a canceled request must delete nothing")
}

// TestDeleteApp_CanceledContextBeatsInvalidRequest proves the cross-verb
// ctx-first contract (matching remove/restart, which reject a canceled ctx
// inside acquireRuntimeLock before any request-shape check): a pre-canceled
// context paired with an invalid request (empty AppID) returns
// context.Canceled, NOT the usage-validation refusal — so the ctx check
// beats every request-shape refusal. Nothing is emitted, no Docker call is
// made, and nothing on disk is touched.
func TestDeleteApp_CanceledContextBeatsInvalidRequest(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-cancel-invalid-app", 18080), nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var events int
	// The request is invalid (empty AppID, mismatched ConfirmationName), so a
	// live context would refuse with ErrCodeUsageValidation. The canceled
	// context must take precedence.
	res, err := fx.eng.DeleteApp(ctx, types.DeleteRequest{
		AppID:            "",
		ConfirmationName: "delete-cancel-invalid-app",
	}, func(string, float64, string) {
		events++
	}, &fakeConfirmer{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled,
		"a canceled context must beat the request-shape refusal")
	var typed *types.Error
	assert.NotErrorAs(t, err, &typed,
		"the ctx error must precede the usage-validation refusal: a canceled "+
			"context surfaces bare context.Canceled, not a typed usage error")
	assert.Nil(t, res)
	assert.Zero(t, events, "a canceled request must refuse before the first progress event")
	assert.Zero(t, fx.fake.calls, "a canceled request must refuse before any docker command")
	lockPath := filepath.Join(fx.stateDir, "runtime.lock")
	_, statErr := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(statErr),
		"a canceled request must not acquire the runtime.lock")
	_, statErr = os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a canceled request must delete nothing")
}

// TestDeleteApp_MismatchedStackPathRefusesBeforeDocker proves the
// fail-closed StackPath cross-check (, the Remove c39
// precedent): a provided req.StackPath that does not match the
// AppID-resolved managed stack refuses with ErrCodeUsageValidation before
// any Docker call and deletes nothing.
func TestDeleteApp_MismatchedStackPathRefusesBeforeDocker(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-stackpath-mismatch-app", 18080), nil)

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
		StackPath:        filepath.Join(fx.stackBase, "some-other-path"),
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "stack path does not match the managed stack")
	assert.Zero(t, fx.fake.calls,
		"the stack-path cross-check must refuse before any docker command")
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a stack-path mismatch must delete nothing")
}

// TestDeleteApp_DaemonUnavailableDuringVolumeListPropagates proves the
// hard carve-out shared with Remove: an unreachable daemon
// (ErrCodeDockerUnavailable) from the planning volume listing propagates
// unchanged rather than being swallowed as an empty list. The deletion
// refuses before the confirm, so nothing is deleted.
func TestDeleteApp_DaemonUnavailableDuringVolumeListPropagates(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-daemon-down-app", 18080), nil)
	fx.fake.runFn = func(int, docker.Invocation) (docker.CommandResult, error) {
		return docker.CommandResult{}, types.NewError(
			types.ErrCodeDockerUnavailable,
			"docker daemon is not reachable",
			"start the docker service and retry",
		)
	}

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typed.Code)
	_, statErr := os.Stat(fx.stackPath)
	assert.NoError(t, statErr, "a daemon-down planning failure must delete nothing")
}

// TestDeleteApp_OpportunisticVolumeListFailureDoesNotBlock proves an
// ordinary named-volume inspect failure is opportunistic (mirroring the
// Remove posture): the deletion still proceeds and reports an empty
// surviving-volume set rather than failing.
func TestDeleteApp_OpportunisticVolumeListFailureDoesNotBlock(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-vol-fail-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectVolumeListInvocation" {
			return docker.CommandResult{Stderr: "boom", ExitCode: 1}, errors.New("exit status 1")
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err,
		"an opportunistic volume-list failure must not block the deletion")
	require.NotNil(t, res)
	assert.Empty(t, res.RemainingNamedVolumes,
		"an unavailable volume listing yields an empty surviving set")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeDownInvocation",
		"the deletion still runs down after the opportunistic volume-list failure")
	_, statErr := os.Stat(fx.stackPath)
	assert.True(t, os.IsNotExist(statErr), "the deletion still removed the stack directory")
}

// TestDeleteApp_EmitsOnlyDeletePrefixedStepIDs is the whole-stream guard:
// every emitted step ID must carry the step_delete_ prefix — no
// step_install_, step_update_, or step_remove_ leak (the c39/c40 guard
// precedent).
func TestDeleteApp_EmitsOnlyDeletePrefixedStepIDs(t *testing.T) {
	t.Parallel()

	app := appFixture("delete-step-guard-app", 18080)
	app.Networks = []catalog.Network{{Name: "wdm_back", Internal: false}}
	fx := newDeleteFixture(t, app, nil)
	scriptDeleteHappyPath(fx, "wdm-delete-step-guard-app_data")

	var steps []string
	_, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, steps, "the deletion must emit progress steps")
	for _, step := range steps {
		assert.True(t, strings.HasPrefix(step, "step_delete_"),
			"the delete progress stream must only carry step_delete_* IDs, got %q", step)
	}
}

// TestDeleteApp_BackupListerFailureDegradesCount proves the opportunistic
// .wdm-backups/ snapshot count: when the lister cannot read the backup
// directory (here a .wdm-backups that is a FILE, not a directory, so
// ListConfigBackups errors), the plan degrades to a count of 0 rather
// than failing, and the deletion still proceeds. The file list still names
// the backups path so the user sees it go.
func TestDeleteApp_BackupListerFailureDegradesCount(t *testing.T) {
	t.Parallel()

	fx := newDeleteFixture(t, appFixture("delete-bad-backups-app", 18080), nil)
	scriptDeleteHappyPath(fx, "wdm-delete-bad-backups-app_data")

	// Replace the seeded.wdm-backups/ directory with a FILE so the lister
	// errors (a symlinked/non-directory backup root is a hard lister fail).
	backupRoot := filepath.Join(fx.stackPath, ".wdm-backups")
	require.NoError(t, os.RemoveAll(backupRoot))
	require.NoError(t, os.WriteFile(backupRoot, []byte("not a directory"), 0o644))

	confirmer := &fakeConfirmer{}
	res, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, confirmer)
	require.NoError(t, err, "a backup-lister failure must not block the deletion")
	require.NotNil(t, res)

	require.Len(t, confirmer.calls, 1)
	assert.Contains(t, confirmer.calls[0].Message, "deletes 0 backup snapshot(s)",
		"a lister failure degrades the count to 0, not a fault")
	_, statErr := os.Stat(fx.stackPath)
	assert.True(t, os.IsNotExist(statErr), "the deletion still removed the stack directory")
}

// TestDeleteApp_DownArgvHasNoDashV is the core-level real-docker-binary
// proof for §19:453: a REAL internal/docker client over a fake `docker`
// binary that logs argv runs `docker compose... down` and the down argv
// NEVER contains -v. Cannot run in parallel: it mutates PATH.
func TestDeleteApp_DownArgvHasNoDashV(t *testing.T) {
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

	fx := newDeleteFixture(t, appFixture("delete-argv-app", 18080), nil)
	core.SetInstallDockerClientFactoryForTest(fx.eng, realDockerClientFactory(t))

	_, err := fx.eng.DeleteApp(t.Context(), types.DeleteRequest{
		AppID:            fx.appID,
		ConfirmationName: fx.appID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the deletion completes against the fake docker binary")

	logged, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	logText := string(logged)
	t.Logf("captured docker argv:\n%s", logText)

	assert.Contains(t, logText, "[down]",
		"the destructive deletion must run docker compose down")
	assert.NotContains(t, logText, "[-v]",
		"destructive deletion must NEVER pass -v to docker compose down (§19:453)")
	assert.NotContains(t, logText, "[--volumes]",
		"destructive deletion must NEVER pass --volumes to docker compose down (§19:453)")
}
