package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the wdm.v1 JSON envelope each lifecycle leaf emits on
// stdout under --json (PRD §32). The pkg/types contract tests already pin the
// exhaustive snake_case keys on each result type; here the representative-key
// checks only confirm the right payload reached the envelope's data object.

// decodeEnvelopeData decodes one line of stdout as a wdm.v1 envelope,
// asserts the schema and that data is a JSON object, and returns the
// data decoded into a map for representative-key assertions. require is
// used throughout because a malformed envelope makes every downstream
// assertion meaningless (golang-stretchr-testify: require for
// preconditions).
func decodeEnvelopeData(t *testing.T, line string) map[string]any {
	t.Helper()

	var env types.Envelope
	require.NoError(t, json.Unmarshal([]byte(line), &env), "stdout line is not a JSON envelope")
	require.Equal(t, types.EnvelopeSchema, env.Schema, "envelope schema must be wdm.v1")

	// PRD §32 mandates envelope.data is a JSON object, never an array or
	// scalar. NewEnvelope enforces this on the write side; re-checking the
	// decoded bytes proves the leaf did not bypass it.
	trimmed := strings.TrimSpace(string(env.Data))
	require.True(t, strings.HasPrefix(trimmed, "{"), "envelope data must be a JSON object, got %q", trimmed)

	var data map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &data), "envelope data must decode into an object")
	return data
}

// nonEmptyLines splits stdout into the JSON lines NDJSON consumers see.
// It drops whitespace-only elements wherever they appear, so the strict
// "nothing but the envelope bytes" discipline is pinned separately by
// the raw-stdout equality assertions alongside each line-count check.
func nonEmptyLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// --- Point 1: apps install --json -> exactly one envelope wrapping InstallResult.

func TestAppsInstall_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		installResult: &types.InstallResult{
			AppID:          "vaultwarden",
			StackPath:      "/home/test/docker/vaultwarden",
			ComposeProject: "wdm-vaultwarden",
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "install", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "install --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "/home/test/docker/vaultwarden", data["stack_path"])
	assert.Equal(t, "wdm-vaultwarden", data["compose_project"])
}

// --- Point 2: apps update --json -> one envelope wrapping UpdateResult,
// for both a dry-run-shaped and an apply-shaped result.

func TestAppsUpdate_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		result  *types.UpdateResult
		assertD func(t *testing.T, data map[string]any)
	}{
		{
			name: "dry_run_available",
			args: []string{"apps", "update", "vaultwarden", "--dry-run", "--json"},
			result: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.0.0",
				NewTemplateVersion:      "1.1.0",
				UpdatedServices:         []string{"server: img:1.0.0 -> img:1.1.0"},
				RiskClassifications:     []string{"safe"},
			},
			assertD: func(t *testing.T, data map[string]any) {
				t.Helper()
				assert.Equal(t, "vaultwarden", data["app_id"])
				assert.Equal(t, "1.0.0", data["previous_template_version"])
				assert.Equal(t, "1.1.0", data["new_template_version"])
				// Dry-run leaves no backup_path; the omitempty key is absent.
				_, hasBackup := data["backup_path"]
				assert.False(t, hasBackup, "dry-run result must not carry a backup_path")
			},
		},
		{
			name: "apply_with_backup_and_status",
			args: []string{"apps", "update", "vaultwarden", "--yes", "--json"},
			result: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.0.0",
				NewTemplateVersion:      "1.1.0",
				UpdatedServices:         []string{"server: img:1.0.0 -> img:1.1.0"},
				RiskClassifications:     []string{"safe"},
				BackupPath:              "/home/test/docker/vaultwarden/.wdm-backups/1700000000-update",
				Status:                  &types.AppStatus{AppID: "vaultwarden", State: "running"},
			},
			assertD: func(t *testing.T, data map[string]any) {
				t.Helper()
				assert.Equal(t, "vaultwarden", data["app_id"])
				assert.Equal(t, "/home/test/docker/vaultwarden/.wdm-backups/1700000000-update", data["backup_path"])
				status, ok := data["status"].(map[string]any)
				require.True(t, ok, "apply result must nest the status object")
				assert.Equal(t, "running", status["state"])
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{updateResult: tc.result}
			stdout, _, err := runLeaf(t, fake, tc.args...)
			require.NoError(t, err)

			lines := nonEmptyLines(stdout)
			require.Len(t, lines, 1, "update --json must emit exactly one envelope on stdout")
			assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

			tc.assertD(t, decodeEnvelopeData(t, lines[0]))
		})
	}
}

// --- Point 3: apps remove --json -> one envelope wrapping RemoveResult,
// with the kept-resource keys present under data.

