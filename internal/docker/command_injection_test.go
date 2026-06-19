package docker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// owaspCommandInjectionPayloads is the canonical command-injection
// payload set from the threat model: shell command separators, logical operators, pipes, command
// substitution (backtick and $), redirection, newline splices,
// null-byte and flag-injection shapes. Every payload, fed through any
// name-shaped Docker input (project / network / image / service), must
// be refused by validation because internal/docker accepts only a
// strict character class — there is no shell to inject into.
var owaspCommandInjectionPayloads = []struct {
	name    string
	payload string
}{
	{name: "semicolon separator", payload: "wdm; ls"},
	{name: "double ampersand", payload: "wdm && whoami"},
	{name: "single ampersand background", payload: "wdm & whoami"},
	{name: "pipe to cat passwd", payload: "wdm | cat /etc/passwd"},
	{name: "double pipe", payload: "wdm || id"},
	{name: "backtick substitution", payload: "wdm`whoami`"},
	{name: "dollar paren substitution", payload: "wdm$(whoami)"},
	{name: "dollar brace expansion", payload: "wdm${PATH}"},
	{name: "output redirection", payload: "wdm > /tmp/owned"},
	{name: "input redirection", payload: "wdm < /etc/shadow"},
	{name: "newline splice", payload: "wdm\nrm -rf /"},
	{name: "carriage return splice", payload: "wdm\rreboot"},
	{name: "null byte", payload: "wdm\x00ls"},
	{name: "leading dash flag injection", payload: "--privileged"},
	{name: "single quote breakout", payload: "wdm' ; ls '"},
	{name: "double quote breakout", payload: "wdm\" ; ls \""},
	{name: "embedded space", payload: "wdm app"},
	{name: "glob expansion", payload: "wdm*"},
	{name: "tilde expansion", payload: "~root"},
	{name: "subshell parens", payload: "(ls)"},
}

// TestCommandInjection_NameShapedInputsRefuseOWASPPayloads drives the
// full OWASP payload set through every public Docker wrapper whose
// input is a name-shaped token — project names (ComposeUp / ComposeDown
// / ComposePull / ComposeRestart / InspectProjectContainers /
// ListProjectNamedVolumes),
// network names (EnsureNetwork / RemoveNetwork), named volume removal
// (RemoveNamedVolume), image references (InspectImageDigest),
// and service names (ComposeLogs). Each payload must be refused as a
// typed ErrCodeUsageValidation, and the injected command executor must
// NEVER be invoked: validation rejects the input before argv is even
// built, so no process is spawned (PRD §12, §31).
func TestCommandInjection_NameShapedInputsRefuseOWASPPayloads(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	composeFile := filepath.Join(stackDir, "docker-compose.yml")
	envFile := filepath.Join(stackDir, ".env")

	// project wires a ComposeProject whose only hostile field is the
	// project name, so a refusal is attributable to the name validator.
	project := func(name string) ComposeProject {
		return ComposeProject{
			ComposeFile: composeFile,
			EnvFile:     envFile,
			ProjectName: name,
		}
	}

	vectors := []struct {
		name string
		call func(ctx context.Context, client Client, payload string) error
	}{
		{
			name: "compose up project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeUp(ctx, client, project(payload), ComposeUpOptions{})
			},
		},
		{
			name: "compose down project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeDown(ctx, client, project(payload))
			},
		},
		{
			name: "compose down remove images project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeDownRemoveImages(ctx, client, project(payload))
			},
		},
		{
			name: "compose pull project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposePull(ctx, client, project(payload))
			},
		},
		{
			name: "compose restart project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeRestart(ctx, client, project(payload))
			},
		},
		{
			name: "compose stop project name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeStop(ctx, client, project(payload))
			},
		},
		{
			name: "container list project name",
			call: func(ctx context.Context, client Client, payload string) error {
				_, err := InspectProjectContainers(ctx, client, payload)
				return err
			},
		},
		{
			name: "volume list project name",
			call: func(ctx context.Context, client Client, payload string) error {
				_, err := ListProjectNamedVolumes(ctx, client, payload)
				return err
			},
		},
		{
			name: "network name",
			call: func(ctx context.Context, client Client, payload string) error {
				return EnsureNetwork(ctx, client, NetworkSpec{Name: payload})
			},
		},
		{
			name: "network removal name",
			call: func(ctx context.Context, client Client, payload string) error {
				return RemoveNetwork(ctx, client, payload)
			},
		},
		{
			name: "named volume removal name",
			call: func(ctx context.Context, client Client, payload string) error {
				return RemoveNamedVolume(ctx, client, payload)
			},
		},
		{
			name: "image reference",
			call: func(ctx context.Context, client Client, payload string) error {
				_, err := InspectImageDigest(ctx, client, payload)
				return err
			},
		},
		{
			name: "compose logs service name",
			call: func(ctx context.Context, client Client, payload string) error {
				return ComposeLogs(
					ctx,
					client,
					project("wdm-app"),
					ComposeLogsOptions{Services: []string{payload}},
					func(ComposeLogEntry) {},
				)
			},
		},
	}

	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			t.Parallel()

			for _, tc := range owaspCommandInjectionPayloads {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					executed := false
					client, err := New(
						WithCommandExecutor(func(context.Context, commandSpec) (CommandResult, error) {
							executed = true
							return CommandResult{}, nil
						}),
						WithStreamExecutor(func(context.Context, commandSpec, RawLogSink) error {
							executed = true
							return nil
						}),
					)
					require.NoError(t, err)

					err = vector.call(t.Context(), client, tc.payload)
					require.Error(t, err)

					var typedErr *types.Error
					require.ErrorAs(t, err, &typedErr)
					assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code,
						"hostile %q must be refused as usage validation", tc.payload)
					assert.False(t, executed,
						"a rejected injection payload must never spawn a process")
				})
			}
		})
	}
}

