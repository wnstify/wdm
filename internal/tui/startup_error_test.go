package tui

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupErrorModelRendersFriendlyErrorAndQuits(t *testing.T) {
	t.Parallel()

	m := newStartupErrorModel(errors.New("config.toml is invalid"))

	view := m.View()
	assert.Contains(t, view, "Could not start wdm")
	assert.Contains(t, view, "config.toml is invalid")
	assert.Contains(t, view, "Quit: q")

	next, cmd := m.Update(runeKey('q'))
	got, ok := next.(startupErrorModel)
	require.True(t, ok)
	require.NotNil(t, cmd)
	assert.Contains(t, got.View(), "Goodbye")
}

func TestStartupErrorModelInitStartsNoCommand(t *testing.T) {
	t.Parallel()

	m := newStartupErrorModel(errors.New("startup failed"))

	assert.Nil(t, m.Init())
}

func TestRunStartupErrorHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := RunStartupError(
		ctx,
		errors.New("startup failed"),
		tea.WithoutRenderer(),
		tea.WithInput(nil),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
