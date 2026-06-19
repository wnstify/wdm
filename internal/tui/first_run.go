package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

var firstRunSteps = []string{
	"Welcome",
	"Check system requirements",
	"Choose app",
	"Enter app domain",
	"Choose stack path",
	"Confirm generated settings",
	"Generate files and secrets",
	"Validate Compose and Docker readiness",
	"Deploy after confirmation",
	"Show local URL and Pangolin next step",
}

func newFirstRunModel(eng engine.Engine) model {
	m := newModel(eng)
	m.firstRun = true
	m.screen = screenFirstRunWelcome
	return m
}

func (m model) startFirstRunInstall() (tea.Model, tea.Cmd) {
	m.screen = screenInstallCatalog
	m.busy = true
	m.err = nil
	m.installResult = nil
	return m, m.loadCatalogAppsCmd()
}

func (m model) firstRunSystemCheckView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("First-run wizard"))
	b.WriteString("\n")
	b.WriteString(firstRunSteps[1])
	b.WriteString("\n\n")
	b.WriteString("Docker and Compose readiness are checked by the install engine before deployment.\n")
	b.WriteString("The install flow will also validate generated files, Compose config, paths, ports, and confirmation gates.\n\n")
	b.WriteString("> Continue to choose app [selected]\n\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) firstRunWelcomeView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("First-run wizard"))
	b.WriteString("\n\n")
	b.WriteString("This guided setup will install your first app.\n\n")
	b.WriteString("Steps\n\n")
	for i, step := range firstRunSteps {
		prefix := "  "
		if i == 0 {
			prefix = "> "
		}
		b.WriteString(prefix)
		b.WriteString(step)
		if i == 0 {
			b.WriteString(" [selected]")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) firstRunInstallView(body string, step int) string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("First-run wizard"))
	b.WriteString("\n")
	if step >= 1 && step <= len(firstRunSteps) {
		b.WriteString(firstRunSteps[step-1])
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(body)
	return b.String()
}

func (m model) firstRunConfirmationView() string {
	return m.firstRunInstallView(m.confirmationView(), 9)
}

func (m model) firstRunInstallStep() int {
	switch m.progress.step {
	case types.StepInstallRender,
		types.StepInstallWriteFiles,
		types.StepInstallResourceDegraded:
		return 7
	case types.StepInstallComposeValidate:
		return 8
	case types.StepInstallConfirm,
		types.StepInstallNetworkCreate,
		types.StepInstallDeploy,
		types.StepInstallLockUpdate,
		types.StepInstallStatus:
		return 9
	}

	switch m.installFieldCursor {
	case 0:
		return 4
	case 1:
		return 5
	default:
		return 6
	}
}
