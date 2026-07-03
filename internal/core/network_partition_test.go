package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// labelPartitionClient is a fake docker.Client that scripts the network
// managed-label probe per network in call order: each label inspect pops the
// next scripted response (stdout "true" for a wdm-owned network, anything else
// for a foreign one, or an error), and the follow-up removal of an owned
// network reports success. It lets the shared partition loop be driven directly
// without a real daemon.
type labelPartitionClient struct {
	t      *testing.T
	labels []labelResponse
	next   int
}

type labelResponse struct {
	stdout string
	err    error
}

func (c *labelPartitionClient) Run(_ context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	c.t.Helper()
	switch fmt.Sprintf("%T", inv) {
	case "docker.networkManagedLabelInvocation":
		require.Less(c.t, c.next, len(c.labels), "unexpected extra label inspect")
		resp := c.labels[c.next]
		c.next++
		return docker.CommandResult{Stdout: resp.stdout}, resp.err
	case "docker.removeNetworkInvocation":
		return docker.CommandResult{}, nil
	default:
		c.t.Fatalf("unexpected invocation %T", inv)
		return docker.CommandResult{}, nil
	}
}

// TestPartitionManagedNetworks_PartitionsRemovedRetained pins the shared
// compose-derived network-removal loop the delete and uninstall paths delegate
// to: an owned network is removed, a foreign one is retained with the exact
// "not wdm-managed" reason, and an inspect fault is retained with the daemon
// error. Ordering of both partitions follows the input names.
func TestPartitionManagedNetworks_PartitionsRemovedRetained(t *testing.T) {
	daemonErr := errors.New("docker daemon unreachable")
	client := &labelPartitionClient{
		t: t,
		labels: []labelResponse{
			{stdout: "true\n"},  // managed-net: owned → removed
			{stdout: "false\n"}, // foreign-net: unowned → retained
			{err: daemonErr},    // error-net: inspect fault → retained
		},
	}

	names := []string{"managed-net", "foreign-net", "error-net"}
	removed, retained := partitionManagedNetworks(context.Background(), client, names)

	assert.Equal(t, []string{"managed-net"}, removed)
	require.Len(t, retained, 2)
	assert.Equal(t, types.RetainedNetwork{
		Name:   "foreign-net",
		Reason: "network is not wdm-managed (missing wdm.managed=true label)",
	}, retained[0])
	assert.Equal(t, types.RetainedNetwork{
		Name:   "error-net",
		Reason: daemonErr.Error(),
	}, retained[1])
}
