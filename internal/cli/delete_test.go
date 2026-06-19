package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps delete` leaf: the verbatim
// flag-to-request mapping (including DeleteNamedVolumes staying false), the
// wdm.v1 envelope it emits under --json, progress suppression, the
// confirmer it hands the engine (wired assumeYes=false because
// delete_destructive is NOT a safe confirmation), the structural
// no-destructive-flags proof (-v/--volumes/--force/--yes all fail flag
// parsing before the engine is built), the empty-stdout error path, the
// plain finish-block content, the ExactArgs(1) refusals, and factory-error
// propagation. They drive RunE end-to-end through NewRootCmd (the honest
// path since the persistent --json flag only resolves through the root)
// reusing the shared fakeEngine / runLeaf from fake_engine_test.go and the
// decodeEnvelopeData / nonEmptyLines helpers from envelope_contract_test.go.

// --- Verbatim flag-to-request mapping. The shared fakeEngine does not
// record the DeleteRequest, so a local wrapper embeds it and overrides only
// DeleteApp to capture the request before delegating — fake_engine_test.go
// stays untouched (the apps restart precedent).

// recordingDeleteEngine embeds *fakeEngine and overrides DeleteApp to record
// the request it received. Every other method is inherited from the embedded
// fakeEngine, so the engine.Engine interface stays satisfied.
type recordingDeleteEngine struct {
	*fakeEngine
	gotReq types.DeleteRequest
}

func (r *recordingDeleteEngine) DeleteApp(
	ctx context.Context,
	req types.DeleteRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.DeleteResult, error) {
	r.gotReq = req
	return r.fakeEngine.DeleteApp(ctx, req, onProgress, confirmer)
}

// runDeleteLeaf drives one `apps delete` invocation through NewRootCmd with
// the recording engine wired as the lazy factory result, mirroring runLeaf
// but typed to the local wrapper (runLeaf returns *fakeEngine).
func runDeleteLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return eng, nil
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(args)
	root.SetContext(t.Context())

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestAppsDelete_MapsFlagsOntoRequestVerbatim(t *testing.T) {
	t.Parallel()

	rec := &recordingDeleteEngine{
		fakeEngine: &fakeEngine{deleteResult: &types.DeleteResult{AppID: "vaultwarden"}},
	}

	_, _, err := runDeleteLeaf(t, rec, "apps", "delete", "vaultwarden",
		"--confirm-name", "vaultwarden",
		"--stack-path", "/home/test/docker/vaultwarden",
		"--json")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", rec.gotReq.AppID, "the positional app-id must map onto DeleteRequest.AppID")
	assert.Equal(t, "vaultwarden", rec.gotReq.ConfirmationName,
		"--confirm-name must map verbatim onto DeleteRequest.ConfirmationName")
	assert.Equal(t, "/home/test/docker/vaultwarden", rec.gotReq.StackPath,
		"--stack-path must map onto DeleteRequest.StackPath")
	assert.False(t, rec.gotReq.DeleteNamedVolumes,
		"DeleteNamedVolumes must always be false — the CLI registers no flag that sets it")
}

func TestAppsDelete_OmittedOptionalFlagsAreEmpty(t *testing.T) {
	t.Parallel()

	rec := &recordingDeleteEngine{
		fakeEngine: &fakeEngine{deleteResult: &types.DeleteResult{AppID: "vaultwarden"}},
	}

	// No --confirm-name and no --stack-path: the leaf passes the empty
	// strings verbatim and the engine owns the mismatch refusal (the CLI
	// never pre-validates the typed name).
	_, _, err := runDeleteLeaf(t, rec, "apps", "delete", "vaultwarden", "--json")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", rec.gotReq.AppID)
	assert.Empty(t, rec.gotReq.ConfirmationName, "an omitted --confirm-name must leave ConfirmationName empty")
	assert.Empty(t, rec.gotReq.StackPath, "an omitted --stack-path must leave StackPath empty")
	assert.False(t, rec.gotReq.DeleteNamedVolumes)
}

// --- Single-envelope --json with raw-stdout byte equality and the
// snake_case keys (deleted_paths, remaining_named_volumes,
// removed_networks, retained_networks).

func TestAppsDelete_JSON_EmitsSingleResultEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		deleteResult: &types.DeleteResult{
			AppID:                 "vaultwarden",
			DeletedPaths:          []string{"docker-compose.yml", ".env", "/home/test/docker/vaultwarden"},
			RemainingNamedVolumes: []string{"wdm-vaultwarden_data"},
			RemovedNetworks:       []string{"pangolin"},
			RetainedNetworks: []types.RetainedNetwork{
				{Name: "shared", Reason: "network shared has active endpoints"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "delete --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, []any{"docker-compose.yml", ".env", "/home/test/docker/vaultwarden"}, data["deleted_paths"],
		"deleted_paths must ride under its snake_case key as a JSON array")
	assert.Equal(t, []any{"wdm-vaultwarden_data"}, data["remaining_named_volumes"])
	assert.Equal(t, []any{"pangolin"}, data["removed_networks"])
	retained, ok := data["retained_networks"].([]any)
	require.True(t, ok, "result must carry the retained_networks array")
	require.Len(t, retained, 1)
}

// --- Progress suppression and confirmer wiring. Under --json the engine
// receives a nil ProgressFn (PRD §32); in plain mode it receives a non-nil
// one. Both modes hand the engine a non-nil Confirmer wired assumeYes=false.

func TestAppsDelete_JSON_SuppressesProgress_PlainWiresIt(t *testing.T) {
	t.Parallel()

	t.Run("json_suppresses_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{deleteResult: &types.DeleteResult{AppID: "vaultwarden"}}
		stdout, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden", "--json")
		require.NoError(t, err)

		assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (PRD §32)")
		lines := nonEmptyLines(stdout)
		require.Len(t, lines, 1, "--json stdout must carry only the envelope")
		assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
	})

	t.Run("plain_wires_progress", func(t *testing.T) {
		t.Parallel()

		fake := &fakeEngine{deleteResult: &types.DeleteResult{AppID: "vaultwarden"}}
		stdout, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden")
		require.NoError(t, err)

		assert.False(t, fake.progressWasNil, "plain mode must hand the engine a non-nil ProgressFn")
		assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
			"plain mode stdout must be the finish screen, not a JSON envelope")
	})
}

// TestAppsDelete_PassesConfirmerWiredAssumeYesFalse pins that the leaf hands
// the engine a non-nil Confirmer AND that it is the shared *cliConfirmer
// wired with assumeYes=false (and acceptDBRisk=false). This is load-bearing:
// the destructive "delete_destructive" Kind is NOT a safe confirmation, so a
// future flag drift that set assumeYes=true could auto-accept it. Asserting
// the recorded confirmer's fields directly is the deterministic proof that
// no --yes path exists; the live no-TTY decline behavior for this Kind is
// already exhaustively pinned by confirm_test.go's safe-no-TTY arm (the
// delete_destructive Kind takes the same non-database-risk branch).
func TestAppsDelete_PassesConfirmerWiredAssumeYesFalse(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{deleteResult: &types.DeleteResult{AppID: "vaultwarden"}}
	_, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden", "--json")
	require.NoError(t, err)

	require.NotNil(t, fake.confirmer, "delete must pass a non-nil Confirmer to the engine")
	c, ok := fake.confirmer.(*cliConfirmer)
	require.True(t, ok, "delete must pass the shared *cliConfirmer")
	assert.False(t, c.yes, "delete must wire assumeYes=false — delete_destructive is not a safe confirmation")
	assert.False(t, c.acceptDBRisk, "delete must wire acceptDBRisk=false — delete produces no database-risk warning")
}

// --- Structural no-destructive-flags proof: each rejected flag fails flag
// parsing before the engine factory is constructed (a sentinel factory that
// fails the test if invoked). This is the same structural proof apps remove
// uses for -v, extended to the load-bearing --yes exclusion.

func TestAppsDelete_RejectsDestructiveAndYesFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{name: "yes is not registered", args: []string{"apps", "delete", "vaultwarden", "--yes"}},
		{name: "v shorthand is not registered", args: []string{"apps", "delete", "vaultwarden", "-v"}},
		{name: "volumes is not registered", args: []string{"apps", "delete", "vaultwarden", "--volumes"}},
		{name: "force is not registered", args: []string{"apps", "delete", "vaultwarden", "--force"}},
		{name: "purge is not registered", args: []string{"apps", "delete", "vaultwarden", "--purge"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The factory must never be reached: flag parsing runs before RunE,
			// so an unknown flag cannot construct the engine.
			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an unknown-flag refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "an unknown flag must surface an error")
			assert.Empty(t, outBuf.String(), "an unknown-flag refusal must write nothing to stdout")
		})
	}
}

// --- Error path. A typed *types.Error propagates out of Execute, and stdout
// stays empty in BOTH the --json and plain modes.

func TestAppsDelete_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUserCanceled, "deletion canceled", "re-run and confirm")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden", "--json"}},
		{"plain", []string{"apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "a typed engine error must propagate out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// --- Plain finish-block content: the permanent-deletion headline, the
// deleted-paths list, the remaining named volumes as a bulleted list, the
// networks removed, and the manual `docker network rm` hint for any retained
// network (only when non-empty).

func TestAppsDelete_PlainFinish_RendersDeletedPathsAndSurvivors(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		deleteResult: &types.DeleteResult{
			AppID:                 "vaultwarden",
			DeletedPaths:          []string{"docker-compose.yml", ".env", "/home/test/docker/vaultwarden"},
			RemainingNamedVolumes: []string{"wdm-vaultwarden_data"},
			RemovedNetworks:       []string{"pangolin"},
			RetainedNetworks: []types.RetainedNetwork{
				{Name: "shared", Reason: "network shared has active endpoints"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden")
	require.NoError(t, err)

	assert.Contains(t, stdout, "vaultwarden was permanently deleted", "the headline must state the permanent deletion")
	assert.Contains(t, stdout, "Deleted:", "the finish screen must list the deleted paths")
	assert.Contains(t, stdout, "- docker-compose.yml", "each deleted path must be listed")
	assert.Contains(t, stdout, "- /home/test/docker/vaultwarden", "the stack directory must be listed")
	assert.Contains(t, stdout, "- wdm-vaultwarden_data", "surviving named volumes must be listed")
	assert.Contains(t, stdout, "Networks removed:", "removed networks must be shown when present")
	assert.Contains(t, stdout, "- pangolin", "each removed network must be listed")
	assert.Contains(t, stdout, "could not be removed", "a retained network must warn")
	assert.Contains(t, stdout, "docker network rm shared", "a retained network must show the manual command")
}

// TestAppsDelete_PlainFinish_HonestEmptyVolumeState pins the empty-list
// honesty (mirroring writeRemoveFinish): an empty RemainingNamedVolumes does
// NOT claim zero volumes survived — the listing is opportunistic — and empty
// removed/retained network lists omit those blocks entirely.
func TestAppsDelete_PlainFinish_HonestEmptyVolumeState(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		deleteResult: &types.DeleteResult{
			AppID:        "vaultwarden",
			DeletedPaths: []string{"/home/test/docker/vaultwarden"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden")
	require.NoError(t, err)

	assert.Contains(t, stdout, "none reported (Docker inspection data may be unavailable)",
		"an empty named-volume list must state the volumes could not be enumerated, not assert zero")
	assert.NotContains(t, stdout, "Networks removed:",
		"the removed-networks block must be omitted when no networks were removed")
	assert.NotContains(t, stdout, "could not be removed",
		"the retained-networks warning must be omitted when none were retained")
}

// --- ExactArgs(1) refusals: zero or two positional args fail before the
// engine factory runs.

func TestAppsDelete_ExactArgs_RefusesWithoutConstructingEngine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"zero args", []string{"apps", "delete"}},
		{"two args", []string{"apps", "delete", "a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				t.Fatal("engine factory must not be constructed on an arg-count refusal")
				return nil, nil
			})

			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "an arg-count violation must surface an error")
			assert.Empty(t, outBuf.String(), "an arg-count refusal must write nothing to stdout")
		})
	}
}

// --- Factory-error propagation (the catalog_test.go precedent): a failed
// engine factory surfaces out of Execute and never produces output, since
// the leaf builds the engine inside RunE after the --json read.

func TestAppsDelete_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"apps", "delete", "vaultwarden", "--confirm-name", "vaultwarden", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}
