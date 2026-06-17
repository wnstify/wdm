package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests drive CheckSelfUpdate and ApplySelfUpdate end-to-end against the
// offline fake-binary-release fixture (self_update_fixture_test.go) and an
// INJECTED install target inside a t.TempDir. No test resolves the real
// os.Executable, stages over the real binary, or execs the real test runner —
// the executablePath/resolveSymlinks/runSmoke seams are always injected.

// fakeTarget creates a fake wdm executable inside a fresh 0o700 directory and
// returns the path. The parent is pinned 0o700 because t.TempDir can be 0o775
// on umask-0002 hosts (the established internal/core/state fixture posture).
func fakeTarget(t *testing.T, contents string) string {
	t.Helper()
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))
	dir := filepath.Join(parent, "bin")
	require.NoError(t, os.Mkdir(dir, 0o700))
	exe := filepath.Join(dir, "wdm")
	require.NoError(t, os.WriteFile(exe, []byte(contents), 0o755))
	return exe
}

// smokeStub builds a runSmoke seam returning a fixed version/err, recording
// the binary path it was asked to smoke so a test can assert it ran the NEW
// target (and never the real test runner).
type smokeStub struct {
	version string
	err     error
	calls   []string
}

func (s *smokeStub) run(_ context.Context, binaryPath string) (string, error) {
	s.calls = append(s.calls, binaryPath)
	return s.version, s.err
}

// selfUpdateEngine builds an engine wired to the fake-binary-release fixture,
// an injected install target, and a controllable smoke seam. version is the
// running binary version the engine reports.
func selfUpdateEngine(
	t *testing.T,
	fr *fakeBinaryRelease,
	version, targetPath string,
	smoke func(context.Context, string) (string, error),
	extra ...core.Option,
) *core.Engine {
	t.Helper()
	opts := []core.Option{
		fr.clientOption(),
		core.WithVersion(version),
		core.WithSelfUpdateDeps(
			func() (string, error) { return targetPath, nil },
			func(p string) (string, error) { return p, nil },
			fr.stageCandidate,
			smoke,
		),
	}
	opts = append(opts, extra...)
	eng, _ := newTestEngine(t, opts...)
	return eng
}

// --- CheckSelfUpdate ---

func TestCheckSelfUpdate_ReportsAvailableWhenVersionsDiffer(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.NoError(t, err)
	assert.Equal(t, "v1.0.0", status.CurrentVersion)
	assert.Equal(t, selfUpdateReleaseTag, status.LatestVersion)
	assert.True(t, status.UpdateAvailable)
	assert.True(t, status.Verified)
	assert.False(t, status.CheckedAt.IsZero())
	assert.Empty(t, status.Notes)
}

func TestCheckSelfUpdate_UpToDateWhenVersionsMatch(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "current binary")
	eng := selfUpdateEngine(t, fr, selfUpdateReleaseTag, target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.NoError(t, err)
	assert.False(t, status.UpdateAvailable)
	assert.True(t, status.Verified)
}

func TestCheckSelfUpdate_DevBuildNeverOffersUpdate(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "dev binary")
	eng := selfUpdateEngine(t, fr, "dev", target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.NoError(t, err)
	assert.Equal(t, "dev", status.CurrentVersion)
	assert.False(t, status.UpdateAvailable)
	assert.NotEmpty(t, status.Notes)
}

func TestCheckSelfUpdate_TransportFailureMapsToExit8(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	fr.srv.Close() // server gone → transport failure
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.Error(t, err)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)
}

func TestCheckSelfUpdate_BadSignatureMapsToExit3(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	fr.sig = []byte("not a valid signature") // verification fault
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.Error(t, err)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", err)
}

func TestCheckSelfUpdate_TamperedBinaryMapsToExit3(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	fr.binary = []byte("tampered bytes that do not match SHA256SUMS")
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.Error(t, err)
	assert.Nil(t, status)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", err)
}

func TestCheckSelfUpdate_ClosedEngine(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)
	require.NoError(t, eng.Close())

	status, err := eng.CheckSelfUpdate(t.Context(), types.SelfUpdateQuery{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, status)
}

func TestCheckSelfUpdate_CanceledContext(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old binary")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	status, err := eng.CheckSelfUpdate(ctx, types.SelfUpdateQuery{})
	require.Error(t, err)
	assert.Nil(t, status)
	assert.ErrorIs(t, err, context.Canceled)
}

// --- ApplySelfUpdate happy path ---

