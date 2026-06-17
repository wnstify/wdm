package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

type backupsLoadedMsg struct {
	backups []types.BackupInfo
	err     error
}

type restoreFinishedMsg struct {
	result *types.RestoreBackupResult
	err    error
}

func (m model) selectBackupsApp() (tea.Model, tea.Cmd) {
	if len(m.apps) == 0 {
		return m, nil
	}

	m.backupAppID = m.apps[m.appCursor].AppID
	m.screen = screenBackupsList
	m.busy = true
	m.err = nil
	m.backups = nil
	m.backupCursor = 0
	m.restoreResult = nil
	return m, m.listBackupsCmd(m.backupAppID)
}

func (m model) listBackupsCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		backups, err := m.eng.ListBackups(m.ctx, appID)
		return backupsLoadedMsg{backups: backups, err: err}
	}
}

func (m model) selectBackupSnapshot() (tea.Model, tea.Cmd) {
	if len(m.backups) == 0 {
		return m, nil
	}

	m.busy = true
	m.err = nil
	m.restoreResult = nil
	m.progress = progressMsg{}
	return m, m.restoreBackupCmd(m.backups[m.backupCursor].SnapshotID)
}

func (m model) restoreBackupCmd(snapshotID string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.RestoreBackup(ctx, types.RestoreBackupRequest{
			AppID:      m.backupAppID,
			SnapshotID: snapshotID,
		}, progress, confirmer)
		return restoreFinishedMsg{result: result, err: err}
	})
}

func (m model) backupsAppsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Backups"))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Loading managed apps...\n")
	case m.err != nil:
		b.WriteString("Could not load managed apps: ")
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

func (m model) backupsListView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Config backups for " + m.backupAppID))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Restoring config backup...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
	case m.err != nil:
		b.WriteString("Backup action failed: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	case len(m.backups) == 0:
		b.WriteString("No config backups found.\n")
	default:
		for i, backup := range m.backups {
			prefix := "  "
			suffix := ""
			if i == m.backupCursor {
				prefix = "> "
				suffix = " [selected]"
			}
			fmt.Fprintf(
				&b,
				"%s%s  %s  %s  %d file(s)%s\n",
				prefix,
				backup.SnapshotID,
				backup.Operation,
				formatBackupTime(backup.CreatedAt),
				len(backup.Files),
				suffix,
			)
		}
	}

	b.WriteString("\n")
	b.WriteString(helpLine())
	return b.String()
}

func (m model) restoreResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Config restore complete"))
	b.WriteString("\n\n")

	result := m.restoreResult
	if result == nil {
		b.WriteString("No config restore result returned.\n\n")
		b.WriteString(helpLine())
		return b.String()
	}

	if result.Status != nil && result.Status.NeedsAttention {
		fmt.Fprintf(&b, "%s config restored from snapshot %s; see the status below for services that need attention.\n",
			result.AppID, result.SnapshotID)
	} else {
		fmt.Fprintf(&b, "%s config restored from snapshot %s.\n", result.AppID, result.SnapshotID)
	}

	writeStringList(&b, "\nRestored config files:", result.RestoredFiles)
	if result.BoundaryNotice != "" {
		fmt.Fprintf(&b, "\n%s\n", result.BoundaryNotice)
	}
	if result.NextAction != "" {
		fmt.Fprintf(&b, "\nNext: %s\n", result.NextAction)
	}
	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	b.WriteString("\n")
	b.WriteString(helpLine())
	return b.String()
}

func formatBackupTime(t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}
