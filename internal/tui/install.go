package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

const (
	installFieldDomain      = "domain"
	installFieldStackPath   = "stack_path"
	installFieldPlaceholder = "placeholder"
)

type installField struct {
	label    string
	key      string
	target   string
	required bool
	value    string
}

type catalogAppsLoadedMsg struct {
	apps []types.CatalogApp
	err  error
}

type catalogAppLoadedMsg struct {
	app *types.CatalogApp
	err error
}

type installFinishedMsg struct {
	result *types.InstallResult
	err    error
}

func (m model) loadCatalogAppsCmd() tea.Cmd {
	return func() tea.Msg {
		apps, err := m.eng.AvailableApps(m.ctx, types.CatalogQuery{})
		return catalogAppsLoadedMsg{apps: apps, err: err}
	}
}

func (m model) selectCatalogApp() (tea.Model, tea.Cmd) {
	app := m.selectedCatalogApp()
	if app == nil {
		return m, nil
	}

	m.busy = true
	m.err = nil
	m.catalogDetail = nil
	m.installFields = nil
	m.installResult = nil
	return m, m.loadCatalogAppCmd(app.AppID)
}

func (m model) selectedCatalogApp() *types.CatalogApp {
	if m.catalogCursor < 0 || m.catalogCursor >= len(m.catalogApps) {
		return nil
	}
	return &m.catalogApps[m.catalogCursor]
}

func (m model) loadCatalogAppCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		app, err := m.eng.AvailableApp(m.ctx, types.CatalogAppQuery{AppID: appID})
		if err == nil && app == nil {
			err = fmt.Errorf("catalog app %q not found", appID)
		}
		return catalogAppLoadedMsg{app: app, err: err}
	}
}

func newInstallFields(app *types.CatalogApp) []installField {
	domainField := installField{
		label:    "Domain",
		target:   installFieldDomain,
		required: true,
	}
	fields := []installField{domainField, {
		label:  "Stack path",
		target: installFieldStackPath,
	}}

	for _, placeholder := range app.Placeholders {
		switch {
		case placeholder.Secret:
			continue
		case placeholder.Type == "domain":
			fields[0] = installField{
				label:    placeholder.Key,
				key:      placeholder.Key,
				target:   installFieldDomain,
				required: placeholder.Required,
			}
		default:
			fields = append(fields, installField{
				label:    placeholder.Key,
				key:      placeholder.Key,
				target:   installFieldPlaceholder,
				required: placeholder.Required,
			})
		}
	}

	return fields
}

func (m model) updateInstallFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up) && m.installFieldCursor > 0:
		m.installFieldCursor--
	case key.Matches(msg, m.keys.Down) && m.installFieldCursor < len(m.installFields):
		m.installFieldCursor++
	case key.Matches(msg, m.keys.Select):
		if m.installFieldCursor < len(m.installFields) {
			m.installFieldCursor++
			return m, nil
		}
		return m.submitInstall()
	case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
		m = m.deleteInstallInputRune()
	case msg.Type == tea.KeySpace:
		m = m.appendInstallInput(" ")
	case msg.Type == tea.KeyRunes:
		m = m.appendInstallInput(string(msg.Runes))
	}

	return m, nil
}

// updatePortRemapKey drives the port-conflict input: Enter re-invokes install
// with the chosen port, digits build the port, backspace erases. Non-digit
// runes are ignored so the field only ever holds a port number. Esc (handled
// as Back in the top-level Update) cancels fail-closed.
func (m model) updatePortRemapKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Select):
		return m.submitPortRemap()
	case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
		if m.portRemapInput != "" {
			runes := []rune(m.portRemapInput)
			m.portRemapInput = string(runes[:len(runes)-1])
		}
	case msg.Type == tea.KeyRunes:
		for _, r := range msg.Runes {
			if r >= '0' && r <= '9' {
				m.portRemapInput += string(r)
			}
		}
	}
	return m, nil
}

