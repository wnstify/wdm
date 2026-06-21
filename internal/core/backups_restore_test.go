package core_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// restoreFixture wires one fully-managed stack on disk for
// Engine.RestoreBackup tests: a .wdm.lock manifest, an on-disk
// docker-compose.yml /.env at the "pre-restore" content, a REAL
// state.CreateConfigBackup snapshot of that content, and the fake docker
// client seam (every Docker call observable; managed-only refusals make
// none, the happy path makes exactly the post-restore status inspect). The
// caller then overwrites the on-disk config so a successful restore is
// observable: the files must change BACK to the snapshot bytes.
type restoreFixture struct {
	eng        *core.Engine
	stateDir   string
	stackBase  string
	stackPath  string
	appID      string
	snapshotID string
	fake       *fakeDockerClient

	composePath string
	envPath     string
}

const (
	// restoreSnapshotCompose is the compose content captured in the snapshot
	// and restored back; restorePostCompose is what the test overwrites the
	// on-disk file with so the restore is observable.
	restoreSnapshotCompose = "services:\n  app:\n    image: docker.io/example/app:1.0.0\n"
	restorePostCompose     = "services:\n  app:\n    image: docker.io/example/app:9.9.9-broken\n"
	restoreSnapshotEnv     = "SITE_NAME=Original Site\nDB_PASSWORD=keep-me\n"
	restorePostEnv         = "SITE_NAME=Edited Site\nDB_PASSWORD=changed\n"
)

// newRestoreFixture builds the managed stack at the snapshot content,
// snapshots it, then leaves the on-disk config at the snapshot content (the
// caller mutates it before RestoreBackup so the restore is observable).
// mutateLock may adjust the manifest before it is written.
func newRestoreFixture(t *testing.T, mutateLock func(*state.StackLock)) *restoreFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	appID := "restore-app"
	stackPath := filepath.Join(stackBase, appID)
	// RestoreBackup never binds ports, so the lock port is fixed at 18080 to
	// match restartContainerInspectStdout's published-port line (so the
	// post-restore status fusion reports no port_mismatch on the happy path).
	const hostPort = 18080

	lock := statusStackLock(appID, stackPath, []int{hostPort})
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	composePath := filepath.Join(stackPath, "docker-compose.yml")
	envPath := filepath.Join(stackPath, ".env")
	require.NoError(t, os.WriteFile(composePath, []byte(restoreSnapshotCompose), 0o644))
	require.NoError(t, os.WriteFile(envPath, []byte(restoreSnapshotEnv), 0o600))

	// The REAL state writer creates a snapshot of the current config
	// (.wdm.lock, docker-compose.yml,.env), exactly the artifact
	// RestoreBackup restores from.
	snapshotPath, err := state.CreateConfigBackup(stackPath, "update", nil)
	require.NoError(t, err)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &restoreFixture{
		eng:         eng,
		stateDir:    stateDir,
		stackBase:   stackBase,
		stackPath:   stackPath,
		appID:       appID,
		snapshotID:  filepath.Base(snapshotPath),
		fake:        fake,
		composePath: composePath,
		envPath:     envPath,
	}
}

// overwriteConfigPostSnapshot replaces the on-disk config with the
// post-snapshot content so a successful restore is observable: the files
// must revert to the snapshot bytes.
func (fx *restoreFixture) overwriteConfigPostSnapshot(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(fx.composePath, []byte(restorePostCompose), 0o644))
	require.NoError(t, os.WriteFile(fx.envPath, []byte(restorePostEnv), 0o600))
}

// snapshotStackTree returns a path->bytes map of every regular file under
// the stack dir, so before/after comparisons can prove a decline or a
// refusal left the on-disk config byte-identical.
func snapshotStackTree(t *testing.T, stackPath string) map[string][]byte {
	t.Helper()
	tree := map[string][]byte{}
	require.NoError(t, filepath.Walk(stackPath, func(path string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		if info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		rel, relErr := filepath.Rel(stackPath, path)
		require.NoError(t, relErr)
		tree[rel] = data
		return nil
	}))
	return tree
}

// restoreRunningInspect wires the fake so the post-restore status verify
// reports a single healthy running "app" service (matching statusStackLock's
// image pin), so the result status is State "running" with no attention
// reasons. It reuses restart's inspect-output shape.
func restoreRunningInspect(fx *restoreFixture, t *testing.T) {
	t.Helper()
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.projectContainerListInvocation":
			return docker.CommandResult{Stdout: statusTestContainerID + "\n"}, nil
		case "docker.containerInspectInvocation":
			return docker.CommandResult{Stdout: restartContainerInspectStdout(t, "app", fx.appID)}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}
}

