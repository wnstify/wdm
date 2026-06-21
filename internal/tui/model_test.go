package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestModel_MainMenuRendersPRDActionsAndNonColorSelection(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})

	view := m.View()
	for _, action := range []string{
		"Install an app",
		"Update apps",
		"Check my apps",
		"Backups",
		"Settings",
	} {
		assert.Contains(t, view, action)
	}
	assert.Contains(t, view, "> Install an app [selected]", "selection must not rely on color alone")
	assert.Contains(t, view, "Back: b")
	assert.Contains(t, view, "Quit: q")
}

func TestModel_SmallTerminalPromptsResizeAtAnySize(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})

	view := m.View()
	assert.Contains(t, view, "Please resize")
	assert.Contains(t, view, "80x24")
	assert.Contains(t, view, "current 40x10")
	assert.Contains(t, view, "Quit: q")
}

func TestModel_NavigationBackAndQuitStayVisible(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())
	m = updateModel(t, m, downKey())

	view := m.View()
	assert.Contains(t, view, "> Backups [selected]")

	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	m = updateModel(t, m, cmd())

	childView := m.View()
	assert.Contains(t, childView, "Backups")
	assert.Contains(t, childView, "Back: b")
	assert.Contains(t, childView, "Quit: q")

	m = updateModel(t, m, runeKey('b'))
	assert.Contains(t, m.View(), "> Backups [selected]", "Back returns to dashboard without losing selection")

	next, cmd = m.Update(runeKey('q'))
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	assert.Contains(t, m.View(), "Goodbye", "quit must leave a visible exit message")
}

func TestModel_NonTextScreenBAndQKeepShortcuts(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	require.Equal(t, screenDashboard, m.screen)
	require.False(t, m.isTextEntryScreen())

	// 'b' on a non-text screen triggers Back, not typed input.
	m = updateModel(t, m, runeKey('b'))
	assert.False(t, m.exiting)

	// 'q' on a non-text screen triggers Quit.
	next, cmd := m.Update(runeKey('q'))
	m = assertModel(t, next)
	require.NotNil(t, cmd)
	assert.True(t, m.exiting)
	assert.Equal(t, tea.Quit(), cmd())
}

func TestModel_HelpLineIsContextAware(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})

	m.screen = screenDeleteName
	require.True(t, m.isTextEntryScreen())
	textHelp := m.helpLine()
	assert.Contains(t, textHelp, "Esc")
	assert.Contains(t, textHelp, "Ctrl+C")
	assert.NotContains(t, textHelp, "Back: b")
	assert.NotContains(t, textHelp, "Quit: q")

	m.screen = screenDashboard
	require.False(t, m.isTextEntryScreen())
	navHelp := m.helpLine()
	assert.Contains(t, navHelp, "Back: b")
	assert.Contains(t, navHelp, "Quit: q")
}

func TestApp_CloseClosesEngineOnce(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	app, err := New(fake)
	require.NoError(t, err)

	require.NoError(t, app.Close())
	require.NoError(t, app.Close())
	assert.Equal(t, 1, fake.closeCount())
}

func TestApp_CloseReturnsFirstCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close failed")
	fake := &fakeEngine{closeErr: closeErr}
	app, err := New(fake)
	require.NoError(t, err)

	assert.ErrorIs(t, app.Close(), closeErr)
	assert.ErrorIs(t, app.Close(), closeErr)
	assert.Equal(t, 1, fake.closeCount())
}

func TestApp_RunClosesEngineAfterProgramExit(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	app, err := New(fake)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = app.Run(ctx, tea.WithoutRenderer(), tea.WithInput(nil))

	require.ErrorContains(t, err, "context canceled")
	assert.Equal(t, 1, fake.closeCount())
}

func TestEngineBridge_ProgressAndConfirmationCallbacksUseMessages(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	bridge := newEngineBridge(sender.Send)
	confirmation := types.Confirmation{
		Kind:    "update_deploy",
		Title:   "Recreate containers",
		Message: "The stack will be recreated.",
	}
	type resultMsg struct {
		accepted bool
		err      error
	}

	cmd := engineCommand(t.Context(), bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		progress(types.StepRestartExecute, 0.50, "Stopping containers")
		accepted, err := confirmer.Confirm(ctx, confirmation)
		return resultMsg{accepted: accepted, err: err}
	})

	resultC := make(chan tea.Msg, 1)
	go func() {
		resultC <- cmd()
	}()

	progress := sender.waitProgress(t)
	assert.Equal(t, types.StepRestartExecute, progress.step)
	assert.Equal(t, "Stopping containers", progress.message)

	request := sender.waitConfirmation(t)
	assert.Equal(t, confirmation, request.confirmation)
	request.reply <- confirmationReply{accepted: true}

	result := requireResult[resultMsg](t, resultC)
	require.NoError(t, result.err)
	assert.True(t, result.accepted)
}

func TestModel_ConfirmationPromptAnswersThroughReplyChannel(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	reply := make(chan confirmationReply, 1)
	confirmation := types.Confirmation{
		Kind:    "delete_destructive",
		Title:   "Delete uptime-kuma",
		Message: "This deletes the stack files and backups.",
	}

	m = updateModel(t, m, confirmationRequestedMsg{
		confirmation: confirmation,
		reply:        reply,
	})

	view := m.View()
	assert.Contains(t, view, confirmation.Title)
	assert.Contains(t, view, confirmation.Message)
	assert.Contains(t, view, "Yes: y")
	assert.Contains(t, view, "No: n")

	m = updateModel(t, m, runeKey('y'))
	select {
	case got := <-reply:
		assert.True(t, got.accepted)
	case <-time.After(time.Second):
		t.Fatal("confirmation reply was not sent")
	}
	assert.NotContains(t, m.View(), confirmation.Title, "answering should dismiss the modal")
}

