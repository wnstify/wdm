package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

const (
	resourceFieldMemory = "memory"
	resourceFieldCPUs   = "cpus"
	resourceFieldPIDs   = "pids"
)

// resourceField is one editable resource limit on the Resources screen. It
// pre-fills with the value currently in effect (original) so an unchanged
// field maps to a nil pointer in the ReconfigureRequest — distinguishing
// "leave unchanged" from "set" (issue #28). hint shows the catalog's allowed
// band next to the field.
type resourceField struct {
	label    string
	target   string
	original string
	value    string
	hint     string
}

type resourceSettingsLoadedMsg struct {
	settings *types.ResourceSettings
	err      error
}

type reconfigureFinishedMsg struct {
	result *types.ReconfigureResult
	err    error
}

// loadResourceSettingsCmd reads the app's current per-service resource limits
// and allowed bands through the engine's read-only inspection view.
func (m model) loadResourceSettingsCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		settings, err := m.eng.ResourceSettings(m.ctx, appID)
		if err == nil && settings == nil {
			err = fmt.Errorf("resource settings unavailable for %q", appID)
		}
		return resourceSettingsLoadedMsg{settings: settings, err: err}
	}
}

// resourceServiceSettings returns the single adjustable service this screen
// edits, or nil when none is adjustable. v1 reconfigures one service per app;
// the screen targets the first overridable service the catalog declares.
func resourceServiceSettings(settings *types.ResourceSettings) *types.ResourceServiceSettings {
	if settings == nil {
		return nil
	}
	for i := range settings.Services {
		if settings.Services[i].Adjustable {
			return &settings.Services[i]
		}
	}
	return nil
}

// newResourceFields builds the editable fields from the selected service's
// current values and bands. Each field pre-fills with the value in effect so
// an untouched field sends nil.
func newResourceFields(svc *types.ResourceServiceSettings) []resourceField {
	currentPIDs := ""
	if svc.CurrentPIDs > 0 {
		currentPIDs = strconv.Itoa(svc.CurrentPIDs)
	}
	return []resourceField{
		{
			label:    "Memory",
			target:   resourceFieldMemory,
			original: svc.CurrentMemory,
			value:    svc.CurrentMemory,
			hint:     resourceBandHint(svc.MemoryMin, svc.MemoryRecommended, svc.MemoryMax),
		},
		{
			label:    "CPUs",
			target:   resourceFieldCPUs,
			original: svc.CurrentCPUs,
			value:    svc.CurrentCPUs,
			hint:     resourceBandHint(svc.CPUsMin, svc.CPUsRecommended, svc.CPUsMax),
		},
		{
			label:    "PIDs",
			target:   resourceFieldPIDs,
			original: currentPIDs,
			value:    currentPIDs,
			hint:     resourcePIDsHint(svc.PIDsDefault, svc.PIDsMax),
		},
	}
}

// resourceBandHint renders the allowed memory/CPU band as a reader-facing
// hint, omitting bounds the catalog left unset.
func resourceBandHint(minVal, recommended, maxVal string) string {
	var parts []string
	if minVal != "" {
		parts = append(parts, "min "+minVal)
	}
	if recommended != "" {
		parts = append(parts, "recommended "+recommended)
	}
	if maxVal != "" {
		parts = append(parts, "max "+maxVal)
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " / ") + ")"
}

// resourcePIDsHint renders the allowed pids band; pids has no minimum — it is
// a containment cap, not a sizing requirement.
func resourcePIDsHint(defaultVal, maxVal int) string {
	var parts []string
	if defaultVal > 0 {
		parts = append(parts, "default "+strconv.Itoa(defaultVal))
	}
	if maxVal > 0 {
		parts = append(parts, "max "+strconv.Itoa(maxVal))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " / ") + ")"
}

func (m model) updateResourcesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Up) && m.resourceFieldCursor > 0:
		m.resourceFieldCursor--
	case key.Matches(msg, m.keys.Down) && m.resourceFieldCursor < len(m.resourceFields):
		m.resourceFieldCursor++
	case key.Matches(msg, m.keys.Select):
		if m.resourceFieldCursor < len(m.resourceFields) {
			m.resourceFieldCursor++
			return m, nil
		}
		return m.submitReconfigure()
	case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
		m = m.deleteResourceInputRune()
	case msg.Type == tea.KeySpace:
		m = m.appendResourceInput(" ")
	case msg.Type == tea.KeyRunes:
		m = m.appendResourceInput(string(msg.Runes))
	}

	return m, nil
}

func (m model) appendResourceInput(value string) model {
	if m.resourceFieldCursor >= len(m.resourceFields) {
		return m
	}
	m.resourceFields[m.resourceFieldCursor].value += value
	return m
}

