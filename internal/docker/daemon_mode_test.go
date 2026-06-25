package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// infoFakeClient returns a scripted `docker info` SecurityOptions
// projection (or error) for the InfoInvocation, mirroring the real
// execClient seam so IsRootlessDaemon is exercised through Client.Run.
type infoFakeClient struct {
	stdout string
	err    error
}

func (f infoFakeClient) Run(_ context.Context, inv Invocation) (CommandResult, error) {
	if _, ok := inv.(InfoInvocation); !ok {
		return CommandResult{}, errors.New("unexpected invocation")
	}
	if f.err != nil {
		return CommandResult{}, f.err
	}
	return CommandResult{Stdout: f.stdout}, nil
}

func TestIsRootlessDaemon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		stdout       string
		wantRootless bool
	}{
		{
			name:         "rootless marker present",
			stdout:       `["name=seccomp,profile=builtin","name=cgroupns","name=rootless"]`,
			wantRootless: true,
		},
		{
			name:         "rootful daemon has no rootless marker",
			stdout:       `["name=seccomp,profile=builtin","name=cgroupns"]`,
			wantRootless: false,
		},
		{
			name:         "empty security options is not rootless",
			stdout:       "null",
			wantRootless: false,
		},
		{
			name:         "blank output is not rootless",
			stdout:       "   \n",
			wantRootless: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rootless, err := IsRootlessDaemon(t.Context(), infoFakeClient{stdout: tt.stdout})
			require.NoError(t, err)
			require.Equal(t, tt.wantRootless, rootless)
		})
	}
}

func TestIsRootlessDaemon_NilClientFailsClosed(t *testing.T) {
	t.Parallel()

	_, err := IsRootlessDaemon(t.Context(), nil)
	require.Error(t, err)
}

func TestIsRootlessDaemon_ProbeErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("daemon unreachable")
	_, err := IsRootlessDaemon(t.Context(), infoFakeClient{err: sentinel})
	require.ErrorIs(t, err, sentinel)
}

func TestIsRootlessDaemon_MalformedOutputFailsClosed(t *testing.T) {
	t.Parallel()

	_, err := IsRootlessDaemon(t.Context(), infoFakeClient{stdout: "{not-json"})
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
}
