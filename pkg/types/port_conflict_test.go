package types_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// TestPortConflictError_ExposesStructuredDetail verifies the typed conflict
// error carries the conflicting binding and the deterministic suggestion, and
// that both detail recovery (errors.As → *PortConflictError) and exit-code
// mapping (errors.As → *Error with ErrCodeUsageValidation) survive wrapping —
// the contract the CLI envelope and cmd/wdm's exitCodeFor depend on.
func TestPortConflictError_ExposesStructuredDetail(t *testing.T) {
	t.Parallel()

	conflict := types.NewPortConflictError("web", 80, 8080, 8081,
		types.NewError(types.ErrCodeUsageValidation, "local port is already in use", "free 127.0.0.1:8080 or pass --port 8080=NEW"))

	wrapped := fmt.Errorf("planning install: %w", conflict)

	var detail *types.PortConflictError
	require.True(t, errors.As(wrapped, &detail))
	assert.Equal(t, "web", detail.Service)
	assert.Equal(t, 80, detail.ContainerPort)
	assert.Equal(t, 8080, detail.ConflictingHostPort)
	assert.Equal(t, 8081, detail.SuggestedHostPort)

	assert.True(t, types.IsCode(wrapped, types.ErrCodeUsageValidation),
		"conflict must still map to the usage-validation exit code")
	assert.NotEmpty(t, conflict.Error())
}

// TestPortConflictError_SuggestionZeroMeansNone documents the fail-closed
// sentinel: a zero suggested port means no free port was found.
func TestPortConflictError_SuggestionZeroMeansNone(t *testing.T) {
	t.Parallel()

	conflict := types.NewPortConflictError("web", 80, 8080, 0,
		types.NewError(types.ErrCodeUsageValidation, "local port is already in use", ""))

	assert.Equal(t, 0, conflict.SuggestedHostPort)
}

// TestPortConflictError_ZeroValueErrorNoPanic verifies a zero-value or
// directly-constructed conflict error (nil Err) returns a stable string from
// Error() instead of panicking on the nil delegate.
func TestPortConflictError_ZeroValueErrorNoPanic(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		assert.Equal(t, "port conflict", (&types.PortConflictError{}).Error())
	})
}

// TestInstallRequest_PortOverrides_JSONContract verifies the override map
// serializes under the stable "port_overrides" key (and is omitted when empty,
// so existing goldens stay byte-compatible).
func TestInstallRequest_PortOverrides_JSONContract(t *testing.T) {
	t.Parallel()

	got, err := json.Marshal(types.InstallRequest{
		AppID:         "appflowy",
		PortOverrides: map[int]int{8080: 8081},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"app_id":"appflowy","port_overrides":{"8080":8081}}`, string(got))

	bare, err := json.Marshal(types.InstallRequest{AppID: "appflowy"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"app_id":"appflowy"}`, string(bare))
}
