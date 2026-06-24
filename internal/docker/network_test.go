package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

type ensureNetworkFakeClient struct {
	runFn func(context.Context, Invocation) (CommandResult, error)
	calls []Invocation
}

func (f *ensureNetworkFakeClient) Run(ctx context.Context, inv Invocation) (CommandResult, error) {
	f.calls = append(f.calls, inv)
	if f.runFn != nil {
		return f.runFn(ctx, inv)
	}
	return CommandResult{}, nil
}

func TestEnsureNetwork_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := EnsureNetworkReport(
		t.Context(),
		nil,
		NetworkSpec{Name: "wdm_default", Internal: true},
	)
	requireUsageValidationError(t, err)
}

func TestEnsureNetwork_RejectsInvalidNetworkSpecBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tooLong := strings.Repeat("a", 64)

	tests := []struct {
		name    string
		network NetworkSpec
	}{
		{
			name:    "blank name",
			network: NetworkSpec{Name: "   ", Internal: false},
		},
		{
			name:    "leading whitespace trap",
			network: NetworkSpec{Name: " wdm_default", Internal: false},
		},
		{
			name:    "trailing whitespace trap",
			network: NetworkSpec{Name: "wdm_default ", Internal: false},
		},
		{
			name:    "uppercase rejected",
			network: NetworkSpec{Name: "Wdm_Default", Internal: false},
		},
		{
			name:    "leading digit rejected",
			network: NetworkSpec{Name: "1wdm_default", Internal: false},
		},
		{
			name:    "slash rejected",
			network: NetworkSpec{Name: "wdm/default", Internal: false},
		},
		{
			name:    "backslash rejected",
			network: NetworkSpec{Name: "wdm\\default", Internal: false},
		},
		{
			name:    "dot rejected",
			network: NetworkSpec{Name: "wdm.default", Internal: false},
		},
		{
			name:    "space rejected",
			network: NetworkSpec{Name: "wdm default", Internal: false},
		},
		{
			name:    "length over 63 rejected",
			network: NetworkSpec{Name: tooLong, Internal: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{}
			_, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestEnsureNetwork_InspectMatchSkipsCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		network      NetworkSpec
		inspectValue string
	}{
		{
			name:         "existing internal true matches desired true",
			network:      NetworkSpec{Name: "wdm_internal", Internal: true},
			inspectValue: "true\n",
		},
		{
			name:         "existing internal false matches desired false",
			network:      NetworkSpec{Name: "wdm_default", Internal: false},
			inspectValue: "false\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					inspectInv, ok := inv.(networkInspectInvocation)
					require.True(t, ok)
					require.Equal(t, tt.network.Name, inspectInv.name)
					return CommandResult{Stdout: tt.inspectValue}, nil
				},
			}

			_, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			require.NoError(t, err)
			require.Len(t, fake.calls, 1)
			_, isCreate := fake.calls[0].(networkCreateInvocation)
			require.False(t, isCreate)
		})
	}
}

func TestEnsureNetwork_InspectMismatchReturnsUsageValidationWithExactHint(t *testing.T) {
	t.Parallel()

	networkName := "wdm_default"
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			inspectInv, ok := inv.(networkInspectInvocation)
			require.True(t, ok)
			require.Equal(t, networkName, inspectInv.name)
			return CommandResult{Stdout: "true\n"}, nil
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: networkName, Internal: false},
	)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	require.Equal(
		t,
		"network "+networkName+" exists with mismatched internal flag",
		typedErr.Hint,
	)
	require.Len(t, fake.calls, 1)
}

