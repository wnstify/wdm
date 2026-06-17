package system

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

const procMeminfoPath = "/proc/meminfo"

// DockerVersions is the normalized Docker/Compose version snapshot
// consumed by system readiness checks.
type DockerVersions struct {
	DockerVersion  string
	ComposeVersion string
}

// DockerVersionCheck is the injected check that proves Docker daemon
// reachability and Compose V2 availability without making internal/system
// depend on the low-level Docker package.
type DockerVersionCheck func(context.Context) (DockerVersions, error)

// DockerReadiness reports Docker prerequisites that passed the
// readiness gate.
type DockerReadiness struct {
	DockerVersion  string
	ComposeVersion string
	DockerGroupID  int
}

// HostResources reports host capacity values consumed by install
// planning.
type HostResources struct {
	CPUCores         int
	TotalMemoryBytes uint64
}

// ReadinessReport combines startup Docker readiness and host resource
// probes.
type ReadinessReport struct {
	Docker DockerReadiness
	Host   HostResources
}

type dockerGroupLookup func() (int, error)
type hostResourceProbe func() (HostResources, error)

// CheckReadiness verifies Docker group membership, Docker daemon
// reachability, Compose V2 availability, and host resource detection.
func CheckReadiness(ctx context.Context, identity Identity, versionCheck DockerVersionCheck) (ReadinessReport, error) {
	return checkReadiness(
		ctx,
		identity,
		versionCheck,
		lookupDockerGroupID,
		DetectHostResources,
	)
}

// DetectHostResources probes host CPU and total memory capacity.
func DetectHostResources() (HostResources, error) {
	meminfo, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		return HostResources{}, hostResourceProbeError(
			fmt.Errorf("reading %s: %w", procMeminfoPath, err),
		)
	}
	return detectHostResources(runtime.NumCPU(), meminfo)
}

func checkReadiness(
	ctx context.Context,
	identity Identity,
	versionCheck DockerVersionCheck,
	lookupGroup dockerGroupLookup,
	probeHostResources hostResourceProbe,
) (ReadinessReport, error) {
	if err := ctx.Err(); err != nil {
		return ReadinessReport{}, err
	}
	if versionCheck == nil {
		return ReadinessReport{}, missingDockerVersionCheckError()
	}
	if lookupGroup == nil {
		return ReadinessReport{}, missingDockerGroupLookupError()
	}
	if probeHostResources == nil {
		return ReadinessReport{}, missingHostResourceProbeError()
	}

	dockerGroupID, err := lookupGroup()
	if err != nil {
		return ReadinessReport{}, err
	}
	if err := requireDockerGroup(identity, dockerGroupID); err != nil {
		return ReadinessReport{}, err
	}

	versions, err := versionCheck(ctx)
	if err != nil {
		return ReadinessReport{}, err
	}

	host, err := probeHostResources()
	if err != nil {
		return ReadinessReport{}, err
	}

	return readinessReport(versions, dockerGroupID, host), nil
}

func readinessReport(versions DockerVersions, dockerGroupID int, host HostResources) ReadinessReport {
	return ReadinessReport{
		Docker: DockerReadiness{
			DockerVersion:  versions.DockerVersion,
			ComposeVersion: versions.ComposeVersion,
			DockerGroupID:  dockerGroupID,
		},
		Host: host,
	}
}

func lookupDockerGroupID() (int, error) {
	group, err := user.LookupGroup("docker")
	if err != nil {
		return 0, dockerGroupError(fmt.Errorf("looking up docker group: %w", err))
	}
	groupID, err := strconv.Atoi(group.Gid)
	if err != nil {
		return 0, dockerGroupError(fmt.Errorf("parsing docker group id %q: %w", group.Gid, err))
	}
	return groupID, nil
}

func requireDockerGroup(identity Identity, dockerGroupID int) error {
	if identity.GID == dockerGroupID {
		return nil
	}
	for _, groupID := range identity.GroupIDs {
		if groupID == dockerGroupID {
			return nil
		}
	}
	return dockerGroupError(
		fmt.Errorf("user %q is not a member of docker group %d", identity.Username, dockerGroupID),
	)
}

func detectHostResources(cpuCores int, meminfo []byte) (HostResources, error) {
	if cpuCores <= 0 {
		return HostResources{}, hostResourceProbeError(
			fmt.Errorf("cpu cores must be positive, got %d", cpuCores),
		)
	}

	totalMemoryBytes, err := parseMemTotalBytes(meminfo)
	if err != nil {
		return HostResources{}, hostResourceProbeError(err)
	}

	return HostResources{
		CPUCores:         cpuCores,
		TotalMemoryBytes: totalMemoryBytes,
	}, nil
}

func parseMemTotalBytes(meminfo []byte) (uint64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemTotal:" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("invalid memtotal line %q", line)
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing memtotal value %q: %w", fields[1], err)
		}
		if kib == 0 {
			return 0, fmt.Errorf("memtotal must be greater than zero")
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("memtotal not found in %s", procMeminfoPath)
}

func missingDockerVersionCheckError() error {
	return types.NewError(
		types.ErrCodeUsageValidation,
		"docker version check is required",
		"pass a docker version checker",
	)
}

func missingDockerGroupLookupError() error {
	return types.NewError(
		types.ErrCodeUsageValidation,
		"docker group lookup is required",
		"pass a docker group lookup function",
	)
}

func missingHostResourceProbeError() error {
	return types.NewError(
		types.ErrCodeUsageValidation,
		"host resource probe is required",
		"pass a host resource probe function",
	)
}

func dockerGroupError(cause error) error {
	return types.WrapError(
		types.ErrCodePermissionDenied,
		"docker group access is required",
		"add your user to the docker group, then log out and back in",
		cause,
	)
}

func hostResourceProbeError(cause error) error {
	return types.WrapError(
		types.ErrCodeUsageValidation,
		"host resource probe failed",
		"run wdm on a supported Linux amd64 host",
		cause,
	)
}
