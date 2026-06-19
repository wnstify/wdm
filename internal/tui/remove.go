package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

var removeActions = []string{
	"Safe remove (keep files and volumes)",
	"Permanently delete app files",
	"Return to app actions",
}

type removeFinishedMsg struct {
	result *types.RemoveResult
	err    error
}

type deleteFinishedMsg struct {
	result *types.DeleteResult
	err    error
}

func (m model) selectRemoveAction() (tea.Model, tea.Cmd) {
	switch removeActions[m.removeActionCursor] {
	case "Safe remove (keep files and volumes)":
		m.busy = true
		m.err = nil
		m.removeResult = nil
		m.progress = progressMsg{}
		return m, m.removeAppCmd(m.activeAppID())
	case "Permanently delete app files":
		m.screen = screenDeleteName
		m.err = nil
		m.deleteNameInput = ""
		m.deleteResult = nil
	case "Return to app actions":
		m.screen = screenAppActions
	}
	return m, nil
}

func (m model) removeAppCmd(appID string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Remove(ctx, types.RemoveRequest{AppID: appID}, progress, confirmer)
		return removeFinishedMsg{result: result, err: err}
	})
}

func (m model) updateDeleteNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Select):
		m.busy = true
		m.err = nil
		m.deleteResult = nil
		m.progress = progressMsg{}
		return m, m.deleteAppCmd(m.activeAppID(), m.deleteNameInput)
	case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
		m = m.deleteDeleteNameRune()
	case msg.Type == tea.KeySpace:
		m.deleteNameInput += " "
	case msg.Type == tea.KeyRunes:
		m.deleteNameInput += string(msg.Runes)
	}
	return m, nil
}

func (m model) deleteDeleteNameRune() model {
	if m.deleteNameInput == "" {
		return m
	}
	runes := []rune(m.deleteNameInput)
	m.deleteNameInput = string(runes[:len(runes)-1])
	return m
}

func (m model) deleteAppCmd(appID, confirmationName string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.DeleteApp(ctx, types.DeleteRequest{
			AppID:            appID,
			ConfirmationName: confirmationName,
		}, progress, confirmer)
		return deleteFinishedMsg{result: result, err: err}
	})
}

func (m model) removeActionsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Remove " + m.activeAppID()))
	b.WriteString("\n\n")
	if m.err != nil {
		b.WriteString("Remove action failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}
	if m.busy {
		b.WriteString("Removing ")
		b.WriteString(m.activeAppID())
		b.WriteString("...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
		b.WriteString("\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	for i, action := range removeActions {
		prefix := "  "
		suffix := ""
		if i == m.removeActionCursor {
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

func (m model) deleteNameView() string {
	var b strings.Builder
	appID := m.activeAppID()
	b.WriteString(titleStyle().Render("Permanently delete " + appID))
	b.WriteString("\n\n")
	if m.busy {
		b.WriteString("Deleting ")
		b.WriteString(appID)
		b.WriteString("...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
		b.WriteString("\n")
		b.WriteString(m.helpLine())
		return b.String()
	}
	if m.err != nil {
		b.WriteString("Delete failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Type %s to confirm permanent deletion.\n\n", appID)
	b.WriteString("Name: ")
	b.WriteString(m.deleteNameInput)
	b.WriteString("\n\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) removeResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Safe remove complete"))
	b.WriteString("\n\n")

	result := m.removeResult
	if result == nil {
		b.WriteString("No remove result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if result.StackPath != "" {
		fmt.Fprintf(&b, "%s is removed from %s\n", result.AppID, result.StackPath)
	} else {
		fmt.Fprintf(&b, "%s is removed\n", result.AppID)
	}
	b.WriteString("Files and data were kept.\n")
	writeStringList(&b, "\nKept on disk:", result.PreservedPaths)
	writeStringListWithEmpty(&b, "\nNamed volumes:", result.RemainingNamedVolumes, "  none reported (Docker inspection data may be unavailable)")
	writeStringList(&b, "\nNetworks left in place:", result.RemainingNetworks)
	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) deleteResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Destructive delete complete"))
	b.WriteString("\n\n")

	result := m.deleteResult
	if result == nil {
		b.WriteString("No delete result returned.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	fmt.Fprintf(&b, "%s was permanently deleted\n", result.AppID)
	writeStringList(&b, "\nDeleted:", result.DeletedPaths)
	writeStringListWithEmpty(&b, "\nNamed volumes (not deleted):", result.RemainingNamedVolumes, "  none reported (Docker inspection data may be unavailable)")
	writeStringList(&b, "\nNetworks removed:", result.RemovedNetworks)
	writeDeleteRetainedNetworks(&b, result)

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeDeleteRetainedNetworks appends a warning naming the wdm-created networks
// that could not be removed during deletion and the manual `docker network rm
// <name>` command to finish the cleanup. Network removal is best-effort, so a
// retained network never fails the deletion; this just surfaces the follow-up,
// mirroring writeUninstallRetainedNetworks.
func writeDeleteRetainedNetworks(b *strings.Builder, result *types.DeleteResult) {
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

func writeStringList(b *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(title)
	b.WriteByte('\n')
	for _, value := range values {
		b.WriteString("  - ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
}

func writeStringListWithEmpty(b *strings.Builder, title string, values []string, empty string) {
	b.WriteString(title)
	b.WriteByte('\n')
	if len(values) == 0 {
		b.WriteString(empty)
		b.WriteByte('\n')
		return
	}
	for _, value := range values {
		b.WriteString("  - ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
}