func TestAppsRemove_JSON_EmitsResultEnvelopeWithKeptResources(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		removeResult: &types.RemoveResult{
			AppID:                 "vaultwarden",
			StackPath:             "/home/test/docker/vaultwarden",
			ComposeProject:        "wdm-vaultwarden",
			PreservedPaths:        []string{"/home/test/docker/vaultwarden"},
			RemainingNamedVolumes: []string{"wdm-vaultwarden_data"},
			RemainingNetworks:     []string{"wdm-shared"},
			Status:                &types.AppStatus{AppID: "vaultwarden", State: "removed"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "remove", "vaultwarden", "--yes", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "remove --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	// The kept-resources contract (PRD §19):
	// preserved paths, surviving named volumes, and networks left in place
	// must all surface under their snake_case keys so a --json consumer can
	// see exactly what wdm did not delete.
	assert.Equal(t, []any{"/home/test/docker/vaultwarden"}, data["preserved_paths"])
	assert.Equal(t, []any{"wdm-vaultwarden_data"}, data["remaining_named_volumes"])
	assert.Equal(t, []any{"wdm-shared"}, data["remaining_networks"])
}

// --- Point 4: apps status --json -> one envelope wrapping the AppStatus
// DIRECTLY as data.

func TestAppsStatus_JSON_WrapsAppStatusDirectly(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statusResult: &types.AppStatus{
			AppID:            "vaultwarden",
			State:            "needs_attention",
			NeedsAttention:   true,
			AttentionReasons: []string{"container_exited"},
			Services: []types.ServiceStatus{
				{Service: "server", State: "exited", NeedsAttention: true},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "status", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "status --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	// The AppStatus fields sit at the top of data — app_id and state are
	// direct keys, NOT nested under a "status" wrapper. The AppStatus IS the
	// envelope.data object.
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "needs_attention", data["state"])
	assert.Equal(t, true, data["needs_attention"])
	assert.NotContains(t, data, "status", "AppStatus must be data directly, not nested under a status key")
}

// TestAppsStatus_NeedsAttention_ExitsZero pins that a needs_attention
// stack is a successful read (exit 0), matching the leaf godoc: the exit
// code reports whether status could be read, not the app's health.
func TestAppsStatus_NeedsAttention_ExitsZero(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		statusResult: &types.AppStatus{AppID: "vaultwarden", State: "needs_attention", NeedsAttention: true},
	}

	_, _, err := runLeaf(t, fake, "apps", "status", "vaultwarden", "--json")
	assert.NoError(t, err, "a needs_attention status is a successful read and must not error")
}

// --- Point 5: apps logs --json -> NDJSON, one complete envelope per
// LogLine, no array, no batch envelope.

func TestAppsLogs_JSON_EmitsOneEnvelopePerLine(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	fake := &fakeEngine{
		logsLines: []types.LogLine{
			{Timestamp: ts, AppID: "vaultwarden", Service: "server", Stream: "stdout", Text: "started"},
			{Timestamp: ts.Add(time.Second), AppID: "vaultwarden", Service: "db", Stream: "stderr", Text: "ready"},
			{Timestamp: ts.Add(2 * time.Second), AppID: "vaultwarden", Service: "server", Stream: "stdout", Text: "listening"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "logs", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 3, "logs --json must emit one envelope per streamed line (NDJSON)")
	assert.Equal(t, strings.Join(lines, "\n")+"\n", stdout, "stdout must be exactly the NDJSON lines")

	// Each line independently decodes to a complete envelope whose data is
	// a single LogLine object carrying that line's identity — never an
	// array, never a batch wrapper.
	wantServices := []string{"server", "db", "server"}
	wantText := []string{"started", "ready", "listening"}
	for i, line := range lines {
		data := decodeEnvelopeData(t, line)
		assert.Equal(t, "vaultwarden", data["app_id"], "line %d app_id", i)
		assert.Equal(t, wantServices[i], data["service"], "line %d service", i)
		assert.Equal(t, wantText[i], data["text"], "line %d text", i)
	}
}

// TestAppsLogs_JSON_NoLines_EmitsNothing pins the empty-stream shape: a
// log read that yields zero lines writes nothing to stdout (no batch
// envelope, no empty array), so a consumer reading NDJSON sees an empty
// stream rather than a spurious object.
func TestAppsLogs_JSON_NoLines_EmitsNothing(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{logsLines: nil}
	stdout, _, err := runLeaf(t, fake, "apps", "logs", "vaultwarden", "--json")
	require.NoError(t, err)
	assert.Empty(t, stdout, "a zero-line log read must emit nothing on stdout")
	assert.False(t, fake.logLineWasNil, "logs must hand the engine a non-nil LogLineFn")
}

// --- Point 6: progress suppression. Under --json the engine receives a
// nil ProgressFn and stdout carries only envelope bytes; in plain mode
// the engine receives a non-nil ProgressFn.

func TestLifecycleProgressSuppression_JSONNilPlainNonNil(t *testing.T) {
	t.Parallel()

	// Each case runs the same leaf twice: once with --json (progress must
	// be suppressed) and once plain (progress must be wired). The fake
	// records the nil-ness of the ProgressFn it received.
	cases := []struct {
		name string
		args []string
		fake func() *fakeEngine
	}{
		{
			name: "install",
			args: []string{"apps", "install", "vaultwarden"},
			fake: func() *fakeEngine {
				return &fakeEngine{installResult: &types.InstallResult{AppID: "vaultwarden", StackPath: "/s"}}
			},
		},
		{
			name: "update",
			args: []string{"apps", "update", "vaultwarden", "--yes"},
			fake: func() *fakeEngine {
				return &fakeEngine{updateResult: &types.UpdateResult{AppID: "vaultwarden", NewTemplateVersion: "1.0.0"}}
			},
		},
		{
			name: "remove",
			args: []string{"apps", "remove", "vaultwarden", "--yes"},
			fake: func() *fakeEngine {
				return &fakeEngine{removeResult: &types.RemoveResult{AppID: "vaultwarden", StackPath: "/s"}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"_json_suppresses_progress", func(t *testing.T) {
			t.Parallel()

			fake := tc.fake()
			jsonArgs := append(append([]string{}, tc.args...), "--json")
			stdout, _, err := runLeaf(t, fake, jsonArgs...)
			require.NoError(t, err)

			assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
			lines := nonEmptyLines(stdout)
			require.Len(t, lines, 1, "--json stdout must carry only the envelope")
			assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
			// Confirm stdout is the envelope, not a plain finish screen.
			data := decodeEnvelopeData(t, lines[0])
			assert.Equal(t, "vaultwarden", data["app_id"])
		})

		t.Run(tc.name+"_plain_wires_progress", func(t *testing.T) {
			t.Parallel()

			fake := tc.fake()
			stdout, _, err := runLeaf(t, fake, tc.args...)
			require.NoError(t, err)

			assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
			// Plain mode writes a human finish screen, not an envelope, so
			// stdout must not begin with the wdm.v1 envelope shape.
			assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
				"plain mode stdout must be the finish screen, not a JSON envelope")
		})
	}
}

// TestLifecyclePassesConfirmer pins that the lifecycle leaves hand the
// engine a non-nil Confirmer (the shared cliConfirmer). The envelope
// contract does not depend on the confirmation flow, but a nil confirmer
// would make the engine refuse with ErrCodeUsageValidation, so the
// presence of the confirmer is part of the leaf's call contract.
func TestLifecyclePassesConfirmer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		fake func() *fakeEngine
	}{
		{
			name: "install",
			args: []string{"apps", "install", "vaultwarden", "--json"},
			fake: func() *fakeEngine {
				return &fakeEngine{installResult: &types.InstallResult{AppID: "vaultwarden", StackPath: "/s"}}
			},
		},
		{
			name: "update",
			args: []string{"apps", "update", "vaultwarden", "--yes", "--json"},
			fake: func() *fakeEngine {
				return &fakeEngine{updateResult: &types.UpdateResult{AppID: "vaultwarden", NewTemplateVersion: "1.0.0"}}
			},
		},
		{
			name: "remove",
			args: []string{"apps", "remove", "vaultwarden", "--yes", "--json"},
			fake: func() *fakeEngine {
				return &fakeEngine{removeResult: &types.RemoveResult{AppID: "vaultwarden", StackPath: "/s"}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := tc.fake()
			_, _, err := runLeaf(t, fake, tc.args...)
			require.NoError(t, err)
			assert.NotNil(t, fake.confirmer, "lifecycle leaf must pass a non-nil Confirmer to the engine")
		})
	}
}

// --- Point 7: error path. When the engine returns a typed *types.Error,
// stdout is EMPTY under --json (the error propagates out of Execute and
// nothing was emitted).

func TestLifecycleErrorPath_JSON_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "stack is not managed", "run wdm apps list")

	cases := []struct {
		name string
		args []string
	}{
		{"install", []string{"apps", "install", "vaultwarden", "--json"}},
		{"update", []string{"apps", "update", "vaultwarden", "--json"}},
		{"remove", []string{"apps", "remove", "vaultwarden", "--yes", "--json"}},
		{"status", []string{"apps", "status", "vaultwarden", "--json"}},
		{"logs", []string{"apps", "logs", "vaultwarden", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no envelope may be written to stdout on the error path")
		})
	}
}

// --- Point 8: the database-risk confirmation Kind literal pin (CLI side).

// TestConfirmationKindDatabaseRisk_MatchesEngineLiteral pins the
// CLI-local constant to the exact bytes the engine emits as the
// database-risk Confirmation.Kind. The engine side
// (internal/core/update.go) and the CLI side
// (internal/cli/install.go:confirmationKindDatabaseRisk) pin the SAME
// string independently — by design, because the two layers cannot import
// each other's literal across the pkg/engine facade.
// Failure direction: if the engine renamed its Kind and only this
// constant lagged, the shared cliConfirmer would no longer recognize the
// database-risk confirmation as special — it would fall through to the
// SAFE branch, where --yes auto-accepts and a TTY "y" is honored. That is
// a fail-OPEN regression: a database-risk update would be authorized
// without --accept-database-risk. Both
// sides therefore pin the literal so a one-sided rename breaks a test on
// each side rather than silently weakening the gate.
func TestConfirmationKindDatabaseRisk_MatchesEngineLiteral(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "update_database_risk", confirmationKindDatabaseRisk)
}
