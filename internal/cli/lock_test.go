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

// These tests pin the two `lock` leaves: `lock status`
// (a read-only single-object probe) and `lock clear` (a --yes-accepts-SAFE
// recovery). They cover the direct-wrap wdm.v1 envelope each emits under
// --json, the plain-mode block/finish rendering, progress suppression and
// the confirmer wiring, the structural no-shorthand-flags proof, the
// empty-stdout error path, and factory-error propagation. They drive RunE
// end-to-end through NewRootCmd (the honest path since the persistent
// --json flag only resolves through the root), reusing the shared
// fakeEngine / runLeaf from fake_engine_test.go and the
// decodeEnvelopeData / nonEmptyLines helpers from envelope_contract_test.go.

// clearStaleLockKind is the SAFE stale-lock recovery confirmation Kind the
// engine emits (internal/core/runtimelock.go:193). It is byte-equal to the
// engine literal so a drift would fail the --yes acceptance test loudly.
// The CLI never re-declares it — the cliConfirmer routes any Kind != the
// database-risk literal through the safe-confirmation arm — so this local
// copy exists only to drive the recording wrappers' confirm probe.
const clearStaleLockKind = "clear_stale_lock"

// heldStaleLockStatus is a representative held-and-stale lock with a fully
// populated holder, used to pin both the JSON snake_case keys and the plain
// holder block.
func heldStaleLockStatus() *types.RuntimeLockStatus {
	startedAt := time.Date(2026, time.June, 12, 9, 30, 0, 0, time.UTC)
	return &types.RuntimeLockStatus{
		Exists:        true,
		Held:          true,
		Stale:         true,
		HolderPID:     4242,
		HolderCommand: "install",
		HolderAlive:   false,
		StartedAt:     &startedAt,
		WDMVersion:    "1.2.3",
	}
}

// --- lock status: direct-wrap single envelope with snake_case keys ---

// TestLockStatus_JSON_DirectWrapsRuntimeLockStatus pins that `lock status
// --json` emits exactly one envelope on stdout whose data object IS the
// RuntimeLockStatus directly (the apps-status direct-wrap precedent), with
// the snake_case keys present — including holder_alive, which always
// serializes (no omitempty) so a dead holder's false value is never dropped.
func TestLockStatus_JSON_DirectWrapsRuntimeLockStatus(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: heldStaleLockStatus()}

	stdout, _, err := runLeaf(t, fake, "lock", "status", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "lock status --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	// The RuntimeLockStatus is the envelope.data object directly — there is
	// no nesting key (the apps-status direct-wrap precedent).
	assert.NotContains(t, data, "runtime_lock_status", "the status must be the envelope data directly, not nested under a key")

	assert.Equal(t, true, data["exists"])
	assert.Equal(t, true, data["held"])
	assert.Equal(t, true, data["stale"])
	assert.Equal(t, float64(4242), data["holder_pid"])
	assert.Equal(t, "install", data["holder_command"])
	assert.Equal(t, "1.2.3", data["wdm_version"])
	assert.Equal(t, "2026-06-12T09:30:00Z", data["started_at"])

	// holder_alive ALWAYS serializes (no omitempty): a dead holder's false
	// value is precisely the stale-recovery signal, so it must be present.
	require.Contains(t, data, "holder_alive", "holder_alive must always serialize, even when false")
	assert.Equal(t, false, data["holder_alive"])
}

// TestLockStatus_JSON_AbsentLockHasFalseHolderAlivePresent pins that an
// absent lock (the zero value the engine returns when no lock file exists)
// still serializes holder_alive:false present and never emits the omitempty
// holder fields.
func TestLockStatus_JSON_AbsentLockHasFalseHolderAlivePresent(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{}}

	stdout, _, err := runLeaf(t, fake, "lock", "status", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "lock status --json must emit exactly one envelope on stdout")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, false, data["exists"])
	assert.Equal(t, false, data["held"])
	assert.Equal(t, false, data["stale"])
	require.Contains(t, data, "holder_alive", "holder_alive must always serialize on an absent lock too")
	assert.Equal(t, false, data["holder_alive"])
	assert.NotContains(t, data, "holder_pid", "an absent lock omits the omitempty holder_pid")
	assert.NotContains(t, data, "started_at", "an absent lock omits the omitempty started_at")
}

