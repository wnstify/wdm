package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps backups list` and `apps backups restore` leaf
// bodies. They mirror the established internal/cli idioms — driving NewRootCmd through runLeaf with the
// recording fakeEngine, the raw-stdout byte discipline from
// envelope_contract_test.go, the keyed-payload list envelope from
// catalog_test.go, and the finish-screen + confirmer wiring from
// restart_test.go — and lock the two leaves' contract: the wdm.v1 envelope
// shapes (the "backups" object key for list, the RestoreBackupResult wrapped
// directly for restore), the plain-mode renderings (the list columns, the
// restore finish block relaying BoundaryNotice and NextAction verbatim, the
// needs-attention neutral-headline arm), --yes accepting the SAFE
// "restore_config" confirmation through the real cliConfirmer, the declined
// prompt -> empty stdout + typed error, the --stack-path verbatim mapping,
// ExactArgs enforcement, the factory-not-on-help invariant, the empty-stdout
// error paths, and the nil-ProgressFn-under-json vs non-nil-in-plain
// suppression for restore.
// Wording invariant: the restore copy under test says
// "config restore" and relays the engine's boundary/next-action text
// verbatim. The forbidden-undo-vocabulary token is assembled at runtime
// (forbiddenUndoVerb) rather than written as a source literal, so this file
// itself stays clean of it while still asserting the rendering never emits
// it.

// forbiddenUndoVerb is the undo-vocabulary word the invariant's wording
// invariant forbids in all config-restore copy. It is assembled at runtime
// from fragments so this test file carries no source literal of it while
// still asserting the rendered copy never emits it ("roll" + "back").
var forbiddenUndoVerb = "roll" + "back"

// sampleBackups returns two snapshots covering the list-rendering surface
// (distinct operations, multiple captured files) in the engine's
// newest-first order.
func sampleBackups() []types.BackupInfo {
	return []types.BackupInfo{
		{
			SnapshotID: "1717000000000000001-update",
			Operation:  "update",
			CreatedAt:  time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
			Path:       "/home/test/docker/vaultwarden/.wdm-backups/1717000000000000001-update",
			Files:      []string{"docker-compose.yml", ".env", ".wdm.lock"},
		},
		{
			SnapshotID: "1716000000000000000-install",
			Operation:  "install",
			CreatedAt:  time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC),
			Path:       "/home/test/docker/vaultwarden/.wdm-backups/1716000000000000000-install",
			Files:      []string{".wdm.lock"},
		},
	}
}

// sampleRestoreResult returns a fully populated RestoreBackupResult with a
// clean (non-needs-attention) status so the finish-block and envelope
// assertions exercise the whole projection.
func sampleRestoreResult() *types.RestoreBackupResult {
	return &types.RestoreBackupResult{
		AppID:          "vaultwarden",
		SnapshotID:     "1717000000000000001-update",
		RestoredFiles:  []string{"docker-compose.yml", ".env", ".wdm.lock"},
		BoundaryNotice: "config restore restores only wdm config files; app data, databases, uploaded files, media libraries, and Docker volumes are unchanged",
		NextAction:     "config restored to disk; the running containers still use the old config — run `wdm apps update` to recreate them and apply the restored config",
		Status:         &types.AppStatus{AppID: "vaultwarden", State: "running"},
	}
}

// --- apps backups list ---

// TestAppsBackupsList_JSON_EmitsSingleEnvelopeUnderBackupsKey pins that
// `apps backups list --json` writes exactly one wdm.v1 envelope on stdout
// whose data object carries the snapshots under the stable "backups" key —
// never a top-level JSON array (PRD §32 mandates envelope.data is an
// object) — mirroring the apps-list/catalog-list envelope precedent.
func TestAppsBackupsList_JSON_EmitsSingleEnvelopeUnderBackupsKey(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{backupsResult: sampleBackups()}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "list", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "backups list --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	backups, ok := data["backups"].([]any)
	require.True(t, ok, "envelope data must carry the snapshots under the backups key as an array")
	require.Len(t, backups, 2, "both snapshots must appear under backups")

	first, ok := backups[0].(map[string]any)
	require.True(t, ok, "each backups entry must be a JSON object")
	assert.Equal(t, "1717000000000000001-update", first["snapshot_id"])
	assert.Equal(t, "update", first["operation"])
}

// TestAppsBackupsList_JSON_EmptyResultNormalizesToEmptyArray pins the nil ->
// []types.BackupInfo normalization: a stack that never backed up must emit
// "backups": [], not "backups": null, so a jq/NDJSON consumer iterates a
// real empty array.
func TestAppsBackupsList_JSON_EmptyResultNormalizesToEmptyArray(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{backupsResult: nil}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "list", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "backups list --json must emit exactly one envelope even with no snapshots")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
	assert.NotContains(t, lines[0], `"backups":null`, "a nil list must normalize to an empty array, not null")

	data := decodeEnvelopeData(t, lines[0])
	backups, ok := data["backups"].([]any)
	require.True(t, ok, "backups key must decode to an array, not null")
	assert.Empty(t, backups, "a stack with no backups must emit an empty backups array")
}

// TestAppsBackupsList_Plain_EmitsTabSeparatedLines pins the plain-mode
// contract: one snapshot per line, newest first, as
// "<snapshot_id>\t<operation>\t<created_at>\t<N file(s)>", tab-separated so
// cut(1)/awk(1) parse the leading fields, and no envelope bytes.
func TestAppsBackupsList_Plain_EmitsTabSeparatedLines(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{backupsResult: sampleBackups()}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "list", "vaultwarden")
	require.NoError(t, err)

	assert.Equal(t,
		"1717000000000000001-update\tupdate\t2026-06-11T10:00:00Z\t3 file(s)\n"+
			"1716000000000000000-install\tinstall\t2026-06-10T09:00:00Z\t1 file(s)\n",
		stdout,
		"plain list must emit one tab-separated line per snapshot in newest-first order")
}

// TestAppsBackupsList_Plain_EmptyEmitsNothing pins that a stack with no
// backups exits 0 with empty stdout in plain mode (mirroring apps list on a
// fresh system).
func TestAppsBackupsList_Plain_EmptyEmitsNothing(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{backupsResult: nil}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "list", "vaultwarden")
	require.NoError(t, err)
	assert.Empty(t, stdout, "a stack with no backups must emit nothing on stdout in plain mode")
}

// TestAppsBackupsList_ErrorPath_StdoutEmpty pins that a typed engine error
// (e.g. the generic state-lister fault, or an unmanaged-stack refusal)
// propagates out of Execute with no envelope written, for both --json and
// plain mode.
func TestAppsBackupsList_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeGeneric, "config backups could not be listed", "check stack directory permissions and retry")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"apps", "backups", "list", "vaultwarden", "--json"}},
		{"plain", []string{"apps", "backups", "list", "vaultwarden"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// TestAppsBackupsList_RequiresExactlyOneArg pins cobra.ExactArgs(1): zero or
// two positional args fail before RunE, so the engine is never consulted.
func TestAppsBackupsList_RequiresExactlyOneArg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"no args", []string{"apps", "backups", "list"}},
		{"two args", []string{"apps", "backups", "list", "a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an arg-count refusal")
				return nil, nil
			})
			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "backups list must reject a wrong argument count")
			assert.Empty(t, outBuf.String(), "an arg-count failure must write nothing to stdout")
		})
	}
}

// --- apps backups restore ---

// TestAppsBackupsRestore_JSON_WrapsResultDirectly pins that
// `apps backups restore <app> <snapshot> --json` writes exactly one wdm.v1
// envelope whose data IS the RestoreBackupResult object directly, carrying
// the snake_case keys including restored_files / boundary_notice /
// next_action and the nested status object.
func TestAppsBackupsRestore_JSON_WrapsResultDirectly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{restoreResult: sampleRestoreResult()}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "backups restore --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"], "the RestoreBackupResult must be the envelope data directly")
	assert.Equal(t, "1717000000000000001-update", data["snapshot_id"])
	// RestoredFiles rides under its snake_case key as a JSON array.
	assert.Equal(t, []any{"docker-compose.yml", ".env", ".wdm.lock"}, data["restored_files"])
	// The boundary notice and next action ride under their snake_case keys.
	assert.Contains(t, data["boundary_notice"], "config restore restores only wdm config files")
	assert.Contains(t, data["next_action"], "wdm apps update")
	// The post-restore status is nested under "status" as an object.
	status, ok := data["status"].(map[string]any)
	require.True(t, ok, "result must nest the status object")
	assert.Equal(t, "running", status["state"])
}

// TestAppsBackupsRestore_JSON_SuppressesProgress_PlainWiresIt pins the PRD
// §32 progress contract: under --json the engine receives a nil ProgressFn;
// in plain mode it receives a non-nil one. Both modes hand the engine a
// non-nil Confirmer.
func TestAppsBackupsRestore_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{restoreResult: sampleRestoreResult()}
		stdout, _, err := runLeaf(t, fake, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--json")
		require.NoError(t, err)

		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
		assert.NotNil(t, fake.confirmer, "restore must pass a non-nil Confirmer to the engine")
		lines := nonEmptyLines(stdout)
		require.Len(t, lines, 1, "--json stdout must carry only the envelope")
		assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{restoreResult: sampleRestoreResult()}
		stdout, _, err := runLeaf(t, fake, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--yes")
		require.NoError(t, err)

		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.NotNil(t, fake.confirmer, "restore must pass a non-nil Confirmer to the engine")
		// Plain mode writes a human finish screen, not an envelope.
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

// TestAppsBackupsRestore_Plain_RendersFinishBlock pins the plain-mode finish
// screen on a clean result: the config-restore headline names the snapshot,
// the restored files are listed, the engine's boundary notice and recreate
// next-action are relayed verbatim, and the status state
// renders. The forbidden undo verb (forbiddenUndoVerb) must never appear.
func TestAppsBackupsRestore_Plain_RendersFinishBlock(t *testing.T) {
	t.Parallel()

	result := sampleRestoreResult()
	fake := &fakeEngine{restoreResult: result}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--yes")
	require.NoError(t, err)

	// Headline names the snapshot and asserts a clean config restore.
	assert.Contains(t, stdout, "vaultwarden config restored from snapshot 1717000000000000001-update")
	assert.NotContains(t, stdout, "see the status below", "a clean result must not render the neutral needs-attention headline")

	// Restored config files listed.
	assert.Contains(t, stdout, "Restored config files:")
	assert.Contains(t, stdout, "- docker-compose.yml")
	assert.Contains(t, stdout, "- .env")
	assert.Contains(t, stdout, "- .wdm.lock")

	// Boundary notice relayed verbatim from the engine.
	assert.Contains(t, stdout, result.BoundaryNotice,
		"the engine's config-restore boundary notice must be relayed verbatim")

	// Recreate next-action surfaced prominently and verbatim.
	assert.Contains(t, stdout, result.NextAction,
		"the recreate next-action must be surfaced verbatim from the engine")
	assert.Contains(t, stdout, "Next:", "the next-action must be labeled so the user sees the recreate guidance")

	// Status state shown.
	assert.Contains(t, stdout, "Status: running")

	// Wording invariant: the forbidden undo verb must never
	// appear in the rendered copy. The token is assembled at runtime
	// (forbiddenUndoVerb) so this file holds no literal of it.
	assert.NotContains(t, strings.ToLower(stdout), forbiddenUndoVerb,
		"config-restore copy must never use the forbidden undo verb")
}

// TestAppsBackupsRestore_NeedsAttention_ExitsZeroAndRendersNeutralHeadline
// pins that a needs_attention result is a successful restore (exit 0) and
// that plain mode renders the neutral headline rather than asserting a clean
// restore — the gate mirrors writeRemoveFinish / writeRestartFinish.
func TestAppsBackupsRestore_NeedsAttention_ExitsZeroAndRendersNeutralHeadline(t *testing.T) {
	t.Parallel()

	result := sampleRestoreResult()
	result.Status = &types.AppStatus{
		AppID:          "vaultwarden",
		State:          "needs_attention",
		NeedsAttention: true,
		Message:        "post-restore status verification found issues; run apps status for details",
	}
	fake := &fakeEngine{restoreResult: result}

	stdout, _, err := runLeaf(t, fake, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--yes")
	require.NoError(t, err, "a needs_attention restore is a successful operation and must not error")

	assert.Contains(t, stdout, "see the status below", "needs-attention headline must defer to the status block")
	assert.Contains(t, stdout, "needs_attention", "the status state must be shown")
	// The recreate next-action must still surface — it is the the invariant
	// correctness contract regardless of post-restore health.
	assert.Contains(t, stdout, result.NextAction, "the recreate next-action must surface even on a needs-attention result")
}

// TestAppsBackupsRestore_ErrorPath_StdoutEmpty pins that a typed engine
// error propagates out of Execute with no envelope/finish written, for both
// --json and plain mode.
func TestAppsBackupsRestore_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "backup snapshot was not found for this app", "run wdm apps backups list")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"apps", "backups", "restore", "vaultwarden", "nope", "--json"}},
		{"plain", []string{"apps", "backups", "restore", "vaultwarden", "nope", "--yes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// TestAppsBackupsRestore_RequiresExactlyTwoArgs pins cobra.ExactArgs(2):
// fewer or more positional args fail before RunE, so the engine is never
// consulted.
func TestAppsBackupsRestore_RequiresExactlyTwoArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"no args", []string{"apps", "backups", "restore"}},
		{"one arg", []string{"apps", "backups", "restore", "vaultwarden"}},
		{"three args", []string{"apps", "backups", "restore", "a", "b", "c"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an arg-count refusal")
				return nil, nil
			})
			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "backups restore must reject a wrong argument count")
			assert.Empty(t, outBuf.String(), "an arg-count failure must write nothing to stdout")
		})
	}
}

// --- --stack-path mapping (recording wrapper) ---

// recordingRestoreEngine embeds *fakeEngine and overrides RestoreBackup to
// record the request it received and to actually drive the confirmer with a
// SAFE "restore_config" confirmation (the base fakeEngine never calls
// Confirm). Every other method is inherited, so engine.Engine stays
// satisfied. The shared fake_engine_test.go stays untouched (the per-track
// recording-wrapper precedent).
type recordingRestoreEngine struct {
	*fakeEngine
	gotReq            types.RestoreBackupRequest
	confirmOnInvoke   bool // when set, RestoreBackup calls confirmer.Confirm with a restore_config payload
	confirmAccepted   bool // the bool the confirmer returned
	confirmInvokedErr error
}

// restoreConfirmationKind is the SAFE config-restore confirmation kind the
// engine emits (internal/core/backups.go:533). It is byte-equal to the
// engine literal so a drift would fail this test loudly. The CLI never
// re-declares it — the cliConfirmer routes any Kind != the database-risk
// literal through the safe-confirmation arm — so this local copy exists only
// to drive the wrapper's confirm probe.
const restoreConfirmationKind = "restore_config"

func (r *recordingRestoreEngine) RestoreBackup(
	ctx context.Context,
	req types.RestoreBackupRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestoreBackupResult, error) {
	r.gotReq = req
	if r.confirmOnInvoke && confirmer != nil {
		r.confirmAccepted, r.confirmInvokedErr = confirmer.Confirm(ctx, types.Confirmation{
			Kind:    restoreConfirmationKind,
			Title:   "config restore " + req.AppID,
			Message: "this is a config restore: it rewrites wdm config files only",
		})
	}
	return r.fakeEngine.RestoreBackup(ctx, req, onProgress, confirmer)
}

// runRestoreLeaf drives one `apps backups restore` invocation through
// NewRootCmd with the recording engine wired as the lazy factory result,
// mirroring runLeaf but typed to the local wrapper (runLeaf returns
// *fakeEngine).
func runRestoreLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return eng, nil
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(args)
	root.SetContext(t.Context())

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestAppsBackupsRestore_MapsArgsAndStackPathOntoRequest pins that the two
// positional args map onto RestoreBackupRequest.AppID /.SnapshotID and that
// --stack-path maps verbatim onto RestoreBackupRequest.StackPath.
func TestAppsBackupsRestore_MapsArgsAndStackPathOntoRequest(t *testing.T) {
	t.Parallel()

	rec := &recordingRestoreEngine{
		fakeEngine: &fakeEngine{restoreResult: sampleRestoreResult()},
	}

	_, _, err := runRestoreLeaf(t, rec, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update",
		"--stack-path", "/home/test/docker/vaultwarden", "--json")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", rec.gotReq.AppID, "the first positional arg must map onto RestoreBackupRequest.AppID")
	assert.Equal(t, "1717000000000000001-update", rec.gotReq.SnapshotID,
		"the second positional arg must map onto RestoreBackupRequest.SnapshotID")
	assert.Equal(t, "/home/test/docker/vaultwarden", rec.gotReq.StackPath,
		"--stack-path must map verbatim onto RestoreBackupRequest.StackPath")
}

// TestAppsBackupsRestore_OmittedStackPathIsEmpty pins that an omitted
// --stack-path leaves RestoreBackupRequest.StackPath empty (the engine's
// "resolve by app id, no cross-check" signal).
func TestAppsBackupsRestore_OmittedStackPathIsEmpty(t *testing.T) {
	t.Parallel()

	rec := &recordingRestoreEngine{
		fakeEngine: &fakeEngine{restoreResult: sampleRestoreResult()},
	}

	_, _, err := runRestoreLeaf(t, rec, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--json")
	require.NoError(t, err)

	assert.Empty(t, rec.gotReq.StackPath, "an omitted --stack-path must leave RestoreBackupRequest.StackPath empty")
}

// --- --yes accepts the SAFE restore_config confirmation (real cliConfirmer) ---

// TestAppsBackupsRestore_Yes_AcceptsSafeRestoreConfirmation pins that --yes,
// wired through the real cliConfirmer the leaf constructs (acceptDBRisk
// false), accepts the SAFE "restore_config" confirmation. The recording
// wrapper drives confirmer.Confirm with a restore_config payload, and the
// assertion is that the confirmer returned (true, nil) — proving the
// safe-confirmation --yes arm in confirm.go satisfies this Kind, the
func TestAppsBackupsRestore_Yes_AcceptsSafeRestoreConfirmation(t *testing.T) {
	t.Parallel()

	rec := &recordingRestoreEngine{
		fakeEngine:      &fakeEngine{restoreResult: sampleRestoreResult()},
		confirmOnInvoke: true,
	}

	_, _, err := runRestoreLeaf(t, rec, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update", "--yes")
	require.NoError(t, err)

	require.NoError(t, rec.confirmInvokedErr, "the real cliConfirmer must not error on a --yes safe confirmation")
	assert.True(t, rec.confirmAccepted,
		"--yes must accept the SAFE restore_config confirmation through the real cliConfirmer")
}

// TestAppsBackupsRestore_NoYesNoTTY_DeclinesEmptyStdoutTypedError pins the
// fail-closed decline: without --yes and without a TTY (runLeaf wires an
// empty buffer for stdin, never a terminal), the real cliConfirmer declines
// the safe confirmation as (false, nil), the engine maps that to
// ErrCodeUserCanceled, and the leaf returns a typed error with empty stdout.
func TestAppsBackupsRestore_NoYesNoTTY_DeclinesEmptyStdoutTypedError(t *testing.T) {
	t.Parallel()

	// A wrapper that drives the confirmer like the engine would and maps a
	// decline to ErrCodeUserCanceled, mirroring internal/core's
	// confirmRestoreBackup, so the leaf's no-TTY-no-yes path is exercised
	// end-to-end through the real cliConfirmer.
	rec := &decliningRestoreEngine{fakeEngine: &fakeEngine{restoreResult: sampleRestoreResult()}}

	stdout, _, err := runRestoreLeaf(t, rec, "apps", "backups", "restore", "vaultwarden", "1717000000000000001-update")

	require.Error(t, err, "a declined safe confirmation must surface a typed error out of Execute")
	var typed *types.Error
	require.ErrorAs(t, err, &typed, "the decline must be a typed *types.Error")
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code, "a declined config restore must map to ErrCodeUserCanceled (exit 7)")
	assert.Empty(t, stdout, "a declined config restore must write nothing to stdout")
}

// decliningRestoreEngine drives the leaf's real cliConfirmer and faithfully
// reproduces the engine's confirmRestoreBackup decline mapping: a (false,
// nil) from the confirmer becomes ErrCodeUserCanceled. It proves the leaf's
// no-TTY/no-yes path reaches the confirmer and that a decline surfaces as
// exit 7 with empty stdout, without depending on a real internal/core
// instance.
type decliningRestoreEngine struct {
	*fakeEngine
}

func (d *decliningRestoreEngine) RestoreBackup(
	ctx context.Context,
	req types.RestoreBackupRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestoreBackupResult, error) {
	confirmed, err := confirmer.Confirm(ctx, types.Confirmation{
		Kind:    restoreConfirmationKind,
		Title:   "config restore " + req.AppID,
		Message: "this is a config restore: it rewrites wdm config files only",
	})
	if err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, types.NewError(
			types.ErrCodeUserCanceled,
			"config restore canceled before any file was rewritten",
			"re-run the config restore and confirm the prompt",
		)
	}
	return d.fakeEngine.RestoreBackup(ctx, req, onProgress, confirmer)
}

// --- shared invariants ---

// TestAppsBackups_FactoryNotInvokedOnHelp pins the PRD §14 self-update
// smoke-check invariant for both leaves and the group: --help exits 0 and
// never constructs the engine (the factory is consulted only inside RunE).
func TestAppsBackups_FactoryNotInvokedOnHelp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"group help", []string{"apps", "backups", "--help"}},
		{"list help", []string{"apps", "backups", "list", "--help"}},
		{"restore help", []string{"apps", "backups", "restore", "--help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed for --help")
				return nil, nil
			})
			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.NoError(t, err, "--help must exit 0 without constructing the engine")
		})
	}
}

// TestAppsBackups_FactoryError_Propagates pins that a failed engine factory
// surfaces out of Execute and never produces output for both leaves — each
// leaf builds the engine inside RunE, so a construction failure is the first
// thing it can hit after the --json read (mirroring the apps-list /
// catalog-list factory-error precedent).
func TestAppsBackups_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"list", []string{"apps", "backups", "list", "vaultwarden"}},
		{"restore", []string{"apps", "backups", "restore", "vaultwarden", "1717000000000000001-update"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			factoryErr := errors.New("engine factory failed")
			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, factoryErr
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err)
			assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
			assert.Empty(t, outBuf.String(), "no output may be written when the engine cannot be built")
		})
	}
}
