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

// These tests pin the `self-update check` / `self-update apply` CLI leaves
// (PRD §14). They follow the established internal/cli idioms —
// driving NewRootCmd through runLeaf with the recording fakeEngine, the
// wdm.v1 envelope assertions from envelope_contract_test.go, and the
// finish-screen + confirmer wiring + sentinel-factory --help proof from
// backups_test.go. The review focus is the PRD §14 invariant: --help (and
// --version) must never construct the engine or touch the network.

// selfUpdateConfirmationKind is the SAFE self-update confirmation kind the
// engine emits (internal/core/self_update_apply.go via
// types.ConfirmationKindSelfUpdate). It is byte-equal to the engine literal
// so a drift would fail this test loudly. The CLI never re-declares it — the
// cliConfirmer routes any Kind != the database-risk literal through the safe
// confirmation arm — so this local copy exists only to drive the recording
// wrapper's confirm probe.
const selfUpdateConfirmationKind = types.ConfirmationKindSelfUpdate

// sampleSelfUpdateStatus is a representative read-only check result with an
// update available, verified, and a realistic operator-guidance note. (The
// check path never probes writability — it only ever appends the dev-build
// note — so the fixture seeds a neutral operator note rather than a
// writability-shaped one.)
func sampleSelfUpdateStatus() *types.SelfUpdateStatus {
	return &types.SelfUpdateStatus{
		CurrentVersion:  "v1.0.0",
		LatestVersion:   "v1.1.0",
		UpdateAvailable: true,
		Verified:        true,
		CheckedAt:       time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
		Notes:           []string{"run 'wdm self-update apply' to install the verified update"},
	}
}

// sampleSelfUpdateResult is a representative successful apply result: the
// binary was replaced, the smoke check passed, and the previous binary is
// retained for rollback.
func sampleSelfUpdateResult() *types.SelfUpdateResult {
	return &types.SelfUpdateResult{
		PreviousVersion:    "v1.0.0",
		AppliedVersion:     "v1.1.0",
		Replaced:           true,
		SmokeOK:            true,
		RolledBack:         false,
		PreviousBinaryPath: "/home/test/.local/bin/wdm.previous",
		Message:            "updated wdm v1.0.0 -> v1.1.0; the previous binary is kept at /home/test/.local/bin/wdm.previous",
	}
}

// --- self-update check: plain output. ---

// TestSelfUpdateCheck_Plain_RendersStatusBlock pins the plain-mode status
// block: current/latest versions, the update-available and verified yes/no
// flags, and the operator-guidance notes the engine attached.
func TestSelfUpdateCheck_Plain_RendersStatusBlock(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{selfUpdateStatus: sampleSelfUpdateStatus()}

	stdout, _, err := runLeaf(t, fake, "self-update", "check")
	require.NoError(t, err)

	assert.Contains(t, stdout, "current version\tv1.0.0")
	assert.Contains(t, stdout, "latest version\tv1.1.0")
	assert.Contains(t, stdout, "update available\tyes")
	assert.Contains(t, stdout, "verified\tyes")
	assert.Contains(t, stdout, "Notes:")
	assert.Contains(t, stdout, "run 'wdm self-update apply' to install the verified update")

	// Plain mode must not emit a JSON envelope.
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the status block, not a JSON envelope")
}

// TestSelfUpdateCheck_Plain_DevBuildNoUpdate pins the no-update shape: the
// flags read "no" and an empty latest version renders the placeholder.
func TestSelfUpdateCheck_Plain_DevBuildNoUpdate(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{selfUpdateStatus: &types.SelfUpdateStatus{
		CurrentVersion:  "dev",
		LatestVersion:   "",
		UpdateAvailable: false,
		Verified:        false,
		Notes:           []string{"running a development build; self-update is not offered for dev binaries"},
	}}

	stdout, _, err := runLeaf(t, fake, "self-update", "check")
	require.NoError(t, err)

	assert.Contains(t, stdout, "current version\tdev")
	assert.Contains(t, stdout, "latest version\t(none)")
	assert.Contains(t, stdout, "update available\tno")
	assert.Contains(t, stdout, "verified\tno")
	assert.Contains(t, stdout, "development build")
}

// --- self-update check: JSON wraps SelfUpdateStatus directly. ---