func TestEnsureNetwork_InspectMissingCreatesNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		network       NetworkSpec
		inspectResult CommandResult
		inspectErr    error
	}{
		{
			// Classic phrasing from older Docker daemons / CLI versions.
			name:    "missing identified by classic stderr",
			network: NetworkSpec{Name: "wdm_default", Internal: false},
			inspectResult: CommandResult{
				Stderr: "Error response from daemon: No such network: wdm_default",
			},
			inspectErr: errors.New("exit status 1"),
		},
		{
			// Classic phrasing carried only on the error cause (no stderr).
			name:       "missing identified by classic error cause",
			network:    NetworkSpec{Name: "wdm_internal", Internal: true},
			inspectErr: errors.New("Error response from daemon: No such network: wdm_internal"),
		},
		{
			// Modern phrasing observed verbatim on Docker 29.5.3,
			// `docker network inspect --format {{.Internal}} <name>` for an
			// absent network. This is the form the classic indicator missed,
			// blocking install of every catalog app declaring external networks.
			name:    "missing identified by docker 29.x stderr",
			network: NetworkSpec{Name: "kuma", Internal: false},
			inspectResult: CommandResult{
				Stderr: "Error response from daemon: network kuma not found",
			},
			inspectErr: errors.New("exit status 1"),
		},
		{
			// Modern phrasing carried only on the error cause (no stderr).
			name:       "missing identified by docker 29.x error cause",
			network:    NetworkSpec{Name: "wdm_internal", Internal: true},
			inspectErr: errors.New("Error response from daemon: network wdm_internal not found"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runCalls := 0
			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					runCalls++

					switch runCalls {
					case 1:
						inspectInv, ok := inv.(networkInspectInvocation)
						require.True(t, ok)
						require.Equal(t, tt.network.Name, inspectInv.name)
						return tt.inspectResult, tt.inspectErr
					case 2:
						createInv, ok := inv.(networkCreateInvocation)
						require.True(t, ok)
						require.Equal(t, tt.network.Name, createInv.name)
						require.Equal(t, tt.network.Internal, createInv.internal)
						return CommandResult{}, nil
					default:
						t.Fatalf("unexpected run call %d", runCalls)
						return CommandResult{}, nil
					}
				},
			}

			_, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			require.NoError(t, err)
			require.Len(t, fake.calls, 2)
		})
	}
}

func TestEnsureNetwork_InspectErrorWithoutMissingIndicatorReturnsUnchanged(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("permission denied")
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			inspectInv, ok := inv.(networkInspectInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm_default", inspectInv.name)
			return CommandResult{Stderr: "permission denied"}, wantErr
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", Internal: false},
	)
	require.Same(t, wantErr, err)
	require.Len(t, fake.calls, 1)
}

func TestEnsureNetwork_UnrelatedNotFoundErrorPropagatesWithoutCreate(t *testing.T) {
	t.Parallel()

	// A bare "not found" matcher would dangerously treat a missing docker
	// binary as a missing network and take the create path. The phrase is the
	// real exec.LookPath failure text; it names "not found" but not
	// "network wdm_default not found", so it must propagate unchanged with no
	// create call.
	wantErr := errors.New(`exec: "docker": executable file not found in $PATH`)
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			inspectInv, ok := inv.(networkInspectInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm_default", inspectInv.name)
			return CommandResult{}, wantErr
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", Internal: false},
	)
	require.Same(t, wantErr, err)
	require.Len(t, fake.calls, 1)
	_, isCreate := fake.calls[0].(networkCreateInvocation)
	require.False(t, isCreate)
}

func TestEnsureNetwork_CreateErrorReturnsUnchanged(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("create failed")
	runCalls := 0
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			runCalls++
			switch runCalls {
			case 1:
				inspectInv, ok := inv.(networkInspectInvocation)
				require.True(t, ok)
				require.Equal(t, "wdm_default", inspectInv.name)
				return CommandResult{
					Stderr: "Error response from daemon: No such network: wdm_default",
				}, errors.New("exit status 1")
			case 2:
				createInv, ok := inv.(networkCreateInvocation)
				require.True(t, ok)
				require.Equal(t, "wdm_default", createInv.name)
				require.False(t, createInv.internal)
				return CommandResult{}, wantErr
			default:
				t.Fatalf("unexpected run call %d", runCalls)
				return CommandResult{}, nil
			}
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", Internal: false},
	)
	require.Same(t, wantErr, err)
	require.Len(t, fake.calls, 2)
}

func TestEnsureNetwork_InspectSuccessRequiresExactTrueOrFalseToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{
			name:   "unexpected token",
			stdout: "maybe\n",
		},
		{
			name:   "leading whitespace",
			stdout: " false\n",
		},
		{
			name:   "trailing whitespace before newline",
			stdout: "false \n",
		},
		{
			name:   "multiple trailing newlines",
			stdout: "false\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					inspectInv, ok := inv.(networkInspectInvocation)
					require.True(t, ok)
					require.Equal(t, "wdm_default", inspectInv.name)
					return CommandResult{Stdout: tt.stdout}, nil
				},
			}

			_, err := EnsureNetworkReport(
				t.Context(),
				fake,
				NetworkSpec{Name: "wdm_default", Internal: false},
			)
			requireUsageValidationError(t, err)
			require.Len(t, fake.calls, 1)
		})
	}
}

