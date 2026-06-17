package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// expectedDatabaseRiskWarning is the exact PRD §20 database-risk
// warning, reproduced here independently of the production constant so
// a drift in either side fails the verbatim assertion. It must stay
// byte-identical to the documented fenced block — six lines
// (four content, two blank separators) and no trailing newline.
const expectedDatabaseRiskWarning = "This update may change the app database.\n" +
	"\n" +
	"wdm does not back up app data or databases.\n" +
	"If the app migrates its database, restoring old config later may not restore the app.\n" +
	"\n" +
	"Proceed only if you have your own backup."

// databaseRiskUpdateApp returns a catalog app classified as a
// database-risk update: its image pin is bumped past the manifest the
// fixture writes (1.0.0), so plan.updateAvailable is true and the
// "database" class gates the apply path.
func databaseRiskUpdateApp(appID string, risks ...string) catalog.App {
	app := appFixture(appID, 18080)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	app.RiskClassification = append([]string(nil), risks...)
	return app
}

// rewindToOlderPin rewinds the fixture manifest to the pre-update image
// tag so the catalog's bumped pin reads as a candidate update.
func rewindToOlderPin(lock *state.StackLock) {
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
}

// assertNoStackMutations proves the database-risk gate left no on-disk
// side effect: no backup snapshot directory and no stack files written
// by the engine before the warning cleared.
func assertNoStackMutations(t *testing.T, fx *updateTestFixture) {
	t.Helper()

	assert.NoDirExists(t, filepath.Join(fx.stackPath, state.BackupDirName),
		"a declined or refused database-risk update must not create a backup snapshot")
	assert.NoFileExists(t, filepath.Join(fx.stackPath, "docker-compose.yml"),
		"a declined or refused database-risk update must not rewrite compose")
	assert.NoFileExists(t, filepath.Join(fx.stackPath, ".env"),
		"a declined or refused database-risk update must not rewrite .env")
	assert.Zero(t, fx.fake.calls,
		"the database-risk gate sits before any docker command")
}

// TestUpdate_DatabaseRiskApplyAcceptedRecreatesWithTwoConfirms proves a
// database-risk update fires BOTH confirmation sites: the
// database-risk warning (before backup) AND the recreate confirmation
// (after tag rewrite, before pull). An accepting confirmer clears both,
// the backup precedes the rewrite which precedes the recreate confirm,
// and the update then deploys to a durable success. The two payloads are
// the verbatim database warning followed by the recreate consequence
// payload.
func TestUpdate_DatabaseRiskApplyAcceptedRecreatesWithTwoConfirms(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("db-accept-app", "database"), false, nil, nil)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)

	require.NoError(t, err, "an accepted database-risk update completes end to end")
	require.NotNil(t, res)

	// fires twice for a database-risk update: the warning before
	// backup, the recreate confirm before pull.
	require.Len(t, confirmer.calls, 2, "a database-risk update confirms twice (warning + recreate)")
	assert.Equal(t, "update_database_risk", confirmer.calls[0].Kind,
		"the database-risk warning must come first (before backup, row 32)")
	assert.Equal(t, "update_deploy", confirmer.calls[1].Kind,
		"the recreate confirmation must come after the tag rewrite, before pull (row 38)")

	// Step order: the database-risk confirm precedes the backup; the
	// recreate confirm precedes pull and deploy.
	assert.Less(t, stepIndex(t, steps, types.StepUpdateConfirm), stepIndex(t, steps, types.StepUpdateBackup),
		"the database-risk warning must precede the backup")
	assert.Less(t, stepIndex(t, steps, types.StepUpdateRender), stepIndex(t, steps, types.StepUpdatePull),
		"the rewrite must precede the pull")
	assert.Less(t, stepIndex(t, steps, types.StepUpdatePull), stepIndex(t, steps, types.StepUpdateDeploy),
		"the pull must precede the recreate")
	assert.DirExists(t, fx.backupRoot, "an accepted database-risk update takes a backup")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composePullInvocation")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation")
}

// TestUpdate_DatabaseRiskWarningTextIsVerbatim asserts the full warning
// string character-for-character so any drift from the exact PRD §20
// text fails the build. The Title carries the app and
// version context so the load-bearing Message body stays verbatim.
func TestUpdate_DatabaseRiskWarningTextIsVerbatim(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("db-text-app", "database"), false, nil, nil)

	confirmer := &fakeConfirmer{}
	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)
	require.NoError(t, err)

	require.Len(t, confirmer.calls, 2, "a database-risk update confirms twice (warning + recreate)")
	got := confirmer.calls[0]
	assert.Equal(t, expectedDatabaseRiskWarning, got.Message,
		"the database-risk warning must reproduce the exact PRD §20 text")
	assert.Equal(t, "update_database_risk", got.Kind)
	assert.Contains(t, got.Title, "db-text-app",
		"the prompt title must identify the app without altering the warning body")
	assert.NotContains(t, got.Message, "db-text-app",
		"app identity must ride in Title, never spliced into the verbatim warning body")
}

