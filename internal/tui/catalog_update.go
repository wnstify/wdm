package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// catalogNetworkNotice is shown before the read-only check contacts the
// catalog endpoint, so the network action is understood beforehand (PRD §22,
// the invariant — network actions are explicit).
const catalogNetworkNotice = "This will contact the catalog server over the network."

// catalogApplyNetworkNotice is shown on the review screen before the
// user authorizes the apply, so the download is understood before it
// runs. The apply re-confirms through the catalog_update modal first.
const catalogApplyNetworkNotice = "Applying downloads the verified catalog over the network."

// catalogUpdateCheckedMsg carries the read-only Engine.CheckCatalogUpdate
// result back to the model. The check takes no Confirmer or ProgressFn,
// mirroring the Status flow.
type catalogUpdateCheckedMsg struct {
	status *types.CatalogUpdateStatus
	err    error
}

// catalogUpdateFinishedMsg carries the Engine.ApplyCatalogUpdate result
// back to the model after the catalog_update confirmation and the
// streamed apply complete.
type catalogUpdateFinishedMsg struct {
	result *types.CatalogUpdateResult
	err    error
}

// checkCatalogUpdateCmd issues the read-only catalog-update check. It
// never touches deployed apps and runs without a Confirmer
// or ProgressFn, so it uses a plain command rather than engineCommand.
func (m model) checkCatalogUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.eng.CheckCatalogUpdate(m.ctx, types.CatalogUpdateQuery{})
		return catalogUpdateCheckedMsg{status: status, err: err}
	}
}

// applyCatalogUpdateCmd downloads, verifies, and applies the catalog
// update surfaced by the preceding check. It runs through engineCommand
// so the catalog_update confirmation is raised before the download and so
// progress streams to the view. The request mirrors the checked status;
// the engine re-verifies the artifact before any write.
func (m model) applyCatalogUpdateCmd() tea.Cmd {
	req := types.CatalogUpdateRequest{}
	if m.catalogUpdateStatus != nil {
		req.Channel = m.catalogUpdateStatus.Channel
		req.TargetVersion = m.catalogUpdateStatus.LatestVersion
	}

	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.ApplyCatalogUpdate(ctx, req, progress, confirmer)
		return catalogUpdateFinishedMsg{result: result, err: err}
	})
}

// selectCatalogUpdate authorizes the apply when an update is available
// and verified. An unverified latest is fail-closed: Enter does nothing
// and the view keeps the warning visible. Selecting with no available
// update is also a no-op.
func (m model) selectCatalogUpdate() (tea.Model, tea.Cmd) {
	if !m.catalogUpdateApplyable() {
		return m, nil
	}

	m.busy = true
	m.err = nil
	m.catalogUpdateResult = nil
	m.progress = progressMsg{}
	return m, m.applyCatalogUpdateCmd()
}

// catalogUpdateApplyable reports whether the checked status permits an
// apply: an available update whose latest metadata passed verification
// (PRD §22, §23).
func (m model) catalogUpdateApplyable() bool {
	return m.catalogUpdateStatus != nil &&
		m.catalogUpdateStatus.UpdateAvailable &&
		m.catalogUpdateStatus.Verified
}

// catalogUpdateScreenView dispatches the two catalog-update screens so
// the central screenView router carries a single case for the flow.
func (m model) catalogUpdateScreenView() string {
	if m.screen == screenCatalogUpdateResult {
		return m.catalogUpdateResultView()
	}
	return m.catalogUpdateView()
}

func (m model) catalogUpdateView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Update catalog"))
	b.WriteString("\n\n")

	switch {
	case m.busy && m.catalogUpdateStatus == nil:
		b.WriteString(catalogNetworkNotice)
		b.WriteString("\n\n")
		b.WriteString("Checking the catalog server...\n")
	case m.busy:
		b.WriteString(catalogApplyNetworkNotice)
		b.WriteString("\n\n")
		b.WriteString("Downloading and applying the catalog update...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		// A failed apply (declined confirmation, or a future network or
		// verification failure) leaves catalogUpdateStatus populated by the
		// preceding check, so the label distinguishes the apply stage from a
		// check that failed before any status was reported.
		if m.catalogUpdateStatus != nil {
			b.WriteString("Could not apply the catalog update: ")
		} else {
			b.WriteString("Could not check for catalog updates: ")
		}
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	default:
		m.writeCatalogUpdateStatus(&b)
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) writeCatalogUpdateStatus(b *strings.Builder) {
	status := m.catalogUpdateStatus
	if status == nil {
		b.WriteString("No catalog status available.\n")
		return
	}

	b.WriteString("Channel: ")
	b.WriteString(catalogUpdateField(status.Channel))
	b.WriteByte('\n')
	b.WriteString("Current (local): ")
	b.WriteString(catalogUpdateField(status.CurrentVersion))
	b.WriteByte('\n')
	b.WriteString("Latest available: ")
	b.WriteString(catalogUpdateField(status.LatestVersion))
	b.WriteByte('\n')

	if status.Verified {
		b.WriteString("Verification: latest catalog passed checksum, signature, and attestation.\n")
	} else {
		b.WriteString("Verification: WARNING the latest catalog is NOT verified. Apply is blocked until it verifies.\n")
	}

	if !status.UpdateAvailable {
		b.WriteString("\nThe catalog is up to date.\n")
		return
	}

	b.WriteString("\nChanges\n")
	if len(status.Changes) == 0 {
		b.WriteString("No per-app changes were reported.\n")
	} else {
		for _, change := range status.Changes {
			writeCatalogChange(b, change)
		}
	}

	b.WriteString("\n")
	if m.catalogUpdateApplyable() {
		b.WriteString(catalogApplyNetworkNotice)
		b.WriteString("\n")
		b.WriteString("Press Enter to download and apply the catalog update. You will confirm before it downloads.\n")
	} else {
		b.WriteString("This update cannot be applied until the latest catalog verifies.\n")
	}
}

func (m model) catalogUpdateResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Catalog updated"))
	b.WriteString("\n\n")

	result := m.catalogUpdateResult
	if result == nil {
		b.WriteString("No catalog update result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	b.WriteString("Channel: ")
	b.WriteString(catalogUpdateField(result.Channel))
	b.WriteByte('\n')
	b.WriteString("Version: ")
	b.WriteString(catalogUpdateField(result.PreviousVersion))
	b.WriteString(" -> ")
	b.WriteString(catalogUpdateField(result.AppliedVersion))
	b.WriteByte('\n')
	if result.VerificationDetail != "" {
		b.WriteString("Verification: ")
		b.WriteString(result.VerificationDetail)
		b.WriteByte('\n')
	}

	if len(result.Changes) > 0 {
		b.WriteString("\nChanges\n")
		for _, change := range result.Changes {
			writeCatalogChange(&b, change)
		}
	}

	b.WriteString("\nApplied: ")
	b.WriteString(formatBackupTime(result.AppliedAt))
	b.WriteByte('\n')

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func writeCatalogChange(b *strings.Builder, change types.CatalogChange) {
	b.WriteString("- ")
	b.WriteString(change.AppID)
	if change.Kind != "" {
		b.WriteString(" (")
		b.WriteString(change.Kind)
		b.WriteByte(')')
	}
	if change.Summary != "" {
		b.WriteString(": ")
		b.WriteString(change.Summary)
	}
	b.WriteByte('\n')
}

func catalogUpdateField(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
