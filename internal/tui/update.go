package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// updateNetworkNotice is shown while an app update runs, so the network action
// is understood beforehand (PRD §20, the invariant — network actions are
// explicit). Update planning resolves catalog-pinned image tags against the
// image registry to disclose the digests they point at; the
// registry never changes which image is applied — the catalog stays the sole
// update source — so the notice names the contact without implying the
// registry drives the result.
const updateNetworkNotice = "Updating contacts the image registry over the network to check image digests. The catalog remains the source of the update."

type updateFinishedMsg struct {
	result *types.UpdateResult
	err    error
}

func (m model) selectUpdateApp() (tea.Model, tea.Cmd) {
	if len(m.apps) == 0 {
		return m, nil
	}

	m.busy = true
	m.err = nil
	m.updateResult = nil
	m.progress = progressMsg{}
	return m, m.updateAppCmd(m.apps[m.appCursor].AppID)
}

func (m model) updateAppCmd(appID string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Update(ctx, types.UpdateRequest{AppID: appID}, progress, confirmer)
		return updateFinishedMsg{result: result, err: err}
	})
}

func (m model) updateAppsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Update apps"))
	b.WriteString("\n\n")

	switch {
	case m.busy && len(m.apps) == 0:
		b.WriteString("Loading managed apps...\n")
	case m.busy:
		b.WriteString(updateNetworkNotice)
		b.WriteString("\n\n")
		b.WriteString("Updating ")
		b.WriteString(m.selectedUpdateAppID())
		b.WriteString("...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		b.WriteString("Update failed: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	case len(m.apps) == 0:
		b.WriteString("No managed apps found.\n")
	default:
		for i, app := range m.apps {
			prefix := "  "
			suffix := ""
			if i == m.appCursor {
				prefix = "> "
				suffix = " [selected]"
			}
			b.WriteString(prefix)
			b.WriteString(app.AppID)
			if app.TemplateName != "" && app.TemplateName != app.AppID {
				b.WriteString("  ")
				b.WriteString(app.TemplateName)
			}
			b.WriteString(suffix)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	b.WriteString(helpLine())
	return b.String()
}

func (m model) updateResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Update complete"))
	b.WriteString("\n\n")

	if m.updateResult == nil {
		b.WriteString("No update result returned.\n\n")
		b.WriteString(helpLine())
		return b.String()
	}

	result := m.updateResult
	b.WriteString(result.AppID)
	b.WriteByte('\n')
	if result.PreviousTemplateVersion != "" || result.NewTemplateVersion != "" {
		b.WriteString("Version: ")
		b.WriteString(result.PreviousTemplateVersion)
		b.WriteString(" -> ")
		b.WriteString(result.NewTemplateVersion)
		b.WriteByte('\n')
	}
	for _, service := range result.UpdatedServices {
		b.WriteString("- ")
		b.WriteString(service)
		b.WriteByte('\n')
	}
	if len(result.RiskClassifications) > 0 {
		b.WriteString("Risks: ")
		b.WriteString(strings.Join(result.RiskClassifications, ", "))
		b.WriteByte('\n')
	}
	if result.BackupPath != "" {
		b.WriteString("Backup: ")
		b.WriteString(result.BackupPath)
		b.WriteByte('\n')
	}
	if result.Status != nil {
		b.WriteString("State: ")
		b.WriteString(result.Status.State)
		b.WriteByte('\n')
		if result.Status.Message != "" {
			b.WriteString(result.Status.Message)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	b.WriteString(helpLine())
	return b.String()
}

func (m model) selectedUpdateAppID() string {
	if m.appCursor >= 0 && m.appCursor < len(m.apps) {
		return m.apps[m.appCursor].AppID
	}
	return "selected app"
}
