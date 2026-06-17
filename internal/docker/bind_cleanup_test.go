package docker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestRemoveBindMountContents_UsesBoundedDockerRun(t *testing.T) {
	t.Parallel()

	stackPath := filepath.Join(t.TempDir(), "stack with spaces")
	var captured []string
	client, err := New(WithCommandExecutor(func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		captured = append([]string(nil), cmd.argv...)
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	require.NoError(t, RemoveBindMountContents(t.Context(), client, stackPath))

	require.Equal(t, []string{
		"run",
		"--rm",
		"--pull=never",
		"--network",
		"none",
		"--mount",
		"type=bind,src=" + filepath.Clean(stackPath) + ",dst=/wdm-delete-target",
		"docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
		"find",
		"/wdm-delete-target",
		"-xdev",
		"-depth",
		"-mindepth",
		"1",
		"-delete",
	}, captured)
	assert.NotContains(t, captured, "sh")
	assert.NotContains(t, captured, "-c")
	assert.NotContains(t, captured, "--privileged")
	assert.NotContains(t, strings.Join(captured, " "), "down -v")
}

func TestEnsureBindMountCleanupHelperAvailable_InspectsDigestPinnedImage(t *testing.T) {
	t.Parallel()

	var captured []string
	client, err := New(WithCommandExecutor(func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		captured = append([]string(nil), cmd.argv...)
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	require.NoError(t, EnsureBindMountCleanupHelperAvailable(t.Context(), client))

	require.Equal(t, []string{
		"image",
		"inspect",
		"--format",
		imageDigestInspectFormat,
		"docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
	}, captured)
}

func TestRemoveBindMountContents_RefusesUnsafePathBeforeExecutor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "relative", path: "stack/app"},
		{name: "comma", path: filepath.Join(t.TempDir(), "stack,with-comma")},
		{name: "newline", path: filepath.Join(t.TempDir(), "stack\nnewline")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			executed := false
			client, err := New(WithCommandExecutor(func(context.Context, commandSpec) (CommandResult, error) {
				executed = true
				return CommandResult{}, nil
			}))
			require.NoError(t, err)

			err = RemoveBindMountContents(t.Context(), client, tt.path)
			require.Error(t, err)
			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
			assert.False(t, executed, "an unsafe bind path must not spawn docker")
		})
	}
}

func TestBindMountCleanupRunAllowlistRejectsUnsafeShapes(t *testing.T) {
	t.Parallel()

	stackPath := filepath.Join(t.TempDir(), "stack")
	validMount := "type=bind,src=" + filepath.Clean(stackPath) + ",dst=/wdm-delete-target"

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "tag-only helper image",
			argv: []string{
				"run", "--rm", "--pull=never", "--network", "none", "--mount",
				validMount, "docker.io/library/busybox:1.36.1", "find",
				"/wdm-delete-target", "-xdev", "-depth", "-mindepth", "1", "-delete",
			},
		},
		{
			name: "missing local-only pull policy",
			argv: []string{
				"run", "--rm", "--network", "none", "--mount",
				validMount, "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
				"find", "/wdm-delete-target", "-xdev", "-depth", "-mindepth", "1", "-delete",
			},
		},
		{
			name: "pull policy can never pull",
			argv: []string{
				"run", "--rm", "--pull=always", "--network", "none", "--mount",
				validMount, "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
				"find", "/wdm-delete-target", "-xdev", "-depth", "-mindepth", "1", "-delete",
			},
		},
		{
			name: "shell rejected",
			argv: []string{
				"run", "--rm", "--pull=never", "--network", "none", "--mount",
				validMount, "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
				"sh", "-c", "rm -rf /wdm-delete-target/*",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCommandSpec(commandSpec{argv: tt.argv})
			require.Error(t, err)
			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
		})
	}
}