// TestCommandInjection_HostilePathContentReachesExecutorAsSingleArgv is
// the structural-safety half of the proof: when a
// hostile value DOES survive input validation — an absolute compose /
// env path is only required to be non-blank and absolute, so a path
// whose own name embeds shell metacharacters passes — it must reach the
// executor as exactly ONE argv element, byte-identical, never spliced
// into a shell string. The argv-only contract means there is no shell
// interpreter in the call: no element is "sh" or "-c", and the
// metacharacter-bearing path occupies a single, unsplit token. This is
// the defense-in-depth backstop for any future input whose validator is
// looser than the strict name validators.
func TestCommandInjection_HostilePathContentReachesExecutorAsSingleArgv(t *testing.T) {
	t.Parallel()

	// A directory whose NAME embeds command-injection metacharacters.
	// filepath.Clean leaves it intact; validateAbsolutePath accepts it
	// because it is non-blank and absolute.
	hostileDir := filepath.Join(t.TempDir(), "stack; rm -rf / && echo `whoami` $(id)")
	hostileCompose := filepath.Join(hostileDir, "docker-compose.yml")
	hostileEnv := filepath.Join(hostileDir, ".env")
	project := ComposeProject{
		ComposeFile: hostileCompose,
		EnvFile:     hostileEnv,
		ProjectName: "wdm-app",
	}

	var captured []string
	client, err := New(WithCommandExecutor(func(_ context.Context, cmd commandSpec) (CommandResult, error) {
		captured = append([]string(nil), cmd.argv...)
		return CommandResult{}, nil
	}))
	require.NoError(t, err)

	require.NoError(t, ComposeUp(t.Context(), client, project, ComposeUpOptions{}))

	require.NotEmpty(t, captured, "the executor must have received the argv")
	assert.NotContains(t, captured, "sh", "argv must not contain a shell interpreter")
	assert.NotContains(t, captured, "-c", "argv must not contain a shell command flag")
	assert.NotContains(t, captured, "bash")

	// The hostile paths must each appear as exactly one byte-identical
	// argv element — never split on whitespace or metacharacters.
	assert.Contains(t, captured, hostileCompose,
		"the hostile compose path must be a single unsplit argv element")
	assert.Contains(t, captured, hostileEnv,
		"the hostile env path must be a single unsplit argv element")

	// No argv element may be a fragment of the payload (e.g. "rm",
	// "-rf", "whoami") — that would mean the path was tokenized.
	for _, fragment := range []string{"rm", "-rf", "&&", "echo", "`whoami`", "$(id)", ";"} {
		assert.NotContains(t, captured, fragment,
			"metacharacter fragment %q must not appear as its own argv element", fragment)
	}
}

