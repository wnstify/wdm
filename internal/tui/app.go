package tui

import (
	"context"
	"errors"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wnstify/wdm/pkg/engine"
)

// ErrNilEngine reports an attempt to construct a TUI without an engine facade.
var ErrNilEngine = errors.New("tui: nil engine")

// App owns one Bubble Tea program model and the engine backing it.
type App struct {
	eng engine.Engine

	closeOnce sync.Once
	closeErr  error
}

// New constructs a TUI application around eng.
func New(eng engine.Engine) (*App, error) {
	if eng == nil {
		return nil, ErrNilEngine
	}

	return &App{eng: eng}, nil
}

// Run constructs and runs an [App], closing eng when the program exits.
func Run(ctx context.Context, eng engine.Engine, opts ...tea.ProgramOption) error {
	app, err := New(eng)
	if err != nil {
		return err
	}

	return app.Run(ctx, opts...)
}

// Run starts the Bubble Tea program and closes the engine on every return path.
func (a *App) Run(ctx context.Context, opts ...tea.ProgramOption) (err error) {
	defer func() {
		if closeErr := a.Close(); err == nil {
			err = closeErr
		}
	}()

	if ctx == nil {
		ctx = context.Background()
	}

	programOpts := make([]tea.ProgramOption, 0, len(opts)+1)
	programOpts = append(programOpts, tea.WithContext(ctx))
	programOpts = append(programOpts, opts...)

	var program *tea.Program
	send := func(msg tea.Msg) {
		if program != nil {
			program.Send(msg)
		}
	}
	program = tea.NewProgram(newModelWithContextSender(ctx, a.eng, send), programOpts...)
	_, err = program.Run()
	return err
}

// Close releases the owned engine once and returns the first close error.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		a.closeErr = a.eng.Close()
	})
	return a.closeErr
}