func TestApplySelfUpdate_HappyPathReplacesAndRetainsPrevious(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "OLD BINARY CONTENTS")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	confirmer := &fakeConfirmer{}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	var steps []string
	onProgress := func(step string, _ float64, _ string) { steps = append(steps, step) }

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, onProgress, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "v1.0.0", result.PreviousVersion)
	assert.Equal(t, selfUpdateReleaseTag, result.AppliedVersion)
	assert.True(t, result.Replaced)
	assert.True(t, result.SmokeOK)
	assert.False(t, result.RolledBack)
	assert.Equal(t, target+".previous", result.PreviousBinaryPath)

	// The target now holds the verified candidate bytes.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, selfUpdateCandidateBinary, string(got))

	// wdm.previous holds the old binary, byte-for-byte.
	prev, err := os.ReadFile(target + ".previous")
	require.NoError(t, err)
	assert.Equal(t, "OLD BINARY CONTENTS", string(prev))

	// The smoke check ran the NEW target binary (never the real test runner).
	require.Len(t, smoke.calls, 1)
	assert.Equal(t, target, smoke.calls[0])

	// Exactly one self_update confirmation.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, types.ConfirmationKindSelfUpdate, confirmer.calls[0].Kind)

	// Progress carries the self-update step family.
	assert.Contains(t, steps, types.StepSelfUpdatePlanning)
	assert.Contains(t, steps, types.StepSelfUpdateReplace)
	assert.Contains(t, steps, types.StepSelfUpdateSmoke)
	assert.NotContains(t, steps, types.StepSelfUpdateRollback)
}

func TestApplySelfUpdate_PreservesExecutableMode(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	require.NoError(t, os.Chmod(target, 0o711))
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{version: selfUpdateReleaseTag}).run)

	_, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), info.Mode().Perm())
}

// TestApplySelfUpdate_StaledPreviousTmpDoesNotBlockApply proves the self-
// healing posture: a crash during a prior copy can leave a wdm.previous.tmp in
// the persistent install directory, and because that temp is created O_EXCL the
// retention copy would otherwise fail every future self-update. The apply must
// best-effort remove the stale temp before the O_EXCL create and succeed.
func TestApplySelfUpdate_StaledPreviousTmpDoesNotBlockApply(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "OLD BINARY CONTENTS")

	// Seed a stale crash leftover that an O_EXCL create would collide with.
	staleTmp := target + ".previous.tmp"
	require.NoError(t, os.WriteFile(staleTmp, []byte("leftover from a crashed run"), 0o600))

	smoke := &smokeStub{version: selfUpdateReleaseTag}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Replaced)
	assert.True(t, result.SmokeOK)

	// The verified candidate is installed.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, selfUpdateCandidateBinary, string(got))

	// wdm.previous holds the real old binary, byte-for-byte (the stale temp was
	// not reused).
	prev, err := os.ReadFile(target + ".previous")
	require.NoError(t, err)
	assert.Equal(t, "OLD BINARY CONTENTS", string(prev))

	// The temp was consumed/cleaned, not left behind.
	_, statErr := os.Stat(staleTmp)
	assert.True(t, os.IsNotExist(statErr), "the wdm.previous.tmp must not survive a successful apply")
}

func TestApplySelfUpdate_TargetVersionMatchApplies(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{version: selfUpdateReleaseTag}).run)

	result, err := eng.ApplySelfUpdate(
		t.Context(),
		types.SelfUpdateRequest{TargetVersion: selfUpdateReleaseTag},
		nil, &fakeConfirmer{},
	)
	require.NoError(t, err)
	assert.True(t, result.Replaced)
}

// --- ApplySelfUpdate rollback ---

func TestApplySelfUpdate_VersionMismatchRollsBack(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "OLD BINARY CONTENTS")
	// The new binary reports the WRONG version → smoke fails → rollback.
	smoke := &smokeStub{version: "v9.9.9-impostor"}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	var steps []string
	result, err := eng.ApplySelfUpdate(
		t.Context(), types.SelfUpdateRequest{},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		&fakeConfirmer{},
	)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a rolled-back self-update is a generic failure (exit 1), got %v", err)
	require.NotNil(t, result)
	assert.False(t, result.Replaced)
	assert.False(t, result.SmokeOK)
	assert.True(t, result.RolledBack)
	// After a SUCCESSFUL rollback the retained wdm.previous was renamed onto
	// the target and no longer exists, so PreviousBinaryPath (documented as the
	// path of the retained wdm.previous) is empty rather than naming the now-
	// live target.
	assert.Empty(t, result.PreviousBinaryPath)
	_, statErr := os.Stat(target + ".previous")
	assert.True(t, os.IsNotExist(statErr),
		"wdm.previous must be consumed by a successful rollback restore")

	// The target is restored byte-for-byte to the OLD binary.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "OLD BINARY CONTENTS", string(got))

	assert.Contains(t, steps, types.StepSelfUpdateRollback)
}