func (m model) deleteResourceInputRune() model {
	if m.resourceFieldCursor >= len(m.resourceFields) {
		return m
	}

	value := m.resourceFields[m.resourceFieldCursor].value
	if value == "" {
		return m
	}

	runes := []rune(value)
	m.resourceFields[m.resourceFieldCursor].value = string(runes[:len(runes)-1])
	return m
}

func (m model) submitReconfigure() (tea.Model, tea.Cmd) {
	if m.resourceService == "" {
		m.err = fmt.Errorf("no adjustable service selected")
		return m, nil
	}

	req, changed := m.reconfigureRequest()
	if !changed {
		m.err = fmt.Errorf("no resource limits changed")
		return m, nil
	}

	m.busy = true
	m.err = nil
	return m, m.reconfigureCmd(req)
}

// reconfigureRequest builds the ReconfigureRequest from the edited fields. A
// field left at its pre-filled original maps to nil (leave unchanged); a
// field the user changed maps to a non-nil pointer (set). changed reports
// whether any field differs, so the screen can refuse a no-op submit before
// touching the engine, matching the engine's own usage-error contract.
func (m model) reconfigureRequest() (req types.ReconfigureRequest, changed bool) {
	req = types.ReconfigureRequest{AppID: m.activeAppID(), Service: m.resourceService}

	for _, field := range m.resourceFields {
		if field.value == field.original {
			continue
		}
		changed = true
		value := field.value
		switch field.target {
		case resourceFieldMemory:
			req.Memory = &value
		case resourceFieldCPUs:
			req.CPUs = &value
		case resourceFieldPIDs:
			if pids, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				req.PIDs = &pids
			}
		}
	}

	return req, changed
}

func (m model) reconfigureCmd(req types.ReconfigureRequest) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Reconfigure(ctx, req, progress, confirmer)
		return reconfigureFinishedMsg{result: result, err: err}
	})
}

func (m model) resourcesView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Manage resources"))
	b.WriteString("\n\n")

	switch {
	case m.busy && m.resourceSettings == nil:
		b.WriteString("Loading resource limits for ")
		b.WriteString(m.activeAppID())
		b.WriteString("...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	case m.busy:
		b.WriteString("Reconfiguring ")
		b.WriteString(m.activeAppID())
		b.WriteString("...\n")
		if m.progress.message != "" {
			b.WriteString(m.progress.message)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.resourceLoadErr != nil {
		b.WriteString("Could not load resource settings: ")
		b.WriteString(m.resourceLoadErr.Error())
		b.WriteString("\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.reconfigureResult != nil {
		writeReconfigureResult(&b, m.reconfigureResult)
		b.WriteString("\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.err != nil {
		b.WriteString("Reconfigure failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n\n")
	}

	if m.resourceService == "" {
		b.WriteString("This app does not allow resource overrides.\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	b.WriteString("Service: ")
	b.WriteString(m.resourceService)
	b.WriteString("\n\n")

	for i, field := range m.resourceFields {
		prefix := "  "
		suffix := ""
		if i == m.resourceFieldCursor {
			prefix = "> "
			suffix = " [selected]"
		}
		b.WriteString(prefix)
		b.WriteString(field.label)
		b.WriteString(": ")
		b.WriteString(field.value)
		if field.hint != "" {
			b.WriteString(" ")
			b.WriteString(field.hint)
		}
		b.WriteString(suffix)
		b.WriteByte('\n')
	}

	prefix := "  "
	suffix := ""
	if m.resourceFieldCursor == len(m.resourceFields) {
		prefix = "> "
		suffix = " [selected]"
	}
	b.WriteString(prefix)
	b.WriteString("Apply changes")
	b.WriteString(suffix)
	b.WriteByte('\n')

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeReconfigureResult renders the post-reconfigure summary: the applied
// limits and the runtime status after the recreate.
func writeReconfigureResult(b *strings.Builder, result *types.ReconfigureResult) {
	b.WriteString("Reconfigure complete.\n")
	if result.Service != "" {
		b.WriteString("Service: ")
		b.WriteString(result.Service)
		b.WriteByte('\n')
	}
	if result.Memory != "" {
		b.WriteString("Memory: ")
		b.WriteString(result.Memory)
		b.WriteByte('\n')
	}
	if result.CPUs != "" {
		b.WriteString("CPUs: ")
		b.WriteString(result.CPUs)
		b.WriteByte('\n')
	}
	if result.PIDs > 0 {
		fmt.Fprintf(b, "PIDs: %d\n", result.PIDs)
	}
	if result.BackupPath != "" {
		b.WriteString("Config backup: ")
		b.WriteString(result.BackupPath)
		b.WriteByte('\n')
	}
	if result.Status != nil && result.Status.State != "" {
		b.WriteString("State: ")
		b.WriteString(result.Status.State)
		b.WriteByte('\n')
	}
}
