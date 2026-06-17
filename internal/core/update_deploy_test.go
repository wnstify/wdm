package core_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// decliningConfirmer returns a confirmer that records its payloads and
// declines every prompt.
func decliningConfirmer() *fakeConfirmer {
	return &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
}

// assertManifestUnchanged proves the .wdm.lock at path is byte-identical
// to before, i.e. the commit point was never reached.
func assertManifestUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		".wdm.lock must stay byte-identical until the commit point")
}

// assertUpdateRestoredPreviousConfig proves the sad path
// restored the pre-update config files the newUpdateApplyFixture seeds:
// the compose pinned at 1.0.0, the .env carrying the OLD regenerable=true
// API_TOKEN plus the reused regenerable=false DB_PASSWORD and the
// non-secret SITE_NAME, and the sidecar at its pre-update content — all
// byte-identical to before the rewrite. It asserts the NEW (2.0.0)
// artifacts are gone, i.e. the rewrite was undone.
func assertUpdateRestoredPreviousConfig(t *testing.T, fx *updateApplyFixture) {
	t.Helper()

	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, "services:\n  app:\n    image: docker.io/example/app:1.0.0\n", string(composeAfter),
		"the compose is restored to the pre-update bytes")
	assert.NotContains(t, string(composeAfter), "docker.io/example/app:2.0.0",
		"the rewritten candidate tag is gone after restore")

	envAfter, err := os.ReadFile(fx.envPath)
	require.NoError(t, err)
	assert.Equal(t, renderEnvFixture(map[string]string{
		"DB_PASSWORD": dbPasswordInstallValue,
		"API_TOKEN":   apiTokenInstallValue,
		"SITE_NAME":   siteNameInstallValue,
	}), string(envAfter), "the .env is restored to the pre-update bytes")
	assert.NotContains(t, string(envAfter), regeneratedAPIToken,
		"the regenerated secret is gone after restore")

	sidecarAfter, err := os.ReadFile(fx.sidecarPath)
	require.NoError(t, err)
	assert.Equal(t, "echo old\n", string(sidecarAfter), "the sidecar is restored to the pre-update bytes")

	// .env keeps secret-file mode after the restore (RestoreConfigBackup
	// preserves the snapshot's permission bits).
	assert.Equal(t, os.FileMode(0o600), fileModePerm(t, fx.envPath),
		".env must keep 0o600 after restore")
}

// TestUpdate_RecreateConfirmDeclineRestoresPreviousConfig proves the
// decline sad path: a decline on a
// non-database recreate prompt keeps types.ErrCodeUserCanceled, runs no
// Docker mutation, and — because the rewrite already exposed the new
// bytes — restores the step-3 snapshot byte-for-byte so the previous
// compose /.env / sidecar are back on disk; the manifest is uncommitted,
// the backup survives, and the error hint reports the config restore
// without ever saying "rollback".
func TestUpdate_RecreateConfirmDeclineRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("recreate-decline-app"), false, nil, nil)
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	confirmer := decliningConfirmer()
	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)

	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code,
		"a declined recreate keeps user-canceled through the restore wrapper")
	assert.Contains(t, typed.Hint, fx.backupRoot, "the decline hint names the restored backup path")
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback",
		"no user-facing string may say rollback")

	// Exactly one confirmation (the recreate) fired, and no Docker
	// mutation ran past it.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, "update_deploy", confirmer.calls[0].Kind)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	assert.NotContains(t, steps, types.StepUpdatePull)
	assert.Contains(t, steps, types.StepUpdateConfigRestore, "the decline emits the config-restore step")

	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
	assert.DirExists(t, fx.backupRoot, "the pre-update backup survives a decline")

	// Both locks released: a second apply runs to completion.
	_, err = fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the per-stack flock must be released after a decline")
}

// TestUpdate_RecreateNilConfirmerRefusesAndRestores proves a nil
// confirmer refuses the recreate with types.ErrCodeUsageValidation (the
// install posture) AFTER the backup and rewrite have run, runs no Docker
// mutation, and — because the rewrite already exposed the new bytes — the
// sad path restores the previous config. The usage-validation
// code is preserved through the restore wrapper (a clean refusal keeps its
// code per the error matrix) and the original message survives in the
// cause; the manifest stays uncommitted and the backup survives.
func TestUpdate_RecreateNilConfirmerRefusesAndRestores(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("recreate-nil-app"), false, nil, nil)
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required before deployment")
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback")

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
	assert.DirExists(t, fx.backupRoot, "the backup is taken before the recreate confirmation")
}

// TestUpdate_RecreateConfirmerErrorPropagatesAndRestores proves a
// confirmer error at the recreate gate propagates wrapped (the sentinel
// stays reachable via errors.Is, distinct from a clean decline), runs no
// Docker mutation, and — because the rewrite already exposed the new bytes
// — the sad path restores the previous config. A confirmer error
// is not a clean refusal code, so the surfaced error is ErrCodeGeneric
// while the sentinel remains in the cause chain.
func TestUpdate_RecreateConfirmerErrorPropagatesAndRestores(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("recreate-err-app"), false, nil, nil)
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	sentinel := errors.New("confirm backend down")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return true, sentinel
		},
	}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "a confirmer error must propagate through the wrap chain")
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code, "a confirmer error is re-coded to generic after restore")

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
}

