package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// launchCheckTimeout bounds the daily-on-launch update check. The
// catalog and self-update checks run sequentially under this single deadline,
// so a slow or unreachable network never stalls the session: when the deadline
// elapses the check fails silently and retries on the next launch
// (RecordDailyLaunchCheck records only on success). Verify-in-check on a slow
// link may exceed the budget — accepted pre-v1 behavior.
const launchCheckTimeout = 5 * time.Second

// launchCheckNotice is shown while the launch check contacts the network, so
// the network action is understood before results appear (the invariant —
// network actions are explicit). It mirrors catalogNetworkNotice and
// selfUpdateNetworkNotice.
const launchCheckNotice = "Checking for updates… contacting GitHub."

// dailyLaunchCheckDueMsg carries the read-only Engine.DailyLaunchCheckDue
// answer back to the model. The gate is a fast local-disk read with no
// network work, so Init can issue it concurrently without blocking the first
// render.
type dailyLaunchCheckDueMsg struct {
	due bool
	err error
}

// dailyLaunchCheckFinishedMsg carries the read-only catalog and self-update
// check results back to the model after the bounded launch check completes.
// Each surface reports its own status and error so the model can record only
// when both succeeded and surface an available update without nagging
// when there is none.
type dailyLaunchCheckFinishedMsg struct {
	catalog    *types.CatalogUpdateStatus
	catalogErr error
	self       *types.SelfUpdateStatus
	selfErr    error
}

// dailyLaunchCheckRecordedMsg carries the Engine.RecordDailyLaunchCheck
// outcome back to the model. The result is informational only: a record
// failure never surfaces to the user (the check simply retries next launch).
type dailyLaunchCheckRecordedMsg struct {
	err error
}

// dailyLaunchCheckDueCmd issues the read-only launch-check gate. It runs no
// network work and acquires no lock, so it is safe to batch into Init
// alongside the first render.
func (m model) dailyLaunchCheckDueCmd() tea.Cmd {
	return func() tea.Msg {
		due, err := m.eng.DailyLaunchCheckDue(m.ctx)
		return dailyLaunchCheckDueMsg{due: due, err: err}
	}
}

// runDailyLaunchCheckCmd runs the bounded daily launch check: it contacts the
// catalog endpoint then the release endpoint under a single launchCheckTimeout
// deadline (the invariant — read-only Check* surfaces, no Confirmer or
// ProgressFn). Both statuses and errors are returned so the model can apply
// the record-on-success rule (record only when both errors are nil).
func (m model) runDailyLaunchCheckCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, launchCheckTimeout)
		defer cancel()

		catalog, catalogErr := m.eng.CheckCatalogUpdate(ctx, types.CatalogUpdateQuery{})
		self, selfErr := m.eng.CheckSelfUpdate(ctx, types.SelfUpdateQuery{})
		return dailyLaunchCheckFinishedMsg{
			catalog:    catalog,
			catalogErr: catalogErr,
			self:       self,
			selfErr:    selfErr,
		}
	}
}

// recordDailyLaunchCheckCmd records a successful launch check so the gate
// closes for the rest of the local calendar day. The caller invokes this only
// after both checks succeeded.
func (m model) recordDailyLaunchCheckCmd() tea.Cmd {
	return func() tea.Msg {
		return dailyLaunchCheckRecordedMsg{err: m.eng.RecordDailyLaunchCheck(m.ctx)}
	}
}

// launchCheckSummary builds the transient banner for a completed launch
// check. It includes a line only for a surface that reports an available
// update; if neither does, it returns "" so the banner clears rather than
// nagging "up to date". The wording mirrors the catalog and self-update views
// (current → latest).
func launchCheckSummary(catalog *types.CatalogUpdateStatus, self *types.SelfUpdateStatus) string {
	var lines []string
	if catalog != nil && catalog.UpdateAvailable {
		lines = append(lines, "Catalog update available: "+
			catalogUpdateField(catalog.CurrentVersion)+" → "+catalogUpdateField(catalog.LatestVersion))
	}
	if self != nil && self.UpdateAvailable {
		lines = append(lines, "wdm update available: "+
			selfUpdateField(self.CurrentVersion)+" → "+selfUpdateField(self.LatestVersion))
	}
	return strings.Join(lines, "\n")
}
