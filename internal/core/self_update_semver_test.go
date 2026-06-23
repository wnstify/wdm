package core

import "testing"

// selfUpdateAvailable and selfUpdateNotOlder must treat a self-update as a
// strictly-newer-only operation when both versions are valid semver, so a
// moved or forged "latest" can never downgrade or re-install the same version.
// Invalid/dev pairs fall back to the prior "differs"/"permit" posture so
// unstamped builds still self-update.
func TestSelfUpdateAvailable_StrictlyNewer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{name: "downgrade refused", current: "v2.0.0", latest: "v1.0.0", want: false},
		{name: "forward upgrade", current: "v1.0.0", latest: "v2.0.0", want: true},
		{name: "same version", current: "v1.0.0", latest: "v1.0.0", want: false},
		{name: "no v prefix forward", current: "1.0.0", latest: "1.1.0", want: true},
		{name: "invalid latest falls back to differs", current: "v1.0.0", latest: "nightly", want: true},
		{name: "invalid latest equal falls back", current: "weird", latest: "weird", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := selfUpdateAvailable(tt.current, tt.latest); got != tt.want {
				t.Errorf("selfUpdateAvailable(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestSelfUpdateNotOlder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		current   string
		candidate string
		want      bool
	}{
		{name: "downgrade blocked", current: "v2.0.0", candidate: "v1.0.0", want: false},
		{name: "same blocked", current: "v1.0.0", candidate: "v1.0.0", want: false},
		{name: "newer permitted", current: "v1.0.0", candidate: "v1.0.1", want: true},
		{name: "invalid current permits", current: "dev", candidate: "v1.0.0", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := selfUpdateNotOlder(tt.current, tt.candidate); got != tt.want {
				t.Errorf("selfUpdateNotOlder(%q, %q) = %v, want %v", tt.current, tt.candidate, got, tt.want)
			}
		})
	}
}