// TestRestoreBackup_HappyPathRestoresConfigAndReturnsRecreateNextAction is
// the end-to-end arc: planning → confirm (before the file rewrite) → shared
// config-restore → post-restore status verify → populated
// RestoreBackupResult. It proves the frozen step stream, the safe confirm
// payload, that the on-disk config reverts to the snapshot bytes (the shared
// restore actually ran), the byte-equal boundary notice, the recreate
// next-action copy, and the fused running status — plus that
// the runtime.lock is released (a second RestoreBackup succeeds).
func TestRestoreBackup_HappyPathRestoresConfigAndReturnsRecreateNextAction(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	restoreRunningInspect(fx, t)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.NoError(t, err, "the config restore runs to completion")
	require.NotNil(t, res)

	// The frozen restore step stream fires in order (planning twice — the
	// initial emission plus the plan summary — then confirm, execute, status).
	assert.Equal(t, []string{
		types.StepRestorePlanning,
		types.StepRestorePlanning,
		types.StepRestoreConfirm,
		types.StepRestoreExecute,
		types.StepRestoreStatus,
	}, steps)

	// The confirm payload: the SAFE restore_config kind, app name, stack
	// path, the config-restore boundary, the runtime-keeps-old-config
	// consequence, and the per-file rewrite lines. NEVER "rollback".
	require.Len(t, confirmer.calls, 1, "the confirmer is asked exactly once before the rewrite")
	payload := confirmer.calls[0]
	assert.Equal(t, "restore_config", payload.Kind)
	assert.Contains(t, payload.Title, fx.appID)
	assert.Contains(t, payload.Message, "app: "+fx.appID)
	assert.Contains(t, payload.Message, "stack path: "+fx.stackPath)
	assert.Contains(t, payload.Message, "snapshot: "+fx.snapshotID)
	assert.Contains(t, payload.Message, "config restore")
	assert.Contains(t, payload.Message, "does NOT restore app data, databases")
	assert.Contains(t, payload.Message, "keep the old config")
	assert.Contains(t, payload.Message, "rewrites config file docker-compose.yml")
	assert.NotContains(t, strings.ToLower(payload.Message), "rollback")

	// The shared restore actually ran: the on-disk config reverts to the
	// snapshot bytes (the pre-restore overwrite is gone).
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, restoreSnapshotCompose, string(composeAfter),
		"docker-compose.yml is restored to the snapshot bytes")
	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, restoreSnapshotEnv, string(envAfter),
		".env is restored to the snapshot bytes")
	// The restored.env keeps secret-file mode (RestoreConfigBackup preserves
	// the snapshot's permission bits).
	assert.Equal(t, os.FileMode(0o600), fileModePerm(t, fx.envPath),
		".env must keep 0o600 after restore")

	// The result: app/snapshot identity, restored files, the byte-equal
	// boundary notice, the recreate next-action, and the fused running status.
	assert.Equal(t, fx.appID, res.AppID)
	assert.Equal(t, fx.snapshotID, res.SnapshotID)
	assert.ElementsMatch(t, []string{".env", ".wdm.lock", "docker-compose.yml"}, res.RestoredFiles)
	assert.Equal(t, state.ConfigRestoreBoundaryNotice, res.BoundaryNotice,
		"BoundaryNotice must be byte-equal to state.ConfigRestoreBoundaryNotice")
	assert.NotEmpty(t, res.NextAction)
	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.Status.AttentionReasons)

	// No "rollback" anywhere in the user-facing result.
	assert.NotContains(t, strings.ToLower(res.NextAction), "rollback")
	assert.NotContains(t, strings.ToLower(res.BoundaryNotice), "rollback")

	// The runtime.lock was released: a second restore on the same engine
	// succeeds.
	res2, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "RestoreBackup must release runtime.lock so a second call succeeds")
	require.NotNil(t, res2)
}