// TestSelfUpdateCheck_JSON_WrapsStatusDirectly pins that --json emits exactly
// one wdm.v1 envelope wrapping the SelfUpdateStatus directly (no extra
// nesting), mirroring how `apps status` wraps its AppStatus.
func TestSelfUpdateCheck_JSON_WrapsStatusDirectly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{selfUpdateStatus: sampleSelfUpdateStatus()}

	stdout, _, err := runLeaf(t, fake, "self-update", "check", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "check --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "v1.0.0", data["current_version"], "the SelfUpdateStatus must be the envelope data directly")
	assert.Equal(t, "v1.1.0", data["latest_version"])
	assert.Equal(t, true, data["update_available"])
	assert.Equal(t, true, data["verified"])
	// No extra "status" nesting (the AppStatus precedent).
	assert.NotContains(t, data, "status", "check --json must wrap SelfUpdateStatus directly, not nest it")
}

// --- self-update apply: plain finish block. ---

// TestSelfUpdateApply_Plain_RendersFinishBlock pins the plain-mode finish
// block on a clean apply: the version transition, the replaced/smoke/rolled-
// back flags, the engine's message, and the retained wdm.previous path.
func TestSelfUpdateApply_Plain_RendersFinishBlock(t *testing.T) {
	t.Parallel()

	result := sampleSelfUpdateResult()
	fake := &fakeEngine{selfUpdateResult: result}

	stdout, _, err := runLeaf(t, fake, "self-update", "apply", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "wdm self-update\tv1.0.0 -> v1.1.0")
	assert.Contains(t, stdout, "replaced\tyes")
	assert.Contains(t, stdout, "smoke check\tok")
	assert.Contains(t, stdout, "rolled back\tno")
	assert.Contains(t, stdout, result.Message)
	assert.Contains(t, stdout, "previous binary kept at\t/home/test/.local/bin/wdm.previous")

	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the finish block, not a JSON envelope")
}

// TestSelfUpdateApply_Plain_RolledBackReportedClearly pins that a FAILED
// smoke check that rolled back is rendered clearly on stdout (rolled back:
// yes, smoke check: failed, plus the rollback message) and is NEVER hidden as
// success. ApplySelfUpdate is the one engine method that returns a non-nil
// result alongside a non-nil error on this path, so the leaf
// surfaces the finish block BEFORE returning the error and exiting nonzero.
func TestSelfUpdateApply_Plain_RolledBackReportedClearly(t *testing.T) {
	t.Parallel()

	result := &types.SelfUpdateResult{
		PreviousVersion: "v1.0.0",
		AppliedVersion:  "v1.1.0",
		Replaced:        false,
		SmokeOK:         false,
		RolledBack:      true,
		Message:         "self-update to v1.1.0 failed its smoke check; the previous binary (v1.0.0) was restored",
	}
	rollbackErr := types.NewError(
		types.ErrCodeGeneric,
		"self-update failed its smoke check and was rolled back",
		"the previous binary was restored; no action is needed",
	)
	// The recording wrapper returns BOTH the result and the error together —
	// the rollback contract the shared fakeEngine cannot express.
	rec := &recordingSelfUpdateEngine{
		fakeEngine:     &fakeEngine{},
		rollbackResult: result,
		rollbackErr:    rollbackErr,
	}

	stdout, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply", "--yes")

	// The rollback still propagates as a nonzero-exit error.
	require.Error(t, err, "a rolled-back self-update must propagate a non-nil error")
	assert.ErrorIs(t, err, rollbackErr)

	// And the finish block is rendered on stdout on the error path so the
	// rollback is reported, never hidden.
	assert.Contains(t, stdout, "rolled back\tyes", "a rollback must be reported on stdout, never hidden")
	assert.Contains(t, stdout, "smoke check\tfailed", "a failed smoke check must read 'failed'")
	assert.Contains(t, stdout, "replaced\tno")
	assert.Contains(t, stdout, result.Message)
}

