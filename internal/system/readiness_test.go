package system

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestCheckReadiness_SucceedsWithDockerGroupVersionsAndResources(t *testing.T) {
	t.Parallel()

	versionCalled := false
	groupLookupCalled := false
	hostProbeCalled := false
	report, err := checkReadiness(
		t.Context(),
		Identity{EUID: 1000, GID: 1000, GroupIDs: []int{1000, 999}},
		func(_ context.Context) (DockerVersions, error) {
			versionCalled = true
			return DockerVersions{
				DockerVersion:  "24.0.7",
				ComposeVersion: "2.29.2",
			}, nil
		},
		func() (int, error) {
			groupLookupCalled = true
			return 999, nil
		},
		func() (HostResources, error) {
			hostProbeCalled = true
			return detectHostResources(
				4,
				[]byte("MemTotal:        2097152 kB\nMemFree:          100000 kB\n"),
			)
		},
	)

	require.NoError(t, err)
	require.True(t, groupLookupCalled, "docker group lookup must run")
	require.True(t, versionCalled, "docker version check must run after group validation")
	require.True(t, hostProbeCalled, "host probe must run after docker checks")
	assert.Equal(t, ReadinessReport{
		Docker: DockerReadiness{
			DockerVersion:  "24.0.7",
			ComposeVersion: "2.29.2",
			DockerGroupID:  999,
		},
		Host: HostResources{CPUCores: 4, TotalMemoryBytes: 2 * 1024 * 1024 * 1024},
	}, report)
}

func TestCheckReadiness_ExportedRejectsNilVersionCheck(t *testing.T) {
	t.Parallel()

	_, err := CheckReadiness(t.Context(), Identity{}, nil)
	require.Error(t, err)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func TestCheckReadiness_ExportedPropagatesCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := CheckReadiness(
		ctx,
		Identity{},
		func(_ context.Context) (DockerVersions, error) {
			return DockerVersions{}, nil
		},
	)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCheckReadiness_RejectsMissingDockerGroupBeforeDockerCommand(t *testing.T) {
	t.Parallel()

	versionCalled := false
	hostProbeCalled := false
	_, err := checkReadiness(
		t.Context(),
		Identity{EUID: 1000, GID: 1000, GroupIDs: []int{1000}},
		func(_ context.Context) (DockerVersions, error) {
			versionCalled = true
			return DockerVersions{}, nil
		},
		func() (int, error) { return 999, nil },
		func() (HostResources, error) {
			hostProbeCalled = true
			return HostResources{CPUCores: 2, TotalMemoryBytes: 1024}, nil
		},
	)

	require.Error(t, err)
	require.False(t, versionCalled, "docker command must not run when docker group membership is absent")
	require.False(t, hostProbeCalled, "host probe must not run when docker group membership is absent")

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodePermissionDenied, typedErr.Code)
	assert.Contains(t, typedErr.Hint, "docker group")
}

func TestCheckReadiness_PropagatesDockerVersionCheckError(t *testing.T) {
	t.Parallel()

	versionErr := types.NewError(
		types.ErrCodeDockerUnavailable,
		"docker is unavailable",
		"start Docker and try again",
	)
	hostProbeCalled := false

	_, err := checkReadiness(
		t.Context(),
		Identity{EUID: 1000, GID: 999, GroupIDs: []int{1000}},
		func(_ context.Context) (DockerVersions, error) {
			return DockerVersions{}, versionErr
		},
		func() (int, error) { return 999, nil },
		func() (HostResources, error) {
			hostProbeCalled = true
			return HostResources{CPUCores: 2, TotalMemoryBytes: 1024}, nil
		},
	)

	require.Same(t, versionErr, err)
	require.False(t, hostProbeCalled, "host probe must not run when docker version check fails")
}