// TestRestoreBackup_NextActionNamesRecreatePathAndStatesOldConfig is the
// the invariant exit-criterion test: the next-action names the recreate path
// (NOT plain restart), and the copy states the running containers still use
// the old config until applied. It also proves the next-action would apply
// the restored config: the named recreate path is the apply pipeline behind
// `wdm apps update`, which DOES re-read the on-disk compose (unlike `docker
// compose restart`), and the restored compose differs from the pre-restore
// one — so recreating consumes the restored bytes.
func TestRestoreBackup_NextActionNamesRecreatePathAndStatesOldConfig(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	restoreRunningInspect(fx, t)

	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	lowerNext := strings.ToLower(res.NextAction)
	// The next-action names the recreate path (the apply pipeline / update),
	// NOT plain restart.
	assert.Contains(t, lowerNext, "recreate",
		"the next-action must name the recreate path (decision #47)")
	assert.Contains(t, lowerNext, "update",
		"the recreate path is surfaced as `wdm apps update`")
	assert.NotContains(t, lowerNext, "restart",
		"the next-action must NOT be plain restart (decision #47)")
	// The copy states the runtime still uses the old config until applied.
	assert.Contains(t, lowerNext, "old config",
		"the copy must state the running containers still use the old config")

	// The restored config differs from the pre-restore (overwritten) bytes,
	// so the named recreate path — which re-reads the on-disk compose —
	// would deploy the restored config, while a plain restart would not.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, restoreSnapshotCompose, string(composeAfter))
	assert.NotEqual(t, restorePostCompose, string(composeAfter),
		"the restore changed the on-disk compose, so a recreate applies different bytes than a restart's no-op")
}

// TestRestoreBackup_DeclineLeavesConfigUntouched proves a declined
// confirmation maps to ErrCodeUserCanceled with ZERO on-disk change: the
// confirm runs before the rewrite, so the (post-snapshot, overwritten)
// config stays exactly as it was and no file reverts to the snapshot.
func TestRestoreBackup_DeclineLeavesConfigUntouched(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)

	before := snapshotStackTree(t, fx.stackPath)

	confirmer := decliningConfirmer()
	var steps []string
	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)

	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code)
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback")

	// The confirm fired but the execute step never did, and zero docker
	// calls ran (no status verify after a decline).
	require.Len(t, confirmer.calls, 1)
	assert.NotContains(t, steps, types.StepRestoreExecute)
	assert.Zero(t, fx.fake.calls, "a decline runs no docker command")

	// On-disk config is byte-identical to before — nothing reverted.
	after := snapshotStackTree(t, fx.stackPath)
	assert.Equal(t, before, after, "a declined restore must leave every file byte-identical")
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, restorePostCompose, string(composeAfter),
		"the overwritten compose is untouched after a decline")

	// The runtime.lock and per-stack flock released: a second restore (now
	// accepting) runs to completion.
	restoreRunningInspect(fx, t)
	_, err = fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the locks must be released after a decline")
}

// TestRestoreBackup_NilConfirmerRefusesWithUsageValidation proves a nil
// confirmer refuses with ErrCodeUsageValidation (the install/restart posture)
// before any file is rewritten, leaving the config byte-identical.
func TestRestoreBackup_NilConfirmerRefusesWithUsageValidation(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	before := snapshotStackTree(t, fx.stackPath)

	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required before config restore")

	after := snapshotStackTree(t, fx.stackPath)
	assert.Equal(t, before, after, "a nil-confirmer refusal must leave every file byte-identical")
	assert.Zero(t, fx.fake.calls, "a nil-confirmer refusal runs no docker command")
}

// TestRestoreBackup_ConfirmerErrorPropagatesWrapped proves a confirmer
// backend error propagates wrapped (the sentinel reachable via errors.Is,
// distinct from a clean decline) and leaves the config byte-identical.
func TestRestoreBackup_ConfirmerErrorPropagatesWrapped(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	before := snapshotStackTree(t, fx.stackPath)

	sentinel := errors.New("confirm backend down")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return true, sentinel
		},
	}
	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "a confirmer error must propagate through the wrap chain")

	after := snapshotStackTree(t, fx.stackPath)
	assert.Equal(t, before, after, "a confirmer-error refusal must leave every file byte-identical")
}