func TestEnsureNetwork_CreatePassesSubnetAndGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		network     NetworkSpec
		wantSubnet  string
		wantGateway string
	}{
		{
			name:        "subnet only",
			network:     NetworkSpec{Name: "wg", Internal: false, Subnet: "10.8.0.0/24"},
			wantSubnet:  "10.8.0.0/24",
			wantGateway: "",
		},
		{
			name:        "subnet and gateway",
			network:     NetworkSpec{Name: "wg", Internal: true, Subnet: "10.8.0.0/24", Gateway: "10.8.0.1"},
			wantSubnet:  "10.8.0.0/24",
			wantGateway: "10.8.0.1",
		},
		{
			name:        "host-bit subnet normalized to network form",
			network:     NetworkSpec{Name: "wg", Internal: false, Subnet: "10.8.0.5/24"},
			wantSubnet:  "10.8.0.0/24",
			wantGateway: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runCalls := 0
			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					runCalls++
					switch runCalls {
					case 1:
						_, ok := inv.(networkInspectInvocation)
						require.True(t, ok)
						return CommandResult{
							Stderr: "Error response from daemon: network wg not found",
						}, errors.New("exit status 1")
					case 2:
						createInv, ok := inv.(networkCreateInvocation)
						require.True(t, ok)
						require.Equal(t, tt.wantSubnet, createInv.subnet)
						require.Equal(t, tt.wantGateway, createInv.gateway)
						return CommandResult{}, nil
					default:
						t.Fatalf("unexpected run call %d", runCalls)
						return CommandResult{}, nil
					}
				},
			}

			_, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			require.NoError(t, err)
			require.Len(t, fake.calls, 2)
		})
	}
}

func TestEnsureNetwork_ExistingSubnetMatchSkipsCreate(t *testing.T) {
	t.Parallel()

	runCalls := 0
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			runCalls++
			switch runCalls {
			case 1:
				_, ok := inv.(networkInspectInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "false\n"}, nil
			case 2:
				_, ok := inv.(networkSubnetInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "10.8.0.0/24\n"}, nil
			default:
				t.Fatalf("unexpected run call %d", runCalls)
				return CommandResult{}, nil
			}
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wg", Internal: false, Subnet: "10.8.0.0/24"},
	)
	require.NoError(t, err)
	require.Len(t, fake.calls, 2)
	for _, call := range fake.calls {
		_, isCreate := call.(networkCreateInvocation)
		require.False(t, isCreate)
	}
}

// TestEnsureNetwork_CreateThreadsAppIDIntoLabels proves a spec carrying an app
// ID stamps that ID into the create invocation so the builder emits the PRD §10
// ownership labels on the newly-created network.
func TestEnsureNetwork_CreateThreadsAppIDIntoLabels(t *testing.T) {
	t.Parallel()

	runCalls := 0
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			runCalls++
			switch runCalls {
			case 1:
				return CommandResult{
					Stderr: "Error response from daemon: network wdm_default not found",
				}, errors.New("exit status 1")
			case 2:
				createInv, ok := inv.(networkCreateInvocation)
				require.True(t, ok)
				require.Equal(t, "wdm_default", createInv.name)
				require.Equal(t, "n8n", createInv.appID)
				return CommandResult{}, nil
			default:
				t.Fatalf("unexpected run call %d", runCalls)
				return CommandResult{}, nil
			}
		},
	}

	created, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", AppID: "n8n"},
	)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, fake.calls, 2)
}

// TestEnsureNetwork_InvalidAppIDRefusesBeforeDaemon proves a malformed app ID
// (here, an injection attempt) is refused by spec validation before any daemon
// call — the label value can never reach the create argv.
func TestEnsureNetwork_InvalidAppIDRefusesBeforeDaemon(t *testing.T) {
	t.Parallel()

	fake := &ensureNetworkFakeClient{
		runFn: func(context.Context, Invocation) (CommandResult, error) {
			t.Fatal("no daemon call may run when the app id is invalid")
			return CommandResult{}, nil
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", AppID: "n8n; reboot"},
	)
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	require.Empty(t, fake.calls)
}

func TestEnsureNetwork_ExistingSubnetMismatchReturnsUsageValidation(t *testing.T) {
	t.Parallel()

	runCalls := 0
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			runCalls++
			switch runCalls {
			case 1:
				_, ok := inv.(networkInspectInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "false\n"}, nil
			case 2:
				_, ok := inv.(networkSubnetInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "10.9.0.0/24\n"}, nil
			default:
				t.Fatalf("unexpected run call %d", runCalls)
				return CommandResult{}, nil
			}
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wg", Internal: false, Subnet: "10.8.0.0/24"},
	)
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	require.Equal(t, "network wg exists with mismatched subnet", typedErr.Hint)
	require.Len(t, fake.calls, 2)
}

