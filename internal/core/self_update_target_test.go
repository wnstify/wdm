package core_test

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// The writability gate decides whether wdm may replace its own
// binary without privilege escalation BEFORE any download/staging is
// attempted. These tests drive it through the injected os.Executable /
// EvalSymlinks seams so they never depend on the test binary's own install
// location, and they NEVER shell out to sudo (the no-elevation invariant
// is structural — the gate has no exec path).

// writableExecutable creates a fake wdm executable file inside a fresh
// 0o700 directory and returns the file path. t.TempDir can be 0o775 on
// umask-0002 hosts, so the parent is pinned 0o700 first to keep
// restrictive-mode behavior portable.
func writableExecutable(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))
	dir := filepath.Join(parent, "bin")
	require.NoError(t, os.Mkdir(dir, 0o700))
	exe := filepath.Join(dir, "wdm")
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/true\n"), 0o755))
	return exe
}

func requireCoreUsageError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want ErrCodeUsageValidation (exit 2), got %v", err)
}

func TestResolveSelfUpdateTarget_WritableLocationSucceeds(t *testing.T) {
	t.Parallel()

	exe := writableExecutable(t)

	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return exe, nil },
		func(p string) (string, error) { return p, nil },
	)
	require.NoError(t, err)
	assert.Equal(t, exe, target.Path)
	assert.Equal(t, filepath.Dir(exe), target.Dir)

	// The probe leaves the directory untouched: only the fake binary remains.
	entries, err := os.ReadDir(target.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "wdm", entries[0].Name())
}

func TestResolveSelfUpdateTarget_FollowsSymlinkToRealDir(t *testing.T) {
	t.Parallel()

	realExe := writableExecutable(t)

	// A symlink in a separate directory pointing at the real binary. The
	// gate must judge the REAL file's directory (where a replace happens),
	// not the link's directory.
	linkDir := t.TempDir()
	require.NoError(t, os.Chmod(linkDir, 0o700))
	link := filepath.Join(linkDir, "wdm")

	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return link, nil },
		// Stand-in EvalSymlinks: resolves the link path to the real binary.
		func(p string) (string, error) {
			if p == link {
				return realExe, nil
			}
			return p, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, realExe, target.Path)
	assert.Equal(t, filepath.Dir(realExe), target.Dir)
}

func TestResolveSelfUpdateTarget_RefusesNonWritableLocation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory write permissions; cannot prove refusal")
	}
	t.Parallel()

	// A read-only directory holding the fake binary: an unprivileged user
	// cannot create a sibling, so the gate must refuse.
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))
	roDir := filepath.Join(parent, "ro")
	require.NoError(t, os.Mkdir(roDir, 0o700))
	exe := filepath.Join(roDir, "wdm")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	require.NoError(t, os.Chmod(roDir, 0o500)) // r-x: no write
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return exe, nil },
		func(p string) (string, error) { return p, nil },
	)
	requireCoreUsageError(t, err)
	assert.Empty(t, target.Path)

	// The refusal carries the manual-install hint and names no sudo path.
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, core.ManualInstallHintForTest, typed.Hint)
	assert.Contains(t, typed.Hint, "~/.local/bin/wdm")
	assert.NotContains(t, typed.Hint, "sudo")
	assert.NotContains(t, typed.Message, "sudo")
}

func TestResolveSelfUpdateTarget_RefusesNonexistentLocation(t *testing.T) {
	t.Parallel()

	exe := filepath.Join(t.TempDir(), "gone", "wdm")

	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return exe, nil },
		func(p string) (string, error) { return p, nil },
	)
	requireCoreUsageError(t, err)
	assert.Empty(t, target.Path)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, core.ManualInstallHintForTest, typed.Hint)
}

func TestResolveSelfUpdateTarget_ExecutableLookupErrorIsTyped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom: no executable")
	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return "", sentinel },
		func(p string) (string, error) { return p, nil },
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"want ErrCodeGeneric (exit 1), got %v", err)
	assert.Empty(t, target.Path)
}

func TestResolveSelfUpdateTarget_SymlinkResolveErrorIsTyped(t *testing.T) {
	t.Parallel()

	exe := writableExecutable(t)
	sentinel := errors.New("boom: broken symlink")
	target, err := core.ResolveSelfUpdateTargetForTest(
		func() (string, error) { return exe, nil },
		func(string) (string, error) { return "", sentinel },
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"want ErrCodeGeneric (exit 1), got %v", err)
	assert.Empty(t, target.Path)
}

// TestResolveSelfUpdateTarget_GateImportsNoExec is a structural guard: the
// gate's source must NOT import os/exec, so a refusal can never reach for
// sudo or any other process (PRD §11/§14: wdm never escalates privilege).
// Parsing imports (rather than grepping the source) avoids false positives
// from the gate's own doc comment, which legitimately mentions sudo while
// explaining it is never invoked.
func TestResolveSelfUpdateTarget_GateImportsNoExec(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	gateFile := filepath.Join(filepath.Dir(thisFile), "self_update_target.go")

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, gateFile, nil, parser.ImportsOnly)
	require.NoError(t, err)

	for _, imp := range parsed.Imports {
		path, uErr := strconv.Unquote(imp.Path.Value)
		require.NoError(t, uErr)
		assert.NotEqual(t, "os/exec", path,
			"the writability gate must never import os/exec — no privilege escalation")
	}
}