// TestUpdate_DatabaseRiskDeclineCancelsWithoutMutations proves a
// declined warning maps to types.ErrCodeUserCanceled and leaves no
// on-disk side effect (: "leaves no on-disk side
// effect"; exit criterion:356: "cancellation prevents backup AND file
// rewrite").
func TestUpdate_DatabaseRiskDeclineCancelsWithoutMutations(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, databaseRiskUpdateApp("db-decline-app", "database"), rewindToOlderPin)

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)

	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code,
		"a declined database-risk warning maps to user-canceled")
	require.Len(t, confirmer.calls, 1)
	assertNoStackMutations(t, fx)
}

// TestUpdate_DatabaseRiskNilConfirmerRefuses proves the install
// posture: an apply request for a database-risk update with no
// confirmer refuses with types.ErrCodeUsageValidation rather than
// proceeding unattended (PRD §6 "no update may run unattended").
func TestUpdate_DatabaseRiskNilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, databaseRiskUpdateApp("db-nil-app", "database"), rewindToOlderPin)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, nil)

	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")
	assertNoStackMutations(t, fx)
}

// TestUpdate_DatabaseRiskConfirmerErrorPropagates proves a confirmer
// error aborts the update and propagates wrapped, distinct from a
// decline (which is a clean user-canceled), with no on-disk side
// effect.
func TestUpdate_DatabaseRiskConfirmerErrorPropagates(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, databaseRiskUpdateApp("db-err-app", "database"), rewindToOlderPin)

	sentinel := errors.New("confirmer backend down")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, sentinel
		},
	}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)

	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, sentinel, "a confirmer error must propagate through the wrap chain")
	require.Len(t, confirmer.calls, 1)
	assertNoStackMutations(t, fx)
}

// TestUpdate_NonDatabaseApplySkipsWarning proves the database gate is
// scoped to the "database" risk class: an available non-database update
// never shows the database warning, yet still confirms the recreate
// backs
// up, re-renders, and deploys to a durable success — including the
// multi-class case that carries other classes but not "database".
func TestUpdate_NonDatabaseApplySkipsWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		risks []string
	}{
		{name: "safe", risks: []string{"safe"}},
		{name: "major", risks: []string{"major"}},
		{name: "complex", risks: []string{"complex"}},
		{name: "multi-class without database", risks: []string{"major", "complex"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newUpdateApplyFixture(t, updateApplyApp("non-db-app", tt.risks...), false, nil, nil)

			confirmer := &fakeConfirmer{}
			res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)

			require.NoError(t, err, "a non-database update completes end to end")
			require.NotNil(t, res)
			// Only the recreate confirmation fires — never the database
			// warning.
			require.Len(t, confirmer.calls, 1,
				"a non-database update confirms only the recreate, never the database warning")
			assert.Equal(t, "update_deploy", confirmer.calls[0].Kind,
				"the single confirmation is the recreate, not the database warning")
			assert.DirExists(t, fx.backupRoot, "a non-database update still backs up")
		})
	}
}

// TestUpdate_DatabaseRiskMultiClassFiresWarning proves a multi-class
// update that includes "database" alongside other classes still fires
// the warning — membership, not exclusivity, gates the prompt.
func TestUpdate_DatabaseRiskMultiClassFiresWarning(t *testing.T) {
	t.Parallel()

	fx := newUpdateApplyFixture(t, updateApplyApp("db-multi-app", "database", "complex"), false, nil, nil)

	confirmer := &fakeConfirmer{}
	_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)
	require.NoError(t, err)
	require.Len(t, confirmer.calls, 2,
		"a multi-class update including database confirms the warning then the recreate")
	assert.Equal(t, "update_database_risk", confirmer.calls[0].Kind,
		"a multi-class update including database must still show the warning first")
}

// TestUpdate_NoOpDatabaseApplySkipsWarning is the no-op-apply decision
// proof (PRD §20): a stack already on the
// catalog's template version with no image-pin change is not an
// "update", so even though the catalog classifies the app as database
// risk, no candidate update exists to group — plan.riskClassifications
// is empty — and the apply skips the database warning. It still confirms
// the recreate, backs up, re-renders, and redeploys so the rewritten files
// become live. The lone confirmation is the recreate, not the database warning.
func TestUpdate_NoOpDatabaseApplySkipsWarning(t *testing.T) {
	t.Parallel()

	// upToDate: the manifest mirrors the catalog exactly, so the stack
	// is already up to date and the apply is a no-op.
	fx := newUpdateApplyFixture(t, updateApplyApp("noop-db-app", "database"), true, nil, nil)

	confirmer := &fakeConfirmer{}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)

	require.NoError(t, err, "a no-op apply still completes the recreate")
	require.NotNil(t, res)
	require.Len(t, confirmer.calls, 1,
		"a no-op apply has no candidate update, so only the recreate confirmation fires")
	assert.Equal(t, "update_deploy", confirmer.calls[0].Kind,
		"a no-op apply never shows the database warning")
	assert.DirExists(t, fx.backupRoot,
		"a no-op apply still takes a backup")
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a no-op apply still redeploys to make the rewrite live")
}

