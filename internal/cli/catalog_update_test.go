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

// These tests pin the behavior of the `catalog check` and `catalog update`
// leaves (PRD §22): the wdm.v1 envelope each emits under
// --json, the plain-mode block shape, the request mapping, the progress
// suppression contract, the confirmer wiring (including --yes accepting the
// SAFE catalog_update kind), the empty-stdout error path, and the
// --help-builds-no-engine invariant. They reuse the envelope decode helpers
// (envelope_contract_test.go) and the runCatalogLeaf runner (catalog_test.go),
// plus a local recording double for the two catalog-update methods so they
// can record the request/query and drive the real cliConfirmer — the shared
// fakeEngine (fake_engine_test.go) stays untouched, the per-track
// recording-wrapper precedent recordingCatalogEngine already established.

// catalogUpdateEngine is a recording [engine.Engine] for the catalog-update
// leaves. It embeds *fakeEngine to inherit the full interface surface (the
// interface-satisfaction proof and every other method), and overrides the
// two catalog-update methods so a test can:
//   - record the query/request the leaf built (verbatim flag mapping);
//   - optionally invoke the passed Confirmer with a catalog_update
//     Confirmation (the path the shared double deliberately skips), so a
//     test can prove --yes drives the SAFE kind to acceptance through the
//     real cliConfirmer without a TTY.
type catalogUpdateEngine struct {
	*fakeEngine

	// Recorded inputs.
	checkQuery types.CatalogUpdateQuery
	applyReq   types.CatalogUpdateRequest

	// invokeConfirmer makes ApplyCatalogUpdate call the passed Confirmer
	// with a catalog_update Confirmation and honor its verdict (a decline
	// becomes ErrCodeUserCanceled, mirroring the engine). When false, the
	// apply just returns its configured result, like the embedded double.
	invokeConfirmer bool
	confirmAccepted bool
}

func (e *catalogUpdateEngine) CheckCatalogUpdate(
	_ context.Context, query types.CatalogUpdateQuery,
) (*types.CatalogUpdateStatus, error) {
	e.checkQuery = query
	if e.err != nil {
		return nil, e.err
	}
	return e.catalogUpdateStatus, nil
}

func (e *catalogUpdateEngine) ApplyCatalogUpdate(
	ctx context.Context,
	req types.CatalogUpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	e.applyReq = req
	e.progressWasNil = onProgress == nil
	e.confirmer = confirmer
	if e.err != nil {
		return nil, e.err
	}
	if e.invokeConfirmer {
		accepted, cerr := confirmer.Confirm(ctx, types.Confirmation{
			Kind:    types.ConfirmationKindCatalogUpdate,
			Title:   "update stable catalog",
			Message: "install the verified catalog?",
		})
		if cerr != nil {
			return nil, cerr
		}
		e.confirmAccepted = accepted
		if !accepted {
			// Model the engine mapping a decline to ErrCodeUserCanceled.
			return nil, types.NewError(types.ErrCodeUserCanceled, "catalog update canceled", "confirm the prompt")
		}
	}
	return e.catalogUpdateResult, nil
}

// newCatalogUpdateEngine builds a catalogUpdateEngine wrapping a fresh
// fakeEngine so tests can set the embedded result/error fields inline.
func newCatalogUpdateEngine() *catalogUpdateEngine {
	return &catalogUpdateEngine{fakeEngine: &fakeEngine{}}
}

// --- catalog check: --json wraps CatalogUpdateStatus directly as data. ---

func TestCatalogCheck_JSON_WrapsStatusDirectly(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateStatus = &types.CatalogUpdateStatus{
		Channel:         "stable",
		CurrentVersion:  "2026-06-01T00:00:00Z",
		LatestVersion:   "2026-06-13T00:00:00Z",
		UpdateAvailable: true,
		Verified:        true,
		CheckedAt:       time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC),
		Changes: []types.CatalogChange{
			{AppID: "n8n", Kind: "updated", Summary: "template version 1 -> 2"},
		},
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "check", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "catalog check --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	// CatalogUpdateStatus IS the envelope.data object — channel and the
	// boolean flags are direct keys, not nested under a "status" wrapper.
	assert.Equal(t, "stable", data["channel"])
	assert.Equal(t, "2026-06-01T00:00:00Z", data["current_version"])
	assert.Equal(t, "2026-06-13T00:00:00Z", data["latest_version"])
	assert.Equal(t, true, data["update_available"])
	assert.Equal(t, true, data["verified"])
	assert.NotContains(t, data, "status", "CatalogUpdateStatus must be data directly, not nested")
}

// --- catalog check: plain mode renders a scannable status block. ---

