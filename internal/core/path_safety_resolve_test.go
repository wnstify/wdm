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
		name string
		cfg  config
	}{
		{
			name: "relative config path",
			cfg:  config{configPath: "relative/config.toml"},
		},
		{
			name: "relative state dir",
			cfg:  config{stateDir: "relative/state"},
		},
		{
			name: "relative data dir",
			cfg:  config{dataDir: "relative/share"},
		},
		{
			name: "relative stack base dir",
			cfg:  config{stackBaseDir: "relative/docker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := tt.cfg
			err := resolveDirs(&cfg)
			require.Error(t, err)
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
	require.Empty(t, base)
}
