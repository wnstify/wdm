package core

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
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

// WDM-SEC-005: resolveStringPlaceholder must reject CR/LF/NUL in a type:string
// placeholder value before it reaches the .env template (the --set
// line-injection vector that could append KEY=VALUE lines and override a
// generated secret), on BOTH the request-value branch and the default-value
// branch, while letting a clean value through unchanged. The default branch was
// flagged untested in review, so it is driven here through the real
// resolveStringPlaceholder -> validateStringPlaceholderValue path (no mock).
func TestResolveStringPlaceholder_WDMSEC005_RejectsControlChars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		wantError bool
	}{
		{name: "clean value", value: "admin", wantError: false},
		{name: "newline injection", value: "admin\nADMIN_PASSWORD=pwned", wantError: true},
		{name: "carriage return", value: "admin\rextra", wantError: true},
		{name: "nul byte", value: "admin\x00", wantError: true},
	}

	// Both code paths run the same real validator; assert each independently so a
	// future change that drops the check on either branch fails this test.
	branches := []struct {
		name            string
		hasRequestValue bool
	}{
		{name: "request value branch", hasRequestValue: true},
		{name: "default value branch", hasRequestValue: false},
	}

	for _, br := range branches {
		for _, tt := range tests {
			t.Run(br.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				ph := catalog.Placeholder{Name: "ADMIN_USER"}
				value := tt.value
				if !br.hasRequestValue {
					// Drive the default-value branch: the value comes from the
					// catalog default, not the request.
					ph.Default = tt.value
					value = ""
				}

				got, err := resolveStringPlaceholder(ph, value, br.hasRequestValue)
				if !tt.wantError {
					require.NoError(t, err)
					require.Equal(t, tt.value, got)
					return
				}

				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
			})
		}
	}
}

// FuzzValidateStringPlaceholderValue locks the WDM-SEC-005 invariant directly on
// the real validator: if validateStringPlaceholderValue accepts a value (returns
// nil), that value MUST contain no CR/LF/NUL. Any future change that lets a
// control character slip through the .env-injection guard fails here. The seed
// corpus runs under plain `go test`, so the invariant is checked even without
// `-fuzz`.
func FuzzValidateStringPlaceholderValue(f *testing.F) {
	f.Add("admin")
	f.Add("admin\nADMIN_PASSWORD=pwned")
	f.Add("admin\rextra")
	f.Add("admin\x00")

	f.Fuzz(func(t *testing.T, value string) {
		if err := validateStringPlaceholderValue("ADMIN_USER", value); err == nil {
			if strings.ContainsAny(value, "\r\n\x00") {
				t.Fatalf("validateStringPlaceholderValue accepted %q containing a control character", value)
			}
		}
	})
}
