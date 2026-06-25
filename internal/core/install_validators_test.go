package core

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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

// resolveBoolPlaceholder is the only bool-typed placeholder resolver and no
// catalog app currently declares one, so it carries no install-path coverage.
// These cases pin its arms: default fallback, required-missing refusal,
// ParseBool normalization/rejection, and the optional-missing short-circuit to
// "" (consistent with the string and port resolvers).
func TestResolveBoolPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		ph              catalog.Placeholder
		value           string
		hasRequestValue bool
		want            string
		wantErr         bool
	}{
		{name: "request value normalized true", value: "1", hasRequestValue: true, want: "true"},
		{name: "request value normalized false", value: "FALSE", hasRequestValue: true, want: "false"},
		{name: "default applied when unset", ph: catalog.Placeholder{Default: true}, hasRequestValue: false, want: "true"},
		{name: "required and missing", ph: catalog.Placeholder{Name: "FLAG", Required: true}, hasRequestValue: false, wantErr: true},
		{name: "optional and missing short-circuits to empty", ph: catalog.Placeholder{Name: "FLAG"}, hasRequestValue: false, want: ""},
		{name: "invalid request value", value: "maybe", hasRequestValue: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveBoolPlaceholder(tt.ph, tt.value, tt.hasRequestValue)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// resolveStringPlaceholder feeds raw placeholder values into the .env, so its
// non-validation arms (default fallback, required-missing refusal, optional
// omission) carry no install-path coverage. The control-char arms are pinned in
// TestResolveStringPlaceholder_WDMSEC005; these cover the decision around them.
func TestResolveStringPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		ph              catalog.Placeholder
		value           string
		hasRequestValue bool
		want            string
		wantErr         bool
	}{
		{name: "request value passes through", value: "admin", hasRequestValue: true, want: "admin"},
		{name: "default applied when unset", ph: catalog.Placeholder{Default: "fallback"}, want: "fallback"},
		{name: "required and missing", ph: catalog.Placeholder{Name: "USER", Required: true}, wantErr: true},
		{name: "optional and missing yields empty", ph: catalog.Placeholder{Name: "USER"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveStringPlaceholder(tt.ph, tt.value, tt.hasRequestValue)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// resolvePortPlaceholder gates a host port before it reaches a binding, so its
// default fallback, required-missing refusal, and 1..65535 bounds check are
// pinned here against the real resolver.
func TestResolvePortPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		ph              catalog.Placeholder
		value           string
		hasRequestValue bool
		want            string
		wantErr         bool
	}{
		{name: "request value accepted", value: "8080", hasRequestValue: true, want: "8080"},
		{name: "default applied when unset", ph: catalog.Placeholder{Default: 9000}, want: "9000"},
		{name: "required and missing", ph: catalog.Placeholder{Name: "PORT", Required: true}, wantErr: true},
		{name: "non-numeric", value: "abc", hasRequestValue: true, wantErr: true},
		{name: "below range", value: "0", hasRequestValue: true, wantErr: true},
		{name: "above range", value: "65536", hasRequestValue: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolvePortPlaceholder(tt.ph, tt.value, tt.hasRequestValue)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// resolveDomainPlaceholder is the public-hostname gate. The required-missing and
// invalid-domain arms each fail closed with a usage error; the valid arm must
// normalize case and trailing dot.
func TestResolveDomainPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ph      catalog.Placeholder
		value   string
		want    string
		wantErr bool
	}{
		{name: "valid normalized", value: "App.Example.COM.", want: "app.example.com"},
		{name: "required and empty", ph: catalog.Placeholder{Name: "DOMAIN", Required: true}, wantErr: true},
		{name: "url not hostname", value: "https://app.example.com", wantErr: true},
		{name: "ip literal", value: "192.168.0.1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveDomainPlaceholder(tt.ph, tt.value)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// resolvePathPlaceholder binds a host path into the stack, so it must reject
// relative, missing, and inside-stack paths and resolve a valid outside path
// through real symlink evaluation (no mock of the filesystem seam).
func TestResolvePathPlaceholder(t *testing.T) {
	t.Parallel()

	// Resolve the stack root through symlinks up front so the inside-stack
	// comparison is robust on platforms (macOS) where TempDir lives under a
	// symlinked /var -> /private/var.
	stackRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	inside := filepath.Join(stackRoot, "data")
	require.NoError(t, os.Mkdir(inside, 0o755))
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	plan := &installPlan{stackPath: stackRoot}

	t.Run("required and missing", func(t *testing.T) {
		t.Parallel()
		_, err := plan.resolvePathPlaceholder(catalog.Placeholder{Name: "DATA", Required: true}, "", false)
		requireUsageError(t, err)
	})

	t.Run("optional and missing yields empty", func(t *testing.T) {
		t.Parallel()
		got, err := plan.resolvePathPlaceholder(catalog.Placeholder{Name: "DATA"}, "", false)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("relative path rejected", func(t *testing.T) {
		t.Parallel()
		_, err := plan.resolvePathPlaceholder(catalog.Placeholder{Name: "DATA"}, "relative/dir", true)
		requireUsageError(t, err)
	})

	t.Run("non-existent path rejected", func(t *testing.T) {
		t.Parallel()
		_, err := plan.resolvePathPlaceholder(
			catalog.Placeholder{Name: "DATA"},
			filepath.Join(outside, "does-not-exist"),
			true,
		)
		requireUsageError(t, err)
	})

	t.Run("inside stack rejected", func(t *testing.T) {
		t.Parallel()
		_, err := plan.resolvePathPlaceholder(catalog.Placeholder{Name: "DATA"}, inside, true)
		requireUsageError(t, err)
	})

	t.Run("valid outside path resolves", func(t *testing.T) {
		t.Parallel()
		got, err := plan.resolvePathPlaceholder(catalog.Placeholder{Name: "DATA"}, outside, true)
		require.NoError(t, err)
		require.Equal(t, outside, got)
	})
}

// addSyntheticResolvedValue injects wdm-owned template values; a name that
// collides with an already-resolved placeholder is a catalog defect and must
// fail closed rather than silently overwrite.
func TestAddSyntheticResolvedValue(t *testing.T) {
	t.Parallel()

	t.Run("adds new value", func(t *testing.T) {
		t.Parallel()
		plan := &installPlan{resolvedValues: map[string]string{}}
		require.NoError(t, plan.addSyntheticResolvedValue("UID", "1000"))
		require.Equal(t, "1000", plan.resolvedValues["UID"])
		require.Len(t, plan.placeholders, 1)
	})

	t.Run("rejects collision", func(t *testing.T) {
		t.Parallel()
		plan := &installPlan{resolvedValues: map[string]string{"UID": "1000"}}
		err := plan.addSyntheticResolvedValue("UID", "1001")
		require.Error(t, err)
		var typedErr *types.Error
		require.ErrorAs(t, err, &typedErr)
		require.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
	})
}

// resolveTimezone walks an ordered detection chain (explicit value, $TZ,
// /etc/timezone, /etc/localtime symlink) and fails closed when none resolve.
// The injected deps seam drives every arm with no real tzdata or host files;
// passing partial deps also exercises completeTimezoneLookupDeps' os defaults.
func TestResolveTimezone(t *testing.T) {
	t.Parallel()

	okLoad := func(string) (*time.Location, error) { return time.UTC, nil }
	notExist := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	notExistLink := func(string) (string, error) { return "", fs.ErrNotExist }
	hardErr := errors.New("permission denied")
	// Fail-loud stubs for deps a case must never reach: if a future control-flow
	// change calls them, the test fails instead of silently hitting the host FS.
	failLink := func(string) (string, error) {
		t.Errorf("ReadLink must not be called")
		return "", fs.ErrNotExist
	}
	failLoad := func(string) (*time.Location, error) {
		t.Errorf("LoadLocation must not be called")
		return nil, errors.New("unexpected LoadLocation call")
	}

	tests := []struct {
		name    string
		value   string
		deps    timezoneLookupDeps
		want    string
		wantErr bool
	}{
		{
			name:  "explicit value",
			value: "Europe/Bratislava",
			deps:  timezoneLookupDeps{LoadLocation: okLoad},
			want:  "Europe/Bratislava",
		},
		{
			name: "from TZ env",
			deps: timezoneLookupDeps{
				LookupEnv:    func(string) (string, bool) { return "UTC", true },
				ReadFile:     notExist,
				ReadLink:     notExistLink,
				LoadLocation: okLoad,
			},
			want: "UTC",
		},
		{
			name: "from etc timezone",
			deps: timezoneLookupDeps{
				LookupEnv:    func(string) (string, bool) { return "", false },
				ReadFile:     func(string) ([]byte, error) { return []byte("America/New_York\n"), nil },
				ReadLink:     notExistLink,
				LoadLocation: okLoad,
			},
			want: "America/New_York",
		},
		{
			name: "from localtime link",
			deps: timezoneLookupDeps{
				LookupEnv:    func(string) (string, bool) { return "", false },
				ReadFile:     notExist,
				ReadLink:     func(string) (string, error) { return "/usr/share/zoneinfo/Asia/Tokyo", nil },
				LoadLocation: okLoad,
			},
			want: "Asia/Tokyo",
		},
		{
			name: "none detected fails closed",
			deps: timezoneLookupDeps{
				LookupEnv: func(string) (string, bool) { return "", false },
				ReadFile:  notExist,
				ReadLink:  notExistLink,
			},
			wantErr: true,
		},
		{
			name: "etc timezone hard error",
			deps: timezoneLookupDeps{
				LookupEnv:    func(string) (string, bool) { return "", false },
				ReadFile:     func(string) ([]byte, error) { return nil, hardErr },
				ReadLink:     failLink,
				LoadLocation: failLoad,
			},
			wantErr: true,
		},
		{
			name: "localtime link hard error",
			deps: timezoneLookupDeps{
				LookupEnv:    func(string) (string, bool) { return "", false },
				ReadFile:     notExist,
				ReadLink:     func(string) (string, error) { return "", hardErr },
				LoadLocation: failLoad,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveTimezone(tt.value, "", tt.deps)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// timezoneFromLocaltimeLink extracts the IANA name from a zoneinfo symlink
// target. A target without the marker, or with nothing after it, must report
// "not found" so resolveTimezone falls through instead of binding a bad name.
func TestTimezoneFromLocaltimeLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		link   string
		want   string
		wantOK bool
	}{
		{name: "standard target", link: "/usr/share/zoneinfo/Europe/Berlin", want: "Europe/Berlin", wantOK: true},
		{name: "no marker", link: "/etc/localtime", wantOK: false},
		{name: "empty after marker", link: "/usr/share/zoneinfo/", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := timezoneFromLocaltimeLink(tt.link)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// parsePortRange parses a catalog "lo-hi" host range; every malformed shape is a
// catalog defect that must fail closed, and a valid range returns its bounds.
func TestParsePortRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantLo  int
		wantHi  int
		wantErr bool
	}{
		{name: "valid range", spec: "8000-8002", wantLo: 8000, wantHi: 8002},
		{name: "missing dash", spec: "8000", wantErr: true},
		{name: "non-numeric", spec: "a-b", wantErr: true},
		{name: "out of bounds", spec: "0-70000", wantErr: true},
		{name: "lo greater than hi", spec: "9000-8000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lo, hi, err := parsePortRange("web", tt.spec)
			if tt.wantErr {
				require.Error(t, err)
				var typedErr *types.Error
				require.ErrorAs(t, err, &typedErr)
				require.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantLo, lo)
			require.Equal(t, tt.wantHi, hi)
		})
	}
}

// parseMemoryBytes is the Docker `<int><b|k|m|g>` parser feeding resource caps.
// Every unit multiplier, the empty/zero/overflow guards, and the bad-unit arm
// are pinned so a malformed catalog limit cannot slip a wrong byte count through.
func TestParseMemoryBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    uint64
		wantErr bool
	}{
		{name: "bytes", value: "512b", want: 512},
		{name: "kilobytes", value: "2k", want: 2 * 1024},
		{name: "megabytes", value: "256m", want: 256 * 1024 * 1024},
		{name: "gigabytes", value: "1g", want: 1024 * 1024 * 1024},
		{name: "empty", value: "", wantErr: true},
		{name: "bad unit", value: "512x", wantErr: true},
		{name: "non-numeric amount", value: "abcm", wantErr: true},
		{name: "zero amount", value: "0m", wantErr: true},
		{name: "overflow", value: "20000000000000000000g", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseMemoryBytes(tt.value)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// validateMemoryOverride bounds a user memory override to the catalog band. The
// out-of-band arms must fail with a usage error; a malformed catalog band is a
// verification failure, distinguishing operator error from catalog defect.
func TestValidateMemoryOverride(t *testing.T) {
	t.Parallel()

	band := catalog.ResourceProfile{
		Service: "app",
		Memory:  catalog.MemoryBand{Min: "256m", Max: "1g"},
	}

	t.Run("within band", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validateMemoryOverride(band, "512m"))
	})

	t.Run("below min", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateMemoryOverride(band, "128m"))
	})

	t.Run("above max", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateMemoryOverride(band, "2g"))
	})

	t.Run("invalid override value", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateMemoryOverride(band, "lots"))
	})

	t.Run("invalid catalog band", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []catalog.MemoryBand{{Min: "bad", Max: "1g"}, {Min: "256m", Max: "bad"}} {
			err := validateMemoryOverride(catalog.ResourceProfile{Service: "app", Memory: bad}, "512m")
			require.Error(t, err)
			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			require.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
		}
	})
}

// validateCPUOverride bounds a user CPU override to the catalog decimal band,
// mirroring validateMemoryOverride: out-of-band is a usage error, a malformed
// catalog band is a verification failure.
func TestValidateCPUOverride(t *testing.T) {
	t.Parallel()

	band := catalog.ResourceProfile{
		Service: "app",
		CPUs:    catalog.CPUBand{Min: "0.25", Max: "2.0"},
	}

	t.Run("within band", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validateCPUOverride(band, "1.0"))
	})

	t.Run("below min", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateCPUOverride(band, "0.1"))
	})

	t.Run("above max", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateCPUOverride(band, "4.0"))
	})

	t.Run("invalid override value", func(t *testing.T) {
		t.Parallel()
		requireUsageError(t, validateCPUOverride(band, "fast"))
	})

	t.Run("invalid catalog band", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []catalog.CPUBand{{Min: "bad", Max: "2.0"}, {Min: "0.25", Max: "bad"}} {
			err := validateCPUOverride(catalog.ResourceProfile{Service: "app", CPUs: bad}, "1.0")
			require.Error(t, err)
			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			require.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
		}
	})
}

// validatePIDsOverride bounds a user pids override to [1, profile max]; both
// ends of the range and a valid value are pinned against the real validator.
func TestValidatePIDsOverride(t *testing.T) {
	t.Parallel()

	band := catalog.ResourceProfile{Service: "app", PIDs: catalog.PIDsBand{Max: 512}}

	tests := []struct {
		name    string
		pids    int
		wantErr bool
	}{
		{name: "within range", pids: 256},
		{name: "at max", pids: 512},
		{name: "below one", pids: 0, wantErr: true},
		{name: "above max", pids: 1024, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validatePIDsOverride(band, tt.pids)
			if tt.wantErr {
				requireUsageError(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// requireUsageError asserts err carries a typed usage-validation code.
func requireUsageError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}