// submitPortRemap records the chosen host port as a PortOverride and re-invokes
// install (ADR 0004). PortOverrides keys on the catalog host port, but a repeat
// conflict reports the effective (already-remapped) port, so when the busy port
// matches an existing override's target the same catalog key is updated rather
// than adding a stale key that would match no planned binding.
func (m model) submitPortRemap() (tea.Model, tea.Cmd) {
	if m.portConflict == nil {
		return m, nil
	}

	newPort, err := strconv.Atoi(m.portRemapInput)
	if err != nil || newPort <= 0 {
		m.err = fmt.Errorf("enter a host port number")
		return m, nil
	}

	if m.installReq.PortOverrides == nil {
		m.installReq.PortOverrides = map[int]int{}
	}
	catalogPort := m.portConflict.ConflictingHostPort
	for old, remapped := range m.installReq.PortOverrides {
		if remapped == catalogPort {
			catalogPort = old
			break
		}
	}
	m.installReq.PortOverrides[catalogPort] = newPort

	// Keep m.portConflict set: a plain (non-PortConflictError) re-invoke failure
	// leaves applyInstallFinished nothing to repopulate it with, and the remap
	// screen renders "No port conflict." + swallows m.err if it goes nil, dead-
	// ending the user. A fresh conflict overwrites it; success advances the
	// screen and makes the stale value harmless (ADR 0004).
	m.busy = true
	m.err = nil
	return m, m.installCmd(m.installReq)
}

func (m model) appendInstallInput(value string) model {
	if m.installFieldCursor >= len(m.installFields) {
		return m
	}
	m.installFields[m.installFieldCursor].value += value
	return m
}

func (m model) deleteInstallInputRune() model {
	if m.installFieldCursor >= len(m.installFields) {
		return m
	}

	value := m.installFields[m.installFieldCursor].value
	if value == "" {
		return m
	}

	runes := []rune(value)
	m.installFields[m.installFieldCursor].value = string(runes[:len(runes)-1])
	return m
}

func (m model) submitInstall() (tea.Model, tea.Cmd) {
	if m.catalogDetail == nil {
		m.err = fmt.Errorf("no catalog app selected")
		return m, nil
	}

	req := m.installRequest()
	m.installReq = req
	m.busy = true
	m.err = nil
	return m, m.installCmd(req)
}

// applyInstallFinished folds an Install outcome into the model. A remappable
// host-port conflict (a typed *PortConflictError carrying a non-zero
// suggestion) is not a failure: it opens the port-remap screen prefilled with
// the suggestion so the user can accept, retype, or cancel (ADR 0004). A
// fail-closed conflict (suggestion 0) and any other error stay a plain failure
// on the form; success advances to the result screen.
func (m model) applyInstallFinished(msg installFinishedMsg) model {
	m.busy = false

	var conflict *types.PortConflictError
	if errors.As(msg.err, &conflict) && conflict.SuggestedHostPort != 0 {
		m.err = nil
		m.portConflict = conflict
		m.portRemapInput = strconv.Itoa(conflict.SuggestedHostPort)
		m.screen = screenPortRemap
		return m
	}

	m.err = msg.err
	m.installResult = msg.result
	if msg.err == nil {
		m.screen = screenInstallResult
	}
	return m
}

func (m model) installRequest() types.InstallRequest {
	req := types.InstallRequest{AppID: m.catalogDetail.AppID}
	placeholderValues := make(map[string]string)

	for _, field := range m.installFields {
		switch field.target {
		case installFieldDomain:
			req.Domain = field.value
		case installFieldStackPath:
			req.StackPath = field.value
		case installFieldPlaceholder:
			if field.value != "" {
				placeholderValues[field.key] = field.value
			}
		}
	}

	if len(placeholderValues) > 0 {
		req.PlaceholderValues = placeholderValues
	}
	return req
}

