package docker

import (
	"context"
	"errors"
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

	require.NoError(t, EnsureBindMountCleanupHelperAvailable(t.Context(), client, nil))

	require.Equal(t, []string{
		"image",
		"inspect",
		"--format",
		imageDigestInspectFormat,
		"docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
	}, captured)
}

// TestEnsureBindMountCleanupHelperAvailable_PullsPinnedDigestWhenImageAbsent
// pins issue #174: wdm never pulled the digest-pinned cleanup helper, so
// every delete failed closed on machines that had not pulled it manually.
// When the preflight inspect finds the image absent, it must pull the exact
// pinned digest itself — still pre-mutation — and surface the pull as a
// progress step.
func TestEnsureBindMountCleanupHelperAvailable_PullsPinnedDigestWhenImageAbsent(t *testing.T) {
	t.Parallel()

	var captured [][]string
	client, err := New(WithCommandExecutor(func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		captured = append(captured, append([]string(nil), cmd.argv...))
		if cmd.argv[1] == "inspect" {
			return CommandResult{Stderr: "No such image: " + bindCleanupImage, ExitCode: 1},
				errors.New("exit status 1")
		}
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	var steps []string
	onProgress := types.ProgressFn(func(step string, _ float64, _ string) {
		steps = append(steps, step)
	})

	require.NoError(t, EnsureBindMountCleanupHelperAvailable(t.Context(), client, onProgress))

	require.Len(t, captured, 2)
	assert.Equal(t, "inspect", captured[0][1])
	require.Equal(t, []string{"image", "pull", bindCleanupImage}, captured[1])
	assert.Equal(t, []string{types.StepDeleteHelperPull}, steps)
}

// TestEnsureBindMountCleanupHelperAvailable_FailsClosedWhenPullFails pins the
// offline/registry-error half of issue #174: when the image is absent AND the
// pull fails, the preflight still fails closed pre-mutation with the manual
// `docker pull` hint.
func TestEnsureBindMountCleanupHelperAvailable_FailsClosedWhenPullFails(t *testing.T) {
	t.Parallel()

	var captured [][]string
	client, err := New(WithCommandExecutor(func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		captured = append(captured, append([]string(nil), cmd.argv...))
		return CommandResult{Stderr: "dial tcp: no such host", ExitCode: 1},
			errors.New("exit status 1")
	}))
	require.NoError(t, err)

	err = EnsureBindMountCleanupHelperAvailable(t.Context(), client, nil)
	require.Error(t, err)
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeGeneric, typedErr.Code)
	assert.Contains(t, typedErr.Message, "delete cleanup helper image is unavailable")
	assert.Contains(t, typedErr.Hint, "docker pull docker.io/library/busybox@sha256")

	require.Len(t, captured, 2, "the preflight must attempt the pull before failing closed")
	require.Equal(t, []string{"image", "pull", bindCleanupImage}, captured[1])
}

// TestBindCleanupHelperPullAllowlistRejectsNonPinnedRefs proves the argv
// allowlist accepts only the exact pinned-digest helper pull: no tag-based
// pulls, no other digests, no bare top-level `pull`.
func TestBindCleanupHelperPullAllowlistRejectsNonPinnedRefs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
	}{
		{name: "tag-based pull", argv: []string{"image", "pull", "docker.io/library/busybox:1.36.1"}},
		{
			name: "different digest",
			argv: []string{"image", "pull", "docker.io/library/alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		},
		{name: "top-level pull", argv: []string{"pull", bindCleanupImage}},
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