func TestEnsureNetwork_ExistingNetworkWithNoPinnedSubnetSkipsSubnetCheck(t *testing.T) {
	t.Parallel()

	// A spec without a subnet leaves Docker's default addressing alone, so the
	// exists path never issues the subnet inspect.
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(networkInspectInvocation)
			require.True(t, ok)
			return CommandResult{Stdout: "false\n"}, nil
		},
	}

	_, err := EnsureNetworkReport(
		t.Context(),
		fake,
		NetworkSpec{Name: "wdm_default", Internal: false},
	)
	require.NoError(t, err)
	require.Len(t, fake.calls, 1)
}

func TestEnsureNetwork_RejectsInvalidAddressingBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		network NetworkSpec
	}{
		{
			name:    "malformed subnet",
			network: NetworkSpec{Name: "wg", Subnet: "10.8.0.0"},
		},
		{
			name:    "subnet octet out of range",
			network: NetworkSpec{Name: "wg", Subnet: "10.8.300.0/24"},
		},
		{
			name:    "ipv6 subnet rejected",
			network: NetworkSpec{Name: "wg", Subnet: "fd00::/64"},
		},
		{
			name:    "malformed gateway",
			network: NetworkSpec{Name: "wg", Subnet: "10.8.0.0/24", Gateway: "not-an-ip"},
		},
		{
			name:    "gateway without subnet",
			network: NetworkSpec{Name: "wg", Gateway: "10.8.0.1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{}
			_, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestEnsureNetworkReport_ReportsCreatedOnlyForNewNetworks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		network     NetworkSpec
		runFn       func(t *testing.T, runCalls int, inv Invocation) (CommandResult, error)
		wantCreated bool
		wantErr     bool
		wantCode    types.ErrorCode
		wantCalls   int
	}{
		{
			name:    "existing match reports not created",
			network: NetworkSpec{Name: "wdm_default", Internal: false},
			runFn: func(t *testing.T, _ int, inv Invocation) (CommandResult, error) {
				t.Helper()
				_, ok := inv.(networkInspectInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "false\n"}, nil
			},
			wantCreated: false,
			wantCalls:   1,
		},
		{
			name:    "missing network reports created",
			network: NetworkSpec{Name: "wdm_default", Internal: false},
			runFn: func(t *testing.T, runCalls int, inv Invocation) (CommandResult, error) {
				t.Helper()
				switch runCalls {
				case 1:
					_, ok := inv.(networkInspectInvocation)
					require.True(t, ok)
					return CommandResult{
						Stderr: "Error response from daemon: No such network: wdm_default",
					}, errors.New("exit status 1")
				case 2:
					_, ok := inv.(networkCreateInvocation)
					require.True(t, ok)
					return CommandResult{}, nil
				default:
					t.Fatalf("unexpected run call %d", runCalls)
					return CommandResult{}, nil
				}
			},
			wantCreated: true,
			wantCalls:   2,
		},
		{
			name:    "inspect mismatch errors without created",
			network: NetworkSpec{Name: "wdm_default", Internal: true},
			runFn: func(t *testing.T, _ int, inv Invocation) (CommandResult, error) {
				t.Helper()
				_, ok := inv.(networkInspectInvocation)
				require.True(t, ok)
				return CommandResult{Stdout: "false\n"}, nil
			},
			wantCreated: false,
			wantErr:     true,
			wantCode:    types.ErrCodeUsageValidation,
			wantCalls:   1,
		},
		{
			name:    "create error reports not created",
			network: NetworkSpec{Name: "wdm_default", Internal: false},
			runFn: func(t *testing.T, runCalls int, inv Invocation) (CommandResult, error) {
				t.Helper()
				switch runCalls {
				case 1:
					_, ok := inv.(networkInspectInvocation)
					require.True(t, ok)
					return CommandResult{
						Stderr: "Error response from daemon: No such network: wdm_default",
					}, errors.New("exit status 1")
				case 2:
					_, ok := inv.(networkCreateInvocation)
					require.True(t, ok)
					return CommandResult{}, errors.New("create failed")
				default:
					t.Fatalf("unexpected run call %d", runCalls)
					return CommandResult{}, nil
				}
			},
			wantCreated: false,
			wantErr:     true,
			wantCalls:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runCalls := 0
			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					runCalls++
					return tt.runFn(t, runCalls, inv)
				},
			}

			created, err := EnsureNetworkReport(t.Context(), fake, tt.network)
			require.Equal(t, tt.wantCreated, created)
			require.Len(t, fake.calls, tt.wantCalls)
			if tt.wantErr {
				require.Error(t, err)
				require.False(t, created, "created must be false on every error path")
				if tt.wantCode != 0 {
					var typedErr *types.Error
					require.ErrorAs(t, err, &typedErr)
					require.Equal(t, tt.wantCode, typedErr.Code)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestEnsureNetworkReport_RejectsNilClient(t *testing.T) {
	t.Parallel()

	created, err := EnsureNetworkReport(
		t.Context(),
		nil,
		NetworkSpec{Name: "wdm_default"},
	)
	require.False(t, created)
	requireUsageValidationError(t, err)
}

func TestRemoveNetwork_RejectsNilClient(t *testing.T) {
	t.Parallel()

	err := RemoveNetwork(t.Context(), nil, "wdm_default")
	requireUsageValidationError(t, err)
}

func TestRemoveNetwork_RejectsInvalidNameBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		networkName string
	}{
		{name: "blank name", networkName: "   "},
		{name: "uppercase name", networkName: "Wdm_Default"},
		{name: "slash injection", networkName: "wdm/default"},
		{name: "command chained name", networkName: "wdm_default && reboot"},
		{name: "leading dash flag", networkName: "--force"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{}
			err := RemoveNetwork(t.Context(), fake, tt.networkName)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestRemoveNetwork_RemovesNamedNetwork(t *testing.T) {
	t.Parallel()

	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			removeInv, ok := inv.(removeNetworkInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm_default", removeInv.name)
			return CommandResult{}, nil
		},
	}

	err := RemoveNetwork(t.Context(), fake, "wdm_default")
	require.NoError(t, err)
	require.Len(t, fake.calls, 1)
}

func TestRemoveNetwork_PropagatesCommandError(t *testing.T) {
	t.Parallel()

	boom := errors.New("docker failed")
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(removeNetworkInvocation)
			require.True(t, ok)
			return CommandResult{}, boom
		},
	}

	err := RemoveNetwork(t.Context(), fake, "wdm_default")
	require.Same(t, boom, err)
	require.Len(t, fake.calls, 1)
}

func TestRemoveNetworkIfPresent_RejectsNilClient(t *testing.T) {
	t.Parallel()

	err := RemoveNetworkIfPresent(t.Context(), nil, "wdm_default")
	requireUsageValidationError(t, err)
}

func TestRemoveNetworkIfPresent_RejectsInvalidNameBeforeRunningClient(t *testing.T) {
	t.Parallel()

	fake := &ensureNetworkFakeClient{}
	err := RemoveNetworkIfPresent(t.Context(), fake, "Wdm/Default")
	requireUsageValidationError(t, err)
	require.Empty(t, fake.calls)
}

func TestRemoveNetworkIfPresent_RemovesNamedNetwork(t *testing.T) {
	t.Parallel()

	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			removeInv, ok := inv.(removeNetworkInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm_default", removeInv.name)
			return CommandResult{}, nil
		},
	}

	err := RemoveNetworkIfPresent(t.Context(), fake, "wdm_default")
	require.NoError(t, err)
	require.Len(t, fake.calls, 1)
}

// A not-found result is tolerated as success (idempotent) on both the classic
// and modern daemon phrasings, on stderr and on the error string.
func TestRemoveNetworkIfPresent_ToleratesMissingNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result CommandResult
		err    error
	}{
		{
			name:   "classic stderr phrasing",
			result: CommandResult{Stderr: "Error: No such network: wdm_default"},
			err:    errors.New("exit status 1"),
		},
		{
			name:   "modern stderr phrasing",
			result: CommandResult{Stderr: "Error response from daemon: network wdm_default not found"},
			err:    errors.New("exit status 1"),
		},
		{
			name:   "phrasing on error string only",
			result: CommandResult{},
			err:    errors.New("no such network: wdm_default"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, _ Invocation) (CommandResult, error) {
					return tt.result, tt.err
				},
			}

			err := RemoveNetworkIfPresent(t.Context(), fake, "wdm_default")
			require.NoError(t, err)
			require.Len(t, fake.calls, 1)
		})
	}
}

