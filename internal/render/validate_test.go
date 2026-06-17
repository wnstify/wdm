package render_test

import (
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/render"
)

func TestType_IsValid(t *testing.T) {
	t.Parallel()

	valid := []render.Type{
		render.TypeString,
		render.TypeDomain,
		render.TypePort,
		render.TypeSecret,
		render.TypeTimezone,
		render.TypePath,
		render.TypeBool,
	}
	for _, ty := range valid {
		if !ty.IsValid() {
			t.Errorf("Type(%q).IsValid() = false, want true", string(ty))
		}
	}

	invalid := []render.Type{
		render.Type(""),
		render.Type("bogus"),
		render.Type("STRING"),
		render.Type("number"),
	}
	for _, ty := range invalid {
		if ty.IsValid() {
			t.Errorf("Type(%q).IsValid() = true, want false", string(ty))
		}
	}
}

func TestValidatePlaceholders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		placeholders []render.Placeholder
		wantErr      error
	}{
		{
			name:         "nil set passes",
			placeholders: nil,
			wantErr:      nil,
		},
		{
			name:         "empty slice passes",
			placeholders: []render.Placeholder{},
			wantErr:      nil,
		},
		{
			name: "single valid placeholder passes",
			placeholders: []render.Placeholder{
				{Name: "DB_PASSWORD", Type: render.TypeSecret, Required: true},
			},
			wantErr: nil,
		},
		{
			name: "all enum values pass",
			placeholders: []render.Placeholder{
				{Name: "FREE", Type: render.TypeString},
				{Name: "HOST", Type: render.TypeDomain},
				{Name: "PORT", Type: render.TypePort},
				{Name: "PASS", Type: render.TypeSecret},
				{Name: "TZ", Type: render.TypeTimezone},
				{Name: "DATA_PATH", Type: render.TypePath},
				{Name: "ENABLED", Type: render.TypeBool},
			},
			wantErr: nil,
		},
		{
			name: "empty name at index 0 fails with ErrPlaceholderNameEmpty",
			placeholders: []render.Placeholder{
				{Name: "", Type: render.TypeString},
			},
			wantErr: render.ErrPlaceholderNameEmpty,
		},
		{
			name: "empty name at later index fails with ErrPlaceholderNameEmpty",
			placeholders: []render.Placeholder{
				{Name: "FIRST", Type: render.TypeString},
				{Name: "", Type: render.TypeSecret},
			},
			wantErr: render.ErrPlaceholderNameEmpty,
		},
		{
			name: "invalid type fails with ErrPlaceholderTypeInvalid",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.Type("bogus")},
			},
			wantErr: render.ErrPlaceholderTypeInvalid,
		},
		{
			name: "empty type fails with ErrPlaceholderTypeInvalid",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.Type("")},
			},
			wantErr: render.ErrPlaceholderTypeInvalid,
		},
		{
			name: "duplicate name fails with ErrPlaceholderNameDuplicate",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString},
				{Name: "X", Type: render.TypeSecret},
			},
			wantErr: render.ErrPlaceholderNameDuplicate,
		},
		{
			name: "empty name beats invalid type",
			placeholders: []render.Placeholder{
				{Name: "", Type: render.Type("bogus")},
			},
			wantErr: render.ErrPlaceholderNameEmpty,
		},
		{
			name: "invalid type beats duplicate name",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString},
				{Name: "X", Type: render.Type("bogus")},
			},
			wantErr: render.ErrPlaceholderTypeInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := render.ValidatePlaceholders(tc.placeholders)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidatePlaceholders returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePlaceholders returned nil, want error wrapping %v", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidatePlaceholders returned %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

func TestValidateResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		placeholders []render.Placeholder
		resolved     map[string]string
		wantErr      error
	}{
		{
			name:         "nil placeholders and nil resolution pass",
			placeholders: nil,
			resolved:     nil,
			wantErr:      nil,
		},
		{
			name:         "empty placeholders and empty resolution pass",
			placeholders: []render.Placeholder{},
			resolved:     map[string]string{},
			wantErr:      nil,
		},
		{
			name: "required placeholder present passes",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString, Required: true},
			},
			resolved: map[string]string{"X": "value"},
			wantErr:  nil,
		},
		{
			name: "optional placeholder absent passes",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString, Required: false},
			},
			resolved: map[string]string{},
			wantErr:  nil,
		},
		{
			name: "optional placeholder present passes",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString, Required: false},
			},
			resolved: map[string]string{"X": "value"},
			wantErr:  nil,
		},
		{
			name: "mixed required and optional with required present passes",
			placeholders: []render.Placeholder{
				{Name: "REQ", Type: render.TypeSecret, Required: true},
				{Name: "OPT", Type: render.TypeString, Required: false},
			},
			resolved: map[string]string{"REQ": "value"},
			wantErr:  nil,
		},
		{
			name: "required placeholder missing fails with ErrResolutionMissingPlaceholder",
			placeholders: []render.Placeholder{
				{Name: "X", Type: render.TypeString, Required: true},
			},
			resolved: map[string]string{},
			wantErr:  render.ErrResolutionMissingPlaceholder,
		},
		{
			name: "first required missing wins in slice order",
			placeholders: []render.Placeholder{
				{Name: "REQ_A", Type: render.TypeSecret, Required: true},
				{Name: "REQ_B", Type: render.TypeSecret, Required: true},
			},
			resolved: map[string]string{},
			wantErr:  render.ErrResolutionMissingPlaceholder,
		},
		{
			name:         "extra key on empty placeholders fails with ErrResolutionExtraKey",
			placeholders: []render.Placeholder{},
			resolved:     map[string]string{"GHOST": "value"},
			wantErr:      render.ErrResolutionExtraKey,
		},
		{
			name: "extra key alongside declared keys fails with ErrResolutionExtraKey",
			placeholders: []render.Placeholder{
				{Name: "DECLARED", Type: render.TypeString, Required: true},
			},
			resolved: map[string]string{
				"DECLARED": "value",
				"GHOST":    "stray",
			},
			wantErr: render.ErrResolutionExtraKey,
		},
		{
			name: "missing required beats extra key",
			placeholders: []render.Placeholder{
				{Name: "REQ", Type: render.TypeSecret, Required: true},
			},
			resolved: map[string]string{"GHOST": "stray"},
			wantErr:  render.ErrResolutionMissingPlaceholder,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := render.ValidateResolution(tc.placeholders, tc.resolved)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateResolution returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateResolution returned nil, want error wrapping %v", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateResolution returned %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

// TestValidateResolution_ExtraKeyDeterministic confirms the
// extra-key error reports a stable key (the lowest sort order)
// across runs, since the iteration in ValidateResolution copies and
// sorts the resolved-map keys before reporting.
func TestValidateResolution_ExtraKeyDeterministic(t *testing.T) {
	t.Parallel()

	resolved := map[string]string{
		"ZULU":  "z",
		"ALPHA": "a",
		"MIKE":  "m",
	}
	err := render.ValidateResolution(nil, resolved)
	if err == nil {
		t.Fatal("ValidateResolution returned nil, want error wrapping ErrResolutionExtraKey")
	}
	if !errors.Is(err, render.ErrResolutionExtraKey) {
		t.Fatalf("ValidateResolution returned %v, want errors.Is(_, ErrResolutionExtraKey)", err)
	}
	// Sort order means ALPHA is reported first.
	const wantSubstring = `"ALPHA"`
	if msg := err.Error(); !contains(msg, wantSubstring) {
		t.Fatalf("ValidateResolution error %q does not name the lowest-sort-order extra key %s", msg, wantSubstring)
	}
}

// contains is a tiny strings.Contains shim so this test file keeps
// its import surface minimal (errors + testing + the package under
// test).
func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
