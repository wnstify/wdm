package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

type inspectFakeClient struct {
	runFn func(context.Context, Invocation) (CommandResult, error)
	calls []Invocation
}

func (f *inspectFakeClient) Run(ctx context.Context, inv Invocation) (CommandResult, error) {
	f.calls = append(f.calls, inv)
	if f.runFn != nil {
		return f.runFn(ctx, inv)
	}
	return CommandResult{}, nil
}

func TestInspectProjectContainers_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := InspectProjectContainers(t.Context(), nil, "wdm-n8n")
	requireUsageValidationError(t, err)
}

func TestInspectProjectContainers_RejectsInvalidProjectBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		project string
	}{
		{name: "blank", project: "   "},
		{name: "leading whitespace", project: " wdm-n8n"},
		{name: "trailing whitespace", project: "wdm-n8n "},
		{name: "uppercase", project: "WDM-n8n"},
		{name: "slash", project: "wdm/n8n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{}
			_, err := InspectProjectContainers(t.Context(), fake, tt.project)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestInspectProjectContainers_ListsAndInspectsContainers(t *testing.T) {
	t.Parallel()

	const projectName = "wdm-n8n"
	inspectOutputs := map[string]string{
		"abc123def456": `"\/n8n-app"
{"com.docker.compose.project":"wdm-n8n","com.docker.compose.service":"app","wdm.app":"n8n","wdm.managed":"true"}
"running"
true
false
0
"healthy"
{"5678/tcp":[{"HostIp":"127.0.0.1","HostPort":"5678"}],"443/tcp":null}
`,
		"fed456abc123": `"\/n8n-worker"
{"com.docker.compose.project":"wdm-n8n","com.docker.compose.service":"worker","wdm.app":"n8n","wdm.managed":"true"}
"exited"
false
false
1
""
{}
`,
	}
	call := 0
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			call++
			switch call {
			case 1:
				listInv, ok := inv.(projectContainerListInvocation)
				require.True(t, ok)
				require.Equal(t, projectName, listInv.projectName)
				return CommandResult{Stdout: "abc123def456\nfed456abc123\n"}, nil
			case 2, 3:
				inspectInv, ok := inv.(containerInspectInvocation)
				require.True(t, ok)
				out, exists := inspectOutputs[inspectInv.id]
				require.True(t, exists, "unexpected inspect id %q", inspectInv.id)
				return CommandResult{Stdout: out}, nil
			default:
				t.Fatalf("unexpected call %d", call)
				return CommandResult{}, nil
			}
		},
	}

	got, err := InspectProjectContainers(t.Context(), fake, projectName)
	require.NoError(t, err)
	require.Equal(t, []ContainerInfo{
		{
			ID:      "abc123def456",
			Name:    "n8n-app",
			Service: "app",
			Labels: map[string]string{
				"com.docker.compose.project": "wdm-n8n",
				"com.docker.compose.service": "app",
				"wdm.app":                    "n8n",
				"wdm.managed":                "true",
			},
			State: ContainerState{
				Status:     "running",
				Running:    true,
				Restarting: false,
				ExitCode:   0,
				Health:     "healthy",
			},
			Ports: []PublishedPort{
				{
					HostIP:        "127.0.0.1",
					HostPort:      5678,
					ContainerPort: 5678,
					Protocol:      "tcp",
				},
			},
		},
		{
			ID:      "fed456abc123",
			Name:    "n8n-worker",
			Service: "worker",
			Labels: map[string]string{
				"com.docker.compose.project": "wdm-n8n",
				"com.docker.compose.service": "worker",
				"wdm.app":                    "n8n",
				"wdm.managed":                "true",
			},
			State: ContainerState{
				Status:     "exited",
				Running:    false,
				Restarting: false,
				ExitCode:   1,
			},
		},
	}, got)
	require.Len(t, fake.calls, 3)
}

func TestInspectProjectContainers_EmptyListReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(projectContainerListInvocation)
			require.True(t, ok)
			return CommandResult{}, nil
		},
	}

	got, err := InspectProjectContainers(t.Context(), fake, "wdm-n8n")
	require.NoError(t, err)
	require.Empty(t, got)
	require.Len(t, fake.calls, 1)
}

func TestInspectProjectContainers_RejectsMalformedContainerListOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "padded id", stdout: " abc123def456\n"},
		{name: "blank line", stdout: "abc123def456\n\n"},
		{name: "non hex", stdout: "not-container\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					_, ok := inv.(projectContainerListInvocation)
					require.True(t, ok)
					return CommandResult{Stdout: tt.stdout}, nil
				},
			}

			_, err := InspectProjectContainers(t.Context(), fake, "wdm-n8n")
			requireUsageValidationError(t, err)
		})
	}
}