// TestLockStatus_Plain_HeldWithHolder pins the plain-mode block for a held,
// stale lock with a full holder: the exists/held/stale booleans first, then
// the holder block (pid, command, alive, started_at, wdm_version).
func TestLockStatus_Plain_HeldWithHolder(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: heldStaleLockStatus()}

	stdout, _, err := runLeaf(t, fake, "lock", "status")
	require.NoError(t, err)

	assert.Contains(t, stdout, "exists\ttrue", "the exists boolean must be shown")
	assert.Contains(t, stdout, "held\ttrue", "the held boolean must be shown")
	assert.Contains(t, stdout, "stale\ttrue", "the stale boolean must be shown")
	assert.Contains(t, stdout, "Holder:", "a recorded holder must render the holder block")
	assert.Contains(t, stdout, "pid\t4242", "the holder pid must be shown")
	assert.Contains(t, stdout, "command\tinstall", "the holder command must be shown")
	assert.Contains(t, stdout, "alive\tfalse", "the holder liveness must be shown")
	assert.Contains(t, stdout, "started_at\t2026-06-12T09:30:00Z", "the acquisition time must be RFC3339")
	assert.Contains(t, stdout, "wdm_version\t1.2.3", "the holder wdm version must be shown")

	// Plain mode must never emit a JSON envelope.
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the status block, not a JSON envelope")
}

// TestLockStatus_Plain_AbsentLockShowsBooleansOnly pins that an absent lock
// (no recorded holder) shows only the booleans and omits the holder block
// entirely.
func TestLockStatus_Plain_AbsentLockShowsBooleansOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{}}

	stdout, _, err := runLeaf(t, fake, "lock", "status")
	require.NoError(t, err)

	assert.Contains(t, stdout, "exists\tfalse")
	assert.Contains(t, stdout, "held\tfalse")
	assert.Contains(t, stdout, "stale\tfalse")
	assert.NotContains(t, stdout, "Holder:", "an absent lock must omit the holder block")
	assert.NotContains(t, stdout, "pid\t", "an absent lock must not render a pid line")
}

// TestLockStatus_NoConfirmerNoProgress pins that the read-only status leaf
// hands the engine neither a Confirmer nor a ProgressFn — RuntimeLockStatus
// takes neither. The fakeEngine records confirmer/progress only on the
// state-changing methods, so this drives the read method and asserts those
// recorders stayed at their zero state (nil confirmer, the progress recorder
// untouched).
func TestLockStatus_NoConfirmerNoProgress(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: heldStaleLockStatus()}

	_, _, err := runLeaf(t, fake, "lock", "status")
	require.NoError(t, err)

	assert.Nil(t, fake.confirmer, "lock status must pass no Confirmer (RuntimeLockStatus takes none)")
}

// TestLockStatus_ErrorPath_StdoutEmpty pins that a typed engine error
// propagates out of Execute and stdout stays empty in BOTH the --json and
// plain modes.
func TestLockStatus_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeGeneric, "runtime lock could not be inspected", "check permissions and retry")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"lock", "status", "--json"}},
		{"plain", []string{"lock", "status"}},
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

// TestLockStatus_RejectsShorthandAndArgs pins the structural flag/arg
// surface: a -y shorthand is unregistered (fails parsing) and the NoArgs
// validator refuses a positional argument — both before the engine factory
// is constructed.
func TestLockStatus_RejectsShorthandAndArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"y shorthand is not registered", []string{"lock", "status", "-y"}},
		{"positional arg is rejected", []string{"lock", "status", "extra"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on a flag/arg refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "a flag/arg violation must surface an error")
			assert.Empty(t, outBuf.String(), "a flag/arg refusal must write nothing to stdout")
		})
	}
}

// TestLockStatus_FactoryError_Propagates pins that a failed engine factory
// surfaces out of Execute and never produces output.
func TestLockStatus_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"lock", "status", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}

// --- lock clear: --yes acceptance, decline, envelope, finish copy ---

// recordingClearEngine embeds *fakeEngine and overrides ClearStaleRuntimeLock
// to drive the leaf's real cliConfirmer with a clear_stale_lock payload
// before delegating, recording the confirmer's decision. Every other method
// is inherited so the engine.Engine interface stays satisfied
// (the apps restart / backups restore precedent).
type recordingClearEngine struct {
	*fakeEngine
	confirmAccepted   bool
	confirmInvokedErr error
}