// TestUpdate_ValidateFailureRestoresPreviousConfig proves PRD §20 step 9
// plus the sad path: a failed `docker compose config` (the first
// Docker call on the apply path) aborts before the recreate confirmation
// and before any pull or recreate, then restores the previous config
// byte-for-byte. The original docker-layer fault stays reachable via
// errors.Is while the surfaced error is ErrCodeGeneric with a hint naming
// the restored backup; the manifest is uncommitted and the backup survives.
func TestUpdate_ValidateFailureRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	composeErr := errors.New("compose config rejected")
	fx := newUpdateApplyFixture(t, updateApplyApp("validate-fail-app"), false, nil, nil)
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 1 { // compose config validation
			return docker.CommandResult{}, composeErr
		}
		return docker.CommandResult{}, nil
	}
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, composeErr)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.Contains(t, typed.Hint, fx.backupRoot, "the failure hint names the restored backup path")

	assert.Equal(t, 1, fx.fake.calls, "validation is the only Docker call before the abort")
	assert.Empty(t, confirmer.calls, "validation failure aborts before the recreate confirmation")
	assert.NotContains(t, steps, types.StepUpdatePull)
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
	assert.DirExists(t, fx.backupRoot, "the backup precedes validation and survives")
}

// TestUpdate_DeployFailureRestoresPreviousConfig is the:357
// core proof: an induced `docker compose up -d --force-recreate` failure
// restores the previous Compose, env, lock, AND sidecar byte-identical to
// before the update (from the step-3 snapshot), the snapshot survives, and
// the failure surfaces as *types.Error{Code: ErrCodeGeneric} with a Hint
// naming the restored backup path — while the original docker-layer fault
// stays reachable via errors.Is. No `docker compose down` of any kind runs
// (PRD §19) and both locks are released.
func TestUpdate_DeployFailureRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	upErr := errors.New("compose up exploded")
	fx := newUpdateApplyFixture(t, updateApplyApp("deploy-fail-app"), false, nil, nil)
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 3 { // up -d --force-recreate (validate=1, pull=2, up=3)
			return docker.CommandResult{}, upErr
		}
		return docker.CommandResult{}, nil
	}
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)

	// 357 — induced failure surfaces ErrCodeGeneric with a Hint naming
	// the restored backup path; the original fault stays reachable.
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code, "an induced deploy failure surfaces ErrCodeGeneric")
	assert.Contains(t, typed.Hint, fx.backupRoot, "the failure hint names the restored backup path")
	require.ErrorIs(t, err, upErr, "the original docker-layer fault stays reachable via errors.Is")
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback",
		"no user-facing string may say rollback")
	assert.Contains(t, steps, types.StepUpdateConfigRestore, "the failure emits the config-restore step")

	// The deploy fault stops the pipeline before digest capture and the
	// manifest commit, and never runs any compose down.
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeDownInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.imageDigestInspectInvocation")

	// 357 — the previous compose, env, lock, and sidecar are byte-identical.
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
	assert.DirExists(t, fx.backupRoot, "the pre-update backup survives a deploy failure")

	// Both locks released: a fresh stack-lock acquisition succeeds.
	handle, err := state.AcquireStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err, "the per-stack flock must be released after a deploy failure")
	require.NoError(t, handle.Release())
}

// TestUpdate_PullFailureRestoresPreviousConfig proves the same
// sad-path boundary one step earlier: a `docker compose pull` failure
// aborts before the recreate and the manifest commit, then restores the
// previous config byte-for-byte. The pull fault stays reachable via
// errors.Is while the surfaced error is ErrCodeGeneric with a hint naming
// the restored backup; the manifest is uncommitted and the backup survives.
func TestUpdate_PullFailureRestoresPreviousConfig(t *testing.T) {
	t.Parallel()

	pullErr := errors.New("compose pull failed")
	fx := newUpdateApplyFixture(t, updateApplyApp("pull-fail-app"), false, nil, nil)
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 2 { // pull (validate=1, pull=2)
			return docker.CommandResult{}, pullErr
		}
		return docker.CommandResult{}, nil
	}
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, pullErr)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	assert.Contains(t, typed.Hint, fx.backupRoot, "the failure hint names the restored backup path")

	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a pull failure aborts before the recreate")
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
	assert.DirExists(t, fx.backupRoot)
}

// TestUpdate_StatusFailureAfterCommitMarksNeedsAttention proves the
// post-commit-point posture: once the manifest is durable, a failed status inspection never fails the update
// — it marks the result needs-attention with status_check_failed. The
// committed manifest stays in place.
func TestUpdate_StatusFailureAfterCommitMarksNeedsAttention(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("status-fail-app"), false, nil, nil)
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 5 { // status container-id listing fails after the commit
			return docker.CommandResult{ExitCode: 1}, errors.New("docker daemon hiccup")
		}
		return docker.CommandResult{}, nil
	}

	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err, "a post-commit status failure must not fail the durable update")
	require.NotNil(t, res)
	require.NotNil(t, res.Status)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)

	// The manifest committed the update despite the status trouble.
	lock, err := state.ReadStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err)
	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "update", lock.LastSuccessfulOperation.Kind)
	assert.Equal(t, "2.0.0", lock.ImagePins[0].Tag)

	// Post-commit faults NEVER restore: the
	// rewritten compose stays live and no config-restore step is emitted.
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Contains(t, string(composeAfter), "docker.io/example/app:2.0.0",
		"a post-commit status failure must not restore the previous config")
	assert.NotContains(t, steps, types.StepUpdateConfigRestore,
		"no config-restore runs after the commit point")
}