// TestRestoreBackup_PostRestoreStatusFailureMarksNeedsAttention proves the
// verify-posture needs-attention arm: the config restore SUCCEEDS (the files
// revert to the snapshot bytes) but the post-restore container inspect fails,
// so the result is marked needs-attention with status_check_failed rather
// than failing the restore.
func TestRestoreBackup_PostRestoreStatusFailureMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.projectContainerListInvocation" {
			return docker.CommandResult{}, errors.New("daemon unreachable")
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.NoError(t, err, "a post-restore status failure must NOT fail the restore")
	require.NotNil(t, res)

	// The restore still happened: the config reverted to the snapshot bytes.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, restoreSnapshotCompose, string(composeAfter))

	require.NotNil(t, res.Status)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.True(t, res.Status.NeedsAttention)
	assert.Contains(t, res.Status.AttentionReasons, "status_check_failed")
}

// TestRestoreBackup_RefusesUnmanagedMissingAndEmptyInputs covers the
// validate-first refusal matrix: empty app id, empty snapshot id, an
// uninstalled app, and an unmanaged directory all refuse with
// ErrCodeUsageValidation before any docker call, and the empty-input arms
// refuse before the first progress event.
func TestRestoreBackup_RefusesUnmanagedMissingAndEmptyInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		appID          string
		snapshotID     string
		setup          func(t *testing.T, stackBase string)
		wantContains   string
		wantZeroEvents bool
	}{
		{
			name:           "empty app id",
			appID:          "",
			snapshotID:     "1000000000000000000-update",
			wantContains:   "app id is required",
			wantZeroEvents: true,
		},
		{
			name:           "empty snapshot id",
			appID:          "restore-refusal-app",
			snapshotID:     "",
			setup:          func(t *testing.T, stackBase string) { writeRestoreRefusalStack(t, stackBase) },
			wantContains:   "snapshot id is required",
			wantZeroEvents: true,
		},
		{
			name:         "app not installed",
			appID:        "ghost-app",
			snapshotID:   "1000000000000000000-update",
			wantContains: "app is not installed",
		},
		{
			name:       "unmanaged directory",
			appID:      "unmanaged-app",
			snapshotID: "1000000000000000000-update",
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

			eng, stateDir := newTestEngine(t)
			stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
			if tt.setup != nil {
				tt.setup(t, stackBase)
			}
			fake := &fakeDockerClient{}
			core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

			var events int
			res, err := eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
				AppID:      tt.appID,
				SnapshotID: tt.snapshotID,
			}, func(string, float64, string) { events++ }, &fakeConfirmer{})

			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, tt.wantContains)
			assert.Zero(t, fake.calls, "refusals must happen before any docker call")
			if tt.wantZeroEvents {
				assert.Zero(t, events, "request validation must refuse before the first progress event")
			}
		})
	}
}

// writeRestoreRefusalStack writes a minimal managed stack for the
// empty-snapshot-id refusal arm so resolution would succeed if the empty
// snapshot id did not refuse first.
func writeRestoreRefusalStack(t *testing.T, stackBase string) {
	t.Helper()
	appID := "restore-refusal-app"
	stackPath := filepath.Join(stackBase, appID)
	lock := statusStackLock(appID, stackPath, []int{0})
	writeStatusStackLock(t, stackBase, appID, lock)
}

// TestRestoreBackup_UnknownAndTraversalSnapshotRefuse proves snapshot
// validation: an unknown snapshot id and a traversal-shaped id both refuse
// with ErrCodeUsageValidation before any prompt or write — the prompt never
// fires and the config stays byte-identical.
func TestRestoreBackup_UnknownAndTraversalSnapshotRefuse(t *testing.T) {
	t.Parallel()

	for _, snapshotID := range []string{
		"9999999999999999999-update", // well-formed but absent
		"../escape",                  // traversal-shaped
		"not-a-snapshot",             // malformed
	} {
		t.Run(snapshotID, func(t *testing.T) {
			t.Parallel()

			fx := newRestoreFixture(t, nil)
			fx.overwriteConfigPostSnapshot(t)
			before := snapshotStackTree(t, fx.stackPath)

			confirmer := &fakeConfirmer{}
			res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
				AppID:      fx.appID,
				SnapshotID: snapshotID,
			}, nil, confirmer)

			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, "backup snapshot was not found")
			assert.Empty(t, confirmer.calls, "an unknown snapshot must refuse before the prompt")
			assert.Zero(t, fx.fake.calls, "an unknown snapshot must refuse before any docker call")

			after := snapshotStackTree(t, fx.stackPath)
			assert.Equal(t, before, after, "a snapshot refusal must leave every file byte-identical")
		})
	}
}