func TestModel_ConfirmationDeclineAbortsWithoutAccepting(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	reply := make(chan confirmationReply, 1)
	confirmation := types.Confirmation{
		Kind:    "delete_destructive",
		Title:   "Delete uptime-kuma",
		Message: "This deletes the stack files and backups.",
	}

	m = updateModel(t, m, confirmationRequestedMsg{
		confirmation: confirmation,
		reply:        reply,
	})
	require.NotNil(t, m.modal, "modal must be active before declining")

	m = updateModel(t, m, runeKey('n'))

	select {
	case got := <-reply:
		assert.False(t, got.accepted, "decline must report the destructive action as not accepted")
	case <-time.After(time.Second):
		t.Fatal("decline reply was not sent")
	}
	assert.Nil(t, m.modal, "declining must dismiss the modal")
	assert.False(t, m.exiting, "decline must not exit the program")
	assert.NotContains(t, m.View(), confirmation.Title, "declining should dismiss the modal")
}

func TestModel_ConfirmationQuitDeclinesAndQuits(t *testing.T) {
	t.Parallel()

	m := newModel(&fakeEngine{})
	m = updateModel(t, m, tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	reply := make(chan confirmationReply, 1)
	confirmation := types.Confirmation{
		Kind:    "delete_destructive",
		Title:   "Delete uptime-kuma",
		Message: "This deletes the stack files and backups.",
	}

	m = updateModel(t, m, confirmationRequestedMsg{
		confirmation: confirmation,
		reply:        reply,
	})

	next, cmd := m.Update(runeKey('q'))
	m = assertModel(t, next)
	require.NotNil(t, cmd, "quit at the confirmation gate must return a command")
	assert.Equal(t, tea.QuitMsg{}, cmd(), "quit command must emit tea.QuitMsg")

	select {
	case got := <-reply:
		assert.False(t, got.accepted, "quitting must report the destructive action as not accepted")
	case <-time.After(time.Second):
		t.Fatal("quit reply was not sent")
	}
	assert.Nil(t, m.modal, "quitting must dismiss the modal")
	assert.True(t, m.exiting, "quit must set the exiting flag")
	assert.Contains(t, m.View(), "Goodbye", "quit must leave a visible exit message")
}

type recordingSender struct {
	progress     chan progressMsg
	confirmation chan confirmationRequestedMsg
}

func newRecordingSender() *recordingSender {
	return &recordingSender{
		progress:     make(chan progressMsg, 1),
		confirmation: make(chan confirmationRequestedMsg, 1),
	}
}

func (s *recordingSender) Send(msg tea.Msg) {
	switch msg := msg.(type) {
	case progressMsg:
		s.progress <- msg
	case confirmationRequestedMsg:
		s.confirmation <- msg
	}
}

func (s *recordingSender) waitProgress(t *testing.T) progressMsg {
	t.Helper()

	select {
	case msg := <-s.progress:
		return msg
	case <-time.After(time.Second):
		t.Fatal("progress message was not sent")
		return progressMsg{}
	}
}

func (s *recordingSender) waitConfirmation(t *testing.T) confirmationRequestedMsg {
	t.Helper()

	select {
	case msg := <-s.confirmation:
		return msg
	case <-time.After(time.Second):
		t.Fatal("confirmation request was not sent")
		return confirmationRequestedMsg{}
	}
}

func updateModel(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()

	next, cmd := m.Update(msg)
	require.Nil(t, cmd)
	return assertModel(t, next)
}

// runInit runs the model's Init command and feeds every message it produces
// back through Update, returning the settled model. It flattens a tea.Batch
// so a test sees the combined effect of the concurrently-issued startup
// commands (the runtime-lock probe and the daily-launch-check gate) without
// depending on their ordering. Commands those messages spawn in turn are NOT
// followed — a test drives those explicitly.
func runInit(t *testing.T, m model) model {
	t.Helper()

	cmd := m.Init()
	require.NotNil(t, cmd)
	for _, msg := range collectCmdMsgs(cmd) {
		next, _ := m.Update(msg)
		m = assertModel(t, next)
	}
	return m
}

// collectCmdMsgs executes cmd and returns the messages it yields, expanding a
// tea.BatchMsg into its constituent commands' messages.
func collectCmdMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		var msgs []tea.Msg
		for _, sub := range msg {
			msgs = append(msgs, collectCmdMsgs(sub)...)
		}
		return msgs
	default:
		return []tea.Msg{msg}
	}
}

func assertModel(t *testing.T, got tea.Model) model {
	t.Helper()

	m, ok := got.(model)
	require.True(t, ok, "Update returned %T, want tui.model", got)
	return m
}

func requireResult[T any](t *testing.T, ch <-chan tea.Msg) T {
	t.Helper()

	select {
	case msg := <-ch:
		result, ok := msg.(T)
		require.True(t, ok, "command returned %T", msg)
		return result
	case <-time.After(time.Second):
		t.Fatal("command did not return")
		var zero T
		return zero
	}
}

func enterKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyEnter}
}

func downKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyDown}
}

func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}