// TestSelfUpdateApply_JSON_RolledBackLeafEmitsEnvelope pins the --json arm of
// the the invariant rollback path: the leaf emits a single wdm.v1 envelope
// whose data carries rolled_back:true on stdout BEFORE returning the error, so
// a JSON consumer sees the structured rollback even though the command exits
// nonzero. This is the deliberate PRD §32 exception — an ordinary error keeps
// stdout empty (pinned by TestSelfUpdate_ErrorPath_StdoutEmpty), but a
// rolled-back apply is a completed operation whose outcome the contract
// requires reporting.
func TestSelfUpdateApply_JSON_RolledBackLeafEmitsEnvelope(t *testing.T) {
	t.Parallel()

	result := &types.SelfUpdateResult{
		PreviousVersion: "v1.0.0",
		AppliedVersion:  "v1.1.0",
		Replaced:        false,
		SmokeOK:         false,
		RolledBack:      true,
		Message:         "self-update to v1.1.0 failed its smoke check; the previous binary (v1.0.0) was restored",
	}
	rollbackErr := types.NewError(
		types.ErrCodeGeneric,
		"self-update failed its smoke check and was rolled back",
		"the previous binary was restored; no action is needed",
	)
	rec := &recordingSelfUpdateEngine{
		fakeEngine:     &fakeEngine{},
		rollbackResult: result,
		rollbackErr:    rollbackErr,
	}

	stdout, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply", "--yes", "--json")

	require.Error(t, err, "a rolled-back self-update must propagate a non-nil error")
	assert.ErrorIs(t, err, rollbackErr)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "the rollback path must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, true, data["rolled_back"], "the --json rollback envelope must carry rolled_back:true")
	assert.Equal(t, false, data["replaced"])
	assert.Equal(t, false, data["smoke_ok"])
}

// --- self-update apply: JSON wraps SelfUpdateResult directly. ---

// TestSelfUpdateApply_JSON_WrapsResultDirectly pins that --json emits exactly
// one envelope wrapping the SelfUpdateResult directly, carrying the
// snake_case keys including the always-serialized replaced / smoke_ok /
// rolled_back booleans.
func TestSelfUpdateApply_JSON_WrapsResultDirectly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()}

	stdout, _, err := runLeaf(t, fake, "self-update", "apply", "--yes", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "apply --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "v1.0.0", data["previous_version"], "the SelfUpdateResult must be the envelope data directly")
	assert.Equal(t, "v1.1.0", data["applied_version"])
	assert.Equal(t, true, data["replaced"])
	assert.Equal(t, true, data["smoke_ok"])
	assert.Equal(t, false, data["rolled_back"])
	assert.Equal(t, "/home/test/.local/bin/wdm.previous", data["previous_binary_path"])
}

// TestSelfUpdateApply_JSON_RolledBackSerializes pins that a rolled-back result
// serializes rolled_back:true / replaced:false / smoke_ok:false so a JSON
// consumer sees the rollback explicitly even when the apply "succeeded" enough
// to produce a result object.
func TestSelfUpdateApply_JSON_RolledBackSerializes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, EmitJSON(&buf, &types.SelfUpdateResult{
		PreviousVersion: "v1.0.0",
		AppliedVersion:  "v1.1.0",
		Replaced:        false,
		SmokeOK:         false,
		RolledBack:      true,
	}))

	data := decodeEnvelopeData(t, strings.TrimSpace(buf.String()))
	assert.Equal(t, false, data["replaced"])
	assert.Equal(t, false, data["smoke_ok"])
	assert.Equal(t, true, data["rolled_back"], "a rollback must serialize rolled_back:true")
}

// --- Progress suppression + confirmer wiring (PRD §32). ---

// TestSelfUpdateApply_JSON_SuppressesProgress_PlainWiresIt pins the PRD §32
// progress contract: under --json the engine receives a nil ProgressFn; in
// plain mode it receives a non-nil one. Both modes hand the engine a non-nil
// Confirmer.
func TestSelfUpdateApply_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()}
		stdout, _, err := runLeaf(t, fake, "self-update", "apply", "--yes", "--json")
		require.NoError(t, err)

		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
		assert.NotNil(t, fake.confirmer, "apply must pass a non-nil Confirmer to the engine")
		lines := nonEmptyLines(stdout)
		require.Len(t, lines, 1, "--json stdout must carry only the envelope")
		assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()}
		stdout, _, err := runLeaf(t, fake, "self-update", "apply", "--yes")
		require.NoError(t, err)

		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.NotNil(t, fake.confirmer, "apply must pass a non-nil Confirmer to the engine")
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish block, not a JSON envelope")
	})
}

// --- --yes accepts the SAFE self_update confirmation (real cliConfirmer). ---

