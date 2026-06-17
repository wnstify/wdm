//go:build unix

package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// validConfigTOML is the minimal payload that satisfies
// config/schema.json. Used as the baseline that other negative cases
// mutate one field at a time.
const validConfigTOML = `
schema_version = 1
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`

func TestLoadConfigBytes_AcceptsMinimalValid(t *testing.T) {
	t.Parallel()

	settings, err := state.LoadConfigBytes(t.Context(), []byte(validConfigTOML))
	require.NoError(t, err)
	require.NotNil(t, settings)

	assert.Equal(t, 1, settings.SchemaVersion)
	assert.Equal(t, "~/docker", settings.BaseStackPath)
	assert.Empty(t, settings.Timezone)
	assert.Equal(t, "wdm_default", settings.DefaultDockerNetwork)
	assert.Equal(t, "stable", settings.CatalogChannel)
	assert.Equal(t, "daily-on-launch", settings.UpdateCheckPreference)
}

func TestLoadConfigBytes_RejectsInvalidTOML(t *testing.T) {
	t.Parallel()

	// Trailing equals with no value — guaranteed parser failure.
	_, err := state.LoadConfigBytes(t.Context(), []byte("not a valid toml ="))
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrConfigInvalid))
}

func TestLoadConfigBytes_RejectsEmptyPayload(t *testing.T) {
	t.Parallel()

	// Parses to an empty map → schema rejects (missing required fields).
	_, err := state.LoadConfigBytes(t.Context(), []byte(""))
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrConfigInvalid))
}

// TestLoadConfigBytes_RejectsSchemaViolations runs one mutation per
// schema constraint, each case starting from a fresh valid baseline.
// The test asserts the class (errors.Is against ErrConfigInvalid),
// not the exact failure message — santhosh-tekuri's validation
// messages are stable but not part of the public contract.
func TestLoadConfigBytes_RejectsSchemaViolations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{
			"bad_schema_version",
			`schema_version = 2
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`,
		},
		{
			"bad_catalog_channel",
			`schema_version = 1
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "verified"
update_check_preference = "daily-on-launch"
`,
		},
		{
			"bad_update_pref",
			`schema_version = 1
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "weekly"
`,
		},
		{
			"bad_docker_network_with_spaces",
			`schema_version = 1
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "my network"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`,
		},
		{
			"empty_base_stack_path",
			`schema_version = 1
base_stack_path = ""
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`,
		},
		{
			"missing_required_base_stack_path",
			`schema_version = 1
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`,
		},
		{
			"additional_property",
			`schema_version = 1
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
extra_unknown_key = "nope"
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := state.LoadConfigBytes(t.Context(), []byte(tc.toml))
			require.Error(t, err)
			assert.True(t, errors.Is(err, types.ErrConfigInvalid),
				"want wrapped ErrConfigInvalid; got %v", err)
		})
	}
}

func TestLoadConfigBytes_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.LoadConfigBytes(ctx, []byte(validConfigTOML))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestLoadConfig_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.LoadConfig(t.Context(), "relative/config.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestLoadConfig_RejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := state.LoadConfig(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestLoadConfig_MissingFileReturnsNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.toml")

	_, err := state.LoadConfig(t.Context(), path)
	require.Error(t, err)
	// Missing file must NOT wrap ErrConfigInvalid — callers
	// distinguish "no config" (fall back to defaults) from "bad
	// config" (fail loud) via errors.Is.
	assert.True(t, errors.Is(err, os.ErrNotExist), "want wrapped os.ErrNotExist; got %v", err)
	assert.False(t, errors.Is(err, types.ErrConfigInvalid),
		"missing file must not wrap ErrConfigInvalid; got %v", err)
}

// TestLoadConfig_HonorsCanceledContext covers the ctx.Err early
// return in the LoadConfig wrapper (separate from the wrapper's
// delegation to LoadConfigBytes which has its own ctx.Err check).
// A pre-canceled context with any valid absolute path must surface
// context.Canceled before any file I/O is attempted.
func TestLoadConfig_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(validConfigTOML), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.LoadConfig(ctx, path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// TestLoadConfig_DirectoryPathReturnsReadError covers the os.ReadFile
// failure wrap by passing a directory as the path argument. ReadFile
// on a directory returns a *PathError wrapping EISDIR on Linux (and
// macOS); the test exercises the "reading %q" wrap without needing
// chmod 000 or read-only mount fixtures that would be fragile in CI.
// The error must NOT wrap os.ErrNotExist (the directory exists) and
// must NOT wrap types.ErrConfigInvalid (the read never reached
// parsing). Both invariants matter: callers distinguish "no config"
// (fall back to defaults) from "bad config" (fail loud) from "I/O
// problem" (route as a permission / disk hint).
func TestLoadConfig_DirectoryPathReturnsReadError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// dir itself is a directory; ReadFile on it must fail.
	_, err := state.LoadConfig(t.Context(), dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading")
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"directory-as-file error must not wrap os.ErrNotExist; got %v", err)
	assert.False(t, errors.Is(err, types.ErrConfigInvalid),
		"directory-as-file error must not wrap ErrConfigInvalid; got %v", err)
}

func TestLoadConfig_ValidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(validConfigTOML), 0o600))

	settings, err := state.LoadConfig(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, settings)
	assert.Equal(t, 1, settings.SchemaVersion)
	assert.Equal(t, "stable", settings.CatalogChannel)
}

func TestLoadConfig_InvalidFileWrapsErrConfigInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = 99
base_stack_path = "~/docker"
timezone = ""
default_docker_network = "wdm_default"
catalog_channel = "stable"
update_check_preference = "daily-on-launch"
`), 0o600))

	_, err := state.LoadConfig(t.Context(), path)
	require.Error(t, err)
	// engine.New fail with ErrConfigInvalid. engine.New delegates
	// to LoadConfig here.
	assert.True(t, errors.Is(err, types.ErrConfigInvalid))
}

// TestLoadConfig_ExampleFileValidates covers exit criterion
// at line 390: "config/config.example.toml validates against
// config/schema.json". The example file is the canonical reference a
// new user copies — if it ever drifts from the schema, this test
// fails loudly at CI time.
func TestLoadConfig_ExampleFileValidates(t *testing.T) {
	t.Parallel()

	// The test runs from the package directory (internal/state); the
	// example file lives at <repo>/config/config.example.toml.
	wd, err := os.Getwd()
	require.NoError(t, err)
	examplePath := filepath.Join(wd, "..", "..", "config", "config.example.toml")
	abs, err := filepath.Abs(examplePath)
	require.NoError(t, err)
	require.FileExists(t, abs, "config/config.example.toml must exist")

	settings, err := state.LoadConfig(t.Context(), abs)
	require.NoError(t, err)
	require.NotNil(t, settings)
	assert.Equal(t, 1, settings.SchemaVersion)
}