// TestRestoreBackup_MismatchedStackPathRefusesBeforeWrite proves the
// fail-closed StackPath cross-check: a provided req.StackPath that does not
// match the AppID-resolved managed stack refuses with ErrCodeUsageValidation
// before any prompt or write.
func TestRestoreBackup_MismatchedStackPathRefusesBeforeWrite(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	before := snapshotStackTree(t, fx.stackPath)

	confirmer := &fakeConfirmer{}
	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
		StackPath:  filepath.Join(fx.stackBase, "some-other-path"),
	}, nil, confirmer)

	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "stack path does not match the managed stack")
	assert.Empty(t, confirmer.calls, "the cross-check must refuse before the prompt")

	after := snapshotStackTree(t, fx.stackPath)
	assert.Equal(t, before, after, "the cross-check refusal must leave every file byte-identical")
}

// TestRestoreBackup_RefusesBusyStackWithoutBlocking proves the non-blocking
// read posture: a stack whose .wdm.lock flock is held by another operation
// refuses with ErrCodeRuntimeLockHeld instead of stalling, before any prompt
// or write.
func TestRestoreBackup_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
	assert.Zero(t, fx.fake.calls, "a busy stack must refuse before any docker call")
}

// TestRestoreBackup_CorruptManifestSurfacesStaleState proves the fail-closed
// posture on corrupt stack state: a non-JSON.wdm.lock refuses with a wrapped
// types.ErrStaleState before any docker call.
func TestRestoreBackup_CorruptManifestSurfacesStaleState(t *testing.T) {
	t.Parallel()

	eng, stateDir := newTestEngine(t)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, "corrupt-app", "{not json")
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      "corrupt-app",
		SnapshotID: "1000000000000000000-update",
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, types.ErrStaleState)
	assert.Zero(t, fake.calls, "a corrupt manifest must refuse before any docker call")
}

// TestRestoreBackup_EmptyComposeProjectRefuses proves the corrupt-manifest
// guard: a managed lock missing its compose project refuses with
// ErrCodeUsageValidation before any prompt or docker call.
func TestRestoreBackup_EmptyComposeProjectRefuses(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, func(lock *state.StackLock) {
		lock.ComposeProject = ""
	})
	fx.overwriteConfigPostSnapshot(t)

	res, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "missing its compose project")
	assert.Zero(t, fx.fake.calls, "a corrupt manifest must refuse before any docker call")
}

// TestRestoreBackup_CanceledContextEmitsNoEvents proves the validate-first
// contract's ctx arm: a pre-canceled context refuses before the first
// StepRestorePlanning emission and before any docker call.
func TestRestoreBackup_CanceledContextEmitsNoEvents(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var events int
	res, err := fx.eng.RestoreBackup(ctx, types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, func(string, float64, string) { events++ }, &fakeConfirmer{})

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "a canceled context must propagate as context.Canceled")
	assert.Nil(t, res)
	assert.Zero(t, events, "a canceled request must refuse before the first progress event")
	assert.Zero(t, fx.fake.calls, "a canceled request must refuse before any docker call")
}

// TestRestoreBackup_OnlyRestoreStepIDsAppearInStream is the whole-stream
// guard: the full live path emits ONLY step_restore_* IDs (the frozen set has
// exactly four — planning, confirm, execute, status) — no step_update_*,
// step_install_*, step_remove_*, or step_restart_* IDs ever leak in.
func TestRestoreBackup_OnlyRestoreStepIDsAppearInStream(t *testing.T) {
	t.Parallel()

	fx := newRestoreFixture(t, nil)
	fx.overwriteConfigPostSnapshot(t)
	restoreRunningInspect(fx, t)

	var steps []string
	_, err := fx.eng.RestoreBackup(t.Context(), types.RestoreBackupRequest{
		AppID:      fx.appID,
		SnapshotID: fx.snapshotID,
	}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, steps)
	allowed := map[string]struct{}{
		types.StepRestorePlanning: {},
		types.StepRestoreConfirm:  {},
		types.StepRestoreExecute:  {},
		types.StepRestoreStatus:   {},
	}
	for _, step := range steps {
		_, ok := allowed[step]
		assert.Truef(t, ok, "unexpected step id %q on the restore stream", step)
		assert.Truef(t, strings.HasPrefix(step, "step_restore_"),
			"every restore step id must be step_restore_*, got %q", step)
	}
}

