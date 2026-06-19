package core

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// resolveDirs and resolveStackBase are the construction-time gates that keep
// every later write anchored to an absolute, caller-vetted base (PRD §29
// no-write-outside-stack). These tests drive the reject arms directly so a
// relative path can never silently become a working directory.

func TestResolveDirs_RejectsRelativePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     config
		wantErr string
	}{
		{
			name:    "relative config path",
			cfg:     config{configPath: "relative/config.toml", stateDir: "/abs/state", dataDir: "/abs/share", stackBaseDir: "/abs/docker"},
			wantErr: "WithConfigPath requires absolute path",
		},
		{
			name:    "relative state dir",
			cfg:     config{configPath: "/abs/config.toml", stateDir: "relative/state", dataDir: "/abs/share", stackBaseDir: "/abs/docker"},
			wantErr: "WithStateDir requires absolute path",
		},
		{
			name:    "relative data dir",
			cfg:     config{configPath: "/abs/config.toml", stateDir: "/abs/state", dataDir: "relative/share", stackBaseDir: "/abs/docker"},
			wantErr: "WithDataDir requires absolute path",
		},
		{
			name:    "relative stack base dir",
			cfg:     config{configPath: "/abs/config.toml", stateDir: "/abs/state", dataDir: "/abs/share", stackBaseDir: "relative/docker"},
			wantErr: "WithStackBaseDir requires absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Sibling fields are absolute so only the field under test trips
			// the IsAbs gate; asserting the specific message pins each case to
			// its intended branch and keeps it independent of XDG/$HOME lookups.
			cfg := tt.cfg
			err := resolveDirs(&cfg)
			require.Error(t, err)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResolveStackBase_OverrideShortCircuitsExpansion(t *testing.T) {
	t.Parallel()

	// A non-empty override is returned verbatim without consulting settings,
	// so a nil settings pointer must not be dereferenced on this arm.
	base, err := resolveStackBase("/srv/docker", nil)
	require.NoError(t, err)
	require.Equal(t, "/srv/docker", base)
}

func TestResolveStackBase_RejectsRelativeSettingsBase(t *testing.T) {
	t.Parallel()

	// A settings BaseStackPath without a leading "~/" passes through
	// expandHome unchanged, so a relative value reaches the IsAbs gate and
	// must be rejected rather than used as a relative working directory.
	base, err := resolveStackBase("", &types.Settings{BaseStackPath: "docker"})
	require.Error(t, err)
	require.ErrorContains(t, err, "stack base must resolve to an absolute path")
	require.Empty(t, base)
}
