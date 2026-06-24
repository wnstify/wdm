package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// overrideEditWarning is the one-line §3.40 caution shown when the operator
// opens the compose override editor: a structural override can re-add dropped
// capabilities, expose ports on 0.0.0.0, or break wdm tracking if it removes
// the wdm.managed labels or the project name. It is reader-facing and shown
// only for the compose edit, not the env edit.
const overrideEditWarning = "Heads up: editing the override can re-add capabilities, " +
	"expose ports (0.0.0.0), or break wdm tracking if it removes the wdm.managed " +
	"labels or project name."

// editPathResolvedMsg carries the user-owned file path the engine seeded
// (override or .env.user) so Update can launch the editor with tea.ExecProcess.
// isCompose distinguishes the compose override from the .env.user edit so the
// post-edit flow can record which file was touched.
type editPathResolvedMsg struct {
	appID     string
	path      string
	isCompose bool
	err       error
}

// editedMsg signals the editor process has exited and the on-disk file may
// have changed, so Update kicks off the warn-but-allow validation pass.
type editedMsg struct {
	appID     string
	isCompose bool
	err       error
}

// stackValidatedMsg carries the result of the post-edit ValidateStack pass.
// The edit already happened, so validation is warn-but-allow: warnings or a
// returned error become a status line, never a block.
type stackValidatedMsg struct {
	warnings []string
	err      error
}

// viewEnvFetchedMsg carries the redacted, read-only environment view. result
// already has every secret masked by the engine; it never holds a raw secret.
type viewEnvFetchedMsg struct {
	result *types.ViewEnvResult
	err    error
}

// editComposeCmd seeds (create-if-missing) the user compose override and
// resolves its path so the editor can open it.
func (m model) editComposeCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		path, err := m.eng.EnsureUserOverride(m.ctx, appID)
		return editPathResolvedMsg{appID: appID, path: path, isCompose: true, err: err}
	}
}

// rewireDoneMsg carries the outcome of the pre-edit .env.user migration so
// Update can settle a status line and then open the editor. rewired reports a
// re-render+restart happened; declined is true when the operator dismissed the
// confirm (warn-but-allow); err holds any non-decline failure that aborts.
type rewireDoneMsg struct {
	appID    string
	rewired  bool
	declined bool
	err      error
}

// editEnvCmd offers the .env.user migration before opening the editor. A stack
// installed before the env_file overlay landed does not wire .env.user, so the
// edit would never reach the containers; RewireStack re-renders+restarts to
// activate it (detect → confirm → rewire → restart, T8). It runs through the
// bridge confirmer, so a pre-feature stack raises the standard confirm screen;
// an already-wired stack is a silent no-op. The decline (UserCanceled) is
// warn-but-allow — Update still opens the editor.
func (m model) editEnvCmd(appID string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		_ types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		rewired, _, err := m.eng.RewireStack(ctx, appID, confirmer)
		if err != nil {
			if types.IsCode(err, types.ErrCodeUserCanceled) {
				return rewireDoneMsg{appID: appID, declined: true}
			}
			return rewireDoneMsg{appID: appID, err: err}
		}
		return rewireDoneMsg{appID: appID, rewired: rewired}
	})
}

// resolveEnvPathCmd seeds (create-if-missing) the user .env.user and resolves
// its path so the editor can open it. It runs after the migration offer.
func (m model) resolveEnvPathCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		path, err := m.eng.EnsureUserEnv(m.ctx, appID)
		return editPathResolvedMsg{appID: appID, path: path, isCompose: false, err: err}
	}
}

// validateStackCmd runs the warn-but-allow post-edit validation.
func (m model) validateStackCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		warnings, err := m.eng.ValidateStack(m.ctx, appID)
		return stackValidatedMsg{warnings: warnings, err: err}
	}
}

// loadViewEnvCmd reads the redacted, read-only environment view.
func (m model) loadViewEnvCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		result, err := m.eng.ViewEnvRedacted(m.ctx, appID)
		return viewEnvFetchedMsg{result: result, err: err}
	}
}

