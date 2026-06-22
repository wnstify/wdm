package types_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// TestErrorCode_String_AllCases verifies the lowercase identifier
// emitted for each [types.ErrorCode] value. The test enumerates every
// PRD §27 row so adding a code without updating String — or
// reusing an existing string — fails loud. The "invalid" case
// covers the default arm in the switch.
func TestErrorCode_String_AllCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code types.ErrorCode
		want string
	}{
		{types.ErrCodeUnknown, "unknown"},
		{types.ErrCodeGeneric, "generic"},
		{types.ErrCodeUsageValidation, "usage_validation"},
		{types.ErrCodeVerificationFailed, "verification_failed"},
		{types.ErrCodeRuntimeLockHeld, "runtime_lock_held"},
		{types.ErrCodeDockerUnavailable, "docker_unavailable"},
		{types.ErrCodePermissionDenied, "permission_denied"},
		{types.ErrCodeUserCanceled, "user_canceled"},
		{types.ErrCodeNetworkFailure, "network_failure"},
		{types.ErrCodeMigrationFailure, "migration_failure"},
		{types.ErrorCode(99), "invalid(99)"},
		{types.ErrorCode(-1), "invalid(-1)"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.code.String())
		})
	}
}

// TestErrorCode_IntegerValues locks the numeric values to PRD §27
// row order. cmd/wdm casts int(code) directly to the exit code,
// so renumbering any of these is a wire-breaking change.
func TestErrorCode_IntegerValues(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, int(types.ErrCodeUnknown))
	assert.Equal(t, 1, int(types.ErrCodeGeneric))
	assert.Equal(t, 2, int(types.ErrCodeUsageValidation))
	assert.Equal(t, 3, int(types.ErrCodeVerificationFailed))
	assert.Equal(t, 4, int(types.ErrCodeRuntimeLockHeld))
	assert.Equal(t, 5, int(types.ErrCodeDockerUnavailable))
	assert.Equal(t, 6, int(types.ErrCodePermissionDenied))
	assert.Equal(t, 7, int(types.ErrCodeUserCanceled))
	assert.Equal(t, 8, int(types.ErrCodeNetworkFailure))
	assert.Equal(t, 9, int(types.ErrCodeMigrationFailure))
}

func TestNewError_PopulatesAllFields(t *testing.T) {
	t.Parallel()

	err := types.NewError(types.ErrCodeUsageValidation, "msg", "hint")

	require.NotNil(t, err)
	assert.Equal(t, types.ErrCodeUsageValidation, err.Code)
	assert.Equal(t, "msg", err.Message)
	assert.Equal(t, "hint", err.Hint)
	assert.Nil(t, err.Cause)
}

func TestWrapError_AttachesCause(t *testing.T) {
	t.Parallel()

	inner := errors.New("inner failure")
	err := types.WrapError(types.ErrCodeNetworkFailure, "msg", "hint", inner)

	require.NotNil(t, err)
	assert.Equal(t, types.ErrCodeNetworkFailure, err.Code)
	assert.Equal(t, "msg", err.Message)
	assert.Equal(t, "hint", err.Hint)
	require.NotNil(t, err.Cause)
	assert.Same(t, inner, err.Cause)
}

func TestError_ErrorString_WithCause(t *testing.T) {
	t.Parallel()

	inner := errors.New("boom")
	err := types.WrapError(types.ErrCodeDockerUnavailable, "docker missing", "install docker", inner)

	assert.Equal(t, "docker missing [docker_unavailable]: boom", err.Error())
}

func TestError_ErrorString_WithoutCause(t *testing.T) {
	t.Parallel()

	err := types.NewError(types.ErrCodeRuntimeLockHeld, "another wdm is running", "")
	assert.Equal(t, "another wdm is running [runtime_lock_held]", err.Error())
}

func TestError_ErrorString_NilReceiver(t *testing.T) {
	t.Parallel()

	var err *types.Error
	assert.Equal(t, "<nil>", err.Error())
}

