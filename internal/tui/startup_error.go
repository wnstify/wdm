package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

type startupErrorModel struct {
	err     error
	keys    keyMap
	exiting bool
}

func newStartupErrorModel(err error) startupErrorModel {
	return startupErrorModel{
		err:  err,
		keys: defaultKeyMap(),
	}
}

// RunStartupError shows a minimal TUI error screen for startup failures that
// happen before an engine-backed model can be built.
func RunStartupError(ctx context.Context, startupErr error, opts ...tea.ProgramOption) error {
	if ctx == nil {
		ctx = context.Background()
	}

	programOpts := make([]tea.ProgramOption, 0, len(opts)+1)
	programOpts = append(programOpts, tea.WithContext(ctx))
	programOpts = append(programOpts, opts...)

	_, err := tea.NewProgram(newStartupErrorModel(startupErr), programOpts...).Run()
	return err
}

func (m startupErrorModel) Init() tea.Cmd {
	return nil
}

func (m startupErrorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if key.Matches(keyMsg, m.keys.Quit) {
		m.exiting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m startupErrorModel) View() string {
	if m.exiting {
		return "Goodbye.\n"
	}

	var b strings.Builder
	b.WriteString(titleStyle().Render("wdm"))
	b.WriteString("\n\nCould not start wdm.")
	if m.err != nil {
		b.WriteString("\n")
		b.WriteString(m.err.Error())
	}
	b.WriteString("\n\nQuit: q\n")
	return b.String()
}