// updateUserEditMsg routes the edit/view-env message chain: resolve the
// user file path, launch the editor, validate after the editor exits, and
// settle the redacted view. It reports ok=false for unrelated messages so the
// caller falls through to the next dispatcher.
func (m model) updateUserEditMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case rewireDoneMsg:
		if msg.err != nil {
			m.busy = false
			m.err = msg.err
			m.actionMessage = ""
			return m, nil, true
		}
		switch {
		case msg.rewired:
			m.actionMessage = "Migrated this stack so .env.user is now active."
		case msg.declined:
			m.actionMessage = "Overlay not activated; run `wdm update " + msg.appID + "` to activate .env.user later."
		}
		// Rewired, declined, or a silent no-op all proceed to the editor.
		return m, m.resolveEnvPathCmd(msg.appID), true
	case editPathResolvedMsg:
		if msg.err != nil {
			m.busy = false
			m.err = msg.err
			m.actionMessage = ""
			return m, nil, true
		}
		// $VISUAL/$EDITOR are read here (not in pkg/engine) because the TUI
		// cannot import internal/system; ResolveEditorArgv keeps the value a
		// typed argv so shell metacharacters never reach a shell.
		argv, err := engine.ResolveEditorArgv(os.Getenv("VISUAL"), os.Getenv("EDITOR"), msg.path)
		if err != nil {
			m.busy = false
			m.err = err
			return m, nil, true
		}
		appID, isCompose := msg.appID, msg.isCompose
		//nolint:gosec // argv is typed (no shell); the editor is the user's own $VISUAL/$EDITOR
		editorCmd := exec.CommandContext(m.ctx, argv[0], argv[1:]...)
		cmd := tea.ExecProcess(editorCmd, func(execErr error) tea.Msg {
			return editedMsg{appID: appID, isCompose: isCompose, err: execErr}
		})
		return m, cmd, true
	case editedMsg:
		if msg.err != nil {
			m.busy = false
			m.err = msg.err
			return m, nil, true
		}
		// The edit already landed on disk, so validation is warn-but-allow:
		// run ValidateStack for BOTH compose and env edits and surface the
		// outcome as a status line — never a block.
		return m, m.validateStackCmd(msg.appID), true
	case stackValidatedMsg:
		m.busy = false
		m.err = nil
		m.actionMessage = editValidationMessage(msg.warnings, msg.err)
		return m, nil, true
	case viewEnvFetchedMsg:
		m.busy = false
		m.err = msg.err
		m.viewEnv = msg.result
		m.viewEnvErr = msg.err
		return m, nil, true
	default:
		return m, nil, false
	}
}

// editValidationMessage renders the warn-but-allow post-edit status. A
// validation error or warnings are reported as advisory text; a clean pass
// confirms the saved edit. The edit is never blocked here.
func editValidationMessage(warnings []string, err error) string {
	if err != nil {
		return "Saved. Validation reported: " + err.Error() + " (edit kept; review your changes)."
	}
	if len(warnings) > 0 {
		return "Saved. Compose validation warnings: " + strings.Join(warnings, "; ") + "."
	}
	return "Saved. Compose config validates."
}

// viewEnvScreenView renders the redacted, read-only environment list. Every
// value is already masked by the engine, so nothing here can leak a secret;
// secret-backed entries are flagged so the operator knows the value is hidden.
func (m model) viewEnvScreenView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("View env (redacted)"))
	b.WriteString("\n\n")

	if m.busy {
		b.WriteString("Loading environment for ")
		b.WriteString(m.activeAppID())
		b.WriteString("...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.viewEnvErr != nil {
		b.WriteString("Could not load environment: ")
		b.WriteString(m.viewEnvErr.Error())
		b.WriteString("\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.viewEnv == nil || len(m.viewEnv.Entries) == 0 {
		b.WriteString("No environment variables.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	b.WriteString(m.viewEnv.AppID)
	b.WriteString("\n\n")
	for _, entry := range m.viewEnv.Entries {
		b.WriteString(entry.Key)
		b.WriteString("=")
		b.WriteString(entry.Value)
		if entry.Secret {
			b.WriteString(" [secret]")
		}
		b.WriteByte('\n')
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}
