//go:build unix

package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

func TestReadStackEnv_ParsesDotenvWithoutTransformingValues(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	envPath := filepath.Join(stackDir, ".env")
	envBytes := []byte(strings.Join([]string{
		"# top comment",
		"   # comment with leading spaces",
		"",
		"ALPHA=plain",
		`QUOTED="quoted value"`,
		"WITH_EQUALS=left=right=tail",
		"HAS_INLINE_COMMENT=value # keep this",
		"SPACED=  leading spaces kept",
		" KEY =trim-key-only",
		`BACKSLASH=C:\tmp\keep\slashes`,
		"EMPTY=",
	}, "\n"))
	require.NoError(t, os.WriteFile(envPath, envBytes, 0o600))

	got, err := state.ReadStackEnv(stackDir)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"ALPHA":              "plain",
		"QUOTED":             `"quoted value"`,
		"WITH_EQUALS":        "left=right=tail",
		"HAS_INLINE_COMMENT": "value # keep this",
		"SPACED":             "  leading spaces kept",
		"KEY":                "trim-key-only",
		"BACKSLASH":          `C:\tmp\keep\slashes`,
		"EMPTY":              "",
	}, got)
}

func TestReadStackEnv_RejectsNonAbsoluteStackPathWithPlainError(t *testing.T) {
	t.Parallel()

	testCases := []string{
		"",
		"relative/stack",
	}

	for _, stackPath := range testCases {
		t.Run(stackPath, func(t *testing.T) {
			t.Parallel()

			got, err := state.ReadStackEnv(stackPath)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), "absolute")

			var typed *types.Error
			assert.False(t, errors.As(err, &typed))
		})
	}
}

func TestReadStackEnv_MissingEnvReturnsUsageValidationAndWrapsNotExist(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()

	got, err := state.ReadStackEnv(stackDir)
	require.Nil(t, got)

	typed := requireUsageValidationError(t, err)
	assert.Contains(t, typed.Hint, ".env")
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestReadStackEnv_RejectsMalformedLineWithoutEquals(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	envPath := filepath.Join(stackDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("OK=1\nBROKEN secret-do-not-echo\n"), 0o600))

	got, err := state.ReadStackEnv(stackDir)
	require.Nil(t, got)

	typed := requireUsageValidationError(t, err)
	assert.Contains(t, typed.Hint, "line 2")
	assert.NotContains(t, typed.Hint, "secret-do-not-echo")
	assert.NotContains(t, err.Error(), "secret-do-not-echo")
}

func TestReadStackEnv_RejectsEmptyKey(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	envPath := filepath.Join(stackDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(" =value\n"), 0o600))

	got, err := state.ReadStackEnv(stackDir)
	require.Nil(t, got)

	typed := requireUsageValidationError(t, err)
	assert.Contains(t, typed.Hint, "line 1")
	assert.Contains(t, typed.Hint, "empty key")
}

func TestReadStackEnv_RejectsDuplicateKey(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	envPath := filepath.Join(stackDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("API_KEY=first-secret\nAPI_KEY=second-secret\n"), 0o600))

	got, err := state.ReadStackEnv(stackDir)
	require.Nil(t, got)

	typed := requireUsageValidationError(t, err)
	assert.Contains(t, typed.Hint, "API_KEY")
	assert.NotContains(t, typed.Hint, "first-secret")
	assert.NotContains(t, typed.Hint, "second-secret")
	assert.NotContains(t, err.Error(), "first-secret")
	assert.NotContains(t, err.Error(), "second-secret")
}

func TestReadStackEnv_RejectsNonRegularDotEnv(t *testing.T) {
	t.Parallel()

	t.Run("directory", func(t *testing.T) {
		t.Parallel()

		stackDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(stackDir, ".env"), 0o700))

		got, err := state.ReadStackEnv(stackDir)
		require.Nil(t, got)
		requireUsageValidationError(t, err)
	})

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()

		stackDir := t.TempDir()
		target := filepath.Join(stackDir, "target.env")
		require.NoError(t, os.WriteFile(target, []byte("TOKEN=abc\n"), 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(stackDir, ".env")))

		got, err := state.ReadStackEnv(stackDir)
		require.Nil(t, got)
		requireUsageValidationError(t, err)
	})
}

func TestReadStackEnv_DoesNotModifyEnvFileOrCreateTempArtifacts(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	envPath := filepath.Join(stackDir, ".env")
	originalBytes := []byte("TOKEN=abc123\nWITH_EQUALS=a=b=c\n")
	require.NoError(t, os.WriteFile(envPath, originalBytes, 0o640))
	require.NoError(t, os.Chmod(envPath, 0o640))

	beforeInfo, err := os.Stat(envPath)
	require.NoError(t, err)
	beforeBytes, err := os.ReadFile(envPath)
	require.NoError(t, err)

	got, err := state.ReadStackEnv(stackDir)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"TOKEN":       "abc123",
		"WITH_EQUALS": "a=b=c",
	}, got)

	afterInfo, err := os.Stat(envPath)
	require.NoError(t, err)
	afterBytes, err := os.ReadFile(envPath)
	require.NoError(t, err)

	assert.Equal(t, beforeBytes, afterBytes)
	assert.Equal(t, beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())

	_, err = os.Stat(envPath + ".tmp")
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func requireUsageValidationError(t *testing.T, err error) *types.Error {
	t.Helper()

	require.Error(t, err)

	var typed *types.Error
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
	assert.NotEmpty(t, typed.Message)
	assert.NotEmpty(t, typed.Hint)

	return typed
}
