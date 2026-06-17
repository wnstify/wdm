package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestLaunchCheck_InitIssuesDueCmdWithoutBlockingFirstRender(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{dailyLaunchCheckDue: true}
	m := newReadyModel(fake)

	// The first render is the dashboard: Init must not block on the check.
	assert.Contains(t, m.View(), "What do you want to do?")
	assert.NotContains(t, m.View(), launchCheckNotice)

	cmd := m.Init()
	require.NotNil(t, cmd)
	msgs := collectCmdMsgs(cmd)

	var sawDue bool
	for _, msg := range msgs {
		if _, ok := msg.(dailyLaunchCheckDueMsg); ok {
			sawDue = true
		}
	}
	assert.True(t, sawDue, "Init batch must include the daily-launch-check due cmd")
	assert.Equal(t, 1, fake.dailyLaunchCheckDueN, "due cmd must call DailyLaunchCheckDue exactly once")
}

func TestLaunchCheck_DueTrueAnnouncesAndRunsCheck(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	m := newReadyModel(fake)

	next, cmd := m.Update(dailyLaunchCheckDueMsg{due: true})
	m = assertModel(t, next)
	require.NotNil(t, cmd, "a due check must issue the run cmd")

	assert.True(t, m.launchCheckActive)
	assert.Contains(t, m.View(), launchCheckNotice)

	// Draining the run cmd must contact both read-only check surfaces.
	for _, msg := range collectCmdMsgs(cmd) {
		_ = msg
	}
	assert.Equal(t, 1, fake.checkCatalogUpdateN)
	assert.Equal(t, 1, fake.checkSelfUpdateN)
}

func TestLaunchCheck_FinishedWithUpdatesShowsBannerAndRecords(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		catalog *types.CatalogUpdateStatus
		self    *types.SelfUpdateStatus
		want    []string
		absent  []string
	}{
		{
			name: "catalog only",
			catalog: &types.CatalogUpdateStatus{
				UpdateAvailable: true,
				CurrentVersion:  "1.0.0",
				LatestVersion:   "1.1.0",
			},
			self:   &types.SelfUpdateStatus{UpdateAvailable: false},
			want:   []string{"Catalog update available", "1.0.0", "1.1.0"},
			absent: []string{"wdm update available"},
		},
		{
			name:    "self only",
			catalog: &types.CatalogUpdateStatus{UpdateAvailable: false},
			self: &types.SelfUpdateStatus{
				UpdateAvailable: true,
				CurrentVersion:  "0.9.0",
				LatestVersion:   "1.0.0",
			},
			want:   []string{"wdm update available", "0.9.0", "1.0.0"},
			absent: []string{"Catalog update available"},
		},
		{
			name: "both",
			catalog: &types.CatalogUpdateStatus{
				UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "1.1.0",
			},
			self: &types.SelfUpdateStatus{
				UpdateAvailable: true, CurrentVersion: "0.9.0", LatestVersion: "1.0.0",
			},
			want: []string{"Catalog update available", "wdm update available"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{}
			m := newReadyModel(fake)
			m.launchCheckActive = true

			next, cmd := m.Update(dailyLaunchCheckFinishedMsg{catalog: tt.catalog, self: tt.self})
			m = assertModel(t, next)
			require.NotNil(t, cmd, "a successful check must issue the record cmd")

			view := m.View()
			for _, want := range tt.want {
				assert.Contains(t, view, want)
			}
			for _, absent := range tt.absent {
				assert.NotContains(t, view, absent)
			}

			// Running the record cmd records the successful check exactly once.
			for _, msg := range collectCmdMsgs(cmd) {
				next, _ := m.Update(msg)
				m = assertModel(t, next)
			}
			assert.Equal(t, 1, fake.recordDailyLaunchCheckN)
			assert.False(t, m.launchCheckActive, "recording clears the active announce")
		})
	}
}

func TestLaunchCheck_NoUpdatesClearsBannerAndRecords(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	m := newReadyModel(fake)
	m.launchCheckActive = true

	next, cmd := m.Update(dailyLaunchCheckFinishedMsg{
		catalog: &types.CatalogUpdateStatus{UpdateAvailable: false},
		self:    &types.SelfUpdateStatus{UpdateAvailable: false},
	})
	m = assertModel(t, next)
	require.NotNil(t, cmd, "a successful check still records even with no update")

	assert.Empty(t, m.launchCheckBanner, "no update must not nag up-to-date")
	assert.NotContains(t, m.View(), "up to date")

	for _, msg := range collectCmdMsgs(cmd) {
		next, _ := m.Update(msg)
		m = assertModel(t, next)
	}
	assert.Equal(t, 1, fake.recordDailyLaunchCheckN)
}