func (m model) installCmd(req types.InstallRequest) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Install(ctx, req, progress, confirmer)
		return installFinishedMsg{result: result, err: err}
	})
}

func (m model) installCatalogView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Install an app"))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Loading catalog...\n")
	case m.err != nil:
		b.WriteString("Could not load catalog: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	case len(m.catalogApps) == 0:
		b.WriteString("No catalog apps found.\n")
	default:
		for i, app := range m.catalogApps {
			prefix := "  "
			suffix := ""
			if i == m.catalogCursor {
				prefix = "> "
				suffix = " [selected]"
			}
			b.WriteString(prefix)
			b.WriteString(app.AppID)
			if app.Name != "" {
				b.WriteString("  ")
				b.WriteString(app.Name)
			}
			if app.Summary != "" {
				b.WriteString(" - ")
				b.WriteString(app.Summary)
			}
			b.WriteString(suffix)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) installFormView() string {
	var b strings.Builder
	name := selectedCatalogName(m.catalogDetail)
	b.WriteString(titleStyle().Render(name))
	b.WriteString("\n\n")

	if m.busy {
		b.WriteString("Installing ")
		b.WriteString(selectedCatalogID(m.catalogDetail))
		b.WriteString("...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.err != nil {
		b.WriteString("Install failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n")
		m.writeLogPathNotice(&b)
		b.WriteString("\n")
	}

	if m.catalogDetail != nil {
		writeCatalogDetail(&b, m.catalogDetail)
	}

	b.WriteString("\nInstall inputs\n\n")
	for i, field := range m.installFields {
		prefix := "  "
		suffix := ""
		if i == m.installFieldCursor {
			prefix = "> "
			suffix = " [selected]"
		}
		b.WriteString(prefix)
		b.WriteString(field.label)
		b.WriteString(": ")
		b.WriteString(field.value)
		if field.required {
			b.WriteString(" required")
		}
		b.WriteString(suffix)
		b.WriteByte('\n')
	}

	prefix := "  "
	suffix := ""
	if m.installFieldCursor == len(m.installFields) {
		prefix = "> "
		suffix = " [selected]"
	}
	b.WriteString(prefix)
	b.WriteString("Install app")
	b.WriteString(suffix)
	b.WriteByte('\n')

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) installPortRemapView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Port conflict"))
	b.WriteString("\n\n")

	if m.busy {
		b.WriteString("Retrying install...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.portConflict == nil {
		b.WriteString("No port conflict.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	fmt.Fprintf(&b, "127.0.0.1:%d is already in use", m.portConflict.ConflictingHostPort)
	if m.portConflict.Service != "" {
		fmt.Fprintf(&b, " by service %s", m.portConflict.Service)
	}
	b.WriteString(".\n\n")

	if m.err != nil {
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}

	b.WriteString("New host port: ")
	b.WriteString(m.portRemapInput)
	b.WriteString("\n\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func writeCatalogDetail(b *strings.Builder, app *types.CatalogApp) {
	if app.Description != "" {
		b.WriteString(app.Description)
		b.WriteString("\n\n")
	} else if app.Summary != "" {
		b.WriteString(app.Summary)
		b.WriteString("\n\n")
	}

	if len(app.Placeholders) > 0 {
		b.WriteString("Placeholders\n")
		for _, placeholder := range app.Placeholders {
			b.WriteString("- ")
			b.WriteString(placeholder.Key)
			if placeholder.Type != "" {
				b.WriteString(" ")
				b.WriteString(placeholder.Type)
			}
			if placeholder.Required {
				b.WriteString(" required")
			}
			if placeholder.Secret {
				b.WriteString(" generated")
			}
			if placeholder.Default != "" {
				b.WriteString(" default ")
				b.WriteString(placeholder.Default)
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	if len(app.Ports) > 0 {
		b.WriteString("Ports\n")
		for _, port := range app.Ports {
			b.WriteString("- ")
			if port.Service != "" {
				b.WriteString(port.Service)
				b.WriteString(" ")
			}
			fmt.Fprintf(b, "%d -> %d", port.Host, port.Container)
			if port.Protocol != "" {
				b.WriteString("/")
				b.WriteString(port.Protocol)
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	if len(app.ImagePins) > 0 {
		b.WriteString("Images\n")
		for _, image := range app.ImagePins {
			b.WriteString("- ")
			b.WriteString(image.Service)
			b.WriteString(" ")
			b.WriteString(image.Image)
			if image.Tag != "" {
				b.WriteString(":")
				b.WriteString(image.Tag)
			}
			b.WriteByte('\n')
		}
	}
}

func (m model) installResultView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Install complete"))
	b.WriteString("\n\n")

	if m.installResult == nil {
		b.WriteString("Install completed.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.installResult.AppID != "" {
		b.WriteString(m.installResult.AppID)
		b.WriteByte('\n')
	}
	if m.installResult.StackPath != "" {
		b.WriteString("Stack path: ")
		b.WriteString(m.installResult.StackPath)
		b.WriteByte('\n')
	}
	if m.installResult.PostInstallGuidance != nil &&
		m.installResult.PostInstallGuidance.LocalTargetURL != "" {
		b.WriteString("Local target: ")
		b.WriteString(m.installResult.PostInstallGuidance.LocalTargetURL)
		b.WriteByte('\n')
	}
	if m.installResult.PostInstallGuidance != nil &&
		m.installResult.PostInstallGuidance.Pangolin != nil {
		guidance := m.installResult.PostInstallGuidance.Pangolin
		b.WriteString("\nPangolin next step\n")
		if guidance.RecommendedSubdomain != "" {
			b.WriteString("Recommended subdomain: ")
			b.WriteString(guidance.RecommendedSubdomain)
			b.WriteByte('\n')
		}
		if guidance.TargetURL != "" {
			b.WriteString("Target URL: ")
			b.WriteString(guidance.TargetURL)
			b.WriteByte('\n')
		}
		for _, note := range guidance.Notes {
			b.WriteString("- ")
			b.WriteString(note)
			b.WriteByte('\n')
		}
	}
	if m.installResult.PostInstallGuidance != nil &&
		len(m.installResult.PostInstallGuidance.FirstRunNotes) > 0 {
		b.WriteString("\nFirst-run notes\n")
		for _, note := range m.installResult.PostInstallGuidance.FirstRunNotes {
			b.WriteString("- ")
			b.WriteString(note)
			b.WriteByte('\n')
		}
	}

	if m.installResult.PostInstallGuidance != nil &&
		len(m.installResult.PostInstallGuidance.GeneratedCredentials) > 0 {
		writeInstallGeneratedCredentials(&b, m.installResult.PostInstallGuidance.GeneratedCredentials)
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeInstallGeneratedCredentials renders the one-time credential block
// on the TUI finish view. These plaintexts are shown exactly once — wdm
// persists only a one-way hash — so the operator must record each value
// before leaving the screen.
func writeInstallGeneratedCredentials(b *strings.Builder, creds []types.GeneratedCredential) {
	b.WriteString("\nSAVE THIS NOW — shown once, cannot be recovered\n")
	for _, cred := range creds {
		if cred.Label != "" {
			b.WriteString(cred.Label)
			b.WriteString(":\n")
		}
		b.WriteString("  ")
		b.WriteString(cred.Value)
		b.WriteByte('\n')
		if cred.Note != "" {
			b.WriteString("  ")
			b.WriteString(cred.Note)
			b.WriteByte('\n')
		}
	}
}

func selectedCatalogName(app *types.CatalogApp) string {
	if app == nil {
		return "Install an app"
	}
	if app.Name != "" {
		return app.Name
	}
	return app.AppID
}

func selectedCatalogID(app *types.CatalogApp) string {
	if app == nil {
		return "app"
	}
	return app.AppID
}