// TestRestoreBackup_SharedRestorePathInvokedByBothCallers is the decision
// #42a exit-criterion proof: RestoreBackup AND the failed-update automatic
// restore (restoreUpdateOnFailure) call the SAME config-restore code path.
// It pins the property at the source level: both function bodies invoke the
// shared restoreConfigSnapshot helper, and that helper is the ONLY caller of
// state.RestoreConfigBackup in production code. An AST proof is the strongest
// cheap pin here — it survives refactors that a runtime counter would miss
// (it cannot be satisfied by a second, divergent call site), and it directly
// encodes "one restore path, two entry points" without a production test
// seam. The two runtime arcs (the live RestoreBackup happy path here and the
// unmodified restore-boundary suite) prove each entry point works.
func TestRestoreBackup_SharedRestorePathInvokedByBothCallers(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	callers := map[string]bool{
		"restoreUpdateOnFailure": false, // the failed-update auto-restore
		"executeRestoreBackup":   false, // the RestoreBackup entry point
	}
	// state.RestoreConfigBackup must be reachable from production ONLY through
	// the shared helper — no other function may call it directly.
	stateRestoreCallers := map[string]struct{}{}
	sharedHelperName := "restoreConfigSnapshot"
	sharedHelperCallsStateRestore := false

	coreDir := coreSourceDir(t)
	entries, err := os.ReadDir(coreDir)
	require.NoError(t, err)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(coreDir, name), nil, 0)
		require.NoError(t, parseErr)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnName := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callee := call.Fun.(type) {
				case *ast.Ident:
					if callee.Name == sharedHelperName {
						if _, tracked := callers[fnName]; tracked {
							callers[fnName] = true
						}
					}
				case *ast.SelectorExpr:
					pkg, pkgOK := callee.X.(*ast.Ident)
					if pkgOK && pkg.Name == "state" && callee.Sel.Name == "RestoreConfigBackup" {
						stateRestoreCallers[fnName] = struct{}{}
						if fnName == sharedHelperName {
							sharedHelperCallsStateRestore = true
						}
					}
				}
				return true
			})
		}
	}

	for name, invoked := range callers {
		assert.Truef(t, invoked,
			"%s must invoke the shared %s helper (decision #42a)", name, sharedHelperName)
	}
	assert.True(t, sharedHelperCallsStateRestore,
		"the shared %s helper must call state.RestoreConfigBackup", sharedHelperName)
	// The shared helper is the ONLY production caller of
	// state.RestoreConfigBackup: any other caller would be a divergent
	// restore path.
	require.Len(t, stateRestoreCallers, 1,
		"state.RestoreConfigBackup must be called from exactly one production function")
	_, onlyShared := stateRestoreCallers[sharedHelperName]
	assert.True(t, onlyShared,
		"the sole production caller of state.RestoreConfigBackup must be %s", sharedHelperName)
}

// coreSourceDir locates the internal/core production source directory from
// the test working directory so the AST proof reads the real package files.
func coreSourceDir(t *testing.T) string {
	t.Helper()
	// Tests run with the package directory as the working directory, so the
	// current directory holds the production.go files.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	// Sanity: the directory must contain the files under test.
	for _, must := range []string{"backups.go", "update_deploy.go"} {
		_, statErr := os.Stat(filepath.Join(cwd, must))
		require.NoError(t, statErr, "expected %s in the core source dir %s", must, cwd)
	}
	return cwd
}

// TestRestoreBackup_NoRollbackWordingInProductionStringLiterals is the
// the invariant grep posture at the source level: no STRING LITERAL in the
// RestoreBackup production source (backups.go) contains "rollback" — every
// user-facing string (confirmation payload, next-action, boundary notice,
// error messages, progress messages) says "config restore" and states what
// is and is not restored. Doc comments are deliberately exempt: they
// legitimately contrast config restore AGAINST rollback to explain the
// boundary (mirroring update_deploy.go's "rollback never appears in any
// user-facing string" comment), so the check inspects string literals only,
// not comment text.
func TestRestoreBackup_NoRollbackWordingInProductionStringLiterals(t *testing.T) {
	t.Parallel()

	coreDir := coreSourceDir(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(coreDir, "backups.go"), nil, 0)
	require.NoError(t, err)

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		assert.NotContainsf(t, strings.ToLower(lit.Value), "rollback",
			"no string literal in backups.go may contain \"rollback\" (decision #42b): %s", lit.Value)
		return true
	})
}
