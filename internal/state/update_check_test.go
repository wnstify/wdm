//go:build unix

package state_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

func TestLoadUpdateCheckState_AbsentFileReturnsZeroState(t *testing.T) {
	stateDir := t.TempDir()

	st, err := state.LoadUpdateCheckState(stateDir)
	require.NoError(t, err, "an absent state file is the normal first-launch condition, not an error")
	require.NotNil(t, st)
	assert.True(t, st.LastCheck.IsZero(), "absent file must yield a never-checked (zero) LastCheck")
	assert.Equal(t, 0, st.SchemaVersion, "absent file yields the zero-value struct")
}

func TestLoadUpdateCheckState_MalformedFileReturnsTypedError(t *testing.T) {
	stateDir := t.TempDir()
	path := state.UpdateCheckPath(stateDir)
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), security.SecretFileMode))

	st, err := state.LoadUpdateCheckState(stateDir)
	require.Error(t, err, "a present-but-malformed state file must surface as an error")
	assert.Nil(t, st)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric), "a corrupt state file maps to the generic error code")
}

func TestSaveLoadUpdateCheckState_RoundTrip(t *testing.T) {
	stateDir := secureTempDir(t)
	want := time.Date(2026, time.June, 13, 8, 30, 0, 0, time.UTC)

	require.NoError(t, state.SaveUpdateCheckState(stateDir, &state.UpdateCheckState{LastCheck: want}))

	got, err := state.LoadUpdateCheckState(stateDir)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.LastCheck.Equal(want), "round-tripped LastCheck must equal the saved instant")
	assert.Equal(t, 1, got.SchemaVersion, "Save forces schema_version to 1 for forward-compat")
}

func TestSaveUpdateCheckState_CreatesFileWith0600Mode(t *testing.T) {
	stateDir := secureTempDir(t)

	require.NoError(t, state.SaveUpdateCheckState(stateDir, &state.UpdateCheckState{LastCheck: time.Now()}))

	info, err := os.Stat(state.UpdateCheckPath(stateDir))
	require.NoError(t, err)
	assert.Equal(t, security.SecretFileMode, info.Mode().Perm(), "the state file must be created at exactly 0o600")
}

func TestSaveUpdateCheckState_NilStateRejected(t *testing.T) {
	stateDir := t.TempDir()

	err := state.SaveUpdateCheckState(stateDir, nil)
	require.Error(t, err, "a nil state must be rejected rather than written")
}

func TestLoadAndSaveUpdateCheckState_RejectRelativeStateDir(t *testing.T) {
	_, loadErr := state.LoadUpdateCheckState("relative/state")
	require.Error(t, loadErr, "Load must reject a relative state dir")

	saveErr := state.SaveUpdateCheckState("relative/state", &state.UpdateCheckState{})
	require.Error(t, saveErr, "Save must reject a relative state dir")
}

func TestDueForDailyCheck(t *testing.T) {
	// now is a fixed local-time anchor; the cases derive their lastCheck
	// from it so the local-day boundary is exercised deterministically
	// regardless of the host time zone.
	now := time.Date(2026, time.June, 13, 0, 30, 0, 0, time.Local)

	cases := []struct {
		name      string
		lastCheck time.Time
		expected  bool
	}{
		{
			name:      "never checked is due",
			lastCheck: time.Time{},
			expected:  true,
		},
		{
			name:      "earlier same local day is not due",
			lastCheck: time.Date(2026, time.June, 13, 0, 0, 0, 0, time.Local),
			expected:  false,
		},
		{
			name:      "later same local day is not due",
			lastCheck: time.Date(2026, time.June, 13, 23, 59, 59, 0, time.Local),
			expected:  false,
		},
		{
			name: "previous local day at 23:30 vs now 00:30 next day is due",
			// Crosses local midnight: same instant difference is one hour,
			// but the calendar day differs, so the gate must reopen.
			lastCheck: time.Date(2026, time.June, 12, 23, 30, 0, 0, time.Local),
			expected:  true,
		},
		{
			name:      "many days earlier is due",
			lastCheck: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.Local),
			expected:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, state.DueForDailyCheck(tc.lastCheck, now))
		})
	}
}

func TestDueForDailyCheck_UsesLocalDayNotUTC(t *testing.T) {
	// sameLocalDay converts both instants with.In(time.Local) — it keys off
	// the PROCESS local zone, not the inputs' own zones. Pinning input
	// offsets alone proves nothing, so override time.Local for the duration
	// of this test (restored on cleanup). A global zone swap is unsafe under
	// parallelism, so this test must NOT call t.Parallel.
	orig := time.Local
	time.Local = time.FixedZone("TEST+14", 14*60*60)
	t.Cleanup(func() { time.Local = orig })

	// Case A: SAME local (UTC+14) day but DIFFERENT UTC days -> NOT due.
	//   2026-06-13 01:00 +14 == 2026-06-12 11:00Z
	//   2026-06-13 23:00 +14 == 2026-06-13 09:00Z
	// A UTC-day comparison would call this "due"; the local-day rule must not.
	lastA := time.Date(2026, time.June, 13, 1, 0, 0, 0, time.Local)
	nowA := time.Date(2026, time.June, 13, 23, 0, 0, 0, time.Local)
	if state.DueForDailyCheck(lastA, nowA) {
		t.Errorf("same local day spanning a UTC midnight must not be due")
	}

	// Case B: DIFFERENT local (UTC+14) days but SAME UTC day -> due.
	//   2026-06-13 02:00Z == 2026-06-13 16:00 +14 (local 6-13)
	//   2026-06-13 11:00Z == 2026-06-14 01:00 +14 (local 6-14)
	// A UTC-day comparison would call this "not due"; the local-day rule must.
	lastB := time.Date(2026, time.June, 13, 2, 0, 0, 0, time.UTC)
	nowB := time.Date(2026, time.June, 13, 11, 0, 0, 0, time.UTC)
	if !state.DueForDailyCheck(lastB, nowB) {
		t.Errorf("different local days within one UTC day must be due")
	}
}

func TestSaveUpdateCheckState_OverwritesPrevious(t *testing.T) {
	stateDir := secureTempDir(t)
	first := time.Date(2026, time.June, 12, 9, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.June, 13, 9, 0, 0, 0, time.UTC)

	require.NoError(t, state.SaveUpdateCheckState(stateDir, &state.UpdateCheckState{LastCheck: first}))
	require.NoError(t, state.SaveUpdateCheckState(stateDir, &state.UpdateCheckState{LastCheck: second}))

	got, err := state.LoadUpdateCheckState(stateDir)
	require.NoError(t, err)
	assert.True(t, got.LastCheck.Equal(second), "the second save must overwrite the first")
}

func TestUpdateCheckPath_JoinsFilename(t *testing.T) {
	got := state.UpdateCheckPath("/var/lib/wdm/state")
	assert.Equal(t, filepath.Join("/var/lib/wdm/state", state.UpdateCheckFilename), got)
}