func TestApplySelfUpdate_FailedSmokeExitRollsBack(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "OLD BINARY CONTENTS")
	// The new binary exits non-zero (exec error) → rollback.
	smoke := &smokeStub{err: errors.New("exit status 1")}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, applyErr := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, applyErr)
	assert.True(t, types.IsCode(applyErr, types.ErrCodeGeneric),
		"a rolled-back self-update is a generic failure (exit 1), got %v", applyErr)
	require.NotNil(t, result)
	assert.True(t, result.RolledBack)
	assert.False(t, result.Replaced)

	// The underlying exec fault is reachable for diagnostics.
	assert.ErrorIs(t, applyErr, smoke.err)

	// The rollback restores the previous binary byte-for-byte.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "OLD BINARY CONTENTS", string(got))
}

func TestApplySelfUpdate_RollbackRestoresPreviousByteForByte(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	const oldContents = "the precise old binary bytes -- e3b0c44298fc1c14\n"
	target := fakeTarget(t, oldContents)
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{version: "wrong"}).run)

	_, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, oldContents, string(got),
		"the rollback must restore the exact previous binary, not the candidate")
	assert.NotEqual(t, selfUpdateCandidateBinary, string(got))
}

func TestApplySelfUpdate_RollbackFailureLeavesNewBinaryAndJoinsBothFaults(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")

	// The smoke seam fails AND, before returning, removes wdm.previous so the
	// rollback restore (rename previous -> target) cannot succeed: the worst-
	// case partial failure. The new binary is already installed at this point.
	smokeFault := errors.New("smoke exec blew up")
	smoke := func(_ context.Context, _ string) (string, error) {
		require.NoError(t, os.Remove(target+".previous"))
		return "", smokeFault
	}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	require.NotNil(t, result)
	// Honest reporting: the new binary is still installed, NOT rolled back.
	assert.True(t, result.Replaced)
	assert.False(t, result.SmokeOK)
	assert.False(t, result.RolledBack)
	// On a FAILED rollback the result keeps reporting the real wdm.previous
	// sibling so an operator can restore it by hand (unlike the successful-
	// rollback branch, where the previous binary has been consumed).
	assert.Equal(t, target+".previous", result.PreviousBinaryPath)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"want ErrCodeGeneric (exit 1), got %v", err)
	// Both the smoke fault and the restore fault are reachable.
	assert.ErrorIs(t, err, smokeFault)

	// The verified candidate is still on disk (it was not removed by the
	// failed rollback).
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, selfUpdateCandidateBinary, string(got))
}

// TestDefaultRunVersionSmoke_ExecsBinaryAndReturnsVersion exercises the
// PRODUCTION smoke-exec seam (defaultRunVersionSmoke, reached via the
// SmokeForTest export) against a tiny test-created script — NEVER the test
// runner. It proves the argv-only exec returns the trimmed reported version
// and surfaces a non-zero exit as an error.
func TestDefaultRunVersionSmoke_ExecsBinaryAndReturnsVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))

	ok := filepath.Join(dir, "fake-wdm-ok")
	require.NoError(t, os.WriteFile(ok, []byte("#!/bin/sh\necho 'v1.2.3'\n"), 0o755))
	version, err := core.DefaultRunVersionSmokeForTest(t.Context(), ok)
	require.NoError(t, err)
	assert.Equal(t, "v1.2.3", version)

	bad := filepath.Join(dir, "fake-wdm-bad")
	require.NoError(t, os.WriteFile(bad, []byte("#!/bin/sh\nexit 3\n"), 0o755))
	_, err = core.DefaultRunVersionSmokeForTest(t.Context(), bad)
	require.Error(t, err)
}

// --- ApplySelfUpdate refusals (nothing replaced before verification+confirm) ---

