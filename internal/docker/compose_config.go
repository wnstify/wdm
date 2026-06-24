package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wnstify/wdm/pkg/types"
)

// ValidateComposeConfig validates a rendered Compose file through
// `docker compose config --quiet`. It discards stdout so normalized
// output is never exposed.
func ValidateComposeConfig(
	ctx context.Context,
	client Client,
	projectDir string,
	composeFile string,
) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	inv, err := newComposeConfigInvocation(projectDir, composeFile)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, inv)
	return err
}

type composeConfigInvocation struct {
	projectDir   string
	composeFile  string
	overridePath string
}

func (composeConfigInvocation) isDockerInvocation() {}

func newComposeConfigInvocation(projectDir, composeFile string) (composeConfigInvocation, error) {
	cleanProjectDir, cleanComposeFile, err := validateComposeConfigPaths(projectDir, composeFile)
	if err != nil {
		return composeConfigInvocation{}, err
	}

	overridePath, err := resolveOverridePath(cleanProjectDir)
	if err != nil {
		return composeConfigInvocation{}, err
	}

	return composeConfigInvocation{
		projectDir:   cleanProjectDir,
		composeFile:  cleanComposeFile,
		overridePath: overridePath,
	}, nil
}

func validateComposeConfigPaths(projectDir, composeFile string) (string, string, error) {
	cleanProjectDir, err := validateAbsolutePath(
		projectDir,
		"project directory",
		"pass a non-empty absolute path for compose project directory",
	)
	if err != nil {
		return "", "", err
	}

	cleanComposeFile, err := validateAbsolutePath(
		composeFile,
		"compose file",
		"pass a non-empty absolute path for compose file",
	)
	if err != nil {
		return "", "", err
	}

	relPath, err := filepath.Rel(cleanProjectDir, cleanComposeFile)
	if err != nil {
		return "", "", types.WrapError(
			types.ErrCodeUsageValidation,
			"compose file path cannot be resolved against project directory",
			"ensure compose file path and project directory are valid absolute paths",
			fmt.Errorf("resolve compose path containment: %w", err),
		)
	}
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", "", types.NewError(
			types.ErrCodeUsageValidation,
			"compose file must be within project directory",
			"place compose file under the selected project directory",
		)
	}

	return cleanProjectDir, cleanComposeFile, nil
}

func validateAbsolutePath(rawPath, label, hint string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			fmt.Sprintf("%s path is required", label),
			hint,
		)
	}
	if !filepath.IsAbs(trimmed) {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			fmt.Sprintf("%s path must be absolute", label),
			hint,
		)
	}

	return filepath.Clean(trimmed), nil
}