// A removal failure that is NOT a missing-network condition propagates so the
// caller can record it.
func TestRemoveNetworkIfPresent_PropagatesOtherFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("network wdm_default has active endpoints")
	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, _ Invocation) (CommandResult, error) {
			return CommandResult{Stderr: "Error response from daemon: error while removing network: network wdm_default id ... has active endpoints"}, boom
		},
	}

	err := RemoveNetworkIfPresent(t.Context(), fake, "wdm_default")
	require.Same(t, boom, err)
	require.Len(t, fake.calls, 1)
}

func TestRun_NetworkInvocationsBuildExactArgv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inv     Invocation
		wantArg []string
	}{
		{
			name: "inspect",
			inv: networkInspectInvocation{
				name: "wdm_default",
			},
			wantArg: []string{"network", "inspect", "--format", "{{.Internal}}", "wdm_default"},
		},
		{
			name: "create non-internal",
			inv: networkCreateInvocation{
				name: "wdm_default",
			},
			wantArg: []string{"network", "create", "wdm_default"},
		},
		{
			name: "create internal",
			inv: networkCreateInvocation{
				name:     "wdm_internal",
				internal: true,
			},
			wantArg: []string{"network", "create", "--internal", "wdm_internal"},
		},
		{
			name: "subnet inspect",
			inv: networkSubnetInvocation{
				name: "wg",
			},
			wantArg: []string{"network", "inspect", "--format", networkSubnetInspectFormat, "wg"},
		},
		{
			name: "create with subnet",
			inv: networkCreateInvocation{
				name:   "wg",
				subnet: "10.8.0.0/24",
			},
			wantArg: []string{"network", "create", "--subnet", "10.8.0.0/24", "wg"},
		},
		{
			name: "create internal with subnet and gateway",
			inv: networkCreateInvocation{
				name:     "wg",
				internal: true,
				subnet:   "10.8.0.0/24",
				gateway:  "10.8.0.1",
			},
			wantArg: []string{"network", "create", "--internal", "--subnet", "10.8.0.0/24", "--gateway", "10.8.0.1", "wg"},
		},
		{
			name: "create with ownership labels",
			inv: networkCreateInvocation{
				name:  "wdm_default",
				appID: "n8n",
			},
			wantArg: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"--label", "wdm.app=n8n",
				"wdm_default",
			},
		},
		{
			name: "create internal with subnet, gateway, and labels",
			inv: networkCreateInvocation{
				name:     "wg",
				internal: true,
				subnet:   "10.8.0.0/24",
				gateway:  "10.8.0.1",
				appID:    "wireguard",
			},
			wantArg: []string{
				"network", "create",
				"--internal",
				"--subnet", "10.8.0.0/24",
				"--gateway", "10.8.0.1",
				"--label", "wdm.managed=true",
				"--label", "wdm.app=wireguard",
				"wg",
			},
		},
		{
			name: "remove",
			inv: removeNetworkInvocation{
				name: "wdm_default",
			},
			wantArg: []string{"network", "rm", "wdm_default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			invoked := false
			execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
				invoked = true
				require.Equal(t, tt.wantArg, cmd.argv)
				return CommandResult{}, nil
			}

			client, err := New(WithCommandExecutor(execFn))
			require.NoError(t, err)

			_, err = client.Run(t.Context(), tt.inv)
			require.NoError(t, err)
			require.True(t, invoked)
		})
	}
}

