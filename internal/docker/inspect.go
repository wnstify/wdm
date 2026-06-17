package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/wnstify/wdm/pkg/types"
)

const (
	composeProjectLabelFilterPrefix = "label=com.docker.compose.project="
	containerListFormat             = "{{.ID}}"
	containerInspectFormat          = `{{json .Name}}
{{json .Config.Labels}}
{{json .State.Status}}
{{json .State.Running}}
{{json .State.Restarting}}
{{json .State.ExitCode}}
{{if .State.Health}}{{json .State.Health.Status}}{{else}}""{{end}}
{{json .NetworkSettings.Ports}}`
	imageDigestInspectFormat = `{{ join .RepoDigests ","}}`
	volumeListFormat         = "{{.Name}}"
)

var (
	containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	imageRefPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]*$`)
	sha256Pattern      = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	volumeNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// ContainerInfo is the safe subset of Docker container-inspection data
// used by status checks.
type ContainerInfo struct {
	ID      string
	Name    string
	Service string
	Labels  map[string]string
	State   ContainerState
	Ports   []PublishedPort
}

// ContainerState is the safe subset of Docker container runtime state
// used to derive running / needs-attention status.
type ContainerState struct {
	Status     string
	Running    bool
	Restarting bool
	ExitCode   int
	Health     string
}

// PublishedPort is one host-side binding from Docker's
// NetworkSettings.Ports map.
type PublishedPort struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

// InspectProjectContainers lists and inspects containers belonging to one
// Compose project. It returns only labels, status fields, and published
// ports; raw Docker inspect JSON never crosses this API.
func InspectProjectContainers(
	ctx context.Context,
	client Client,
	projectName string,
) ([]ContainerInfo, error) {
	if client == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	normalizedProject, err := validateComposeProjectName(projectName)
	if err != nil {
		return nil, err
	}

	listRes, err := client.Run(
		ctx,
		projectContainerListInvocation{projectName: normalizedProject},
	)
	if err != nil {
		return nil, err
	}

	ids, err := parseContainerIDList(listRes.Stdout)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []ContainerInfo{}, nil
	}

	containers := make([]ContainerInfo, 0, len(ids))
	for _, id := range ids {
		inspectRes, inspectErr := client.Run(ctx, containerInspectInvocation{id: id})
		if inspectErr != nil {
			return nil, inspectErr
		}

		container, parseErr := parseContainerInspectOutput(id, inspectRes.Stdout)
		if parseErr != nil {
			return nil, parseErr
		}
		containers = append(containers, container)
	}

	return containers, nil
}

// InspectImageDigest captures the first sha256 digest reported for an
// image reference. Digest capture is opportunistic: ordinary inspect
// failures and missing digests return an empty string without failing
// the caller.
func InspectImageDigest(ctx context.Context, client Client, imageRef string) (string, error) {
	if client == nil {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	normalizedRef, err := validateImageRef(imageRef)
	if err != nil {
		return "", err
	}

	res, err := client.Run(ctx, imageDigestInspectInvocation{imageRef: normalizedRef})
	if err != nil {
		if ctx.Err() != nil || isUserCanceledError(err) {
			return "", err
		}
		return "", nil
	}

	digest, parseErr := parseImageDigestOutput(res.Stdout)
	if parseErr != nil {
		return "", parseErr
	}
	return digest, nil
}

// ListProjectNamedVolumes lists Docker named volumes with the Compose
// project label Docker writes for the selected stack.
func ListProjectNamedVolumes(
	ctx context.Context,
	client Client,
	projectName string,
) ([]string, error) {
	if client == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	normalizedProject, err := validateComposeProjectName(projectName)
	if err != nil {
		return nil, err
	}

	res, err := client.Run(ctx, projectVolumeListInvocation{projectName: normalizedProject})
	if err != nil {
		return nil, err
	}

	volumes, err := parseVolumeNameList(res.Stdout)
	if err != nil {
		return nil, err
	}
	return volumes, nil
}

// RemoveNamedVolume removes a single named volume. It is the failure-rollback
// counterpart to volume creation during install: a stack install that created
// fresh named volumes and then failed can hand each volume name back here to
// remove it. The name is validated by the same strict validator that guards the
// volume-list parse, so only a well-formed volume name reaches the daemon
// (PRD §12). It removes exactly one named volume — no prune, no force.
func RemoveNamedVolume(ctx context.Context, client Client, volumeName string) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	name, err := validateVolumeName(volumeName)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, removeNamedVolumeInvocation{name: name})
	return err
}

type projectContainerListInvocation struct {
	projectName string
}

func (projectContainerListInvocation) isDockerInvocation() {}

type containerInspectInvocation struct {
	id string
}

func (containerInspectInvocation) isDockerInvocation() {}

type imageDigestInspectInvocation struct {
	imageRef string
}

func (imageDigestInspectInvocation) isDockerInvocation() {}

type projectVolumeListInvocation struct {
	projectName string
}

func (projectVolumeListInvocation) isDockerInvocation() {}

// removeNamedVolumeInvocation maps to `volume rm <name>` so [RemoveNamedVolume]
// can drop a single named volume during install-failure rollback.
type removeNamedVolumeInvocation struct {
	name string
}

func (removeNamedVolumeInvocation) isDockerInvocation() {}

func parseContainerIDList(stdout string) ([]string, error) {
	lines, err := strictDockerOutputLines(stdout, "container id list")
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		id, err := validateContainerID(line)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseContainerInspectOutput(id, stdout string) (ContainerInfo, error) {
	fields, err := strictDockerOutputLines(stdout, "container inspect output")
	if err != nil {
		return ContainerInfo{}, err
	}
	if len(fields) != 8 {
		return ContainerInfo{}, inspectOutputError(
			"container inspect output is invalid",
			"container inspect output must contain the expected safe field set",
			fmt.Errorf("container %s inspect returned %d fields", id, len(fields)),
		)
	}

	var name string
	if err := decodeInspectJSONField(fields[0], "container name", &name); err != nil {
		return ContainerInfo{}, err
	}

	var labels map[string]string
	if err := decodeInspectJSONField(fields[1], "container labels", &labels); err != nil {
		return ContainerInfo{}, err
	}
	if labels == nil {
		labels = map[string]string{}
	}

	var status string
	if err := decodeInspectJSONField(fields[2], "container status", &status); err != nil {
		return ContainerInfo{}, err
	}

	var running bool
	if err := decodeInspectJSONField(fields[3], "container running", &running); err != nil {
		return ContainerInfo{}, err
	}

	var restarting bool
	if err := decodeInspectJSONField(fields[4], "container restarting", &restarting); err != nil {
		return ContainerInfo{}, err
	}

	var exitCode int
	if err := decodeInspectJSONField(fields[5], "container exit code", &exitCode); err != nil {
		return ContainerInfo{}, err
	}

	var health string
	if err := decodeInspectJSONField(fields[6], "container health", &health); err != nil {
		return ContainerInfo{}, err
	}

	ports, err := parseDockerPorts(fields[7])
	if err != nil {
		return ContainerInfo{}, err
	}

	return ContainerInfo{
		ID:      id,
		Name:    strings.TrimPrefix(name, "/"),
		Service: labels["com.docker.compose.service"],
		Labels:  labels,
		State: ContainerState{
			Status:     status,
			Running:    running,
			Restarting: restarting,
			ExitCode:   exitCode,
			Health:     health,
		},
		Ports: ports,
	}, nil
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func parseDockerPorts(raw string) ([]PublishedPort, error) {
	var portMap map[string][]dockerPortBinding
	if err := decodeInspectJSONField(raw, "container ports", &portMap); err != nil {
		return nil, err
	}
	if len(portMap) == 0 {
		return nil, nil
	}

	var ports []PublishedPort
	for key, bindings := range portMap {
		containerPort, protocol, err := parseContainerPortKey(key)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			hostPort, err := parsePortNumber(binding.HostPort, "host port")
			if err != nil {
				return nil, err
			}
			ports = append(ports, PublishedPort{
				HostIP:        binding.HostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      protocol,
			})
		}
	}

	sort.Slice(ports, func(i, j int) bool {
		if ports[i].ContainerPort != ports[j].ContainerPort {
			return ports[i].ContainerPort < ports[j].ContainerPort
		}
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		if ports[i].HostIP != ports[j].HostIP {
			return ports[i].HostIP < ports[j].HostIP
		}
		return ports[i].HostPort < ports[j].HostPort
	})

	return ports, nil
}

func parseContainerPortKey(key string) (int, string, error) {
	portPart, protocol, ok := strings.Cut(key, "/")
	if !ok || protocol == "" || strings.Contains(protocol, "/") {
		return 0, "", inspectOutputError(
			"container port mapping is invalid",
			"inspect output must use Docker port keys like 8080/tcp",
			fmt.Errorf("invalid container port key %q", key),
		)
	}

	containerPort, err := parsePortNumber(portPart, "container port")
	if err != nil {
		return 0, "", err
	}
	if protocol != "tcp" && protocol != "udp" {
		return 0, "", inspectOutputError(
			"container port protocol is invalid",
			"inspect output must use tcp or udp port protocols",
			fmt.Errorf("invalid container port protocol %q", protocol),
		)
	}
	return containerPort, protocol, nil
}

func parsePortNumber(raw, label string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, inspectOutputError(
			"container port mapping is invalid",
			"inspect output must contain numeric ports in range 1-65535",
			fmt.Errorf("%s %q is invalid", label, raw),
		)
	}
	return port, nil
}

func decodeInspectJSONField(raw, label string, dest any) error {
	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return inspectOutputError(
			"container inspect output is invalid",
			"container inspect output must contain valid JSON fields",
			fmt.Errorf("decode %s: %w", label, err),
		)
	}
	return nil
}

func parseImageDigestOutput(stdout string) (string, error) {
	value := stripSingleDockerLineEnding(stdout)
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, "\r\n") || value != strings.TrimSpace(value) {
		return "", inspectOutputError(
			"image digest inspect output is invalid",
			"image digest output must be a single unpadded line",
			fmt.Errorf("image digest inspect returned malformed output %q", stdout),
		)
	}

	var firstDigest string
	for _, entry := range strings.Split(value, ",") {
		if entry == "" || entry != strings.TrimSpace(entry) {
			return "", inspectOutputError(
				"image digest inspect output is invalid",
				"image digest output must contain unpadded comma-separated entries",
				fmt.Errorf("image digest inspect returned malformed entry %q", entry),
			)
		}
		candidate := entry
		if _, digest, ok := strings.Cut(entry, "@"); ok {
			candidate = digest
		}
		if firstDigest == "" && sha256Pattern.MatchString(candidate) {
			firstDigest = candidate
		}
	}

	return firstDigest, nil
}

func parseVolumeNameList(stdout string) ([]string, error) {
	lines, err := strictDockerOutputLines(stdout, "volume name list")
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		name, err := validateVolumeName(line)
		if err != nil {
			return nil, err
		}
		seen[name] = struct{}{}
	}

	volumes := make([]string, 0, len(seen))
	for name := range seen {
		volumes = append(volumes, name)
	}
	sort.Strings(volumes)
	return volumes, nil
}

func strictDockerOutputLines(stdout, label string) ([]string, error) {
	if stdout == "" {
		return nil, nil
	}

	value := stripSingleDockerLineEnding(stdout)
	if value == "" {
		return nil, inspectOutputError(
			"docker inspect output is invalid",
			"docker output must not contain blank lines",
			fmt.Errorf("%s is blank", label),
		)
	}

	lines := strings.Split(value, "\n")
	for _, line := range lines {
		if line == "" || strings.Contains(line, "\r") || line != strings.TrimSpace(line) {
			return nil, inspectOutputError(
				"docker inspect output is invalid",
				"docker output must contain exact unpadded tokens",
				fmt.Errorf("%s contains malformed line %q", label, line),
			)
		}
	}

	return lines, nil
}

func stripSingleDockerLineEnding(stdout string) string {
	value := strings.TrimSuffix(stdout, "\n")
	return strings.TrimSuffix(value, "\r")
}

func validateContainerID(rawID string) (string, error) {
	if !containerIDPattern.MatchString(rawID) {
		return "", inspectOutputError(
			"container id is invalid",
			"docker container IDs must be lowercase hex",
			fmt.Errorf("container id %q does not match allowed format", rawID),
		)
	}
	return rawID, nil
}

func validateImageRef(rawRef string) (string, error) {
	if strings.TrimSpace(rawRef) == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"image reference is required",
			"pass an explicit tagged image reference",
		)
	}
	if strings.HasPrefix(rawRef, "-") || !imageRefPattern.MatchString(rawRef) {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"image reference is invalid",
			"use a Docker image reference containing only letters, digits, '.', '_', '-', '/', ':', or '@'",
			fmt.Errorf("image reference %q does not match allowed format", rawRef),
		)
	}
	for _, r := range rawRef {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", types.WrapError(
				types.ErrCodeUsageValidation,
				"image reference is invalid",
				"use a Docker image reference without whitespace or control characters",
				fmt.Errorf("image reference %q contains unsafe character", rawRef),
			)
		}
	}
	return rawRef, nil
}

func validateVolumeName(rawName string) (string, error) {
	if !volumeNamePattern.MatchString(rawName) {
		return "", inspectOutputError(
			"volume name is invalid",
			"docker volume names must be unpadded names without path separators",
			fmt.Errorf("volume name %q does not match allowed format", rawName),
		)
	}
	return rawName, nil
}

func validateComposeProjectLabelFilter(rawFilter string) error {
	projectName, ok := strings.CutPrefix(rawFilter, composeProjectLabelFilterPrefix)
	if !ok {
		return usageValidationError(
			"compose project label filter is invalid",
			fmt.Errorf("filter %q must target compose project label", rawFilter),
		)
	}
	_, err := validateComposeProjectName(projectName)
	return err
}

func inspectOutputError(message, hint string, cause error) error {
	return types.WrapError(types.ErrCodeUsageValidation, message, hint, cause)
}

func isUserCanceledError(err error) bool {
	var typedErr *types.Error
	return errors.As(err, &typedErr) && typedErr.Code == types.ErrCodeUserCanceled
}