// TestUpdate_RestoreFailureFailsClosedWithBothCauses is the
// fail-closed proof: when the restore ITSELF fails after a
// deploy failure, the update fails closed — the returned error is
// ErrCodeGeneric, BOTH the original deploy fault and the restore failure
// are reachable via errors.Is, and the message/hint convey the uncertain
// state and name where the snapshot lives for manual recovery. The
// original cause is never lost. The restore failure is induced by removing
// the backup root during the deploy call (after the rewrite, before the
// restore), so the restore stats a vanished backup root and fails.
func TestUpdate_RestoreFailureFailsClosedWithBothCauses(t *testing.T) {
	t.Parallel()

	upErr := errors.New("compose up exploded")
	fx := newUpdateApplyFixture(t, updateApplyApp("restore-fail-app"), false, nil, nil)
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 3 { // up -d --force-recreate
			// Sabotage the restore: remove the backup root the sad path
			// will read from, so RestoreConfigBackup fails to stat it.
			require.NoError(t, os.RemoveAll(fx.backupRoot))
			return docker.CommandResult{}, upErr
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)

	// Fail closed: ErrCodeGeneric, both causes reachable, uncertain-state
	// message + snapshot location in the hint.
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
	require.ErrorIs(t, err, upErr, "the original deploy fault must survive the fail-closed join")
	assert.Contains(t, err.Error(), "could not be fully restored",
		"the message must convey the uncertain restore state")
	assert.Contains(t, typed.Hint, fx.backupRoot, "the hint must name where the snapshot lived")
	assert.NotContains(t, strings.ToLower(err.Error()), "rollback")

	// Both locks released even on the fail-closed path.
	handle, err := state.AcquireStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err, "the per-stack flock must be released on the fail-closed path")
	require.NoError(t, handle.Release())
}

// TestUpdate_RestoreFailureMarksNeedsAttentionSurfacedViaStatus is the
// BLOCKING-fix proof for 's second conjunct ("return a typed
// error AND mark the app as needing attention") and:381's durable
// needs-attention state: when the restore ITSELF fails after a deploy
// failure, the sad path must persist a marker so a LATER Engine.Status
// fuses §18 condition 7 (last_operation_failed) instead of reporting a
// plain "running" over a half-restored stack. It reuses the restore
// sabotage from TestUpdate_RestoreFailureFailsClosedWithBothCauses (the
// backup root is removed during the deploy, so RestoreConfigBackup fails),
// then drives Status with a HEALTHY container so the ONLY needs-attention
// reason is the nulled last_successful_operation the marker wrote — proving
// the marker, not container health, drives the §18 marking. The returned
// error must still carry BOTH causes and ErrCodeGeneric: the marker must
// not eat the fail-closed join.
// The sabotage (backup root removed) leaves the on-disk.wdm.lock healthy,
// so this exercises the marker's parse-success arm: AcquireStackLock reads
// the live on-disk manifest, nulls last_successful_operation, and writes it
// back through a fresh exclusive flock.
func TestUpdate_RestoreFailureMarksNeedsAttentionSurfacedViaStatus(t *testing.T) {
	t.Parallel()

	upErr := errors.New("compose up exploded")
	fx := newUpdateApplyFixture(t, updateApplyApp("restore-fail-marks-app"), false, nil, nil)
	hostPort := freeLocalTCPPort(t)

	// Seed the manifest's local ports so Status's container inspect can be
	// matched against them; the on-disk lock keeps these fields when the
	// marker only nulls last_successful_operation.
	lock := updateStackLockForApp(updateApplyApp("restore-fail-marks-app"), fx.stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	lock.LocalPorts = []int{hostPort}
	stackBase := filepath.Dir(fx.stackPath)
	writeStatusStackLock(t, stackBase, fx.appID, lock)

	// mid-deploy), so the update fails closed.
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 3 { // up -d --force-recreate
			require.NoError(t, os.RemoveAll(fx.backupRoot))
			return docker.CommandResult{}, upErr
		}
		return docker.CommandResult{}, nil
	}

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)

	// The fail-closed join is intact: the marker must not eat either cause
	// or change the code.
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code,
		"the marker must not change the fail-closed ErrCodeGeneric")
	require.ErrorIs(t, err, upErr, "the original deploy fault must survive the marker")
	assert.Contains(t, err.Error(), "could not be fully restored",
		"the fail-closed message must survive the marker")

	// marker nulled last_successful_operation on the live.wdm.lock. The
	// container is HEALTHY, so the marker is the sole needs-attention driver.
	healthyInspect := statusContainerInspectStdout(t, "app", fx.appID, hostPort, "running", true, false, 0, "healthy")
	scriptStatusDocker(&statusTestFixture{fake: fx.fake}, statusTestContainerID+"\n", healthyInspect, nil)

	status, err := fx.eng.Status(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "needs_attention", status.State,
		"a failed-restore stack surfaces needs_attention via §18 condition 7")
	assert.True(t, status.NeedsAttention)
	assert.Contains(t, status.AttentionReasons, "last_operation_failed",
		"the marker's nulled last_successful_operation drives the §18 marking")

	// The marker really nulled the on-disk manifest (parse-success arm).
	committed, err := state.ReadStackLock(t.Context(), fx.manifestPath)
	require.NoError(t, err)
	assert.Nil(t, committed.LastSuccessfulOperation,
		"the marker nulls last_successful_operation on the live .wdm.lock")
	// Every other field the marker preserved is intact, so Status can still
	// inspect the stack by Compose project and ports.
	assert.Equal(t, "wdm-"+fx.appID, committed.ComposeProject)
	assert.Equal(t, []int{hostPort}, committed.LocalPorts)
}

