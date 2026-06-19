package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// selfUpdateNetworkNotice is shown before the read-only check contacts the
// release endpoint, so the network action is understood beforehand (PRD §14,
// the invariant — network actions are explicit).
const selfUpdateNetworkNotice = "This will contact the release server over the network."

// selfUpdateApplyNetworkNotice is shown on the review screen before the
// user authorizes the apply, so the download is understood before it runs.
// The apply re-confirms through the self_update modal first.
const selfUpdateApplyNetworkNotice = "Applying downloads and verifies the new binary over the network before replacing wdm."

// selfUpdateCheckedMsg carries the read-only Engine.CheckSelfUpdate result
// back to the model. The check takes no Confirmer or ProgressFn, mirroring
// the Status flow.
type selfUpdateCheckedMsg struct {
	status *types.SelfUpdateStatus
	err    error
}

// selfUpdateFinishedMsg carries the Engine.ApplySelfUpdate result back to the
// model after the self_update confirmation and the streamed apply complete.
// The result may be non-nil ALONGSIDE a non-nil error on the rollback paths
// (PRD §14) so the model can surface the rollback outcome rather
// than only a bare error.
type selfUpdateFinishedMsg struct {
	result *types.SelfUpdateResult
	err    error
}

// checkSelfUpdateCmd issues the read-only self-update check. It never
// touches deployed apps and runs without a Confirmer or ProgressFn, so it
// uses a plain command rather than engineCommand.
func (m model) checkSelfUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.eng.CheckSelfUpdate(m.ctx, types.SelfUpdateQuery{})
		return selfUpdateCheckedMsg{status: status, err: err}
	}
}

// applySelfUpdateCmd downloads, verifies, and applies the binary update
// surfaced by the preceding check. It runs through engineCommand so the
// self_update confirmation is raised before the download and so progress
// streams to the view. The request mirrors the checked status; the engine
// re-verifies the artifact before any replace and keeps the prior binary
// for rollback.
func (m model) applySelfUpdateCmd() tea.Cmd {
	req := types.SelfUpdateRequest{}
	if m.selfUpdateStatus != nil {
		req.TargetVersion = m.selfUpdateStatus.LatestVersion
	}

	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.ApplySelfUpdate(ctx, req, progress, confirmer)
		return selfUpdateFinishedMsg{result: result, err: err}
	})
}

// selectSelfUpdate authorizes the apply when an update is available and
// verified. An unverified latest is fail-closed: Enter does nothing and
// the view keeps the warning visible. Selecting with no available update
// is also a no-op.
func (m model) selectSelfUpdate() (tea.Model, tea.Cmd) {
	if !m.selfUpdateApplyable() {
		return m, nil
	}

	m.busy = true
	m.err = nil
	m.selfUpdateResult = nil
	m.progress = progressMsg{}
	return m, m.applySelfUpdateCmd()
}

// selfUpdateApplyable reports whether the checked status permits an apply:
// an available update whose latest release passed verification (PRD §14,
// §22, §23, the invariant).
func (m model) selfUpdateApplyable() bool {
	return m.selfUpdateStatus != nil &&
		m.selfUpdateStatus.UpdateAvailable &&
		m.selfUpdateStatus.Verified
}

// selfUpdateScreenView dispatches the two self-update screens so the
// central screenView router carries a single case for the flow.
func (m model) selfUpdateScreenView() string {
	if m.screen == screenSelfUpdateResult {
		return m.selfUpdateResultView()
	}
	return m.selfUpdateView()
}

func (m model) selfUpdateView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Update wdm"))
	b.WriteString("\n\n")

	switch {
	case m.busy && m.selfUpdateStatus == nil:
		b.WriteString(selfUpdateNetworkNotice)
		b.WriteString("\n\n")
		b.WriteString("Checking the release server...\n")
	case m.busy:
		b.WriteString(selfUpdateApplyNetworkNotice)
		b.WriteString("\n\n")
		b.WriteString("Downloading, verifying, and replacing the wdm binary...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		// A failed apply (declined confirmation, a network or verification
		// failure, or a non-writable executable path) leaves
		// selfUpdateStatus populated by the preceding check, so the label
		// distinguishes the apply stage from a check that failed before any
		// status was reported.
		if m.selfUpdateStatus != nil {
			b.WriteString("Could not update wdm: ")
		} else {
			b.WriteString("Could not check for a wdm update: ")
		}
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	default:
		m.writeSelfUpdateStatus(&b)
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) writeSelfUpdateStatus(b *strings.Builder) {
	status := m.selfUpdateStatus
	if status == nil {
		b.WriteString("No release status available.\n")
		return
	}

	b.WriteString("Current (this binary): ")
	b.WriteString(selfUpdateField(status.CurrentVersion))
	b.WriteByte('\n')
	b.WriteString("Latest release: ")
	b.WriteString(selfUpdateField(status.LatestVersion))
	b.WriteByte('\n')

	if status.Verified {
		b.WriteString("Verification: latest release passed checksum, signature, and attestation.\n")
	} else {
		b.WriteString("Verification: WARNING the latest release is NOT verified. Apply is blocked until it verifies.\n")
	}

	for _, note := range status.Notes {
		b.WriteString("Note: ")
		b.WriteString(note)
		b.WriteByte('\n')
	}

	if !status.UpdateAvailable {
		b.WriteString("\nwdm is up to date.\n")
		return
	}

	b.WriteString("\n")
	if m.selfUpdateApplyable() {
		b.WriteString(selfUpdateApplyNetworkNotice)
		b.WriteString("\n")
		b.WriteString("Applying keeps the current binary as wdm.previous and rolls back automatically if the new binary fails its smoke check.\n")
		b.WriteString("Press Enter to download and apply the update. You will confirm before it downloads.\n")
	} else {
		b.WriteString("This update cannot be applied until the latest release verifies.\n")
	}
}

func (m model) selfUpdateResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("wdm update"))
	b.WriteString("\n\n")

	result := m.selfUpdateResult
	if result == nil {
		b.WriteString("No self-update result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	// The apply path returns a result ALONGSIDE an error on the rollback paths
	// Make the failure evident so a rolled-back or
	// version-mismatched apply is never read as success, while still showing
	// the structured outcome below.
	if m.err != nil {
		b.WriteString("Self-update did not complete: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}

	b.WriteString("Version: ")
	b.WriteString(selfUpdateField(result.PreviousVersion))
	b.WriteString(" -> ")
	b.WriteString(selfUpdateField(result.AppliedVersion))
	b.WriteByte('\n')
	b.WriteString("Replaced: ")
	b.WriteString(yesNo(result.Replaced))
	b.WriteByte('\n')
	b.WriteString("Smoke check: ")
	b.WriteString(okFailed(result.SmokeOK))
	b.WriteByte('\n')
	b.WriteString("Rolled back: ")
	b.WriteString(yesNo(result.RolledBack))
	b.WriteByte('\n')
	if result.PreviousBinaryPath != "" {
		b.WriteString("Previous binary: ")
		b.WriteString(result.PreviousBinaryPath)
		b.WriteByte('\n')
	}
	if result.Message != "" {
		b.WriteString("\n")
		b.WriteString(result.Message)
		b.WriteByte('\n')
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func selfUpdateField(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func okFailed(v bool) string {
	if v {
		return "ok"
	}
	return "failed"
}