func TestRun_DefaultExecutorNetworkInvocationsUseExpectedArgv(t *testing.T) {
	fakeDocker := `#!/bin/sh
printf 'argv='
for arg in "$@"; do
  printf '[%s]' "$arg"
done
printf '\n'
`
	useFakeDocker(t, fakeDocker)

	client, err := New()
	require.NoError(t, err)

	inspectRes, err := client.Run(
		t.Context(),
		networkInspectInvocation{name: "wdm_default"},
	)
	require.NoError(t, err)
	require.Contains(
		t,
		inspectRes.Stdout,
		"argv=[network][inspect][--format][{{.Internal}}][wdm_default]",
	)

	createRes, err := client.Run(
		t.Context(),
		networkCreateInvocation{name: "wdm_default"},
	)
	require.NoError(t, err)
	require.Contains(t, createRes.Stdout, "argv=[network][create][wdm_default]")
	require.NotContains(t, createRes.Stdout, "[--internal]")

	createInternalRes, err := client.Run(
		t.Context(),
		networkCreateInvocation{name: "wdm_internal", internal: true},
	)
	require.NoError(t, err)
	require.Contains(
		t,
		createInternalRes.Stdout,
		"argv=[network][create][--internal][wdm_internal]",
	)
}

func TestValidateCommandSpec_AllowsNetworkShapes(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "inspect", "--format", "{{.Internal}}", "wdm_default"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "create", "wdm_default"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "create", "--internal", "wdm_internal"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "inspect", "--format", networkSubnetInspectFormat, "wg"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "create", "--subnet", "10.8.0.0/24", "wg"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "create", "--internal", "--subnet", "10.8.0.0/24", "--gateway", "10.8.0.1", "wg"},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"network", "create",
			"--label", "wdm.managed=true",
			"--label", "wdm.app=n8n",
			"wdm_default",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"network", "create",
			"--internal",
			"--subnet", "10.8.0.0/24",
			"--gateway", "10.8.0.1",
			"--label", "wdm.managed=true",
			"--label", "wdm.app=wireguard",
			"wg",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"network", "rm", "wdm_default"},
	}))
	// The managed-network sweep list: the only allowlisted `network ls` shape.
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"network", "ls",
			"--filter", "label=wdm.managed=true",
			"--format", "{{.Name}}",
		},
	}))
}