// TestUpdate_NeedsAttentionMarkerTornLockFallback exercises the marker's
// torn-lock fallback arm directly (the reviewer-permitted unit test for the
// arm that is not cheaply inducible through the public Update API, which
// always commits a parseable .wdm.lock before the sad path): when the live
// .wdm.lock is unparsable, the marker bases itself on the in-scope
// pre-update snapshot — with last_successful_operation nulled — and writes
// it through the flock-free row-27 in-place primitive. The torn bytes must
// be replaced by a valid schema-1 manifest that ReadStackLock parses with a
// nil last_successful_operation, so a subsequent Status would fuse §18
// condition 7.
func TestUpdate_NeedsAttentionMarkerTornLockFallback(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".wdm.lock")
	// A torn on-disk lock: the marker cannot base itself on it, so it falls
	// back to the pre-update snapshot.
	require.NoError(t, os.WriteFile(lockPath, []byte("{ this is not valid json"), 0o600))

	// The in-scope pre-update snapshot the fallback writes, with a non-nil
	// last_successful_operation the marker must null.
	snapshot := state.StackLock{
		SchemaVersion:   1,
		AppID:           "torn-fallback-app",
		TemplateName:    "torn-fallback-app",
		TemplateVersion: "1.0.0",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.06.01",
		StackPath:       dir,
		ComposeProject:  "wdm-torn-fallback-app",
		ImagePins:       []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}},
		LocalPorts:      []int{18080},
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}

	require.NoError(t, core.MarkStackNeedsAttentionAfterFailedRestoreForTest(eng, dir, &snapshot))

	// The torn bytes are gone: the file now parses as a valid manifest with a
	// nil last_successful_operation, so §18 condition 7 fires on a later Status.
	marked, err := state.ReadStackLock(t.Context(), lockPath)
	require.NoError(t, err, "the fallback must replace torn bytes with a parseable manifest")
	assert.Nil(t, marked.LastSuccessfulOperation,
		"the fallback nulls last_successful_operation")
	assert.Equal(t, "torn-fallback-app", marked.AppID,
		"the fallback writes the in-scope pre-update snapshot's identity")
	assert.Equal(t, "wdm-torn-fallback-app", marked.ComposeProject)
	assert.Equal(t, []int{18080}, marked.LocalPorts)

	// The caller's snapshot is not mutated by the marker write (it nulls a copy).
	require.NotNil(t, snapshot.LastSuccessfulOperation,
		"the marker must not mutate the caller's snapshot")
}

// TestUpdate_NeedsAttentionMarkerBasePreference pins the marker's
// base-manifest selection (the fallback ladder in
// markStackNeedsAttentionAfterFailedRestore): a parseable live.wdm.lock is
// preferred over the in-scope snapshot so whatever a partial restore left
// committed is preserved (only the operation flag is nulled); a missing
// live lock falls back to the snapshot; and a missing live lock with no
// snapshot is a typed best-effort error (never a panic).
func TestUpdate_NeedsAttentionMarkerBasePreference(t *testing.T) {
	t.Parallel()

	t.Run("parseable live lock wins over snapshot", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		dir := t.TempDir()
		lockPath := filepath.Join(dir, ".wdm.lock")

		// The live on-disk lock names a DIFFERENT compose project / ports than
		// the snapshot, so basing on it (not the snapshot) is observable.
		live := updateStackLockForApp(updateApplyApp("marker-live-app"), dir)
		live.ComposeProject = "wdm-marker-live-app"
		live.LocalPorts = []int{17070}
		raw, err := json.MarshalIndent(live, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(lockPath, raw, 0o600))

		snapshot := updateStackLockForApp(updateApplyApp("marker-snap-app"), dir)
		snapshot.ComposeProject = "wdm-marker-snap-app"
		snapshot.LocalPorts = []int{19090}

		require.NoError(t, core.MarkStackNeedsAttentionAfterFailedRestoreForTest(eng, dir, &snapshot))

		marked, err := state.ReadStackLock(t.Context(), lockPath)
		require.NoError(t, err)
		assert.Nil(t, marked.LastSuccessfulOperation, "the operation flag is nulled")
		assert.Equal(t, "wdm-marker-live-app", marked.ComposeProject,
			"the marker bases itself on the live on-disk lock, not the snapshot")
		assert.Equal(t, []int{17070}, marked.LocalPorts,
			"the live lock's fields are preserved")
	})

	t.Run("missing live lock falls back to snapshot", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		dir := t.TempDir()
		lockPath := filepath.Join(dir, ".wdm.lock")

		snapshot := updateStackLockForApp(updateApplyApp("marker-missing-app"), dir)
		snapshot.ComposeProject = "wdm-marker-missing-app"
		snapshot.LocalPorts = []int{16060}

		require.NoError(t, core.MarkStackNeedsAttentionAfterFailedRestoreForTest(eng, dir, &snapshot))

		marked, err := state.ReadStackLock(t.Context(), lockPath)
		require.NoError(t, err, "a missing live lock is created from the snapshot")
		assert.Nil(t, marked.LastSuccessfulOperation)
		assert.Equal(t, "wdm-marker-missing-app", marked.ComposeProject)
		assert.Equal(t, []int{16060}, marked.LocalPorts)
	})

	t.Run("missing live lock and nil snapshot is a typed error", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		dir := t.TempDir()

		err := core.MarkStackNeedsAttentionAfterFailedRestoreForTest(eng, dir, nil)
		require.Error(t, err, "no manifest to base the marker on is a best-effort error, not a panic")
		assert.Contains(t, err.Error(), "no manifest available to mark stack needs-attention")
		assert.NoFileExists(t, filepath.Join(dir, ".wdm.lock"),
			"no marker file is written when there is no manifest to base it on")
	})

	t.Run("in-place writer guards a nil manifest", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		err := core.WriteNeedsAttentionMarkerForTest(filepath.Join(dir, ".wdm.lock"), nil)
		require.Error(t, err, "the in-place writer is best-effort and never panics on a nil manifest")
		assert.Contains(t, err.Error(), "no manifest available to mark stack needs-attention")
	})
}