func TestCatalogCheck_Plain_RendersStatusBlock(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateStatus = &types.CatalogUpdateStatus{
		Channel:         "stable",
		CurrentVersion:  "2026-06-01T00:00:00Z",
		LatestVersion:   "2026-06-13T00:00:00Z",
		UpdateAvailable: true,
		Verified:        true,
		Changes: []types.CatalogChange{
			{AppID: "n8n", Kind: "updated", Summary: "template version 1 -> 2"},
		},
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "check")
	require.NoError(t, err)

	assert.Contains(t, stdout, "channel\tstable")
	assert.Contains(t, stdout, "current\t2026-06-01T00:00:00Z")
	assert.Contains(t, stdout, "latest\t2026-06-13T00:00:00Z")
	assert.Contains(t, stdout, "update available\tyes")
	assert.Contains(t, stdout, "verified\tyes")
	assert.Contains(t, stdout, "Changes:")
	assert.Contains(t, stdout, "n8n\tupdated\ttemplate version 1 -> 2")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the status block, not a JSON envelope")
}

// TestCatalogCheck_Plain_NoLocalCatalog_RendersNone pins the empty-version
// rendering: a never-installed local catalog (empty CurrentVersion) shows
// "(none)" rather than a blank field, and a no-changes result renders no
// Changes block.
func TestCatalogCheck_Plain_NoLocalCatalog_RendersNone(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateStatus = &types.CatalogUpdateStatus{
		Channel:         "stable",
		CurrentVersion:  "",
		LatestVersion:   "2026-06-13T00:00:00Z",
		UpdateAvailable: true,
		Verified:        false,
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "check")
	require.NoError(t, err)

	assert.Contains(t, stdout, "current\t(none)")
	assert.Contains(t, stdout, "verified\tno")
	assert.NotContains(t, stdout, "Changes:", "no changes means no Changes block")
}

// --- catalog check: --channel maps verbatim onto the query. ---

func TestCatalogCheck_ChannelFlagMapsToQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"default_empty", []string{"catalog", "check", "--json"}, ""},
		{"explicit_stable", []string{"catalog", "check", "--channel", "stable", "--json"}, "stable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := newCatalogUpdateEngine()
			eng.catalogUpdateStatus = &types.CatalogUpdateStatus{Channel: "stable"}
			_, _, err := runCatalogLeaf(t, eng, tc.args...)
			require.NoError(t, err)
			assert.Equal(t, tc.want, eng.checkQuery.Channel, "the --channel flag must map verbatim onto the query")
		})
	}
}

// --- catalog check: read-only — no Confirmer ever. ---

func TestCatalogCheck_ReadOnly_NoConfirmer(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateStatus = &types.CatalogUpdateStatus{Channel: "stable"}
	// Plain mode is the path that would wire a ProgressFn for a write leaf;
	// the read-only check must still pass no Confirmer (the leaf never builds
	// one and never reaches a confirmer-taking engine method).
	_, _, err := runCatalogLeaf(t, eng, "catalog", "check")
	require.NoError(t, err)
	assert.Nil(t, eng.confirmer, "catalog check is read-only and must pass no Confirmer")
}

// --- catalog update: --json wraps CatalogUpdateResult directly as data. ---

func TestCatalogUpdate_JSON_WrapsResultDirectly(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateResult = &types.CatalogUpdateResult{
		Channel:            "stable",
		PreviousVersion:    "2026-06-01T00:00:00Z",
		AppliedVersion:     "2026-06-13T00:00:00Z",
		VerificationDetail: "checksum, detached signature, and attestation verified",
		AppliedAt:          time.Date(2026, 6, 13, 9, 30, 0, 0, time.UTC),
		Changes: []types.CatalogChange{
			{AppID: "n8n", Kind: "updated", Summary: "template version 1 -> 2"},
		},
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "catalog update --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "stable", data["channel"])
	assert.Equal(t, "2026-06-01T00:00:00Z", data["previous_version"])
	assert.Equal(t, "2026-06-13T00:00:00Z", data["applied_version"])
	assert.Equal(t, "checksum, detached signature, and attestation verified", data["verification_detail"])
	assert.NotContains(t, data, "result", "CatalogUpdateResult must be data directly, not nested")
}

// --- catalog update: plain mode renders a finish block. ---

func TestCatalogUpdate_Plain_RendersFinishBlock(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateResult = &types.CatalogUpdateResult{
		Channel:            "stable",
		PreviousVersion:    "2026-06-01T00:00:00Z",
		AppliedVersion:     "2026-06-13T00:00:00Z",
		VerificationDetail: "attestation verified for release v3 (bundle sha256 abc)",
		Changes: []types.CatalogChange{
			{AppID: "freshrss", Kind: "added", Summary: "new app at template version 1"},
		},
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "channel\tstable")
	assert.Contains(t, stdout, "updated\t2026-06-01T00:00:00Z -> 2026-06-13T00:00:00Z")
	assert.Contains(t, stdout, "verification\tattestation verified for release v3 (bundle sha256 abc)")
	assert.Contains(t, stdout, "Changes:")
	assert.Contains(t, stdout, "freshrss\tadded\tnew app at template version 1")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the finish block, not a JSON envelope")
}

// TestCatalogUpdate_Plain_NoLocalCatalog_RendersNone pins the first-install
// transition: an empty PreviousVersion renders "(none)" on the left of the
// version transition.
func TestCatalogUpdate_Plain_NoLocalCatalog_RendersNone(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateResult = &types.CatalogUpdateResult{
		Channel:         "stable",
		PreviousVersion: "",
		AppliedVersion:  "2026-06-13T00:00:00Z",
	}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes")
	require.NoError(t, err)
	assert.Contains(t, stdout, "updated\t(none) -> 2026-06-13T00:00:00Z")
}

// --- catalog update: --channel and --target-version map verbatim. ---

func TestCatalogUpdate_FlagsMapToRequest(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}
	_, _, err := runCatalogLeaf(t, eng,
		"catalog", "update", "--yes", "--json",
		"--channel", "stable", "--target-version", "2026-06-13T00:00:00Z")
	require.NoError(t, err)

	assert.Equal(t, "stable", eng.applyReq.Channel, "the --channel flag must map verbatim onto the request")
	assert.Equal(t, "2026-06-13T00:00:00Z", eng.applyReq.TargetVersion,
		"the --target-version flag must map verbatim onto the request")
}