// TestUpdate_DryRunDatabaseRiskNeverConfirms proves the DryRun contract
// is untouched by the gate: a database-risk dry-run returns
// the populated check result and never consults the confirmer, even
// when one is supplied.
func TestUpdate_DryRunDatabaseRiskNeverConfirms(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, databaseRiskUpdateApp("db-dryrun-app", "database"), rewindToOlderPin)

	confirmer := &fakeConfirmer{}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, nil, confirmer)

	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, []string{"database"}, res.RiskClassifications,
		"a database-risk dry-run still reports the risk grouping")
	assert.Empty(t, confirmer.calls,
		"DryRun must never consult the confirmer (types.UpdateRequest.DryRun contract)")
	assertNoStackMutations(t, fx)
}

// TestUpdate_DatabaseRiskRefusalsPrecedeWarning proves the validate-first
// and managed-only contracts still hold ahead of the warning: an empty
// app id refuses before the first progress event and before the
// confirmer is consulted, and a busy stack refuses with
// ErrCodeRuntimeLockHeld without ever reaching the warning. The
// confirmer would authorize, so reaching it would be observable.
func TestUpdate_DatabaseRiskRefusalsPrecedeWarning(t *testing.T) {
	t.Parallel()

	t.Run("empty app id refuses before warning and events", func(t *testing.T) {
		t.Parallel()

		fx := newUpdateFixture(t, databaseRiskUpdateApp("db-empty-app", "database"), rewindToOlderPin)

		confirmer := &fakeConfirmer{}
		var events int
		res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: ""}, func(string, float64, string) {
			events++
		}, confirmer)

		require.Error(t, err)
		assert.Nil(t, res)
		assertUsageValidation(t, err)
		assert.ErrorContains(t, err, "app id is required")
		assert.Zero(t, events, "request validation must refuse before the first progress event")
		assert.Empty(t, confirmer.calls, "the confirmer must not be consulted on an invalid request")
	})

	t.Run("busy stack refuses before warning", func(t *testing.T) {
		t.Parallel()

		fx := newUpdateFixture(t, databaseRiskUpdateApp("db-busy-app", "database"), rewindToOlderPin)
		holdFlockExclusive(t, filepath.Join(fx.stackPath, ".wdm.lock"))

		confirmer := &fakeConfirmer{}
		res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, confirmer)

		require.Error(t, err)
		assert.Nil(t, res)
		var typed *types.Error
		require.ErrorAs(t, err, &typed)
		assert.Equal(t, types.ErrCodeRuntimeLockHeld, typed.Code)
		assert.Empty(t, confirmer.calls, "a busy stack refuses before the database-risk warning")
	})
}

// TestUpdate_DatabaseRiskApplyReleasesRuntimeLock proves the runtime
// lock posture survives the new gate on every apply outcome: after an
// accepted, declined, and refused database-risk update returns, a fresh
// runtime.lock acquisition succeeds.
func TestUpdate_DatabaseRiskApplyReleasesRuntimeLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		confirmer types.Confirmer
	}{
		{name: "accepted", confirmer: &fakeConfirmer{}},
		{
			name: "declined",
			confirmer: &fakeConfirmer{
				confirmFn: func(context.Context, types.Confirmation) (bool, error) {
					return false, nil
				},
			},
		},
		{name: "nil confirmer", confirmer: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newUpdateFixture(t, databaseRiskUpdateApp("db-lock-app", "database"), rewindToOlderPin)
			lockPath := filepath.Join(fx.stateDir, "runtime.lock")

			_, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, tt.confirmer)
			require.Error(t, err, "every database-risk apply outcome in this table is a non-nil error")

			probe, err := state.AcquireRuntimeLock(
				t.Context(),
				lockPath,
				state.RuntimeLockMetadata{Command: "posture-probe", WDMVersion: "test"},
			)
			require.NoError(t, err, "Update must release runtime.lock past the database-risk gate")
			require.NoError(t, probe.Release())
		})
	}
}

// TestUpdate_DatabaseRiskApplyContextCancellation proves ctx.Err
// discipline at the gate: canceling on the planning summary event —
// the last emission before the gate runs — is caught by the gate's
// own ctx check, surfaces as context.Canceled, and the confirmer is
// never consulted.
func TestUpdate_DatabaseRiskApplyContextCancellation(t *testing.T) {
	t.Parallel()

	fx := newUpdateFixture(t, databaseRiskUpdateApp("db-cancel-app", "database"), rewindToOlderPin)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	confirmer := &fakeConfirmer{}
	onProgress := func(step string, progress float64, _ string) {
		// The planning summary is StepUpdatePlanning at progress 15
		// (reportUpdateCheck's final emission); canceling there lands
		// the cancellation between planning and the gate so the gate's
		// first statement observes it.
		if step == types.StepUpdatePlanning && progress == 15 {
			cancel()
		}
	}

	res, err := fx.eng.Update(ctx, types.UpdateRequest{AppID: fx.appID}, onProgress, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, confirmer.calls,
		"a canceled context must abort before the database-risk warning")
}
