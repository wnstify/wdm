package core_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/state"
)

// updateCheckConfigTOML is a minimal valid config.toml carrying the given
// update_check_preference. Seeding the config on disk before New is the
// only way to give the engine a non-default preference, because
// UpdateSettings writes config.toml without refreshing the in-memory cache
// (DailyLaunchCheckDue reads the cached preference).
func updateCheckConfigTOML(pref string) string {
	return "schema_version = 1\n" +
		"base_stack_path = \"~/docker\"\n" +
		"timezone = \"\"\n" +
		"default_docker_network = \"wdm_default\"\n" +
		"catalog_channel = \"stable\"\n" +
		"update_check_preference = \"" + pref + "\"\n"
}

// newTestEngineWithPreference builds an engine whose loaded
// update_check_preference is pref, by seeding a config.toml the engine reads
// at construction. It returns the engine and its resolved state dir.
func newTestEngineWithPreference(t *testing.T, pref string) (*core.Engine, string) {
	t.Helper()

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	configPath := filepath.Join(tmp, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(updateCheckConfigTOML(pref)), 0o600))

	eng, err := core.New(
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(configPath),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	return eng, stateDir
}

func TestDailyLaunchCheckDue_ManualAndDisabledNeverDue(t *testing.T) {
	for _, pref := range []string{"manual", "disabled"} {
		t.Run(pref, func(t *testing.T) {
			eng, _ := newTestEngineWithPreference(t, pref)

			// Pin a fixed clock; the preference must short-circuit before
			// the clock or any state read matters.
			core.SwapCoreUpdateCheckNowForTest(t, func() time.Time {
				return time.Date(2026, time.June, 13, 10, 0, 0, 0, time.Local)
			})

			due, err := eng.DailyLaunchCheckDue(t.Context())
			require.NoError(t, err)
			assert.False(t, due, "%s preference must never be due for an automatic launch check", pref)
		})
	}
}

func TestDailyLaunchCheckDue_DailyLifecycle(t *testing.T) {
	eng, _ := newTestEngineWithPreference(t, "daily-on-launch")

	day1 := time.Date(2026, time.June, 13, 10, 0, 0, 0, time.Local)
	day1Later := time.Date(2026, time.June, 13, 22, 0, 0, 0, time.Local)
	day2 := time.Date(2026, time.June, 14, 1, 0, 0, 0, time.Local)

	clock := day1
	core.SwapCoreUpdateCheckNowForTest(t, func() time.Time { return clock })

	// Never checked → due.
	due, err := eng.DailyLaunchCheckDue(t.Context())
	require.NoError(t, err)
	assert.True(t, due, "a never-checked daily-on-launch engine must be due")

	// Record a successful check on day 1.
	require.NoError(t, eng.RecordDailyLaunchCheck(t.Context()))

	// Same local day → not due.
	clock = day1Later
	due, err = eng.DailyLaunchCheckDue(t.Context())
	require.NoError(t, err)
	assert.False(t, due, "after a same-day record the gate must be closed")

	// Next local day → due again.
	clock = day2
	due, err = eng.DailyLaunchCheckDue(t.Context())
	require.NoError(t, err)
	assert.True(t, due, "advancing to the next local day must reopen the gate")
}

func TestRecordDailyLaunchCheck_PersistsAcrossReload(t *testing.T) {
	eng, stateDir := newTestEngineWithPreference(t, "daily-on-launch")

	pinned := time.Date(2026, time.June, 13, 10, 0, 0, 0, time.Local)
	core.SwapCoreUpdateCheckNowForTest(t, func() time.Time { return pinned })

	require.NoError(t, eng.RecordDailyLaunchCheck(t.Context()))

	// A fresh load of the state file must observe the recorded instant.
	st, err := state.LoadUpdateCheckState(stateDir)
	require.NoError(t, err)
	assert.True(t, st.LastCheck.Equal(pinned), "RecordDailyLaunchCheck must persist the pinned instant")
}

func TestDailyLaunchCheckDue_ClosedEngine(t *testing.T) {
	eng, _ := newTestEngineWithPreference(t, "daily-on-launch")
	require.NoError(t, eng.Close())

	_, err := eng.DailyLaunchCheckDue(t.Context())
	assert.ErrorIs(t, err, core.ErrClosed)

	assert.ErrorIs(t, eng.RecordDailyLaunchCheck(t.Context()), core.ErrClosed)
}
