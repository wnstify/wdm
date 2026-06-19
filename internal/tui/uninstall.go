package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// uninstallFinishedMsg carries the Engine.Uninstall result back to the
// model after the uninstall_destructive confirmation and the streamed
// teardown complete. Uninstall is fail-closed: a non-nil result may report
// per-stack failures in result.Failed, in which case the footprint was
// left in place and wdm is still installed.
type uninstallFinishedMsg struct {
	result *types.UninstallResult
	err    error
}

// uninstallCmd tears down every managed app and then removes wdm's own
// footprint. It runs through engineCommand so the uninstall_destructive
// confirmation is raised before any teardown and so progress streams to
// the view. The request is argless: Uninstall always targets the full
// managed set plus wdm's footprint.
func (m model) uninstallCmd() tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Uninstall(ctx, types.UninstallRequest{}, progress, confirmer)
		return uninstallFinishedMsg{result: result, err: err}
	})
}

// uninstallScreenView dispatches the two uninstall screens so the central
// screenView router carries a single case for the flow.
func (m model) uninstallScreenView() string {
	if m.screen == screenUninstallResult {
		return m.uninstallResultView()
	}
	return m.uninstallView()
}

func (m model) uninstallView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Uninstall wdm"))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Uninstalling wdm...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		b.WriteString("Could not uninstall wdm: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	default:
		b.WriteString("Removes wdm and tears down every managed app: containers and images.\n")
		b.WriteString("The wdm-created Docker networks are removed too, so Docker is left clean.\n")
		b.WriteString("Named volumes and per-app stack data are kept; this is not a teardown of your data.\n")
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) uninstallResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Uninstall wdm"))
	b.WriteString("\n\n")

	result := m.uninstallResult
	if result == nil {
		b.WriteString("No uninstall result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if len(result.Failed) > 0 {
		b.WriteString("Uninstall aborted. wdm was not removed.\n")

		b.WriteString("\nFailed\n")
		for _, app := range result.Failed {
			b.WriteString("- ")
			b.WriteString(app.AppID)
			if app.Error != "" {
				b.WriteString(": ")
				b.WriteString(app.Error)
			}
			b.WriteByte('\n')
		}

		if len(result.TornDown) > 0 {
			b.WriteString("\nTorn down before the abort\n")
			for _, app := range result.TornDown {
				b.WriteString("- ")
				b.WriteString(app.AppID)
				b.WriteByte('\n')
			}
		}

		writeUninstallKeptData(&b, result)

		b.WriteString("\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	b.WriteString("wdm was uninstalled. Every managed app was torn down; your data was kept.\n")

	if len(result.TornDown) > 0 {
		b.WriteString("\nTorn down\n")
		for _, app := range result.TornDown {
			b.WriteString("- ")
			b.WriteString(app.AppID)
			b.WriteByte('\n')
		}
	}

	if len(result.RemovedNetworks) > 0 {
		fmt.Fprintf(&b, "\nNetworks removed: %d\n", len(result.RemovedNetworks))
	}

	writeUninstallRetainedNetworks(&b, result)
	writeUninstallKeptData(&b, result)

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeUninstallRetainedNetworks appends a warning naming the wdm-created
// networks that could not be removed and the manual `docker network rm <name>`
// command to finish the cleanup. Network cleanup is best-effort, so a retained
// network never fails the uninstall; this just surfaces the manual follow-up.
func writeUninstallRetainedNetworks(b *strings.Builder, result *types.UninstallResult) {
	if len(result.RetainedNetworks) == 0 {
		return
	}
	b.WriteString("\nWARNING: some wdm-created networks could not be removed. Remove them manually:\n")
	for _, network := range result.RetainedNetworks {
		b.WriteString("- ")
		b.WriteString(network.Name)
		b.WriteString(": docker network rm ")
		b.WriteString(network.Name)
		b.WriteByte('\n')
	}
}

// writeUninstallKeptData appends the kept-data section to b. Named volumes
// and per-app stack directories are never deleted by uninstall, so the
// section always states the preservation guarantee; the per-path list
// follows when the engine reported any kept stack directories.
func writeUninstallKeptData(b *strings.Builder, result *types.UninstallResult) {
	b.WriteString("\nKept (never deleted): named volumes and per-app stack data.\n")
	for _, path := range result.KeptDataPaths {
		b.WriteString("- ")
		b.WriteString(path)
		b.WriteByte('\n')
	}
}