// --- catalog update: progress suppressed under --json, wired in plain. ---

func TestCatalogUpdate_ProgressSuppression_JSONNilPlainNonNil(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()
		eng := newCatalogUpdateEngine()
		eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}
		stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes", "--json")
		require.NoError(t, err)
		assert.True(t, eng.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
		lines := nonEmptyLines(stdout)
		require.Len(t, lines, 1, "--json stdout must carry only the envelope")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()
		eng := newCatalogUpdateEngine()
		eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}
		_, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes")
		require.NoError(t, err)
		assert.False(t, eng.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
	})
}

// --- catalog update: a non-nil Confirmer is always passed. ---

func TestCatalogUpdate_PassesConfirmer(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}
	_, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes", "--json")
	require.NoError(t, err)
	assert.NotNil(t, eng.confirmer, "catalog update must pass a non-nil Confirmer to the engine")
}

// --- catalog update: --yes accepts the SAFE catalog_update confirmation. ---

func TestCatalogUpdate_Yes_AcceptsCatalogUpdateConfirmation(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.invokeConfirmer = true
	eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}

	// --yes with no TTY (runCatalogLeaf wires an empty stdin buffer): the
	// SAFE catalog_update kind must still be accepted, so the apply succeeds
	// through the real cliConfirmer.
	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--yes", "--json")
	require.NoError(t, err, "--yes must accept the SAFE catalog_update confirmation")
	assert.True(t, eng.confirmAccepted, "the confirmer must report acceptance under --yes")

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "a successful apply emits exactly one envelope")
}

// TestCatalogUpdate_NoYes_NonTTY_RefusesConfirmation pins the fail-closed
// posture: no --yes on a non-TTY refuses, which the engine maps to exit 7.
func TestCatalogUpdate_NoYes_NonTTY_RefusesConfirmation(t *testing.T) {
	t.Parallel()

	eng := newCatalogUpdateEngine()
	eng.invokeConfirmer = true
	eng.catalogUpdateResult = &types.CatalogUpdateResult{Channel: "stable", AppliedVersion: "v"}

	stdout, _, err := runCatalogLeaf(t, eng, "catalog", "update", "--json")
	require.Error(t, err, "no --yes on a non-TTY must refuse")
	var typeErr *types.Error
	require.ErrorAs(t, err, &typeErr)
	assert.Equal(t, types.ErrCodeUserCanceled, typeErr.Code,
		"a refused confirmation maps to ErrCodeUserCanceled (exit 7)")
	assert.False(t, eng.confirmAccepted, "the confirmer must report refusal without --yes on a non-TTY")
	assert.Empty(t, stdout, "a refused apply writes nothing to stdout")
}

// --- catalog check / update: empty-stdout error path. ---

func TestCatalogLeaves_ErrorPath_JSON_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeNetworkFailure, "catalog endpoint unreachable", "check connectivity")

	cases := []struct {
		name string
		args []string
	}{
		{"check", []string{"catalog", "check", "--json"}},
		{"update", []string{"catalog", "update", "--yes", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := newCatalogUpdateEngine()
			eng.err = engineErr
			stdout, _, err := runCatalogLeaf(t, eng, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no envelope may be written to stdout on the error path")
		})
	}
}

// --- catalog check / update: --help builds no engine (PRD §14). ---

func TestCatalogLeaves_Help_DoesNotBuildEngine(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("factory must not be called for --help")

	cases := []struct {
		name string
		args []string
	}{
		{"check_help", []string{"catalog", "check", "--help"}},
		{"update_help", []string{"catalog", "update", "--help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, sentinel
			})
			root.SetArgs(tc.args)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetContext(t.Context())

			err := root.Execute()
			require.NoError(t, err, "--help must succeed without building the engine")
		})
	}
}