func TestError_Unwrap_ReturnsCause(t *testing.T) {
	t.Parallel()

	inner := errors.New("inner")
	err := types.WrapError(types.ErrCodeGeneric, "msg", "", inner)

	assert.Same(t, inner, errors.Unwrap(err))
}

func TestError_Unwrap_NilReceiver(t *testing.T) {
	t.Parallel()

	var err *types.Error
	assert.Nil(t, err.Unwrap())
}

func TestError_Unwrap_NoCauseReturnsNil(t *testing.T) {
	t.Parallel()

	err := types.NewError(types.ErrCodeGeneric, "msg", "")
	assert.Nil(t, errors.Unwrap(err))
}

func TestError_ErrorsIsTraversesCauseChain(t *testing.T) {
	t.Parallel()

	err := types.WrapError(types.ErrCodeUsageValidation, "bad config", "fix it", types.ErrConfigInvalid)
	assert.True(t, errors.Is(err, types.ErrConfigInvalid))
}

func TestIsCode_TrueForMatchingCode(t *testing.T) {
	t.Parallel()

	err := types.NewError(types.ErrCodePermissionDenied, "msg", "")
	assert.True(t, types.IsCode(err, types.ErrCodePermissionDenied))
}

func TestIsCode_FalseForDifferentCode(t *testing.T) {
	t.Parallel()

	err := types.NewError(types.ErrCodePermissionDenied, "msg", "")
	assert.False(t, types.IsCode(err, types.ErrCodeNetworkFailure))
}

func TestIsCode_FalseForNonTypedError(t *testing.T) {
	t.Parallel()

	plain := errors.New("plain")
	assert.False(t, types.IsCode(plain, types.ErrCodeGeneric))
}

func TestIsCode_TrueThroughWrap(t *testing.T) {
	t.Parallel()

	inner := types.NewError(types.ErrCodeMigrationFailure, "msg", "")
	wrapped := fmt.Errorf("outer: %w", inner)

	assert.True(t, types.IsCode(wrapped, types.ErrCodeMigrationFailure))
}

func TestIsCode_FalseForNilError(t *testing.T) {
	t.Parallel()

	assert.False(t, types.IsCode(nil, types.ErrCodeGeneric))
}

// TestSentinels_AreDistinctAndStable confirms the package-level
// sentinels are non-nil and don't accidentally compare equal to each
// other. detection paths (errors.Is in cmd/wdm) depend
// on these staying distinct identities.
func TestSentinels_AreDistinctAndStable(t *testing.T) {
	t.Parallel()

	require.NotNil(t, types.ErrConfigInvalid)
	require.NotNil(t, types.ErrStaleState)

	assert.NotEqual(t, types.ErrConfigInvalid, types.ErrStaleState)
}

// TestSentinels_DetectableViaErrorsIs verifies the canonical detection
// pattern callers use. Wrapping each sentinel with fmt.Errorf and then
// matching via errors.Is is the contract every consumer relies on
// (state, cli, cmd/wdm all do this).
func TestSentinels_DetectableViaErrorsIs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		s    error
	}{
		{"ErrConfigInvalid", types.ErrConfigInvalid},
		{"ErrStaleState", types.ErrStaleState},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("layer: %w", tc.s)
			assert.True(t, errors.Is(wrapped, tc.s))
		})
	}
}

// TestError_ImplementsErrorInterface is a compile-time sanity guard
// echoing the var _ error = &Error{} line in error.go. Worth a runtime
// assertion too so a future refactor that breaks the interface fails
// in CI rather than at the next caller's build.
func TestError_ImplementsErrorInterface(t *testing.T) {
	t.Parallel()

	var _ error = (*types.Error)(nil)
	var err error = types.NewError(types.ErrCodeGeneric, "msg", "")
	assert.NotEmpty(t, err.Error())
}