// recordingSelfUpdateEngine embeds *fakeEngine and overrides ApplySelfUpdate
// to record the request it received and to actually drive the confirmer with
// a SAFE "self_update" confirmation (the base fakeEngine never calls Confirm).
// Every other method is inherited, so engine.Engine stays satisfied. The
// precedent).
type recordingSelfUpdateEngine struct {
	*fakeEngine
	gotReq            types.SelfUpdateRequest
	confirmOnInvoke   bool // when set, ApplySelfUpdate calls confirmer.Confirm with a self_update payload
	confirmAccepted   bool // the bool the confirmer returned
	confirmInvokedErr error

	// rollbackResult/rollbackErr drive the the invariant rollback path: when
	// both are set, ApplySelfUpdate returns them TOGETHER (a non-nil result
	// alongside a non-nil error), which the shared fakeEngine cannot express
	// (its ApplySelfUpdate returns (nil, err) when err is set). The leaf must
	// surface the result before returning the error.
	rollbackResult *types.SelfUpdateResult
	rollbackErr    error
}

func (r *recordingSelfUpdateEngine) ApplySelfUpdate(
	ctx context.Context,
	req types.SelfUpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.SelfUpdateResult, error) {
	r.gotReq = req
	r.progressWasNil = onProgress == nil
	r.confirmer = confirmer
	if r.confirmOnInvoke && confirmer != nil {
		r.confirmAccepted, r.confirmInvokedErr = confirmer.Confirm(ctx, types.Confirmation{
			Kind:    selfUpdateConfirmationKind,
			Title:   "update wdm to v1.1.0",
			Message: "Replace the wdm binary v1.0.0 -> v1.1.0?",
		})
	}
	if r.rollbackResult != nil || r.rollbackErr != nil {
		return r.rollbackResult, r.rollbackErr
	}
	return r.fakeEngine.ApplySelfUpdate(ctx, req, onProgress, confirmer)
}

// runSelfUpdateLeaf drives one `self-update...` invocation through
// NewRootCmd with the recording engine wired as the lazy factory result,
// mirroring runLeaf but typed to the local wrapper (runLeaf returns
// *fakeEngine).
func runSelfUpdateLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
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

// TestSelfUpdateApply_Yes_AcceptsSafeConfirmation pins that --yes, wired
// through the real cliConfirmer the leaf constructs (acceptDBRisk false),
// accepts the SAFE "self_update" confirmation. The recording wrapper drives
// confirmer.Confirm with a self_update payload, and the assertion is that the
// confirmer returned (true, nil) — proving the safe-confirmation --yes arm in
// install.go satisfies this Kind.
func TestSelfUpdateApply_Yes_AcceptsSafeConfirmation(t *testing.T) {
	t.Parallel()

	rec := &recordingSelfUpdateEngine{
		fakeEngine:      &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()},
		confirmOnInvoke: true,
	}

	_, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply", "--yes")
	require.NoError(t, err)

	assert.True(t, rec.confirmAccepted, "--yes must accept the SAFE self_update confirmation")
	assert.NoError(t, rec.confirmInvokedErr, "an accepted safe confirmation returns no error")
}

// TestSelfUpdateApply_NoYesNoTTY_RefusesSafeConfirmation pins the fail-closed
// posture: without --yes and without a TTY (runLeaf injects a non-terminal
// stdin), the SAFE self_update confirmation is refused (false, nil) — the
// engine maps that to ErrCodeUserCanceled (exit 7).
func TestSelfUpdateApply_NoYesNoTTY_RefusesSafeConfirmation(t *testing.T) {
	t.Parallel()

	rec := &recordingSelfUpdateEngine{
		fakeEngine:      &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()},
		confirmOnInvoke: true,
	}

	_, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply")
	require.NoError(t, err)

	assert.False(t, rec.confirmAccepted, "no --yes and no TTY must refuse the safe confirmation fail-closed")
	assert.NoError(t, rec.confirmInvokedErr, "a fail-closed refusal returns (false, nil), not an error")
}

// --- --target-version maps onto the request. ---

