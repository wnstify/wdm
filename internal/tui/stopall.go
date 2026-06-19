package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

// stopAllFinishedMsg carries the Engine.StopAll result back to the model
// after the stop_all confirmation and the streamed batch complete. StopAll
// is continue-on-error, so a non-nil result may still report per-stack
// failures in result.Failed.
type stopAllFinishedMsg struct {
	result *types.StopAllResult
	err    error
}

// stopAllCmd stops every managed app at once. It runs through
// engineCommand so the stop_all confirmation is raised before any stop and
// so progress streams to the view. The request is argless: StopAll always
// targets the full managed set.
func (m model) stopAllCmd() tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.StopAll(ctx, types.StopAllRequest{}, progress, confirmer)
		return stopAllFinishedMsg{result: result, err: err}
	})
}

// stopAllScreenView dispatches the two stop-all screens so the central
// screenView router carries a single case for the flow.
func (m model) stopAllScreenView() string {
	if m.screen == screenStopAllResult {
		return m.stopAllResultView()
	}
	return m.stopAllView()
}

func (m model) stopAllView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Stop all apps"))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Stopping all managed apps...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		b.WriteString("Could not stop all apps: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	default:
		b.WriteString("Stops the containers of every managed app without removing them.\n")
		b.WriteString("No data is removed; this is not a teardown.\n")
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) stopAllResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Stop all apps"))
	b.WriteString("\n\n")

	result := m.stopAllResult
	if result == nil {
		b.WriteString("No stop result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	switch {
	case len(result.Stopped) == 0 && len(result.Failed) == 0:
		b.WriteString("No running apps to stop.\n")
	case len(result.Failed) == 0:
		b.WriteString("All running apps were stopped.\n")
	default:
		b.WriteString("Some apps failed to stop; see below.\n")
	}

	if len(result.AlreadyStopped) > 0 {
		b.WriteString("\nAlready stopped (skipped)\n")
		for _, app := range result.AlreadyStopped {
			b.WriteString("- ")
			b.WriteString(app.AppID)
			b.WriteByte('\n')
		}
	}

	if len(result.Stopped) > 0 {
		b.WriteString("\nStopped\n")
		for _, app := range result.Stopped {
			b.WriteString("- ")
			b.WriteString(app.AppID)
			b.WriteByte('\n')
		}
	}

	if len(result.Failed) > 0 {
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
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}
