package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/wnstify/wdm/pkg/types"
)

const (
	bindCleanupImage  = "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	bindCleanupTarget = "/wdm-delete-target"
)

// EnsureBindMountCleanupHelperAvailable verifies the digest-pinned helper
// image is already present locally. Delete calls this before any mutation,
// so the fallback cleanup cannot trigger a registry pull once deletion
// has begun.
func EnsureBindMountCleanupHelperAvailable(ctx context.Context, client Client) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	_, err := client.Run(ctx, imageDigestInspectInvocation{imageRef: bindCleanupImage})
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return types.WrapError(
			types.ErrCodeGeneric,
			"delete cleanup helper image is unavailable",
			"run docker pull docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662 and retry delete; wdm will not pull helper images after deletion begins",
			err,
		)
	}
	return nil
}

// RemoveBindMountContents removes the contents of a host bind-mounted
// directory from inside a bounded helper container. It is for managed-stack
// cleanup only: the caller must prove the path is contained before passing
// it to Docker.
func RemoveBindMountContents(ctx context.Context, client Client, path string) error {
	if client == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"docker client is required",
			"pass a non-nil docker client",
		)
	}

	cleaned, err := validateBindCleanupPath(path)
	if err != nil {
		return err
	}

	_, err = client.Run(ctx, bindMountCleanupInvocation{targetPath: cleaned})
	return err
}

type bindMountCleanupInvocation struct {
	targetPath string
}

func (bindMountCleanupInvocation) isDockerInvocation() {}

func validateBindCleanupPath(rawPath string) (string, error) {
	cleaned, err := validateAbsolutePath(
		rawPath,
		"bind cleanup path",
		"pass an absolute managed stack path",
	)
	if err != nil {
		return "", err
	}
	if strings.Contains(cleaned, ",") {
		return "", types.WrapError(
			types.ErrCodeUsageValidation,
			"bind cleanup path is invalid",
			"choose a stack path without commas before retrying delete",
			fmt.Errorf("bind cleanup path %q contains a comma", cleaned),
		)
	}
	for _, r := range cleaned {
		if unicode.IsControl(r) {
			return "", types.WrapError(
				types.ErrCodeUsageValidation,
				"bind cleanup path is invalid",
				"choose a stack path without control characters before retrying delete",
				fmt.Errorf("bind cleanup path %q contains a control character", cleaned),
			)
		}
	}
	return filepath.Clean(cleaned), nil
}

func bindCleanupMountArg(path string) string {
	return "type=bind,src=" + path + ",dst=" + bindCleanupTarget
}

func validateBindCleanupMountArg(rawMount string) error {
	source, ok := strings.CutPrefix(rawMount, "type=bind,src=")
	if !ok {
		return usageValidationError(
			"bind cleanup mount is invalid",
			fmt.Errorf("mount %q must be a bind source", rawMount),
		)
	}
	source, ok = strings.CutSuffix(source, ",dst="+bindCleanupTarget)
	if !ok {
		return usageValidationError(
			"bind cleanup mount is invalid",
			fmt.Errorf("mount %q must target %s", rawMount, bindCleanupTarget),
		)
	}
	_, err := validateBindCleanupPath(source)
	return err
}