// TestSelfUpdateApply_MapsTargetVersionOntoRequest pins that --target-version
// maps verbatim onto SelfUpdateRequest.TargetVersion, and that an omitted
// flag leaves it empty (the "accept whatever the latest verified release is"
// signal).
func TestSelfUpdateApply_MapsTargetVersionOntoRequest(t *testing.T) {
	t.Parallel()

	t.Run("set", func(t *testing.T) {
		t.Parallel()

		rec := &recordingSelfUpdateEngine{
			fakeEngine: &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()},
		}
		_, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply", "--yes", "--target-version", "v1.1.0", "--json")
		require.NoError(t, err)
		assert.Equal(t, "v1.1.0", rec.gotReq.TargetVersion,
			"--target-version must map verbatim onto SelfUpdateRequest.TargetVersion")
	})

	t.Run("omitted", func(t *testing.T) {
		t.Parallel()

		rec := &recordingSelfUpdateEngine{
			fakeEngine: &fakeEngine{selfUpdateResult: sampleSelfUpdateResult()},
		}
		_, _, err := runSelfUpdateLeaf(t, rec, "self-update", "apply", "--yes", "--json")
		require.NoError(t, err)
		assert.Empty(t, rec.gotReq.TargetVersion, "an omitted --target-version must leave the request field empty")
	})
}

// --- error path: stdout empty. ---

// TestSelfUpdate_ErrorPath_StdoutEmpty pins that a typed engine error
// propagates out of Execute with no envelope/finish/status written, for both
// the check and apply leaves under --json and plain mode.
func TestSelfUpdate_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(
		types.ErrCodeVerificationFailed,
		"the latest release candidate failed verification",
		"check SECURITY.md for the manual verify-and-install flow",
	)

	cases := []struct {
		name string
		args []string
	}{
		{"check json", []string{"self-update", "check", "--json"}},
		{"check plain", []string{"self-update", "check"}},
		{"apply json", []string{"self-update", "apply", "--yes", "--json"}},
		{"apply plain", []string{"self-update", "apply", "--yes"}},
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

// --- the writability refusal: the leaf relays the typed error. ---

// TestSelfUpdateApply_WritabilityRefusal_RelaysTypedError pins the
// review focus: when the engine refuses because the install location is not
// user-writable, the leaf returns the typed ErrCodeUsageValidation error
// (exit 2) untouched, with the manual-install hint intact and stdout empty.
// The CLI never shells out or escalates — it just relays the engine's refusal.
func TestSelfUpdateApply_WritabilityRefusal_RelaysTypedError(t *testing.T) {
	t.Parallel()

	notWritable := types.NewError(
		types.ErrCodeUsageValidation,
		"the wdm install location is not user-writable",
		"install wdm to a user-writable path such as ~/.local/bin/wdm and re-run",
	)
	fake := &fakeEngine{err: notWritable}

	stdout, _, err := runLeaf(t, fake, "self-update", "apply", "--yes")

	require.Error(t, err)
	var typeErr *types.Error
	require.ErrorAs(t, err, &typeErr, "the writability refusal must surface as a *types.Error")
	assert.Equal(t, types.ErrCodeUsageValidation, typeErr.Code, "the writability refusal maps to usage-validation (exit 2)")
	assert.Contains(t, typeErr.Hint, "~/.local/bin/wdm", "the manual-install hint must reach the user")
	assert.Empty(t, stdout, "the refusal writes nothing to stdout")
}

// --- PRD §14 invariant: --help never constructs the engine. ---

// TestSelfUpdate_Help_DoesNotConstructEngine pins the PRD §14 self-update
// smoke-check invariant: --help on the group and both
// leaves exits 0 and NEVER constructs the engine — the sentinel factory
// fails the test if it is consulted. This protects the --version / --help
// paths a self-update depends on from a malformed config.toml.
func TestSelfUpdate_Help_DoesNotConstructEngine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"group help", []string{"self-update", "--help"}},
		{"check help", []string{"self-update", "check", "--help"}},
		{"apply help", []string{"self-update", "apply", "--help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, errors.New("factory must not be consulted for --help")
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

// --- both leaves take no positional arguments. ---

// TestSelfUpdate_RejectExtraArgs pins cobra.NoArgs on both leaves: a stray
// positional argument fails before RunE, so the engine is never consulted.
func TestSelfUpdate_RejectExtraArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"check extra arg", []string{"self-update", "check", "stray"}},
		{"apply extra arg", []string{"self-update", "apply", "stray"}},
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
			require.Error(t, err, "a stray positional argument must be rejected")
			assert.Empty(t, outBuf.String(), "an arg-count failure must write nothing to stdout")
		})
	}
}