func TestValidateCommandSpec_RejectsUnsafeNetworkShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "inspect wrong format literal",
			argv: []string{"network", "inspect", "--format", "{{json .}}", "wdm_default"},
		},
		{
			name: "inspect with extra flag",
			argv: []string{"network", "inspect", "--format", "{{.Internal}}", "wdm_default", "--verbose"},
		},
		{
			name: "inspect blank name",
			argv: []string{"network", "inspect", "--format", "{{.Internal}}", ""},
		},
		{
			name: "inspect uppercase name",
			argv: []string{"network", "inspect", "--format", "{{.Internal}}", "Wdm_Default"},
		},
		{
			name: "create with extra flag",
			argv: []string{"network", "create", "--driver", "bridge", "wdm_default"},
		},
		{
			name: "create with misplaced internal flag",
			argv: []string{"network", "create", "wdm_default", "--internal"},
		},
		{
			name: "create with malformed subnet value",
			argv: []string{"network", "create", "--subnet", "10.8.0.0", "wg"},
		},
		{
			name: "create with out-of-range subnet octet",
			argv: []string{"network", "create", "--subnet", "10.8.300.0/24", "wg"},
		},
		{
			name: "create with ipv6 subnet",
			argv: []string{"network", "create", "--subnet", "fd00::/64", "wg"},
		},
		{
			name: "create with malformed gateway value",
			argv: []string{"network", "create", "--subnet", "10.8.0.0/24", "--gateway", "nope", "wg"},
		},
		{
			name: "create with gateway before subnet",
			argv: []string{"network", "create", "--gateway", "10.8.0.1", "--subnet", "10.8.0.0/24", "wg"},
		},
		{
			name: "create with gateway but no subnet",
			argv: []string{"network", "create", "--gateway", "10.0.0.1", "wdm_net"},
		},
		{
			name: "create with subnet flag but no value and no name",
			argv: []string{"network", "create", "--subnet"},
		},
		{
			name: "create with unknown driver flag",
			argv: []string{"network", "create", "--subnet", "10.8.0.0/24", "--driver", "bridge", "wg"},
		},
		{
			name: "inspect with unknown format literal",
			argv: []string{"network", "inspect", "--format", "{{json .IPAM}}", "wg"},
		},
		{
			name: "create blank name",
			argv: []string{"network", "create", ""},
		},
		{
			name: "create uppercase name",
			argv: []string{"network", "create", "Wdm_Default"},
		},
		{
			name: "create leading whitespace trap",
			argv: []string{"network", "create", " wdm_default"},
		},
		{
			name: "create trailing whitespace trap",
			argv: []string{"network", "create", "wdm_default "},
		},
		{
			name: "create invalid slash",
			argv: []string{"network", "create", "wdm/default"},
		},
		{
			name: "unknown network command",
			argv: []string{"network", "ls"},
		},
		{
			name: "ls with different filter label",
			argv: []string{"network", "ls", "--filter", "label=wdm.app=n8n", "--format", "{{.Name}}"},
		},
		{
			name: "ls with different format",
			argv: []string{"network", "ls", "--filter", "label=wdm.managed=true", "--format", "{{.ID}}"},
		},
		{
			name: "ls without format suffix",
			argv: []string{"network", "ls", "--filter", "label=wdm.managed=true"},
		},
		{
			name: "ls managed shape with trailing flag",
			argv: []string{"network", "ls", "--filter", "label=wdm.managed=true", "--format", "{{.Name}}", "--quiet"},
		},
		{
			name: "ls with quiet flag instead of filter",
			argv: []string{"network", "ls", "--quiet"},
		},
		{
			name: "network remove with extra arg",
			argv: []string{"network", "rm", "wdm_default", "wdm_other"},
		},
		{
			name: "network remove with force flag",
			argv: []string{"network", "rm", "--force", "wdm_default"},
		},
		{
			name: "network remove uppercase name",
			argv: []string{"network", "rm", "Wdm_Default"},
		},
		{
			name: "network remove injected name",
			argv: []string{"network", "rm", "wdm_default && reboot"},
		},
		{
			name: "network prune forbidden",
			argv: []string{"network", "prune", "-f"},
		},
		{
			name: "shell invocation forbidden",
			argv: []string{"sh", "-c", "docker network create wdm_default"},
		},
		{
			name: "compose v1 style forbidden",
			argv: []string{"docker-compose", "network", "create", "wdm_default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCommandSpec(commandSpec{argv: tt.argv})
			requireUsageValidationError(t, err)
		})
	}
}