func TestInspectProjectContainers_RejectsMalformedContainerInspectOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{
			name: "too few fields",
			stdout: `"\/n8n-app"
{}
`,
		},
		{
			name: "bad port number",
			stdout: `"\/n8n-app"
{}
"running"
true
false
0
""
{"5678/tcp":[{"HostIp":"127.0.0.1","HostPort":"not-a-port"}]}
`,
		},
		{
			name: "bad container port key",
			stdout: `"\/n8n-app"
{}
"running"
true
false
0
""
{"not-a-port/tcp":[{"HostIp":"127.0.0.1","HostPort":"5678"}]}
`,
		},
		{
			name: "padded labels field",
			stdout: `"\/n8n-app"
 {}
"running"
true
false
0
""
{}
`,
		},
		{
			name: "padded status field",
			stdout: "\"\\/n8n-app\"\n" +
				"{}\n" +
				"\"running\" \n" +
				"true\n" +
				"false\n" +
				"0\n" +
				"\"\"\n" +
				"{}\n",
		},
		{
			name: "padded bool field",
			stdout: `"\/n8n-app"
{}
"running"
 true
false
0
""
{}
`,
		},
		{
			name: "padded ports field",
			stdout: `"\/n8n-app"
{}
"running"
true
false
0
""
 {}
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call := 0
			fake := &inspectFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					call++
					switch call {
					case 1:
						_, ok := inv.(projectContainerListInvocation)
						require.True(t, ok)
						return CommandResult{Stdout: "abc123def456\n"}, nil
					case 2:
						_, ok := inv.(containerInspectInvocation)
						require.True(t, ok)
						return CommandResult{Stdout: tt.stdout}, nil
					default:
						t.Fatalf("unexpected call %d", call)
						return CommandResult{}, nil
					}
				},
			}

			_, err := InspectProjectContainers(t.Context(), fake, "wdm-n8n")
			requireUsageValidationError(t, err)
		})
	}
}

func TestInspectProjectContainers_PropagatesRequiredCommandErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("docker failed")
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(projectContainerListInvocation)
			require.True(t, ok)
			return CommandResult{}, boom
		},
	}

	_, err := InspectProjectContainers(t.Context(), fake, "wdm-n8n")
	require.ErrorIs(t, err, boom)
}

func TestInspectImageDigest_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := InspectImageDigest(t.Context(), nil, "n8nio/n8n:1.0.0")
	requireUsageValidationError(t, err)
}

func TestInspectImageDigest_RejectsInvalidImageRefBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		image string
	}{
		{name: "blank", image: "   "},
		{name: "leading dash", image: "-bad:1.0.0"},
		{name: "space", image: "n8nio/n8n:1.0.0 latest"},
		{name: "shell metacharacter", image: "n8nio/n8n:1.0.0;echo"},
		{name: "control character", image: "n8nio/n8n:1.0.0\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{}
			_, err := InspectImageDigest(t.Context(), fake, tt.image)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestInspectImageDigest_ReturnsFirstSha256Digest(t *testing.T) {
	t.Parallel()

	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			inspectInv, ok := inv.(imageDigestInspectInvocation)
			require.True(t, ok)
			require.Equal(t, "n8nio/n8n:1.0.0", inspectInv.imageRef)
			return CommandResult{
				Stdout: "n8nio/n8n@" + digest + ",mirror/n8n@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff\n",
			}, nil
		},
	}

	got, err := InspectImageDigest(t.Context(), fake, "n8nio/n8n:1.0.0")
	require.NoError(t, err)
	require.Equal(t, digest, got)
}

func TestInspectImageDigest_ReturnsBareDigest(t *testing.T) {
	t.Parallel()

	digest := "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(imageDigestInspectInvocation)
			require.True(t, ok)
			return CommandResult{Stdout: digest + "\n"}, nil
		},
	}

	got, err := InspectImageDigest(t.Context(), fake, "redis:7.4")
	require.NoError(t, err)
	require.Equal(t, digest, got)
}

func TestInspectImageDigest_AbsenceOrInspectFailureIsNotFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
		err    error
	}{
		{name: "empty output"},
		{name: "no digest", stdout: "<no value>\n"},
		{name: "docker failure", err: errors.New("registry unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					_, ok := inv.(imageDigestInspectInvocation)
					require.True(t, ok)
					return CommandResult{Stdout: tt.stdout}, tt.err
				},
			}

			got, err := InspectImageDigest(t.Context(), fake, "redis:7.4")
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
}

func TestInspectImageDigest_RejectsMalformedDigestOutput(t *testing.T) {
	t.Parallel()

	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name   string
		stdout string
	}{
		{name: "leading whitespace", stdout: " n8nio/n8n@" + digest + "\n"},
		{name: "trailing whitespace", stdout: "n8nio/n8n@" + digest + " \n"},
		{name: "padded comma entry", stdout: "n8nio/n8n@" + digest + ", mirror/n8n@" + digest + "\n"},
		{name: "embedded newline", stdout: "junk\nn8nio/n8n@" + digest + "\n"},
		{name: "multiple trailing newlines", stdout: "n8nio/n8n@" + digest + "\n\n"},
		{name: "carriage return in body", stdout: "n8nio/n8n@" + digest + "\rjunk\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					_, ok := inv.(imageDigestInspectInvocation)
					require.True(t, ok)
					return CommandResult{Stdout: tt.stdout}, nil
				},
			}

			_, err := InspectImageDigest(t.Context(), fake, "redis:7.4")
			requireUsageValidationError(t, err)
		})
	}
}

func TestInspectImageDigest_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelErr := types.WrapError(
		types.ErrCodeUserCanceled,
		"docker command canceled",
		"",
		context.Canceled,
	)
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(imageDigestInspectInvocation)
			require.True(t, ok)
			return CommandResult{}, cancelErr
		},
	}

	_, err := InspectImageDigest(ctx, fake, "redis:7.4")
	require.ErrorIs(t, err, context.Canceled)
}

func TestListProjectNamedVolumes_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := ListProjectNamedVolumes(t.Context(), nil, "wdm-n8n")
	requireUsageValidationError(t, err)
}

func TestListProjectNamedVolumes_ReturnsSortedUniqueNames(t *testing.T) {
	t.Parallel()

	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			listInv, ok := inv.(projectVolumeListInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm-n8n", listInv.projectName)
			return CommandResult{
				Stdout: "wdm-n8n_postgres-data\nwdm-n8n_redis-data\nwdm-n8n_postgres-data\n",
			}, nil
		},
	}

	got, err := ListProjectNamedVolumes(t.Context(), fake, "wdm-n8n")
	require.NoError(t, err)
	require.Equal(t, []string{"wdm-n8n_postgres-data", "wdm-n8n_redis-data"}, got)
}

func TestListProjectNamedVolumes_EmptyOutputReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(projectVolumeListInvocation)
			require.True(t, ok)
			return CommandResult{}, nil
		},
	}

	got, err := ListProjectNamedVolumes(t.Context(), fake, "wdm-n8n")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestListProjectNamedVolumes_RejectsMalformedOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "padded name", stdout: " wdm-n8n_data\n"},
		{name: "blank line", stdout: "wdm-n8n_data\n\n"},
		{name: "slash", stdout: "wdm-n8n/data\n"},
		{name: "backslash", stdout: "wdm-n8n\\data\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{
				runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
					_, ok := inv.(projectVolumeListInvocation)
					require.True(t, ok)
					return CommandResult{Stdout: tt.stdout}, nil
				},
			}

			_, err := ListProjectNamedVolumes(t.Context(), fake, "wdm-n8n")
			requireUsageValidationError(t, err)
		})
	}
}

func TestListProjectNamedVolumes_PropagatesCommandErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("docker failed")
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(projectVolumeListInvocation)
			require.True(t, ok)
			return CommandResult{}, boom
		},
	}

	_, err := ListProjectNamedVolumes(t.Context(), fake, "wdm-n8n")
	require.ErrorIs(t, err, boom)
}

func TestRemoveNamedVolume_RejectsNilClient(t *testing.T) {
	t.Parallel()

	err := RemoveNamedVolume(t.Context(), nil, "wdm-n8n_data")
	requireUsageValidationError(t, err)
}

func TestRemoveNamedVolume_RejectsInvalidNameBeforeRunningClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		volumeName string
	}{
		{name: "blank name", volumeName: ""},
		{name: "leading dash flag", volumeName: "--force"},
		{name: "slash injection", volumeName: "wdm-n8n/data"},
		{name: "backslash injection", volumeName: "wdm-n8n\\data"},
		{name: "command chained name", volumeName: "wdm-n8n_data; reboot"},
		{name: "embedded space", volumeName: "wdm-n8n data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &inspectFakeClient{}
			err := RemoveNamedVolume(t.Context(), fake, tt.volumeName)
			requireUsageValidationError(t, err)
			require.Empty(t, fake.calls)
		})
	}
}

func TestRemoveNamedVolume_RemovesNamedVolume(t *testing.T) {
	t.Parallel()

	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			removeInv, ok := inv.(removeNamedVolumeInvocation)
			require.True(t, ok)
			require.Equal(t, "wdm-n8n_data", removeInv.name)
			return CommandResult{}, nil
		},
	}

	err := RemoveNamedVolume(t.Context(), fake, "wdm-n8n_data")
	require.NoError(t, err)
	require.Len(t, fake.calls, 1)
}

func TestRemoveNamedVolume_PropagatesCommandError(t *testing.T) {
	t.Parallel()

	boom := errors.New("docker failed")
	fake := &inspectFakeClient{
		runFn: func(_ context.Context, inv Invocation) (CommandResult, error) {
			_, ok := inv.(removeNamedVolumeInvocation)
			require.True(t, ok)
			return CommandResult{}, boom
		},
	}

	err := RemoveNamedVolume(t.Context(), fake, "wdm-n8n_data")
	require.Same(t, boom, err)
	require.Len(t, fake.calls, 1)
}

func TestRun_RemoveNamedVolumeBuildsExactArgv(t *testing.T) {
	t.Parallel()

	invoked := false
	execFn := func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		invoked = true
		require.Equal(t, []string{"volume", "rm", "wdm-n8n_data"}, cmd.argv)
		return CommandResult{}, nil
	}

	client, err := New(WithCommandExecutor(execFn))
	require.NoError(t, err)

	_, err = client.Run(t.Context(), removeNamedVolumeInvocation{name: "wdm-n8n_data"})
	require.NoError(t, err)
	require.True(t, invoked)
}

func TestValidateCommandSpec_AllowsInspectionSurface(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"container",
			"ls",
			"--all",
			"--filter",
			"label=com.docker.compose.project=wdm-n8n",
			"--format",
			"{{.ID}}",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"container",
			"inspect",
			"--format",
			containerInspectFormat,
			"abc123def456",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"image",
			"inspect",
			"--format",
			imageDigestInspectFormat,
			"n8nio/n8n:1.0.0",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{
			"volume",
			"ls",
			"--filter",
			"label=com.docker.compose.project=wdm-n8n",
			"--format",
			"{{.Name}}",
		},
	}))
	require.NoError(t, validateCommandSpec(commandSpec{
		argv: []string{"volume", "rm", "wdm-n8n_data"},
	}))
}

func TestValidateCommandSpec_RejectsUnsafeInspectionShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "container list wrong format literal",
			argv: []string{
				"container",
				"ls",
				"--all",
				"--filter",
				"label=com.docker.compose.project=wdm-n8n",
				"--format",
				"{{json .}}",
			},
		},
		{
			name: "container list unsafe project filter",
			argv: []string{
				"container",
				"ls",
				"--all",
				"--filter",
				"label=com.docker.compose.project=../n8n",
				"--format",
				"{{.ID}}",
			},
		},
		{
			name: "container inspect wrong format literal",
			argv: []string{
				"container",
				"inspect",
				"--format",
				"{{json .}}",
				"abc123def456",
			},
		},
		{
			name: "container inspect unsafe id",
			argv: []string{
				"container",
				"inspect",
				"--format",
				containerInspectFormat,
				"not-a-container",
			},
		},
		{
			name: "image inspect wrong format literal",
			argv: []string{
				"image",
				"inspect",
				"--format",
				"{{.RepoDigests}}",
				"redis:7.4",
			},
		},
		{
			name: "image inspect unsafe ref",
			argv: []string{
				"image",
				"inspect",
				"--format",
				imageDigestInspectFormat,
				"redis:7.4 latest",
			},
		},
		{
			name: "volume list wrong format literal",
			argv: []string{
				"volume",
				"ls",
				"--filter",
				"label=com.docker.compose.project=wdm-n8n",
				"--format",
				"{{json .}}",
			},
		},
		{
			name: "volume list unsafe project filter",
			argv: []string{
				"volume",
				"ls",
				"--filter",
				"label=com.docker.compose.project=wdm/n8n",
				"--format",
				"{{.Name}}",
			},
		},
		{
			name: "volume remove with extra arg",
			argv: []string{"volume", "rm", "wdm-n8n_data", "wdm-n8n_other"},
		},
		{
			name: "volume remove with force flag",
			argv: []string{"volume", "rm", "--force", "wdm-n8n_data"},
		},
		{
			name: "volume remove unsafe name",
			argv: []string{"volume", "rm", "wdm-n8n/data"},
		},
		{
			name: "volume remove injected name",
			argv: []string{"volume", "rm", "wdm-n8n_data; reboot"},
		},
		{
			name: "image remove forbidden",
			argv: []string{"image", "rm", "redis:7.4"},
		},
		{
			name: "container remove forbidden",
			argv: []string{"container", "rm", "abc123def456"},
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
