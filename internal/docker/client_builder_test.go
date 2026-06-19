package docker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The project-scoped list and inspect builders are the typed-argv seams that
// feed the status/cleanup paths. These tests pin the exact argv each builder
// emits, prove that argv survives validateCommandSpec (the allowlist that
// guards the no-shell-interpolation invariant), and prove a tampered variant
// of the same argv is rejected — so a future builder change that drifts from
// the allowlist fails loudly.

func TestBuildProjectContainerListCommand_BuildsAllowlistedArgv(t *testing.T) {
	t.Parallel()

	cmd, err := buildProjectContainerListCommand("n8n")
	require.NoError(t, err)
	require.Equal(t, []string{
		"container",
		"ls",
		"--all",
		"--filter",
		composeProjectLabelFilterPrefix + "n8n",
		"--format",
		containerListFormat,
	}, cmd.argv)

	require.NoError(t, validateCommandSpec(cmd))
}

func TestBuildProjectContainerListCommand_RejectsInvalidProjectName(t *testing.T) {
	t.Parallel()

	_, err := buildProjectContainerListCommand("N8N; reboot")
	requireUsageValidationError(t, err)
}

func TestBuildProjectContainerListCommand_TamperedArgvRejectedByAllowlist(t *testing.T) {
	t.Parallel()

	cmd, err := buildProjectContainerListCommand("n8n")
	require.NoError(t, err)

	tampered := commandSpec{argv: append(cmd.argv, "--no-trunc")}
	requireUsageValidationError(t, validateCommandSpec(tampered))
}

func TestBuildContainerInspectCommand_BuildsAllowlistedArgv(t *testing.T) {
	t.Parallel()

	const id = "abc123def456"

	cmd, err := buildContainerInspectCommand(id)
	require.NoError(t, err)
	require.Equal(t, []string{
		"container",
		"inspect",
		"--format",
		containerInspectFormat,
		id,
	}, cmd.argv)

	require.NoError(t, validateCommandSpec(cmd))
}

func TestBuildContainerInspectCommand_RejectsInvalidID(t *testing.T) {
	t.Parallel()

	_, err := buildContainerInspectCommand("not a hex id")
	requireUsageValidationError(t, err)
}

func TestBuildContainerInspectCommand_TamperedArgvRejectedByAllowlist(t *testing.T) {
	t.Parallel()

	cmd, err := buildContainerInspectCommand("abc123def456")
	require.NoError(t, err)
	require.NoError(t, validateCommandSpec(cmd))

	// Swapping the pinned inspect format for an attacker-chosen one must be
	// rejected: the allowlist matches the exact format literal.
	tampered := commandSpec{argv: []string{
		"container",
		"inspect",
		"--format",
		"{{json .}}",
		"abc123def456",
	}}
	requireUsageValidationError(t, validateCommandSpec(tampered))
}

func TestBuildProjectVolumeListCommand_BuildsAllowlistedArgv(t *testing.T) {
	t.Parallel()

	cmd, err := buildProjectVolumeListCommand("n8n")
	require.NoError(t, err)
	require.Equal(t, []string{
		"volume",
		"ls",
		"--filter",
		composeProjectLabelFilterPrefix + "n8n",
		"--format",
		volumeListFormat,
	}, cmd.argv)

	require.NoError(t, validateCommandSpec(cmd))
}

func TestBuildProjectVolumeListCommand_RejectsInvalidProjectName(t *testing.T) {
	t.Parallel()

	_, err := buildProjectVolumeListCommand("n8n && rm -rf /")
	requireUsageValidationError(t, err)
}

func TestBuildProjectVolumeListCommand_TamperedArgvRejectedByAllowlist(t *testing.T) {
	t.Parallel()

	cmd, err := buildProjectVolumeListCommand("n8n")
	require.NoError(t, err)

	tampered := commandSpec{argv: append(cmd.argv, "--quiet")}
	requireUsageValidationError(t, validateCommandSpec(tampered))
}