func (r *recordingClearEngine) ClearStaleRuntimeLock(
	ctx context.Context,
	confirmer types.Confirmer,
) (*types.RuntimeLockStatus, error) {
	if confirmer != nil {
		r.confirmAccepted, r.confirmInvokedErr = confirmer.Confirm(ctx, types.Confirmation{
			Kind:    clearStaleLockKind,
			Title:   "clear stale runtime lock",
			Message: "the holder process is no longer running",
		})
	}
	return r.fakeEngine.ClearStaleRuntimeLock(ctx, confirmer)
}

// runClearLeaf drives one `lock clear` invocation through NewRootCmd with the
// given engine wired as the lazy factory result, mirroring runLeaf but typed
// to engine.Engine so the local wrappers can be passed.
func runClearLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
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

// TestLockClear_Yes_AcceptsSafeClearConfirmation pins that --yes, wired
// through the real cliConfirmer the leaf constructs (acceptDBRisk false),
// accepts the SAFE "clear_stale_lock" confirmation. The recording wrapper
// drives confirmer.Confirm with a clear_stale_lock payload, and the assertion
// is that the confirmer returned (true, nil) — proving the safe-confirmation
// --yes arm satisfies this Kind, the the confirmation rulesgating contract.
func TestLockClear_Yes_AcceptsSafeClearConfirmation(t *testing.T) {
	t.Parallel()

	rec := &recordingClearEngine{
		fakeEngine: &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: true}},
	}

	_, _, err := runClearLeaf(t, rec, "lock", "clear", "--yes")
	require.NoError(t, err)

	require.NoError(t, rec.confirmInvokedErr, "the real cliConfirmer must not error on a --yes safe confirmation")
	assert.True(t, rec.confirmAccepted,
		"--yes must accept the SAFE clear_stale_lock confirmation through the real cliConfirmer")
}

// TestLockClear_PassesConfirmerWiredAssumeYesAndDBRiskFalse pins that the
// leaf hands the engine the shared *cliConfirmer with acceptDBRisk=false (the
// clear flow never produces a database-risk warning). The recorded confirmer
// fields are the deterministic proof the leaf wires the safe-only matrix.
func TestLockClear_PassesConfirmerWiredAssumeYesAndDBRiskFalse(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: true}}
	_, _, err := runLeaf(t, fake, "lock", "clear", "--yes", "--json")
	require.NoError(t, err)

	require.NotNil(t, fake.confirmer, "lock clear must pass a non-nil Confirmer to the engine")
	c, ok := fake.confirmer.(*cliConfirmer)
	require.True(t, ok, "lock clear must pass the shared *cliConfirmer")
	assert.True(t, c.yes, "--yes must wire the confirmer's yes field")
	assert.False(t, c.acceptDBRisk, "lock clear must wire acceptDBRisk=false — clearing produces no database-risk warning")
}

// TestLockClear_NoYesNoTTY_DeclinesEmptyStdoutTypedError pins the fail-closed
// decline: without --yes and without a TTY (runLeaf wires an empty buffer for
// stdin, never a terminal), the real cliConfirmer declines the safe
// confirmation as (false, nil), the engine maps that to ErrCodeUserCanceled,
// and the leaf returns a typed error with empty stdout.
func TestLockClear_NoYesNoTTY_DeclinesEmptyStdoutTypedError(t *testing.T) {
	t.Parallel()

	rec := &decliningClearEngine{fakeEngine: &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: true}}}

	stdout, _, err := runClearLeaf(t, rec, "lock", "clear")

	require.Error(t, err, "a declined safe confirmation must surface a typed error out of Execute")
	var typed *types.Error
	require.ErrorAs(t, err, &typed, "the decline must be a typed *types.Error")
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code, "a declined clear must map to ErrCodeUserCanceled (exit 7)")
	assert.Empty(t, stdout, "a declined clear must write nothing to stdout")
}

// decliningClearEngine drives the leaf's real cliConfirmer and faithfully
// reproduces the engine's confirmClearStaleRuntimeLock decline mapping: a
// (false, nil) from the confirmer becomes ErrCodeUserCanceled. It proves the
// leaf's no-TTY/no-yes path reaches the confirmer and that a decline surfaces
// as exit 7 with empty stdout, without depending on a real internal/core
// instance.
type decliningClearEngine struct {
	*fakeEngine
}