func TestApplySelfUpdate_NonWritableTargetRefusesBeforeDownload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory write permissions; cannot prove refusal")
	}
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	// A read-only directory holding the target: the gate must refuse.
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))
	roDir := filepath.Join(parent, "ro")
	require.NoError(t, os.Mkdir(roDir, 0o700))
	target := filepath.Join(roDir, "wdm")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o755))
	require.NoError(t, os.Chmod(roDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want ErrCodeUsageValidation (exit 2), got %v", err)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, core.ManualInstallHintForTest, typed.Hint)
	assert.NotContains(t, typed.Hint, "sudo")

	// No download happened: the gate short-circuited before any network call.
	assert.Zero(t, fr.httpRequests())
	// The target is unchanged and no wdm.previous was written.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	_, statErr := os.Stat(target + ".previous")
	assert.True(t, os.IsNotExist(statErr))
}

func TestApplySelfUpdate_NilConfirmerRefusesAfterVerifyBeforeReplace(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want ErrCodeUsageValidation (exit 2), got %v", err)

	// Nothing replaced: target unchanged, no previous, smoke never ran.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	_, statErr := os.Stat(target + ".previous")
	assert.True(t, os.IsNotExist(statErr))
	assert.Empty(t, smoke.calls)
}

func TestApplySelfUpdate_DeclinedConfirmerReplacesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) { return false, nil },
	}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled),
		"want ErrCodeUserCanceled (exit 7), got %v", err)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assert.Empty(t, smoke.calls)
}

func TestApplySelfUpdate_ConfirmerErrorPropagates(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	sentinel := errors.New("confirmer backend exploded")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) { return false, sentinel },
	}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{version: selfUpdateReleaseTag}).run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, sentinel)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestApplySelfUpdate_TransportFailureMapsToExit8ReplacesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	fr.srv.Close()
	target := fakeTarget(t, "old")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assert.Empty(t, smoke.calls)
}

func TestApplySelfUpdate_BadSignatureMapsToExit3ReplacesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	fr.sig = []byte("not a valid signature")
	target := fakeTarget(t, "old")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	confirmer := &fakeConfirmer{}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", err)

	// Verification failed BEFORE the confirm and BEFORE any replacement.
	assert.Empty(t, confirmer.calls)
	assert.Empty(t, smoke.calls)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestApplySelfUpdate_TargetVersionMismatchRefusesBeforeReplace(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	smoke := &smokeStub{version: selfUpdateReleaseTag}
	confirmer := &fakeConfirmer{}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	result, err := eng.ApplySelfUpdate(
		t.Context(),
		types.SelfUpdateRequest{TargetVersion: "v2.0.0-not-the-latest"},
		nil, confirmer,
	)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want ErrCodeUsageValidation (exit 2), got %v", err)

	// Refused after verify, before confirm/replace.
	assert.Empty(t, confirmer.calls)
	assert.Empty(t, smoke.calls)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

// --- ApplySelfUpdate lock + lifecycle ---

func TestApplySelfUpdate_HoldsRuntimeLock(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")

	// A second engine sharing the SAME state dir; while the apply holds the
	// runtime.lock, this engine's state-changing op must observe it as busy.
	stateDir := filepath.Join(t.TempDir(), "shared-state")
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	contender, _ := newTestEngine(t,
		fr.clientOption(),
		core.WithVersion("v1.0.0"),
		core.WithStateDir(stateDir),
	)
	contenderSettings, err := contender.Settings(t.Context())
	require.NoError(t, err)

	confirmer := &fakeConfirmer{
		confirmFn: func(ctx context.Context, _ types.Confirmation) (bool, error) {
			lockErr := contender.UpdateSettings(ctx, *contenderSettings)
			assert.True(t, types.IsCode(lockErr, types.ErrCodeRuntimeLockHeld),
				"runtime.lock must be held during self-update apply, got %v", lockErr)
			return true, nil
		},
	}

	eng := selfUpdateEngine(t, fr, "v1.0.0", target,
		(&smokeStub{version: selfUpdateReleaseTag}).run,
		core.WithStateDir(stateDir),
	)

	_, err = eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, confirmer)
	require.NoError(t, err)
	require.Len(t, confirmer.calls, 1)
}

func TestApplySelfUpdate_ClosedEngine(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)
	require.NoError(t, eng.Close())

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

func TestApplySelfUpdate_CanceledContext(t *testing.T) {
	t.Parallel()

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, "old")
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, (&smokeStub{}).run)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := eng.ApplySelfUpdate(ctx, types.SelfUpdateRequest{}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestStageCandidateProduction_ContextCanceledIsNetworkFault pins that the new
// production assembler maps a pre-canceled context to a transport-class fault
// (exit 8), mirroring VerifyCatalogBundleProduction, before any trust work.
func TestStageCandidateProduction_ContextCanceledIsNetworkFault(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	staged, err := release.StageCandidateProduction(ctx, nil, nil, t.TempDir())
	require.Error(t, err)
	assert.Nil(t, staged)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)
}
