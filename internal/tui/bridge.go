package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

type progressMsg struct {
	step    string
	pct     float64
	message string
}

type logLineMsg struct {
	line types.LogLine
}

type confirmationRequestedMsg struct {
	confirmation types.Confirmation
	reply        chan confirmationReply
}

type confirmationReply struct {
	accepted bool
	err      error
}

type engineBridge struct {
	send func(tea.Msg)
}

func newEngineBridge(send func(tea.Msg)) engineBridge {
	if send == nil {
		send = func(tea.Msg) {}
	}
	return engineBridge{send: send}
}

func (b engineBridge) progressFn() types.ProgressFn {
	return func(step string, pct float64, msg string) {
		b.send(progressMsg{step: step, pct: pct, message: msg})
	}
}

func (b engineBridge) logLineFn() types.LogLineFn {
	return func(line types.LogLine) {
		b.send(logLineMsg{line: line})
	}
}

func (b engineBridge) Confirm(ctx context.Context, c types.Confirmation) (bool, error) {
	reply := make(chan confirmationReply, 1)
	b.send(confirmationRequestedMsg{confirmation: c, reply: reply})

	select {
	case result := <-reply:
		return result.accepted, result.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func engineCommand(
	ctx context.Context,
	bridge engineBridge,
	op func(context.Context, types.ProgressFn, types.Confirmer) tea.Msg,
) tea.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}

	return func() tea.Msg {
		return op(ctx, bridge.progressFn(), bridge)
	}
}