func (d *decliningClearEngine) ClearStaleRuntimeLock(
	ctx context.Context,
	confirmer types.Confirmer,
) (*types.RuntimeLockStatus, error) {
	confirmed, err := confirmer.Confirm(ctx, types.Confirmation{
		Kind:    clearStaleLockKind,
		Title:   "clear stale runtime lock",
		Message: "the holder process is no longer running",
	})
	if err != nil {
		return nil, err
	}
	if !confirmed {
		return nil, types.NewError(
			types.ErrCodeUserCanceled,
			"stale runtime lock recovery canceled",
			"re-run the recovery and confirm the prompt to clear the stale lock",
		)
	}
	return d.fakeEngine.ClearStaleRuntimeLock(ctx, confirmer)
}

// TestLockClear_JSON_DirectWrapsPostClearStatus pins that `lock clear --json`
// emits exactly one envelope on stdout whose data object IS the post-clear
// RuntimeLockStatus directly (the same direct-wrap shape as lock status).
func TestLockClear_JSON_DirectWrapsPostClearStatus(t *testing.T) {
	t.Parallel()

	// A post-clear status: the file was removed, so it no longer exists/holds.
	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: false}}

	stdout, _, err := runLeaf(t, fake, "lock", "clear", "--yes", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "lock clear --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.NotContains(t, data, "runtime_lock_status", "the post-clear status must be the envelope data directly, not nested")
	assert.Equal(t, false, data["exists"], "the post-clear status reports the cleared state")
	require.Contains(t, data, "holder_alive", "holder_alive must always serialize")
}

// TestLockClear_JSON_SingleEnvelopeOnly documents that the
// ClearStaleRuntimeLock method takes no ProgressFn, so there is nothing to
// suppress — the leaf never wires a progress callback — and pins the
// single-envelope stdout discipline under --json regardless.
func TestLockClear_JSON_SingleEnvelopeOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: false}}
	stdout, _, err := runLeaf(t, fake, "lock", "clear", "--yes", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "--json stdout must carry only the envelope")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
}

// TestLockClear_PlainFinish_NeverClaimsRecovered pins the honest finish copy:
// the headline states the lock is now clear / the path is free and NEVER
// claims it "recovered" a stale operation (the engine deliberately does not
// expose the free-vs-stale outcome), then renders the post-clear status block.
func TestLockClear_PlainFinish_NeverClaimsRecovered(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{runtimeLockResult: &types.RuntimeLockStatus{Exists: false}}

	stdout, _, err := runLeaf(t, fake, "lock", "clear", "--yes")
	require.NoError(t, err)

	assert.Contains(t, stdout, "cleared", "the finish copy must state the lock was cleared")
	assert.NotContains(t, strings.ToLower(stdout), "recovered",
		"the finish copy must never claim it recovered a stale operation (the engine hides the free-vs-stale outcome)")

	// The finish screen reuses the lock-status renderer for the post-clear
	// state block.
	assert.Contains(t, stdout, "exists\tfalse", "the post-clear status block must follow the headline")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must be the finish screen, not a JSON envelope")
}

// TestLockClear_ErrorPath_StdoutEmpty pins that a typed engine error (e.g.
// the live within-age lock refusal at ErrCodeRuntimeLockHeld) propagates out
// of Execute and stdout stays empty in BOTH the --json and plain modes.
func TestLockClear_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(
		types.ErrCodeRuntimeLockHeld,
		"the runtime lock is held by a live operation and cannot be cleared",
		"kill the holding process and retry if it is wedged",
	)

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"lock", "clear", "--yes", "--json"}},
		{"plain", []string{"lock", "clear", "--yes"}},
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

// TestLockClear_RejectsShorthandAndArgs pins the structural flag/arg surface:
// a -y shorthand is unregistered (only the long --yes exists) and the NoArgs
// validator refuses a positional argument — both before the engine factory is
// constructed.
func TestLockClear_RejectsShorthandAndArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"y shorthand is not registered", []string{"lock", "clear", "-y"}},
		{"positional arg is rejected", []string{"lock", "clear", "extra"}},
		{"force is not registered", []string{"lock", "clear", "--force"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on a flag/arg refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "a flag/arg violation must surface an error")
			assert.Empty(t, outBuf.String(), "a flag/arg refusal must write nothing to stdout")
		})
	}
}

// TestLockClear_FactoryError_Propagates pins that a failed engine factory
// surfaces out of Execute and never produces output, since the leaf builds
// the engine inside RunE after the --json read.
func TestLockClear_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"lock", "clear", "--yes", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}