func TestLaunchCheck_CheckErrorIsSilentAndDoesNotRecord(t *testing.T) {
	t.Parallel()

	checkErr := errors.New("404 no release published")

	for _, tt := range []struct {
		name       string
		catalogErr error
		selfErr    error
	}{
		{name: "catalog error", catalogErr: checkErr},
		{name: "self error", selfErr: checkErr},
		{name: "both errors", catalogErr: checkErr, selfErr: checkErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{}
			m := newReadyModel(fake)
			m.launchCheckActive = true

			next, cmd := m.Update(dailyLaunchCheckFinishedMsg{
				catalog:    &types.CatalogUpdateStatus{UpdateAvailable: true, LatestVersion: "9.9.9"},
				catalogErr: tt.catalogErr,
				self:       &types.SelfUpdateStatus{UpdateAvailable: true, LatestVersion: "9.9.9"},
				selfErr:    tt.selfErr,
			})
			m = assertModel(t, next)

			assert.Nil(t, cmd, "a failed check must not issue the record cmd")
			assert.Equal(t, 0, fake.recordDailyLaunchCheckN, "a failed check must not record")
			assert.NoError(t, m.err, "a failed launch check must never surface an error")
			assert.False(t, m.launchCheckActive)
			assert.Empty(t, m.launchCheckBanner)

			view := m.View()
			assert.NotContains(t, view, checkErr.Error())
			assert.NotContains(t, view, "9.9.9")
		})
	}
}

func TestLaunchCheck_DueFalseDoesNothing(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		msg  dailyLaunchCheckDueMsg
	}{
		{name: "not due (manual/disabled/already checked)", msg: dailyLaunchCheckDueMsg{due: false}},
		{name: "gate error", msg: dailyLaunchCheckDueMsg{err: errors.New("corrupt state")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{}
			m := newReadyModel(fake)

			next, cmd := m.Update(tt.msg)
			m = assertModel(t, next)

			assert.Nil(t, cmd, "a not-due answer must not run the check")
			assert.False(t, m.launchCheckActive)
			assert.NotContains(t, m.View(), launchCheckNotice)
			assert.Equal(t, 0, fake.checkCatalogUpdateN)
			assert.Equal(t, 0, fake.checkSelfUpdateN)
		})
	}
}

func TestLaunchCheck_FirstRenderIsDashboardWhilePending(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{dailyLaunchCheckDue: true}
	m := newReadyModel(fake)

	// Before any check message lands, the dashboard renders normally.
	view := m.View()
	assert.Contains(t, view, "What do you want to do?")
	assert.Contains(t, view, "> Install an app [selected]")
	assert.NotContains(t, view, launchCheckNotice)
}

func TestLaunchCheck_BannerIsTransientOnKeypress(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	m := newReadyModel(fake)

	next, _ := m.Update(dailyLaunchCheckFinishedMsg{
		catalog: &types.CatalogUpdateStatus{UpdateAvailable: true, CurrentVersion: "1.0.0", LatestVersion: "1.1.0"},
		self:    &types.SelfUpdateStatus{UpdateAvailable: false},
	})
	m = assertModel(t, next)
	require.Contains(t, m.View(), "Catalog update available")

	// Any navigation keypress dismisses the transient banner.
	m = updateModel(t, m, downKey())
	assert.Empty(t, m.launchCheckBanner)
	assert.NotContains(t, m.View(), "Catalog update available")
}

func TestLaunchCheck_BannerSurvivesKeypressOffDashboard(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		screen    screen
		wantClear bool
	}{
		{name: "non-dashboard screen keeps banner", screen: screenRuntimeLock, wantClear: false},
		{name: "dashboard screen clears banner", screen: screenDashboard, wantClear: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{}
			m := newReadyModel(fake)
			m.screen = tt.screen
			m.launchCheckBanner = "Catalog update available: 1.0.0 → 1.1.0"

			m = updateModel(t, m, downKey())

			if tt.wantClear {
				assert.Empty(t, m.launchCheckBanner, "a keypress on the dashboard dismisses the banner")
			} else {
				assert.Equal(t, "Catalog update available: 1.0.0 → 1.1.0", m.launchCheckBanner,
					"a keypress off the dashboard must not wipe the banner before the user sees it")
			}
		})
	}
}

func TestLaunchCheck_DueCmdReturnsGateResult(t *testing.T) {
	t.Parallel()

	gateErr := errors.New("gate read failed")
	fake := &fakeEngine{dailyLaunchCheckDue: true, dailyLaunchCheckDueErr: gateErr}
	m := newReadyModel(fake)

	msg := requireMsg[dailyLaunchCheckDueMsg](t, m.dailyLaunchCheckDueCmd())
	assert.True(t, msg.due)
	assert.ErrorIs(t, msg.err, gateErr)
	assert.Equal(t, 1, fake.dailyLaunchCheckDueN)
}

// requireMsg runs cmd and asserts its message is of type T.
func requireMsg[T tea.Msg](t *testing.T, cmd tea.Cmd) T {
	t.Helper()

	require.NotNil(t, cmd)
	msg, ok := cmd().(T)
	require.True(t, ok, "cmd returned unexpected message type")
	return msg
}
