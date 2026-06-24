package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
)

// These tests pin the top-level `edit` command: the --compose/--env path
// resolution, the scriptable --print-path exit, the no-TTY-no-print-path
// guard, the post-edit ValidateStack call for both flags, and the T17
// compose security warning. They drive RunE end-to-end through NewRootCmd
// using the shared fakeEngine double, overriding launchEditor and the TTY
// gate so no real editor spawns and tests stay headless.

// withFakeEditor swaps launchEditor for a recorder and forces editTTYReady
// true so the editor branch is reachable without a real terminal. It returns
// a pointer to the captured argv and restores both globals on cleanup.
func withFakeEditor(t *testing.T) *[]string {
	t.Helper()

	var got []string
	origLaunch := launchEditor
	origTTY := editTTYReadyFn
	launchEditor = func(argv []string) error {
		got = argv
		return nil
	}
	editTTYReadyFn = func() bool { return true }
	t.Cleanup(func() {
		launchEditor = origLaunch
		editTTYReadyFn = origTTY
	})
	return &got
}

func TestEdit_Compose_ResolvesOverridePathAndValidates(t *testing.T) {
	got := withFakeEditor(t)
	t.Setenv("VISUAL", "vi")
	t.Setenv("EDITOR", "")

	fake := &fakeEngine{ensureOverridePath: "/home/u/docker/vaultwarden/docker-compose.override.yml"}

	_, stderr, err := runLeaf(t, fake, "edit", "vaultwarden", "--compose")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", fake.ensureOverrideAppID, "--compose must resolve the override path")
	assert.Empty(t, fake.ensureEnvAppID, "--compose must not touch the env overlay")
	assert.Equal(t, []string{"vi", "/home/u/docker/vaultwarden/docker-compose.override.yml"}, *got)
	assert.True(t, fake.validateStackCalled, "ValidateStack must run after a compose edit")
	assert.Equal(t, "vaultwarden", fake.validateStackAppID)
	assert.Contains(t, stderr, "warning:", "compose edit must print the T17 security warning")
	assert.Contains(t, stderr, "wdm.managed")
}

func TestEdit_Env_ResolvesEnvPathAndValidates(t *testing.T) {
	got := withFakeEditor(t)
	t.Setenv("VISUAL", "vi")
	t.Setenv("EDITOR", "")

	fake := &fakeEngine{ensureEnvPath: "/home/u/docker/vaultwarden/.env.user"}

	_, stderr, err := runLeaf(t, fake, "edit", "vaultwarden", "--env")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", fake.ensureEnvAppID, "--env must resolve the env overlay path")
	assert.Empty(t, fake.ensureOverrideAppID, "--env must not touch the compose override")
	assert.Equal(t, []string{"vi", "/home/u/docker/vaultwarden/.env.user"}, *got)
	assert.True(t, fake.validateStackCalled, "ValidateStack must run after an env edit")
	assert.NotContains(t, stderr, "warning:", "env edit must NOT print the compose security warning")
}

func TestEdit_PrintPath_PrintsPathAndExitsWithoutEditor(t *testing.T) {
	// No fake editor: --print-path must short-circuit before any launch, so
	// the original launchEditor (which would spawn) is never reached. Force
	// the TTY gate false to prove --print-path runs BEFORE it.
	origTTY := editTTYReadyFn
	editTTYReadyFn = func() bool { return false }
	t.Cleanup(func() { editTTYReadyFn = origTTY })

	fake := &fakeEngine{ensureOverridePath: "/home/u/docker/app/docker-compose.override.yml"}

	stdout, _, err := runLeaf(t, fake, "edit", "app", "--compose", "--print-path")
	require.NoError(t, err)

	assert.Equal(t, "/home/u/docker/app/docker-compose.override.yml", strings.TrimSpace(stdout))
	assert.False(t, fake.validateStackCalled, "--print-path must not validate or launch an editor")
}

func TestEdit_NoTTY_NoPrintPath_FailsWithGuidance(t *testing.T) {
	origTTY := editTTYReadyFn
	editTTYReadyFn = func() bool { return false }
	t.Cleanup(func() { editTTYReadyFn = origTTY })

	fake := &fakeEngine{ensureEnvPath: "/home/u/docker/app/.env.user"}

	_, _, err := runLeaf(t, fake, "edit", "app", "--env")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--print-path", "the no-TTY failure must point at --print-path")
	assert.False(t, fake.validateStackCalled, "a refused edit must not validate")
}

func TestEdit_RequiresExactlyOneTarget(t *testing.T) {
	fake := &fakeEngine{}

	_, _, errNeither := runLeaf(t, fake, "edit", "app")
	require.Error(t, errNeither, "neither --compose nor --env must fail")

	_, _, errBoth := runLeaf(t, fake, "edit", "app", "--compose", "--env")
	require.Error(t, errBoth, "both --compose and --env must fail")
}

// TestEdit_ResolveEditorArgv_NoShellInterpolation proves the argv assembly
// keeps a metacharacter-laden editor value as literal argv elements (never a
// shell string), the typed-argv invariant the edit command relies on.
func TestEdit_ResolveEditorArgv_NoShellInterpolation(t *testing.T) {
	argv, err := engine.ResolveEditorArgv("code -w; rm -rf /", "", "/path/to/file")
	require.NoError(t, err)
	assert.Equal(t, []string{"code", "-w;", "rm", "-rf", "/", "/path/to/file"}, argv)
}
