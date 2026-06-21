package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
)

func TestRunWithOptions_BareNonTTYPrintsUsageAndDoesNotConstructEngine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	var engineCalls int
	err := runWithOptions(runOptions{
		stdout:      &out,
		stdinIsTTY:  func() bool { return false },
		stdoutIsTTY: func() bool { return false },
		newEngine: func() (engine.Engine, error) {
			engineCalls++
			return nil, errors.New("engine should not be constructed")
		},
		runTUI: func(context.Context, engine.Engine) error {
			t.Fatal("TUI should not run for non-TTY root invocation")
			return nil
		},
	})

	require.NoError(t, err)
	assert.Zero(t, engineCalls)
	assert.Contains(t, out.String(), "Usage:")
}

func TestRunWithOptions_BareTTYRunsTUIAfterConstructingEngine(t *testing.T) {
	t.Parallel()

	var engineCalls int
	var tuiCalls int
	err := runWithOptions(runOptions{
		stdinIsTTY:  func() bool { return true },
		stdoutIsTTY: func() bool { return true },
		newEngine: func() (engine.Engine, error) {
			engineCalls++
			return nil, nil
		},
		runTUI: func(ctx context.Context, _ engine.Engine) error {
			require.NotNil(t, ctx)
			tuiCalls++
			return nil
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, engineCalls)
	assert.Equal(t, 1, tuiCalls)
}

func TestRunWithOptions_BareTTYShowsStartupErrorWhenEngineConstructionFails(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("config.toml is invalid")
	var startupCalls int
	err := runWithOptions(runOptions{
		stdinIsTTY:  func() bool { return true },
		stdoutIsTTY: func() bool { return true },
		newEngine: func() (engine.Engine, error) {
			return nil, wantErr
		},
		runTUI: func(context.Context, engine.Engine) error {
			t.Fatal("TUI should not run when engine construction fails")
			return nil
		},
		runStartupError: func(_ context.Context, got error) error {
			startupCalls++
			require.ErrorIs(t, got, wantErr)
			return nil
		},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, startupCalls)
}

func TestRunWithOptions_HelpAndVersionBypassEngineEvenWhenTTY(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		args    []string
		wantOut string
	}{
		{name: "help", args: []string{"--help"}, wantOut: "Usage:"},
		{name: "version", args: []string{"--version"}, wantOut: version + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := runWithOptions(runOptions{
				args:        tt.args,
				stdout:      &out,
				stdinIsTTY:  func() bool { return true },
				stdoutIsTTY: func() bool { return true },
				newEngine: func() (engine.Engine, error) {
					return nil, errors.New("engine should not be constructed")
				},
				runTUI: func(context.Context, engine.Engine) error {
					t.Fatal("TUI should not run for help or version")
					return nil
				},
			})

			require.NoError(t, err)
			assert.Contains(t, out.String(), tt.wantOut)
		})
	}
}

func TestDebugRequested(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		args     []string
		expected bool
	}{
		{name: "bare flag", args: []string{"--debug"}, expected: true},
		{name: "explicit true", args: []string{"--debug=true"}, expected: true},
		{name: "explicit one", args: []string{"--debug=1"}, expected: true},
		{name: "no args", args: []string{}, expected: false},
		{name: "explicit false", args: []string{"--debug=false"}, expected: false},
		{name: "explicit zero", args: []string{"--debug=0"}, expected: false},
		{name: "typo", args: []string{"--debugx"}, expected: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, debugRequested(tt.args))
		})
	}
}

func TestRunWithOptions_RefusalRunsBeforeTTYAndEngine(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("refused")
	err := runWithOptions(runOptions{
		refuse:      func() error { return wantErr },
		stdinIsTTY:  func() bool { return true },
		stdoutIsTTY: func() bool { return true },
		newEngine: func() (engine.Engine, error) {
			t.Fatal("engine should not be constructed after refusal")
			return nil, nil
		},
		runTUI: func(context.Context, engine.Engine) error {
			t.Fatal("TUI should not run after refusal")
			return nil
		},
	})

	require.ErrorIs(t, err, wantErr)
}