func TestBuildManagedNetworkListCommand_BuildsExactArgv(t *testing.T) {
	t.Parallel()

	cmd, err := buildManagedNetworkListCommand()
	require.NoError(t, err)
	require.Equal(t, []string{
		"network", "ls",
		"--filter", "label=wdm.managed=true",
		"--format", "{{.Name}}",
	}, cmd.argv)
}

func TestListManagedNetworks_RejectsNilClient(t *testing.T) {
	t.Parallel()

	names, err := ListManagedNetworks(t.Context(), nil)
	requireUsageValidationError(t, err)
	require.Nil(t, names)
}

func TestListManagedNetworks_RunsExactInvocationAndParsesNames(t *testing.T) {
	t.Parallel()

	fake := &ensureNetworkFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(managedNetworkListInvocation)
			require.True(t, ok)
			return CommandResult{Stdout: "wdm_default\nwdm_proxy\n"}, nil
		},
	}

	names, err := ListManagedNetworks(t.Context(), fake)
	require.NoError(t, err)
	require.Equal(t, []string{"wdm_default", "wdm_proxy"}, names)
	require.Len(t, fake.calls, 1)
}

func TestListManagedNetworks_PropagatesCommandError(t *testing.T) {
	t.Parallel()

	boom := errors.New("docker ls failed")
	fake := &ensureNetworkFakeClient{
		runFn: func(context.Context, Invocation) (CommandResult, error) {
			return CommandResult{}, boom
		},
	}

	names, err := ListManagedNetworks(t.Context(), fake)
	require.Same(t, boom, err)
	require.Nil(t, names)
}

func TestParseManagedNetworkNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stdout   string
		expected []string
	}{
		{
			name:     "empty output",
			stdout:   "",
			expected: []string{},
		},
		{
			name:     "single name with trailing newline",
			stdout:   "wdm_default\n",
			expected: []string{"wdm_default"},
		},
		{
			name:     "multiple names",
			stdout:   "wdm_default\nwdm_proxy\nwdm_orphan\n",
			expected: []string{"wdm_default", "wdm_proxy", "wdm_orphan"},
		},
		{
			name:     "no trailing newline",
			stdout:   "wdm_default\nwdm_proxy",
			expected: []string{"wdm_default", "wdm_proxy"},
		},
		{
			name:     "blank lines and surrounding whitespace dropped",
			stdout:   "  wdm_default \n\n\twdm_proxy\t\n  \n",
			expected: []string{"wdm_default", "wdm_proxy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.expected, parseManagedNetworkNames(tt.stdout))
		})
	}
}

// RemoveNetworkIfManaged must remove only networks carrying wdm.managed=true;
// an unlabeled (foreign) network is skipped without a `network rm` ever
// reaching the daemon, and an absent network is an idempotent non-owned
// success (no removal, no skip, no error) so a concurrent teardown cannot
// surface a spurious failure.
func TestRemoveNetworkIfManaged_GatesOnOwnershipLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		labelValue  string
		labelStderr string
		labelErr    error
		wantRemoved bool
		wantSkipped bool
		wantRm      bool
	}{
		{name: "managed network removed", labelValue: "true", wantRemoved: true, wantSkipped: false, wantRm: true},
		{name: "unlabeled network skipped", labelValue: "", wantRemoved: false, wantSkipped: true, wantRm: false},
		{name: "foreign label value skipped", labelValue: "false", wantRemoved: false, wantSkipped: true, wantRm: false},
		{name: "absent network is idempotent non-owned success", labelStderr: "Error response from daemon: network wdm_default not found", labelErr: errors.New("exit status 1"), wantRemoved: false, wantSkipped: false, wantRm: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rmCalled := false
			fake := &ensureNetworkFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					switch inv.(type) {
					case networkManagedLabelInvocation:
						if tt.labelErr != nil {
							return CommandResult{Stderr: tt.labelStderr}, tt.labelErr
						}
						return CommandResult{Stdout: tt.labelValue + "\n"}, nil
					case removeNetworkInvocation:
						rmCalled = true
						return CommandResult{}, nil
					default:
						t.Fatalf("unexpected invocation %T", inv)
						return CommandResult{}, nil
					}
				},
			}

			removed, skipped, err := RemoveNetworkIfManaged(t.Context(), fake, "wdm_default")
			require.NoError(t, err)
			require.Equal(t, tt.wantRemoved, removed)
			require.Equal(t, tt.wantSkipped, skipped)
			require.Equal(t, tt.wantRm, rmCalled)
		})
	}
}
