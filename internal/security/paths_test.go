package security_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
)

func TestRejectUnsafeRoot_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		wantErr error // nil means: must accept
	}{
		// Rejected: empty / relative.
		{"empty", "", security.ErrUnsafePath},
		{"relative_bare", "docker", security.ErrUnsafePath},
		{"relative_dotslash", "./docker", security.ErrUnsafePath},
		{"relative_parent", "../docker", security.ErrUnsafePath},

		// Rejected: filesystem root.
		{"root_slash", "/", security.ErrUnsafePath},

		// Rejected: reserved system paths (exact match).
		{"etc", "/etc", security.ErrUnsafePath},
		{"usr", "/usr", security.ErrUnsafePath},
		{"var", "/var", security.ErrUnsafePath},
		{"bin", "/bin", security.ErrUnsafePath},
		{"sbin", "/sbin", security.ErrUnsafePath},
		{"boot", "/boot", security.ErrUnsafePath},
		{"dev", "/dev", security.ErrUnsafePath},
		{"proc", "/proc", security.ErrUnsafePath},
		{"sys", "/sys", security.ErrUnsafePath},
		{"lib", "/lib", security.ErrUnsafePath},
		{"lib32", "/lib32", security.ErrUnsafePath},
		{"lib64", "/lib64", security.ErrUnsafePath},
		{"root_home", "/root", security.ErrUnsafePath},

		// Rejected: descendants of reserved paths.
		{"etc_descendant", "/etc/wdm", security.ErrUnsafePath},
		{"usr_descendant", "/usr/local/wdm", security.ErrUnsafePath},
		{"var_descendant", "/var/lib/wdm", security.ErrUnsafePath},
		{"root_descendant", "/root/docker", security.ErrUnsafePath},

		// Rejected: traversal that lands inside a reserved path after Clean.
		{"traversal_into_etc", "/home/../etc/wdm", security.ErrUnsafePath},

		// Accepted: user homes, /opt, /srv, and other non-reserved locations.
		{"home_linux", "/home/alice/docker", nil},
		{"home_macos", "/Users/alice/docker", nil},
		{"opt", "/opt/wdm", nil},
		{"srv", "/srv/data", nil},
		{"mnt_external_disk", "/mnt/external/docker", nil},
		{"home_with_trailing_slash", "/home/alice/docker/", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := security.RejectUnsafeRoot(tc.path)
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr),
				"want errors.Is(err, %v); got %v", tc.wantErr, err)
		})
	}
}

func TestEnsureWithinRoot_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		root      string
		candidate string
		wantErr   error // nil means: must accept
	}{
		// Input validation rejections (ErrUnsafePath).
		{"empty_root", "", "/home/alice/docker", security.ErrUnsafePath},
		{"empty_candidate", "/home/alice", "", security.ErrUnsafePath},
		{"relative_root", "home/alice", "/home/alice/docker", security.ErrUnsafePath},
		{"relative_candidate", "/home/alice", "docker", security.ErrUnsafePath},

		// Escape rejections (ErrPathEscape).
		{"sibling", "/home/alice", "/home/bob", security.ErrPathEscape},
		{"parent_dir", "/home/alice/docker", "/home/alice", security.ErrPathEscape},
		{"traversal_above_root", "/home/alice/docker", "/home/alice/docker/../../bob", security.ErrPathEscape},

		// Accepted.
		{"same_path", "/home/alice/docker", "/home/alice/docker", nil},
		{"trailing_slash_root", "/home/alice/docker/", "/home/alice/docker/app", nil},
		{"direct_child", "/home/alice/docker", "/home/alice/docker/app", nil},
		{"deep_child", "/home/alice/docker", "/home/alice/docker/app/sub/leaf", nil},
		{"traversal_within_root", "/home/alice/docker", "/home/alice/docker/app/./sub", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := security.EnsureWithinRoot(tc.root, tc.candidate)
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr),
				"want errors.Is(err, %v); got %v", tc.wantErr, err)
		})
	}
}

func TestSafeJoin_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		root     string
		sub      string
		wantPath string // empty when error expected
		wantErr  error  // nil means: must accept
	}{
		// Input validation rejections.
		{"empty_root", "", "sub", "", security.ErrUnsafePath},
		{"empty_sub", "/home/alice/docker", "", "", security.ErrUnsafePath},
		{"relative_root", "home/alice", "sub", "", security.ErrUnsafePath},

		// Escape rejections.
		{"absolute_sub", "/home/alice/docker", "/etc/passwd", "", security.ErrPathEscape},
		{"parent_traversal_sub", "/home/alice/docker", "../bob", "", security.ErrPathEscape},
		{"deep_traversal_sub", "/home/alice/docker", "app/../../bob", "", security.ErrPathEscape},

		// Accepted.
		{"direct_child", "/home/alice/docker", "vaultwarden", "/home/alice/docker/vaultwarden", nil},
		{"nested_child", "/home/alice/docker", "vaultwarden/.env", "/home/alice/docker/vaultwarden/.env", nil},
		{"dot_subpath", "/home/alice/docker", ".", "/home/alice/docker", nil},
		{"trailing_slash_root", "/home/alice/docker/", "vaultwarden", "/home/alice/docker/vaultwarden", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := security.SafeJoin(tc.root, tc.sub)
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, tc.wantPath, got)
				return
			}
			require.Error(t, err)
			assert.Empty(t, got)
			assert.True(t, errors.Is(err, tc.wantErr),
				"want errors.Is(err, %v); got %v", tc.wantErr, err)
		})
	}
}

// TestSentinels_AreDistinct guards against an accidental refactor
// that collapses [security.ErrUnsafePath] and [security.ErrPathEscape]
// into one sentinel. Callers need to distinguish "input itself is
// forbidden" from "input would escape its sandbox" so the
// user-facing hint can name the cause.
func TestSentinels_AreDistinct(t *testing.T) {
	t.Parallel()

	require.NotNil(t, security.ErrUnsafePath)
	require.NotNil(t, security.ErrPathEscape)
	assert.NotEqual(t, security.ErrUnsafePath, security.ErrPathEscape)
	assert.False(t, errors.Is(security.ErrUnsafePath, security.ErrPathEscape))
	assert.False(t, errors.Is(security.ErrPathEscape, security.ErrUnsafePath))
}
