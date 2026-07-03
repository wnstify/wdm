package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// TestAppsInstall_PortFlag_MapsOverridesVerbatim proves every --port HOST=NEW
// pair reaches types.InstallRequest.PortOverrides as oldHostPort→newHostPort.
func TestAppsInstall_PortFlag_MapsOverridesVerbatim(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}

	_, _, err := runLeaf(t, fake,
		"apps", "install", "appflowy",
		"--port", "8080=8081",
		"--port", "5432=15432",
		"--json",
	)
	require.NoError(t, err)
	assert.Equal(t, map[int]int{8080: 8081, 5432: 15432}, fake.installReq.PortOverrides)
}

// TestAppsInstall_PortFlag_NoFlagLeavesNilMap pins that omitting --port leaves
// PortOverrides nil — unchanged from the pre-flag behavior.
func TestAppsInstall_PortFlag_NoFlagLeavesNilMap(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}
	_, _, err := runLeaf(t, fake, "apps", "install", "appflowy", "--json")
	require.NoError(t, err)
	assert.Nil(t, fake.installReq.PortOverrides)
}

// TestAppsInstall_PortFlag_RefusesMalformed pins the CLI-side refusal of the
// shapes the engine cannot meaningfully report: a missing '=', a non-integer
// HOST/NEW, and a duplicate HOST. Each must refuse before the engine runs and
// write nothing to stdout.
func TestAppsInstall_PortFlag_RefusesMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		port []string
	}{
		{name: "no_equals", port: []string{"8080"}},
		{name: "non_integer_host", port: []string{"abc=8081"}},
		{name: "non_integer_new", port: []string{"8080=xyz"}},
		{name: "duplicate_host", port: []string{"8080=8081", "8080=8082"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}
			args := []string{"apps", "install", "appflowy", "--json"}
			for _, p := range tc.port {
				args = append(args, "--port", p)
			}
			stdout, _, err := runLeaf(t, fake, args...)
			require.Error(t, err)
			assert.Empty(t, stdout, "no envelope may be written on a malformed --port")
			assert.Equal(t, types.InstallRequest{}, fake.installReq, "a malformed --port must refuse before the engine")
		})
	}
}

// TestAppsInstall_PortConflict_JSONEnvelope proves a typed PortConflictError
// from the engine is surfaced under --json as a wdm.v1 envelope on stdout
// carrying {service, conflicting_port, suggested_port} plus the error code,
// message, and hint, and that the command still exits with an error.
func TestAppsInstall_PortConflict_JSONEnvelope(t *testing.T) {
	t.Parallel()

	conflict := types.NewPortConflictError("web", 80, 8080, 8081,
		types.NewError(types.ErrCodeUsageValidation, "local port is already in use",
			"127.0.0.1:8080 is in use; remap it with --port 8080=8081 (or another free port)"))
	fake := &fakeEngine{err: conflict}

	stdout, _, err := runLeaf(t, fake, "apps", "install", "appflowy", "--json")
	require.Error(t, err)
	require.True(t, types.IsCode(err, types.ErrCodeUsageValidation))

	var env types.Envelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &env))
	assert.Equal(t, types.EnvelopeSchema, env.Schema)

	var data struct {
		Code            string `json:"code"`
		Message         string `json:"message"`
		Hint            string `json:"hint"`
		Service         string `json:"service"`
		ConflictingPort int    `json:"conflicting_port"`
		SuggestedPort   int    `json:"suggested_port"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	assert.Equal(t, "usage_validation", data.Code)
	assert.Equal(t, "local port is already in use", data.Message)
	assert.Contains(t, data.Hint, "--port")
	assert.Equal(t, "web", data.Service)
	assert.Equal(t, 8080, data.ConflictingPort)
	assert.Equal(t, 8081, data.SuggestedPort)
}

// TestAppsInstall_PortConflict_PlainNoEnvelope proves the plain (non-JSON)
// path writes no stdout envelope and returns the typed conflict whose hint
// names the --port remap path.
func TestAppsInstall_PortConflict_PlainNoEnvelope(t *testing.T) {
	t.Parallel()

	conflict := types.NewPortConflictError("web", 80, 8080, 8081,
		types.NewError(types.ErrCodeUsageValidation, "local port is already in use",
			"127.0.0.1:8080 is in use; remap it with --port 8080=8081 (or another free port)"))
	fake := &fakeEngine{err: conflict}

	stdout, _, err := runLeaf(t, fake, "apps", "install", "appflowy")
	require.Error(t, err)
	assert.Empty(t, stdout, "plain mode emits no stdout envelope")

	var got *types.PortConflictError
	require.ErrorAs(t, err, &got)
	assert.Contains(t, got.Err.Hint, "--port")
}

// TestAppsInstall_AutoPortFlag_SetsRequest proves --auto-port sets
// types.InstallRequest.AutoPort true.
func TestAppsInstall_AutoPortFlag_SetsRequest(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}
	_, _, err := runLeaf(t, fake, "apps", "install", "appflowy", "--auto-port", "--json")
	require.NoError(t, err)
	assert.True(t, fake.installReq.AutoPort)
}

// TestAppsInstall_AutoPortFlag_DefaultFalse pins that omitting --auto-port
// leaves AutoPort false.
func TestAppsInstall_AutoPortFlag_DefaultFalse(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}
	_, _, err := runLeaf(t, fake, "apps", "install", "appflowy", "--json")
	require.NoError(t, err)
	assert.False(t, fake.installReq.AutoPort)
}

// TestAppsInstall_AutoPortFlag_CombinesWithPort proves --auto-port and --port
// coexist: AutoPort is set and the explicit override reaches PortOverrides
// verbatim (explicit precedence is enforced engine-side).
func TestAppsInstall_AutoPortFlag_CombinesWithPort(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{installResult: &types.InstallResult{AppID: "appflowy", StackPath: "/s"}}
	_, _, err := runLeaf(t, fake, "apps", "install", "appflowy", "--port", "8080=9090", "--auto-port", "--json")
	require.NoError(t, err)
	assert.True(t, fake.installReq.AutoPort)
	assert.Equal(t, map[int]int{8080: 9090}, fake.installReq.PortOverrides)
}

// TestParsePortOverrides is a focused unit test of the parser.
func TestParsePortOverrides(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pairs   []string
		want    map[int]int
		wantErr bool
	}{
		{name: "nil input -> nil map", pairs: nil, want: nil},
		{name: "empty input -> nil map", pairs: []string{}, want: nil},
		{name: "single pair", pairs: []string{"8080=8081"}, want: map[int]int{8080: 8081}},
		{name: "whitespace trimmed", pairs: []string{" 8080 = 8081 "}, want: map[int]int{8080: 8081}},
		{name: "no equals refuses", pairs: []string{"8080"}, wantErr: true},
		{name: "non-integer host refuses", pairs: []string{"abc=8081"}, wantErr: true},
		{name: "non-integer new refuses", pairs: []string{"8080=xyz"}, wantErr: true},
		{name: "duplicate host refuses", pairs: []string{"8080=1", "8080=2"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePortOverrides(tc.pairs)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