// TestUpdate_RestoredFailureSurfacesNeedsAttentionViaStatus is the
// end-to-end needs-attention proof (the invariant, / §18 /
// an induced deploy failure restores the previous config (so the .wdm.lock
// is byte-identical with last_successful_operation still pointing at the
// prior install), a subsequent Engine.Status reports needs_attention through the
// EXISTING frozen §18 runtime-vs-config conditions — here container_exited,
// because the failed force-recreate left a stopped container while the
// restored config matches the old image. A healthy stack still reports
// running. The marking needs no new manifest field and no manifest
// mutation on the sad path.
func TestUpdate_RestoredFailureSurfacesNeedsAttentionViaStatus(t *testing.T) {
	t.Parallel()

	upErr := errors.New("compose up exploded")
	fx := newUpdateApplyFixture(t, updateApplyApp("restored-status-app"), false, nil, nil)
	hostPort := freeLocalTCPPort(t)

	// Seed the manifest's local ports so Status's container inspect can be
	// matched against them; the manifest is restored byte-identical, so this
	// value persists across the failed update.
	lock := updateStackLockForApp(updateApplyApp("restored-status-app"), fx.stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	lock.LocalPorts = []int{hostPort}
	stackBase := filepath.Dir(fx.stackPath)
	writeStatusStackLock(t, stackBase, fx.appID, lock)

	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 3 { // up -d --force-recreate
			return docker.CommandResult{}, upErr
		}
		return docker.CommandResult{}, nil
	}
	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	require.ErrorIs(t, err, upErr)
	assertUpdateRestoredPreviousConfig(t, fx)

	// left — an exited container — against the restored (old) config. Re-script
	// the shared fake to answer the Status invocation sequence by type.
	exitedInspect := statusContainerInspectStdout(t, "app", fx.appID, hostPort, "exited", false, false, 1, "")
	scriptStatusDocker(&statusTestFixture{fake: fx.fake}, statusTestContainerID+"\n", exitedInspect, nil)

	status, err := fx.eng.Status(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, "needs_attention", status.State,
		"a restored-but-broken stack surfaces needs_attention via the §18 conditions")
	assert.True(t, status.NeedsAttention)
	assert.Contains(t, status.AttentionReasons, "container_exited",
		"the failed force-recreate's stopped container drives the §18 marking")

	// Control: a healthy stack still reports running through the same path.
	healthyInspect := statusContainerInspectStdout(t, "app", fx.appID, hostPort, "running", true, false, 0, "healthy")
	scriptStatusDocker(&statusTestFixture{fake: fx.fake}, statusTestContainerID+"\n", healthyInspect, nil)
	healthy, err := fx.eng.Status(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, healthy)
	assert.Equal(t, "running", healthy.State, "a healthy stack still reports running")
	assert.False(t, healthy.NeedsAttention)
}

// TestUpdate_OperationDockerClientCarriesCombinedSecretLiterals is the
// BLOCKING-fix proof (, the confirmation rules, the
// reviewer's prediction): the per-operation Docker client is built over
// the COMBINED generated and reused secret literals so Compose stderr is
// scrubbed of reused install-time secrets too. A capturing factory
// records the redactor handed to the client; after a successful update it
// must redact BOTH the regenerated regenerable=true secret AND the reused
// regenerable=false secret.
func TestUpdate_OperationDockerClientCarriesCombinedSecretLiterals(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("redactor-app"), false, nil, nil)

	var captured security.Redactor
	core.SetInstallDockerClientFactoryForTest(fx.eng, func(r security.Redactor) (docker.Client, error) {
		captured = r
		return fx.fake, nil
	})

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotNil(t, captured, "the apply path must build the operation Docker client")
	// Both secret provenances must be scrubbed: a Compose stderr line that
	// echoed either literal is redacted before it can reach a sink.
	assert.Equal(t,
		security.RedactedPlaceholder,
		captured.Redact(dbPasswordInstallValue),
		"the reused regenerable=false secret must reach the client redactor")
	assert.Equal(t,
		security.RedactedPlaceholder,
		captured.Redact(regeneratedAPIToken),
		"the regenerated regenerable=true secret must reach the client redactor")
	assert.NotContains(t,
		captured.Redact("db="+dbPasswordInstallValue+" token="+regeneratedAPIToken),
		dbPasswordInstallValue,
		"a Compose stderr line carrying both secrets is fully scrubbed")
}

