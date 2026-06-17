package docker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/docker"
)

type fakeClient struct {
	lastInvocation docker.Invocation
}

func (f *fakeClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	f.lastInvocation = inv

	switch inv.(type) {
	case docker.ComposeVersionInvocation:
		return docker.CommandResult{Stdout: "ok"}, nil
	default:
		return docker.CommandResult{}, errors.New("unexpected invocation")
	}
}

var _ docker.Client = (*fakeClient)(nil)

func TestClientCanBeFakedWithTypedInvocations(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{}
	got, err := fake.Run(t.Context(), docker.ComposeVersionInvocation{})
	require.NoError(t, err)
	require.Equal(t, "ok", got.Stdout)

	_, ok := fake.lastInvocation.(docker.ComposeVersionInvocation)
	require.True(t, ok, "fake client receives typed invocation, not raw argv")
}
