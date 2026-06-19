package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

var runtimeLockRecoveryActions = []string{
	"Clear stale lock",
	"Return to dashboard",
}

type runtimeLockStatusLoadedMsg struct {
	status *types.RuntimeLockStatus
	err    error
}

type runtimeLockClearedMsg struct {
	status *types.RuntimeLockStatus
	err    error
}

func (m model) loadRuntimeLockStatusCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.eng.RuntimeLockStatus(m.ctx)
		return runtimeLockStatusLoadedMsg{status: status, err: err}
	}
}

func (m model) updateRuntimeLockKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	actions := m.runtimeLockActions()
	switch {
	case key.Matches(msg, m.keys.Up) && m.runtimeLockCursor > 0:
		m.runtimeLockCursor--
	case key.Matches(msg, m.keys.Down) && m.runtimeLockCursor < len(actions)-1:
		m.runtimeLockCursor++
	case key.Matches(msg, m.keys.Select):
		if len(actions) == 0 {
			return m, nil
		}
		switch actions[m.runtimeLockCursor] {
		case "Clear stale lock":
			m.busy = true
			m.err = nil
			m.runtimeLockMessage = ""
			return m, m.clearRuntimeLockCmd()
		case "Return to dashboard":
			m.screen = screenDashboard
			m.err = nil
			m.runtimeLockMessage = ""
		}
	}

	return m, nil
}

func (m model) runtimeLockActions() []string {
	if m.runtimeLockStatus != nil && m.runtimeLockStatus.Stale {
		return runtimeLockRecoveryActions
	}
	return []string{"Return to dashboard"}
}

func (m model) clearRuntimeLockCmd() tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		_ types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		status, err := m.eng.ClearStaleRuntimeLock(ctx, confirmer)
		return runtimeLockClearedMsg{status: status, err: err}
	})
}

func (m model) runtimeLockView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Runtime lock recovery"))
	b.WriteString("\n\n")

	if m.busy {
		b.WriteString("Clearing runtime lock...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.err != nil {
		b.WriteString("Runtime lock action failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}
	if m.runtimeLockMessage != "" {
		b.WriteString(m.runtimeLockMessage)
		b.WriteString("\n\n")
	}

	if m.runtimeLockStatus != nil {
		writeRuntimeLockStatus(&b, m.runtimeLockStatus)
	} else {
		b.WriteString("No runtime lock status available.\n")
	}

	b.WriteString("\nActions\n\n")
	for i, action := range m.runtimeLockActions() {
		prefix := "  "
		suffix := ""
		if i == m.runtimeLockCursor {
			prefix = "> "
			suffix = " [selected]"
		}
		b.WriteString(prefix)
		b.WriteString(action)
		b.WriteString(suffix)
		b.WriteByte('\n')
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func writeRuntimeLockStatus(b *strings.Builder, status *types.RuntimeLockStatus) {
	fmt.Fprintf(b, "exists: %t\n", status.Exists)
	fmt.Fprintf(b, "held: %t\n", status.Held)
	fmt.Fprintf(b, "stale: %t\n", status.Stale)
	if status.HolderPID != 0 {
		fmt.Fprintf(b, "holder_pid: %d\n", status.HolderPID)
	}
	if status.HolderCommand != "" {
		fmt.Fprintf(b, "holder_command: %s\n", status.HolderCommand)
	}
	if status.HolderPID != 0 || status.HolderCommand != "" {
		fmt.Fprintf(b, "holder_alive: %t\n", status.HolderAlive)
	}
	if status.WDMVersion != "" {
		fmt.Fprintf(b, "wdm_version: %s\n", status.WDMVersion)
	}
}
