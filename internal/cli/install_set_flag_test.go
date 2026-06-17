package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `apps install --set KEY=VALUE` flag added as an
// interstitial: the CLI surface that makes path-placeholder apps (e.g.
// Jellyfin's MEDIA_PATH / MOVIES_PATH / SHOWS_PATH) installable. They
// extend the internal/cli suite — driving NewRootCmd through
// runLeaf with the recording fakeEngine — and lock two contracts: the
// verbatim flag→request mapping, and the CLI-side refusal of malformed or
// duplicate pairs (the engine owns every key/type/value check, so the CLI
// refuses only the shapes the engine cannot meaningfully report).

// TestAppsInstall_SetFlag_MapsPlaceholdersVerbatim proves every --set pair
// reaches types.InstallRequest.PlaceholderValues byte-for-byte, including
// the two adversarial shapes the parser must preserve: a VALUE that itself
// contains '=' (split at the FIRST '=' only) and an empty VALUE (legal —
// the engine owns the semantics, so the CLI passes it through).
func TestAppsInstall_SetFlag_MapsPlaceholdersVerbatim(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		installResult: &types.InstallResult{AppID: "jellyfin", StackPath: "/s"},
	}

	_, _, err := runLeaf(t, fake,
		"apps", "install", "jellyfin",
		"--set", "MEDIA_PATH=/srv/media",
		"--set", "MOVIES_PATH=/srv/movies",
		"--set", "TOKEN=a=b=c", // value contains '=': split at the first only
		"--set", "EMPTY=", // empty value is legal, passed through
		"--json",
	)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"MEDIA_PATH":  "/srv/media",
		"MOVIES_PATH": "/srv/movies",
		"TOKEN":       "a=b=c",
		"EMPTY":       "",
	}, fake.installReq.PlaceholderValues,
		"every --set pair must reach PlaceholderValues verbatim")
}

// TestAppsInstall_SetFlag_NoFlagsLeavesNilMap pins that omitting --set
// leaves PlaceholderValues nil — identical to the pre-flag behavior, so
// existing install paths are unchanged.
func TestAppsInstall_SetFlag_NoFlagsLeavesNilMap(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		installResult: &types.InstallResult{AppID: "freshrss", StackPath: "/s"},
	}

	_, _, err := runLeaf(t, fake, "apps", "install", "freshrss", "--json")
	require.NoError(t, err)
	assert.Nil(t, fake.installReq.PlaceholderValues,
		"with no --set the request must carry a nil PlaceholderValues map")
}

// TestAppsInstall_SetFlag_RefusesMalformedPairs pins the CLI-side refusal
// of the two shapes the engine cannot report (no '=' and an empty KEY) and
// of duplicate keys. Each refusal must:
//   - return an error (so cmd/wdm exits 2 via the usage default arm),
//   - never reach the engine (installReq stays zero — Install was not
//     called), and
//   - write nothing to stdout under --json (no partial envelope leaks).
func TestAppsInstall_SetFlag_RefusesMalformedPairs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		set  []string
	}{
		{name: "no_equals", set: []string{"MEDIA_PATH"}},
		{name: "empty_key", set: []string{"=/srv/media"}},
		{name: "duplicate_key", set: []string{"MEDIA_PATH=/a", "MEDIA_PATH=/b"}},
		{name: "duplicate_key_empty_value", set: []string{"K=", "K=v"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{
				installResult: &types.InstallResult{AppID: "jellyfin", StackPath: "/s"},
			}

			args := []string{"apps", "install", "jellyfin", "--json"}
			for _, s := range tc.set {
				args = append(args, "--set", s)
			}

			stdout, _, err := runLeaf(t, fake, args...)

			require.Error(t, err, "a malformed --set pair must refuse")
			assert.Empty(t, stdout, "no envelope may be written on a malformed --set")
			// The engine must never be consulted: a zero installReq proves
			// Install was not called (the parse error returns before newEngine).
			assert.Equal(t, types.InstallRequest{}, fake.installReq,
				"a malformed --set must refuse before reaching the engine")
		})
	}
}

// TestAppsInstall_SetFlag_ParsesBeforeEngineConstruction pins the ordering
// contract documented at the parse site: a malformed --set pair must refuse
// BEFORE the engine factory runs, so a bad invocation never constructs the
// engine (and never touches runtime.lock). The factory returns a sentinel
// error; if parsing ran after construction, Execute would surface that
// sentinel instead of the parse error.
func TestAppsInstall_SetFlag_ParsesBeforeEngineConstruction(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory must not be consulted")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"apps", "install", "jellyfin", "--set", "MEDIA_PATH"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.NotErrorIs(t, err, factoryErr,
		"a malformed --set must refuse before the engine factory runs")
	assert.ErrorContains(t, err, "invalid --set",
		"the surfaced error must be the parse refusal")
}

// TestAppsInstall_SetFlag_HelpMentionsSet pins the doc-truth surface: the
// help text must describe --set, name the secret-placeholders-are-generated
// constraint, and not invent a universal DOMAIN key (the domain is supplied
// with --domain, which fills the app's domain placeholder whatever its
// catalog name). Help output is the primary CLI documentation
// (golang-cli), so a regression that drops or misstates these is caught
// here rather than on a VM.
func TestAppsInstall_SetFlag_HelpMentionsSet(t *testing.T) {
	t.Parallel()

	// --help exits 0 and never constructs the engine, so the fake result is
	// irrelevant; runLeaf still wires it as the lazy factory return.
	stdout, _, err := runLeaf(t, &fakeEngine{}, "apps", "install", "--help")
	require.NoError(t, err)

	assert.Contains(t, stdout, "--set", "help must document the --set flag")
	assert.Contains(t, stdout, "KEY=VALUE", "help must show the --set KEY=VALUE shape")
	assert.Contains(t, stdout, "secret", "help must note secret placeholders are generated, not settable")
	assert.NotContains(t, stdout, "--set DOMAIN",
		"help must not invent a universal DOMAIN key — no catalog app declares it")
}

// TestParseSetFlags is a focused unit test of the parser itself, covering
// the splitting and refusal logic directly so the table is exhaustive
// without round-tripping through cobra.
func TestParseSetFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pairs   []string
		want    map[string]string
		wantErr bool
	}{
		{name: "nil input -> nil map", pairs: nil, want: nil},
		{name: "empty input -> nil map", pairs: []string{}, want: nil},
		{
			name:  "single pair",
			pairs: []string{"MEDIA_PATH=/srv/media"},
			want:  map[string]string{"MEDIA_PATH": "/srv/media"},
		},
		{
			name:  "value with equals splits at first",
			pairs: []string{"TOKEN=a=b=c"},
			want:  map[string]string{"TOKEN": "a=b=c"},
		},
		{
			name:  "empty value is legal",
			pairs: []string{"K="},
			want:  map[string]string{"K": ""},
		},
		{name: "no equals refuses", pairs: []string{"MEDIA_PATH"}, wantErr: true},
		{name: "empty key refuses", pairs: []string{"=v"}, wantErr: true},
		{name: "duplicate key refuses", pairs: []string{"K=1", "K=2"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseSetFlags(tc.pairs)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got, "a refusal must not return a partial map")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
