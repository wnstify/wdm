package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps list --json` envelope. They mirror the
// envelope_contract_test.go idioms — driving NewRootCmd through runLeaf
// with the recording fakeEngine — and lock the wdm.v1 envelope discipline
// for the read-only list leaf: a single envelope on stdout under --json,
// the "apps" object key (PRD §32 forbids a top-level array), the nil ->
// [] normalization, raw-stdout equality, and the empty-stdout error path.

// TestAppsList_JSON_EmitsSingleEnvelopeUnderAppsKey pins that
// `apps list --json` writes exactly one wdm.v1 envelope on stdout whose
// data object carries the stacks under the stable "apps" key — never a
// top-level JSON array (PRD §32 mandates envelope.data is an object).
func TestAppsList_JSON_EmitsSingleEnvelopeUnderAppsKey(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusResult: []types.AppRuntimeStatus{
			{
				AppInfo: types.AppInfo{AppID: "vaultwarden", TemplateName: "Vaultwarden", StackPath: "/home/test/docker/vaultwarden", CatalogChannel: "stable"},
				State:   "running",
			},
			{
				AppInfo:          types.AppInfo{AppID: "uptime-kuma", TemplateName: "Uptime Kuma", StackPath: "/home/test/docker/uptime-kuma", CatalogChannel: "stable", NeedsAttention: true},
				State:            "needs_attention",
				AttentionReasons: []string{"container_exited"},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "list", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "list --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	apps, ok := data["apps"].([]any)
	require.True(t, ok, "envelope data must carry the stacks under the apps key as an array")
	require.Len(t, apps, 2, "both managed stacks must appear under apps")

	// Representative-key checks: the pkg/types contract tests own the
	// exhaustive AppRuntimeStatus field shape; here we only confirm the
	// right payload — including the LIVE state — reached the envelope under
	// the apps key.
	first, ok := apps[0].(map[string]any)
	require.True(t, ok, "each apps entry must be a JSON object")
	assert.Equal(t, "vaultwarden", first["app_id"])
	assert.Equal(t, "/home/test/docker/vaultwarden", first["stack_path"])
	assert.Equal(t, "running", first["state"], "the live runtime state must reach the JSON envelope")
	assert.Equal(t, false, first["needs_attention"], "a running app must report needs_attention false")

	second, ok := apps[1].(map[string]any)
	require.True(t, ok, "each apps entry must be a JSON object")
	assert.Equal(t, "needs_attention", second["state"], "a degraded app must surface its live state")
	assert.Equal(t, true, second["needs_attention"], "a degraded app must report needs_attention true")
}

// TestAppsList_JSON_EmptyResultNormalizesToEmptyArray pins the nil ->
// []types.AppRuntimeStatus normalization documented on appsListPayload: a
// fresh system with no managed stacks must emit "apps": [], not
// "apps": null, so an NDJSON/jq consumer iterates a real empty array
// instead of special-casing null.
func TestAppsList_JSON_EmptyResultNormalizesToEmptyArray(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{listStatusResult: nil}

	stdout, _, err := runLeaf(t, fake, "apps", "list", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "list --json must emit exactly one envelope even with no stacks")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	// The raw envelope.data must contain "apps":[] — a null would mean the
	// normalization was skipped. Decode and assert the slice is present and
	// empty rather than absent.
	assert.NotContains(t, lines[0], `"apps":null`, "a nil list must normalize to an empty array, not null")

	data := decodeEnvelopeData(t, lines[0])
	apps, ok := data["apps"].([]any)
	require.True(t, ok, "apps key must decode to an array, not null")
	assert.Empty(t, apps, "an empty system must emit an empty apps array")
}

// TestAppsList_Plain_EmitsTabSeparatedLines pins the plain-mode contract
// (PRD §9): one stack per line as "<app_id>\t<stack_path>\t<state>",
// tab-separated so cut(1)/awk(1) parse without quoting, and no envelope
// bytes. The state column carries the LIVE runtime state.
func TestAppsList_Plain_EmitsTabSeparatedLines(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		listStatusResult: []types.AppRuntimeStatus{
			{AppInfo: types.AppInfo{AppID: "vaultwarden", StackPath: "/home/test/docker/vaultwarden"}, State: "running"},
			{AppInfo: types.AppInfo{AppID: "uptime-kuma", StackPath: "/home/test/docker/uptime-kuma"}, State: "needs_attention"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "apps", "list")
	require.NoError(t, err)

	assert.Equal(t,
		"vaultwarden\t/home/test/docker/vaultwarden\trunning\nuptime-kuma\t/home/test/docker/uptime-kuma\tneeds_attention\n",
		stdout,
		"plain list must emit one tab-separated app_id/stack_path/state line per stack")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(stdout), `{"schema"`),
		"plain mode stdout must not be a JSON envelope")
}

// fresh system with no managed stacks exits 0 with empty stdout.
func TestAppsList_Plain_EmptyEmitsNothing(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{listStatusResult: nil}

	stdout, _, err := runLeaf(t, fake, "apps", "list")
	require.NoError(t, err)
	assert.Empty(t, stdout, "an empty system must emit nothing on stdout in plain mode")
}

// TestAppsList_ErrorPath_StdoutEmpty pins that a typed engine error
// propagates out of Execute with no envelope written, for both --json and
// plain mode (matching the lifecycle leaves' error contract).
func TestAppsList_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeGeneric, "catalog read failed", "check ~/.local/share/wdm")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"apps", "list", "--json"}},
		{"plain", []string{"apps", "list"}},
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

// TestAppsList_FactoryError_Propagates pins that a failed engine factory
// surfaces out of Execute and never produces output — the list leaf builds
// the engine inside RunE, so a construction failure is the first thing it
// can hit.
func TestAppsList_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"apps", "list", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}