func TestCheckReadiness_RejectsNilDockerVersionCheck(t *testing.T) {
	t.Parallel()

	groupLookupCalled := false
	hostProbeCalled := false
	_, err := checkReadiness(
		t.Context(),
		Identity{EUID: 1000, GID: 999, GroupIDs: []int{1000}},
		nil,
		func() (int, error) {
			groupLookupCalled = true
			return 999, nil
		},
		func() (HostResources, error) {
			hostProbeCalled = true
			return HostResources{CPUCores: 2, TotalMemoryBytes: 1024}, nil
		},
	)

	require.Error(t, err)
	require.False(t, groupLookupCalled, "docker group lookup must not run when version checker is missing")
	require.False(t, hostProbeCalled, "host probe must not run when version checker is missing")
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func TestDetectHostResources_RejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cpuCores int
		meminfo  []byte
	}{
		{
			name:     "zero cpu",
			cpuCores: 0,
			meminfo:  []byte("MemTotal: 1024 kB\n"),
		},
		{
			name:     "missing memtotal",
			cpuCores: 1,
			meminfo:  []byte("MemFree: 1024 kB\n"),
		},
		{
			name:     "non numeric memtotal",
			cpuCores: 1,
			meminfo:  []byte("MemTotal: nope kB\n"),
		},
		{
			name:     "wrong unit",
			cpuCores: 1,
			meminfo:  []byte("MemTotal: 1024 bytes\n"),
		},
		{
			name:     "extra field",
			cpuCores: 1,
			meminfo:  []byte("MemTotal: 1024 kB padded\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := detectHostResources(tc.cpuCores, tc.meminfo)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
		})
	}
}

func TestRequireDockerGroupAcceptsPrimaryOrSupplementaryGroup(t *testing.T) {
	t.Parallel()

	assert.NoError(t, requireDockerGroup(Identity{GID: 999}, 999))
	assert.NoError(t, requireDockerGroup(Identity{GID: 1000, GroupIDs: []int{999}}, 999))
}

func TestCheckReadiness_PropagatesContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	groupLookupCalled := false
	hostProbeCalled := false
	_, err := checkReadiness(
		ctx,
		Identity{EUID: 1000, GID: 999},
		func(_ context.Context) (DockerVersions, error) {
			return DockerVersions{}, errors.New("should not run")
		},
		func() (int, error) {
			groupLookupCalled = true
			return 999, nil
		},
		func() (HostResources, error) {
			hostProbeCalled = true
			return HostResources{CPUCores: 2, TotalMemoryBytes: 1024}, nil
		},
	)

	require.ErrorIs(t, err, context.Canceled)
	require.False(t, groupLookupCalled, "docker group lookup must not run when context is already canceled")
	require.False(t, hostProbeCalled, "host probe must not run when context is already canceled")
}

func TestCheckReadiness_RejectsNilLookupOrHostProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lookup    dockerGroupLookup
		hostProbe hostResourceProbe
	}{
		{
			name:      "missing lookup",
			lookup:    nil,
			hostProbe: func() (HostResources, error) { return HostResources{}, nil },
		},
		{
			name:      "missing host probe",
			lookup:    func() (int, error) { return 999, nil },
			hostProbe: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkReadiness(
				t.Context(),
				Identity{EUID: 1000, GID: 999, GroupIDs: []int{999}},
				func(_ context.Context) (DockerVersions, error) {
					return DockerVersions{DockerVersion: "24.0.7", ComposeVersion: "2.29.2"}, nil
				},
				tc.lookup,
				tc.hostProbe,
			)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
		})
	}
}

func TestLookupDockerGroupID_UsesSystemDockerGroupOrReturnsTypedError(t *testing.T) {
	t.Parallel()

	groupID, err := lookupDockerGroupID()
	if err == nil {
		assert.Greater(t, groupID, 0)
		return
	}

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodePermissionDenied, typedErr.Code)
}

func TestDetectHostResources_ProbesLiveHostOrReturnsTypedError(t *testing.T) {
	t.Parallel()

	resources, err := DetectHostResources()
	if runtime.GOOS == "linux" {
		require.NoError(t, err)
		assert.Greater(t, resources.CPUCores, 0)
		assert.Greater(t, resources.TotalMemoryBytes, uint64(0))
		return
	}

	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}