// TestUpdate_ContextCancellationMidDeployStillRestores is the
// bounded-context proof: a context
// canceled at the recreate confirmation aborts before any pull or recreate
// and surfaces as context.Canceled (ctx.Err discipline preserved), but the
// step-3 snapshot is STILL restored byte-for-byte because the restore runs
// on the contextless restore primitive — the parent-ctx cancellation that
// triggered the sad path cannot interrupt the restore that the
// cancellation itself caused. The manifest stays uncommitted.
func TestUpdate_ContextCancellationMidDeployStillRestores(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("cancel-predeploy-app"), false, nil, nil)
	manifestBefore, err := os.ReadFile(fx.manifestPath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var steps []string
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			cancel() // cancel between the confirm and the pull
			return true, nil
		},
	}

	res, err := fx.eng.Update(ctx, types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled, "a canceled apply still surfaces context.Canceled")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")

	// The restore completed despite the canceled parent ctx (bounded-ctx
	// discipline): the previous config is back byte-for-byte.
	assert.Contains(t, steps, types.StepUpdateConfigRestore,
		"the canceled apply still emits the config-restore step")
	assertUpdateRestoredPreviousConfig(t, fx)
	assertManifestUnchanged(t, fx.manifestPath, manifestBefore)
}

// TestUpdate_RecreateConfirmationPayloadListsConsequences proves the
// recreate confirmation payload
// surfaces the exact consequences: the stack identity, the template
// version transition, the per-service old → new image change, the localhost
// ports that re-bind on recreate, the volumes the recreate touches, the
// catalog network it will ensure, and the pre-update backup path — and
// carries no secret value. The fixture seeds real manifest local ports
// (the payload's port source is the preserved manifest, not the catalog)
// so the per-port lines are actually exercised.
func TestUpdate_RecreateConfirmationPayloadListsConsequences(t *testing.T) {
	t.Parallel()

	const appID = "recreate-payload-app"
	app := updateApplyApp(appID)
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: true}}
	templates := updateApplyTemplates(app)
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)
	backupRoot := filepath.Join(stackPath, state.BackupDirName)

	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	// The preserved manifest local ports drive the payload's port lines.
	lock.LocalPorts = []int{18080, 19090}
	writeStatusStackLock(t, stackBase, appID, lock)

	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte(renderEnvFixture(map[string]string{
			"DB_PASSWORD": dbPasswordInstallValue,
			"API_TOKEN":   apiTokenInstallValue,
			"SITE_NAME":   siteNameInstallValue,
		})),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))

	// Decline at the recreate prompt: the payload is still captured, and
	// no network/pull/recreate Docker call needs scripting.
	confirmer := decliningConfirmer()
	_, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID}, nil, confirmer)
	require.Error(t, err)

	require.Len(t, confirmer.calls, 1)
	payload := confirmer.calls[0]
	assert.Equal(t, "update_deploy", payload.Kind)
	assert.Contains(t, payload.Title, appID)
	assert.Contains(t, payload.Message, "app: "+appID)
	assert.Contains(t, payload.Message, "compose project: wdm-"+appID)
	assert.Contains(t, payload.Message, "image change: service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0")
	// Each preserved manifest port surfaces a localhost re-bind line
	// this is the assertion that catches a payload that
	// silently drops ports.
	assert.Contains(t, payload.Message, "rebinds 127.0.0.1:18080")
	assert.Contains(t, payload.Message, "rebinds 127.0.0.1:19090")
	assert.Contains(t, payload.Message, "recreates with volume ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro")
	assert.Contains(t, payload.Message, "ensures docker network wdm_front (internal)")
	assert.Contains(t, payload.Message, "config backup: "+backupRoot)
	assert.NotContains(t, payload.Message, dbPasswordInstallValue,
		"the recreate payload must never carry a secret value")
	assert.NotContains(t, payload.Message, regeneratedAPIToken)
}

// TestUpdate_ApplyEmitsNoInstallPrefixedStepIDs is the BLOCKING-fix guard
// for the row-37 frozen update progress API: the full happy-path apply
// must never emit a "step_install"-prefixed step ID. The shared
// ensureInstallNetworks helper emits types.StepInstallNetworkCreate, and
// every curated app declares networks, so a live onProgress on the
// network leg would surface step_install_network_create on the update
// stream. The fixture declares a network (forcing the network leg to run)
// and collects every emitted step ID; pinning the whole stream — not just
// networks — catches any future install-prefixed leak.
func TestUpdate_ApplyEmitsNoInstallPrefixedStepIDs(t *testing.T) {
	t.Parallel()

	app := updateApplyApp("no-install-steps-app")
	app.Networks = []catalog.Network{{Name: "wdm_back", Internal: false}}
	fx := newUpdateApplyFixture(t, app, false, nil, nil)

	// Call order: config(1), network inspect(2, missing -> create),
	// network create(3), pull(4), up(5), digest(6), container ls(7),
	// container inspect(8). The network inspect must report "missing" so
	// EnsureNetwork takes its create path and the apply runs to completion.
	fx.fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		if call == 2 {
			return missingNetworkResult("wdm_back")
		}
		return docker.CommandResult{}, nil
	}

	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err, "the full apply path completes end to end")
	require.NotNil(t, res)

	// The network leg actually ran (proving the nil-progress path is the
	// one exercised, not a no-op skip).
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
	require.NotEmpty(t, steps, "the apply must emit progress steps")
	for _, step := range steps {
		assert.False(t, strings.HasPrefix(step, "step_install"),
			"the update progress stream must never carry an install-prefixed step ID, got %q", step)
	}
	// The leaked ID specifically must be absent.
	assert.NotContains(t, steps, types.StepInstallNetworkCreate)
}

