package docker

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/wnstify/wdm/pkg/types"
)

var (
	dockerServerVersionLinePattern = regexp.MustCompile(`(?im)^\s*Server Version:\s*(\S+)`)
	dockerVersionLinePattern       = regexp.MustCompile(`(?i)^\s*Version:\s*(\S+)`)
	composeVersionTokenPattern     = regexp.MustCompile(`(?i)\bversion\s+(\S+)`)
	versionTokenPattern            = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)
)

var minimumDockerVersion = parsedVersion{major: 20, minor: 10, patch: 0}

// VersionReport is the normalized Docker/Compose version snapshot
// returned by [CheckVersions].
type VersionReport struct {
	DockerVersion  string
	ComposeVersion string
}

// CheckVersions validates Docker engine and Compose plugin versions
// through the typed invocation seam. The client must be non-nil.
func CheckVersions(ctx context.Context, client Client) (VersionReport, error) {
	if client == nil {
		return VersionReport{}, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	dockerRes, err := client.Run(ctx, VersionInvocation{})
	if err != nil {
		return VersionReport{}, err
	}

	dockerVersion, err := parseDockerServerVersion(dockerRes.Stdout)
	if err != nil {
		return VersionReport{}, types.WrapError(
			types.ErrCodeDockerUnavailable,
			"docker engine version check failed",
			"require docker engine 20.10 or newer and verify `docker version` reports server/engine version",
			err,
		)
	}

	dockerOK, err := dockerVersionAtLeastMinimum(dockerVersion)
	if err != nil {
		return VersionReport{}, types.WrapError(
			types.ErrCodeDockerUnavailable,
			"docker engine version check failed",
			"require docker engine 20.10 or newer and verify `docker version` reports server/engine version",
			err,
		)
	}
	if !dockerOK {
		return VersionReport{}, types.WrapError(
			types.ErrCodeDockerUnavailable,
			"docker engine version is unsupported",
			"require docker engine 20.10 or newer",
			fmt.Errorf("detected docker engine version %q", dockerVersion),
		)
	}

	composeRes, err := client.Run(ctx, ComposeVersionInvocation{})
	if err != nil {
		return VersionReport{}, err
	}

	composeVersion, err := parseComposeVersion(composeRes.Stdout)
	if err != nil {
		return VersionReport{}, types.WrapError(
			types.ErrCodeDockerUnavailable,
			"compose plugin version check failed",
			"require compose v2 plugin; legacy compose v1 is unsupported",
			err,
		)
	}

	return VersionReport{
		DockerVersion:  dockerVersion,
		ComposeVersion: composeVersion,
	}, nil
}

func parseDockerServerVersion(stdout string) (string, error) {
	if matches := dockerServerVersionLinePattern.FindStringSubmatch(stdout); len(matches) == 2 {
		return normalizeVersionToken(matches[1])
	}

	lines := strings.Split(stdout, "\n")
	inServerSection := false
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)

		if !inServerSection {
			if strings.HasPrefix(strings.ToLower(trimmed), "server:") {
				inServerSection = true
			}
			continue
		}

		if trimmed == "" {
			continue
		}
		if isTopLevelLine(line) {
			break
		}

		if matches := dockerVersionLinePattern.FindStringSubmatch(line); len(matches) == 2 {
			return normalizeVersionToken(matches[1])
		}
	}

	return "", fmt.Errorf("docker server version not found in docker version output")
}

func parseComposeVersion(stdout string) (string, error) {
	matches := composeVersionTokenPattern.FindStringSubmatch(stdout)
	if len(matches) != 2 {
		return "", fmt.Errorf("compose version not found in docker compose version output")
	}

	normalized, version, err := parseAndNormalizeVersion(matches[1])
	if err != nil {
		return "", fmt.Errorf("invalid compose version token %q: %w", matches[1], err)
	}
	if version.major != 2 {
		return "", fmt.Errorf("compose major version %d is unsupported", version.major)
	}

	return normalized, nil
}

func dockerVersionAtLeastMinimum(version string) (bool, error) {
	_, parsed, err := parseAndNormalizeVersion(version)
	if err != nil {
		return false, err
	}
	return !parsed.lessThan(minimumDockerVersion), nil
}

func normalizeVersionToken(raw string) (string, error) {
	normalized, _, err := parseAndNormalizeVersion(raw)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

func parseAndNormalizeVersion(raw string) (string, parsedVersion, error) {
	token := strings.TrimSpace(raw)
	indices := versionTokenPattern.FindStringSubmatchIndex(token)
	if len(indices) != 8 {
		return "", parsedVersion{}, fmt.Errorf("version %q must start with major.minor or major.minor.patch", raw)
	}
	matchEnd := indices[1]
	if matchEnd < len(token) {
		suffix := token[matchEnd:]
		switch suffix[0] {
		case '-', '+':
			// Accepted metadata/build suffix delimiter.
		default:
			return "", parsedVersion{}, fmt.Errorf("version %q has invalid suffix %q", raw, suffix)
		}
	}

	major, err := strconv.Atoi(token[indices[2]:indices[3]])
	if err != nil {
		return "", parsedVersion{}, fmt.Errorf("parse major version from %q: %w", raw, err)
	}
	minor, err := strconv.Atoi(token[indices[4]:indices[5]])
	if err != nil {
		return "", parsedVersion{}, fmt.Errorf("parse minor version from %q: %w", raw, err)
	}

	patch := 0
	if indices[6] != -1 && indices[7] != -1 {
		patch, err = strconv.Atoi(token[indices[6]:indices[7]])
		if err != nil {
			return "", parsedVersion{}, fmt.Errorf("parse patch version from %q: %w", raw, err)
		}
	}

	parsed := parsedVersion{major: major, minor: minor, patch: patch}
	return parsed.String(), parsed, nil
}

type parsedVersion struct {
	major int
	minor int
	patch int
}

func (v parsedVersion) lessThan(other parsedVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

func (v parsedVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func isTopLevelLine(line string) bool {
	if line == "" {
		return false
	}
	for _, r := range line {
		return !unicode.IsSpace(r)
	}
	return false
}