// TestCommandInjection_AllowlistRejectsShellAndInjectedTokens proves the
// final argv-allowlist backstop: even if a caller could somehow hand a
// crafted argv directly to validateCommandSpec — a shell invocation, an
// injected extra flag appended to an otherwise-valid shape, or a
// metacharacter-bearing token in a name position — the allowlist
// refuses it as usage validation. validateCommandSpec runs on every
// Run before the executor and again inside the default executor, so a
// shape outside the closed set can never spawn a process (PRD §12, §31).
func TestCommandInjection_AllowlistRejectsShellAndInjectedTokens(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()
	composeFile := filepath.Join(stackDir, "docker-compose.yml")
	envFile := filepath.Join(stackDir, ".env")

	tests := []struct {
		name string
		argv []string
	}{
		{
			name: "explicit shell wrapper",
			argv: []string{"sh", "-c", "docker compose up -d"},
		},
		{
			name: "bash shell wrapper",
			argv: []string{"bash", "-c", "rm -rf /"},
		},
		{
			name: "compose v1 binary",
			argv: []string{"compose", "up", "-d", "; rm -rf /"},
		},
		{
			name: "injected flag after valid up shape",
			argv: []string{
				"compose", "-f", composeFile, "--env-file", envFile,
				"--project-name", "wdm-app", "up", "-d", "--privileged",
			},
		},
		{
			name: "metacharacter in project name position",
			argv: []string{
				"compose", "-f", composeFile, "--env-file", envFile,
				"--project-name", "wdm-app; ls", "up", "-d",
			},
		},
		{
			name: "down with volume flag injection",
			argv: []string{
				"compose", "-f", composeFile, "--env-file", envFile,
				"--project-name", "wdm-app", "down", "-v",
			},
		},
		{
			name: "trailing command chained onto network create",
			argv: []string{"network", "create", "wdm_default && reboot"},
		},
		{
			name: "command chained onto network create app label value",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"--label", "wdm.app=n8n; reboot",
				"wdm_default",
			},
		},
		{
			name: "uppercase rejected in network create app label value",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"--label", "wdm.app=N8N",
				"wdm_default",
			},
		},
		{
			name: "unexpected key rejected on network create label",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"--label", "com.example.evil=1",
				"wdm_default",
			},
		},
		{
			name: "wrong managed label value rejected on network create",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=false",
				"--label", "wdm.app=n8n",
				"wdm_default",
			},
		},
		{
			name: "lone managed label without app label rejected on network create",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"wdm_default",
			},
		},
		{
			name: "extra label injected onto network create",
			argv: []string{
				"network", "create",
				"--label", "wdm.managed=true",
				"--label", "wdm.app=n8n",
				"--label", "evil=1",
				"wdm_default",
			},
		},
		{
			name: "trailing command chained onto network remove",
			argv: []string{"network", "rm", "wdm_default && reboot"},
		},
		{
			name: "force flag injected onto network remove",
			argv: []string{"network", "rm", "--force", "wdm_default"},
		},
		{
			name: "trailing command chained onto volume remove",
			argv: []string{"volume", "rm", "wdm-n8n_data; reboot"},
		},
		{
			name: "force flag injected onto volume remove",
			argv: []string{"volume", "rm", "--force", "wdm-n8n_data"},
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
