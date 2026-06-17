package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

type checkVersionsFakeClient struct {
	resultsByInvocation map[string]CommandResult
	errorsByInvocation  map[string]error
	invocations         []string
}

func (f *checkVersionsFakeClient) Run(_ context.Context, inv Invocation) (CommandResult, error) {
	name := invocationName(inv)
	f.invocations = append(f.invocations, name)
	if err := f.errorsByInvocation[name]; err != nil {
		return CommandResult{}, err
	}
	if res, ok := f.resultsByInvocation[name]; ok {
		return res, nil
	}
	return CommandResult{}, nil
}

func invocationName(inv Invocation) string {
	switch inv.(type) {
	case VersionInvocation:
		return "version"
	case ComposeVersionInvocation:
		return "compose-version"
	default:
		return "unknown"
	}
}

func TestCheckVersions_RejectsNilClient(t *testing.T) {
	t.Parallel()

	_, err := CheckVersions(t.Context(), nil)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func TestCheckVersions_SuccessNormalizesAndOrdersInvocations(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Client:\n Version: 25.0.0\nServer:\n Engine:\n  Version: v24.0.7+build\n",
			},
			"compose-version": {
				Stdout: "Docker Compose version v2.29.2\n",
			},
		},
	}

	report, err := CheckVersions(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, VersionReport{
		DockerVersion:  "24.0.7",
		ComposeVersion: "2.29.2",
	}, report)
	require.Equal(t, []string{"version", "compose-version"}, client.invocations)
}

func TestCheckVersions_ShortCircuitsOnDockerRunError(t *testing.T) {
	t.Parallel()

	dockerErr := errors.New("docker unavailable")
	client := &checkVersionsFakeClient{
		errorsByInvocation: map[string]error{
			"version": dockerErr,
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Same(t, dockerErr, err)
	require.Equal(t, []string{"version"}, client.invocations)
}

func TestCheckVersions_ShortCircuitsOnComposeRunError(t *testing.T) {
	t.Parallel()

	composeErr := errors.New("compose unavailable")
	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 24.0.7\n",
			},
		},
		errorsByInvocation: map[string]error{
			"compose-version": composeErr,
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Same(t, composeErr, err)
	require.Equal(t, []string{"version", "compose-version"}, client.invocations)
}

func TestCheckVersions_MapsInvalidDockerOutputToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Client:\n Version: 24.0.7\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Contains(t, typedErr.Hint, "20.10")
	require.Equal(t, []string{"version"}, client.invocations)
}

func TestCheckVersions_MapsOldDockerVersionToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 19.03.15\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Contains(t, typedErr.Hint, "20.10")
	require.Equal(t, []string{"version"}, client.invocations)
}

func TestCheckVersions_MapsMalformedDockerVersionSuffixToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 20.10oops\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Equal(t, []string{"version"}, client.invocations)
}

func TestCheckVersions_MapsInvalidComposeOutputToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 24.0.7\n",
			},
			"compose-version": {
				Stdout: "compose plugin output malformed\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Contains(t, typedErr.Hint, "compose v2")
	require.Equal(t, []string{"version", "compose-version"}, client.invocations)
}

func TestCheckVersions_MapsLegacyComposeV1ToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 24.0.7\n",
			},
			"compose-version": {
				Stdout: "Docker Compose version 1.29.2\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Contains(t, typedErr.Hint, "compose v2")
	require.Equal(t, []string{"version", "compose-version"}, client.invocations)
}

func TestCheckVersions_MapsMalformedComposeVersionSuffixToDockerUnavailable(t *testing.T) {
	t.Parallel()

	client := &checkVersionsFakeClient{
		resultsByInvocation: map[string]CommandResult{
			"version": {
				Stdout: "Server Version: 24.0.7\n",
			},
			"compose-version": {
				Stdout: "Docker Compose version 2.29bogus\n",
			},
		},
	}

	_, err := CheckVersions(t.Context(), client)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	require.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
	require.Equal(t, []string{"version", "compose-version"}, client.invocations)
}

func TestParseDockerServerVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stdout      string
		wantVersion string
		wantErr     bool
	}{
		{
			name:        "server engine nested format",
			stdout:      "Client:\n Version: 25.0.0\nServer:\n Engine:\n  Version: 24.0.7\n",
			wantVersion: "24.0.7",
		},
		{
			name:        "server heading with trailing description",
			stdout:      "Client: Docker Engine - Community\n Version: 25.0.0\nServer: Docker Engine - Community\n Engine:\n  Version: 24.0.7\n",
			wantVersion: "24.0.7",
		},
		{
			name:        "server version single line",
			stdout:      "Server Version: 20.10.24\n",
			wantVersion: "20.10.24",
		},
		{
			name:        "strip v and suffix",
			stdout:      "Server Version: v24.0.7+build\n",
			wantVersion: "24.0.7",
		},
		{
			name:        "patch optional",
			stdout:      "Server Version: 20.10\n",
			wantVersion: "20.10.0",
		},
		{
			name:    "client only output",
			stdout:  "Client:\n Version: 24.0.7\n",
			wantErr: true,
		},
		{
			name:    "malformed version",
			stdout:  "Server Version: twenty.four\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDockerServerVersion(tt.stdout)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantVersion, got)
		})
	}
}

func TestDockerVersionAtLeastMinimum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "exact minimum", version: "20.10.0", want: true},
		{name: "minimum patch", version: "20.10.24", want: true},
		{name: "higher major", version: "24.0.7", want: true},
		{name: "lower minor", version: "20.9.9", want: false},
		{name: "older major", version: "19.03.15", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := dockerVersionAtLeastMinimum(tt.version)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseComposeVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stdout      string
		wantVersion string
		wantErr     bool
	}{
		{
			name:        "prefixed with v",
			stdout:      "Docker Compose version v2.29.2\n",
			wantVersion: "2.29.2",
		},
		{
			name:        "plain numeric",
			stdout:      "Docker Compose version 2.20.2\n",
			wantVersion: "2.20.2",
		},
		{
			name:        "suffix ignored",
			stdout:      "Docker Compose version v2.24.7-desktop.1\n",
			wantVersion: "2.24.7",
		},
		{
			name:    "legacy v1",
			stdout:  "Docker Compose version 1.29.2\n",
			wantErr: true,
		},
		{
			name:    "malformed output",
			stdout:  "compose plugin output malformed\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseComposeVersion(tt.stdout)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantVersion, got)
		})
	}
}
