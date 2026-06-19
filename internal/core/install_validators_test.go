package core

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// checkPortAvailable probes a host port before install commits to it. The
// invalid-protocol arm guards against a malformed catalog reaching the
// listener APIs, and the udp arm proves a free ephemeral port is accepted.

func TestCheckPortAvailable_RejectsInvalidProtocol(t *testing.T) {
	t.Parallel()

	err := checkPortAvailable(t.Context(), types.PortBinding{
		Service:  "web",
		HostIP:   "127.0.0.1",
		HostPort: 0,
		Protocol: "icmp",
	})
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
}

func TestCheckPortAvailable_AcceptsFreeUDPEphemeralPort(t *testing.T) {
	t.Parallel()

	// Host port 0 asks the kernel for a free ephemeral port, so the udp arm
	// binds and closes deterministically without racing a fixed port.
	err := checkPortAvailable(t.Context(), types.PortBinding{
		Service:  "dns",
		HostIP:   "127.0.0.1",
		HostPort: 0,
		Protocol: "udp",
	})
	require.NoError(t, err)
}

// validateTimezone is the IANA-name gate on the configured timezone. The
// empty arm short-circuits before any lookup; the LoadLocation-error arm is
// driven through the injected deps seam so no real tzdata is required.

func TestValidateTimezone_RejectsEmptyValue(t *testing.T) {
	t.Parallel()

	loadCalled := false
	deps := timezoneLookupDeps{
		LoadLocation: func(string) (*time.Location, error) {
			loadCalled = true
			return nil, nil
		},
	}

	_, err := validateTimezone("   ", deps)
	require.Error(t, err)
	require.False(t, loadCalled, "empty timezone must short-circuit before LoadLocation")

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func TestValidateTimezone_RejectsLoadLocationError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("unknown time zone Mars/Olympus")
	deps := timezoneLookupDeps{
		LoadLocation: func(name string) (*time.Location, error) {
			require.Equal(t, "Mars/Olympus", name)
			return nil, wantErr
		},
	}

	got, err := validateTimezone("Mars/Olympus", deps)
	require.Error(t, err)
	require.Empty(t, got)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	require.ErrorIs(t, err, wantErr)
}