// TestUpdate_CommitPreservesInstalledIdentity proves the manifest rewrite
// preserves the installed identity that update does not re-plan: the
// selected domain, local ports, and recommended-resource totals carry
// forward unchanged from the
// pre-update manifest, while the template version and image pin advance.
func TestUpdate_CommitPreservesInstalledIdentity(t *testing.T) {
	t.Parallel()

	const appID = "preserve-identity-app"
	app := updateApplyApp(appID)
	templates := updateApplyTemplates(app)
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)

	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	lock.SelectedDomain = "preserve.example.com"
	lock.LocalPorts = []int{18080}
	lock.RecommendedResources = &state.RecommendedResources{MemoryBytes: 256 * 1024 * 1024, CPUs: 0.5}
	manifestPath := writeStatusStackLock(t, stackBase, appID, lock)

	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte(renderEnvFixture(map[string]string{
			"DB_PASSWORD": dbPasswordInstallValue,
			"API_TOKEN":   apiTokenInstallValue,
			"SITE_NAME":   siteNameInstallValue,
		})),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))

	_, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	committed, err := state.ReadStackLock(t.Context(), manifestPath)
	require.NoError(t, err)
	// Preserved from the pre-update manifest.
	assert.Equal(t, "preserve.example.com", committed.SelectedDomain,
		"the installed domain is preserved across update")
	assert.Equal(t, []int{18080}, committed.LocalPorts,
		"the installed local ports are preserved (update does not re-plan ports)")
	require.NotNil(t, committed.RecommendedResources)
	assert.Equal(t, uint64(256*1024*1024), committed.RecommendedResources.MemoryBytes,
		"the recommended-resource totals are preserved (update does not re-probe the host)")
	assert.InDelta(t, 0.5, committed.RecommendedResources.CPUs, 0.0001)
	// Advanced by the update.
	assert.Equal(t, "2.0.0", committed.ImagePins[0].Tag)
	assert.Equal(t, "update", committed.LastSuccessfulOperation.Kind)
	// The reused regenerable=false secret stays recorded as a generated
	// field after the commit — it is secret-typed and tracked regardless of
	// provenance (resolveUpdatePlaceholder records every secret-typed
	// placeholder before the regenerate/reuse split).
	assert.Contains(t, committed.GeneratedFields, "DB_PASSWORD",
		"a reused regenerable=false secret stays recorded in generated_fields")
	assert.Contains(t, committed.GeneratedFields, "API_TOKEN")
}

// TestUpdate_StaleTempFromValidationDoesNotLinger proves the update-time
// Compose validation workspace is hermetic: no `.tmp` validation artifact
// is left under the stack directory after a successful apply (the
// workspace lives under the OS temp dir and is removed on every path).
func TestUpdate_StaleTempFromValidationDoesNotLinger(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("validate-temp-app"), false, nil, nil)

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(fx.stackPath, "docker-compose.yml.tmp"))
	assert.NoFileExists(t, filepath.Join(fx.stackPath, ".env.tmp"))
}

// TestUpdate_ApplyRetentionCapsAtLimitOverManyRuns is the retention proof over
// real successive applies: one real version bump followed by ten no-op applies —
// eleven backups created on one stack — must leave exactly
// BackupRetentionLimit snapshots, with the oldest unpinned snapshot
// evicted and the most-recent-successful (this last run's) snapshot
// surviving the prune.
func TestUpdate_ApplyRetentionCapsAtLimitOverManyRuns(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("apply-retention-loop-app"), false, nil, nil)

	// First apply is the real 1.0.0 -> 2.0.0 bump; the remaining ten are
	// up-to-date no-ops. Each
	// run adds one snapshot, so the 11th drives eviction.
	const totalRuns = state.BackupRetentionLimit + 1
	var lastBackup string
	for range totalRuns {
		res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.NotEmpty(t, res.BackupPath)
		// A fresh snapshot per run: the path must change each iteration.
		assert.NotEqual(t, lastBackup, res.BackupPath, "each apply takes a distinct snapshot")
		lastBackup = res.BackupPath
	}

	entries, err := os.ReadDir(fx.backupRoot)
	require.NoError(t, err)
	assert.Len(t, entries, state.BackupRetentionLimit,
		"retention caps total snapshots at the limit after eleven applies")

	// Order entries by name (unix-nanos prefix sorts chronologically) and
	// prove the oldest was evicted while this run's snapshot survives.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	assert.DirExists(t, lastBackup,
		"the most-recent-successful (pinned) snapshot is never pruned")
	assert.Equal(t, filepath.Base(lastBackup), names[len(names)-1],
		"this run's snapshot is the newest surviving entry")
}

