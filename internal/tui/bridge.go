package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/types"
)

type progressMsg struct {
	step    string
	message string
}

type confirmationRequestedMsg struct {
	confirmation types.Confirmation
	reply        chan confirmationReply
}

type confirmationReply struct {
	accepted bool
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
	return func(step string, _ float64, msg string) {
		b.send(progressMsg{step: step, message: msg})
	}
}

func (b engineBridge) Confirm(ctx context.Context, c types.Confirmation) (bool, error) {
	reply := make(chan confirmationReply, 1)
	b.send(confirmationRequestedMsg{confirmation: c, reply: reply})

	select {
	case result := <-reply:
		return result.accepted, nil
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