// TestUpdate_ApplyPreservesPriorBackupHistoryVerbatim proves the commit
// point's backup_history clone loop (update_deploy.go appendUpdateBackupHistory)
// preserves a pre-existing ledger entry byte-for-byte while appending this
// run's snapshot record. A forward-compatible read-modify-write must never
// drop or rewrite unknown history bytes.
func TestUpdate_ApplyPreservesPriorBackupHistoryVerbatim(t *testing.T) {
	t.Parallel()

	const appID = "apply-history-preserve-app"
	app := updateApplyApp(appID)
	templates := updateApplyTemplates(app)
	catalogFS := catalogFixtureFSWithFiles(t, templates, app)

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)

	// A pre-existing history entry whose exact bytes (including a field the
	// current writer does not emit) must survive the commit untouched.
	seeded := json.RawMessage(`{"path":"/seeded/snapshot","operation":"install","note":"keep-me-verbatim"}`)
	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	lock.BackupHistory = []json.RawMessage{seeded}
	manifestPath := writeStatusStackLock(t, stackBase, appID, lock)

	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, ".env"),
		[]byte(renderEnvFixture(map[string]string{
			"DB_PASSWORD": dbPasswordInstallValue,
			"API_TOKEN":   apiTokenInstallValue,
			"SITE_NAME":   siteNameInstallValue,
		})),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))

	_, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	committed, err := state.ReadStackLock(t.Context(), manifestPath)
	require.NoError(t, err)
	require.Len(t, committed.BackupHistory, 2,
		"the prior entry is preserved and this run's entry is appended")
	// The seeded entry survives byte-verbatim as the first ledger record.
	assert.JSONEq(t, string(seeded), string(committed.BackupHistory[0]),
		"the pre-existing backup_history entry must survive byte-verbatim")
	assert.Contains(t, string(committed.BackupHistory[0]), "keep-me-verbatim",
		"an unknown field in a prior history entry must not be dropped")
	// The appended entry records this run's snapshot.
	assert.Contains(t, string(committed.BackupHistory[1]), state.BackupDirName,
		"the appended entry records the new snapshot path")
}

// installFakeDockerOnPath drops a fake `docker` executable at the front of
// PATH so a REAL internal/docker client (docker.New) runs it instead of the
// host daemon. Because it mutates the process-global PATH via t.Setenv, a
// test using it MUST NOT call t.Parallel.
func installFakeDockerOnPath(t *testing.T, scriptBody string) {
	t.Helper()

	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	require.NoError(t, os.WriteFile(dockerPath, []byte(scriptBody), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// realDockerClientFactory returns an engine docker-client factory that
// builds a REAL internal/docker client over the operation redactor
// applyUpdate wires (docker.WithRedactor), so the deploy argv and any
// stderr redaction are exercised end to end through the production wrapper.
func realDockerClientFactory(t *testing.T) func(security.Redactor) (docker.Client, error) {
	t.Helper()
	return func(r security.Redactor) (docker.Client, error) {
		return docker.New(docker.WithRedactor(r))
	}
}

// TestUpdate_DeployArgvCarriesForceRecreate is the core-level proof that
// the update recreate reaches `docker compose... up -d --force-recreate`
// (, closing the update_apply_test.go:353 gap where a
// regression to ComposeUpOptions{} would pass every fake-client test). It
// drives a REAL docker.New client through a fake `docker` binary that logs
// every invocation's argv; after a successful apply the up line must carry
// --force-recreate. Cannot run in parallel: it mutates PATH.
func TestUpdate_DeployArgvCarriesForceRecreate(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	// The fake docker echoes each invocation's argv (one bracketed line per
	// call) into $WDM_ARGV_LOG and exits 0 so the apply runs to completion.
	installFakeDockerOnPath(t, `#!/bin/sh
{
  printf 'argv='
  for arg in "$@"; do printf '[%s]' "$arg"; done
  printf '\n'
} >> "$WDM_ARGV_LOG"
exit 0
`)
	t.Setenv("WDM_ARGV_LOG", argvLog)

	fx := newUpdateApplyFixture(t, updateApplyApp("argv-force-recreate-app"), false, nil, nil)
	core.SetInstallDockerClientFactoryForTest(fx.eng, realDockerClientFactory(t))

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "the apply completes against the fake docker binary")

	logged, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	logText := string(logged)
	t.Logf("captured docker argv:\n%s", logText)

	// The deploy invocation's argv must carry the up -d --force-recreate
	// triple in order; a ComposeUpOptions{} regression would drop the flag.
	assert.Contains(t, logText, "[up][-d][--force-recreate]",
		"the update deploy must run docker compose up -d --force-recreate")
}

// TestUpdate_DeployStderrSecretIsRedactedInErrorChain upgrades the redactor
// proof from seam-level to stderr-derived: a deploy failure whose fake
// docker stderr embeds the reused regenerable=false secret must surface a
// typed error whose FULL chain shows [REDACTED] and never the literal
// (; the operation client's WithRedactor carries the
// combined generated + reused literals). Cannot run in parallel: it mutates
// PATH.
func TestUpdate_DeployStderrSecretIsRedactedInErrorChain(t *testing.T) {
	// The fake docker succeeds for every call except `up`, where it writes
	// the reused secret to stderr and exits non-zero so the deploy fails
	// with that stderr flowing through the production redactor + error map.
	installFakeDockerOnPath(t, `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "up" ]; then
    printf 'compose up failed: secret was `+dbPasswordInstallValue+` leaked\n' >&2
    exit 1
  fi
done
exit 0
`)

	fx := newUpdateApplyFixture(t, updateApplyApp("stderr-redact-app"), false, nil, nil)
	core.SetInstallDockerClientFactoryForTest(fx.eng, realDockerClientFactory(t))

	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	t.Logf("redacted deploy error: %v", err)

	// The full error chain must scrub the reused secret and surface the
	// redaction placeholder from the captured stderr.
	assertErrorChainDoesNotContain(t, err, dbPasswordInstallValue)
	assert.Contains(t, err.Error(), security.RedactedPlaceholder,
		"the captured docker stderr must surface redacted in the error chain")
}
