package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `catalog list` / `catalog show` leaf bodies. They
// mirror the established internal/cli idioms —
// driving NewRootCmd through runLeaf with the recording fakeEngine, the
// raw-stdout byte discipline from envelope_contract_test.go, and the
// empty-stdout error path from list_envelope_test.go — and lock the two
// leaves' contract: the wdm.v1 envelope shapes (the "apps" object key for
// list per the apps-list precedent, the CatalogApp wrapped directly for
// show per the apps-status precedent), the plain-mode rendering including
// the wdm-generated secret-placeholder marker, the unknown-app error path,
// the verbatim --channel mapping onto the query, ExactArgs(1) on show, and
// the factory-not-invoked-on-help invariant.

// sampleCatalogApp returns a fully populated CatalogApp covering every
// rendered section (placeholders incl. a secret, ports, image pins,
// resources, risk) so the plain-mode and envelope assertions exercise the
// whole projection.
func sampleCatalogApp() types.CatalogApp {
	return types.CatalogApp{
		AppID:           "uptime-kuma",
		Name:            "Uptime Kuma",
		Summary:         "Self-hosted uptime monitor",
		Description:     "A fancy self-hosted monitoring tool.",
		TemplateName:    "Uptime Kuma",
		TemplateVersion: "2026-06-11",
		Channel:         "stable",
		Placeholders: []types.CatalogPlaceholder{
			{Key: "DOMAIN", Type: "domain", Required: true},
			{Key: "DB_PASSWORD", Type: "secret", Required: true, Secret: true},
			{Key: "TIMEZONE", Type: "timezone", Default: "UTC"},
		},
		Ports: []types.CatalogPort{
			{Service: "app", Host: 3008, Container: 3001, Protocol: "tcp"},
		},
		ImagePins: []types.CatalogImagePin{
			{Service: "app", Image: "louislam/uptime-kuma", Tag: "1.23.16"},
			{Service: "db", Image: "mariadb", Tag: "11.8.6"},
		},
		Resources: []types.CatalogResource{
			{Service: "app", MemoryRecommended: "512m", CPUsRecommended: "1.0"},
		},
		RiskClassification: []string{"database"},
	}
}

// --- recording wrapper: the shared fakeEngine ignores the query, so a
// local wrapper embeds it and overrides only the two browse methods to
// capture the query before delegating — the shared fake_engine_test.go
// stays untouched.

type recordingCatalogEngine struct {
	*fakeEngine
	gotListQuery types.CatalogQuery
	gotShowQuery types.CatalogAppQuery
}

func (e *recordingCatalogEngine) AvailableApps(ctx context.Context, query types.CatalogQuery) ([]types.CatalogApp, error) {
	e.gotListQuery = query
	return e.fakeEngine.AvailableApps(ctx, query)
}

func (e *recordingCatalogEngine) AvailableApp(ctx context.Context, query types.CatalogAppQuery) (*types.CatalogApp, error) {
	e.gotShowQuery = query
	return e.fakeEngine.AvailableApp(ctx, query)
}

// runCatalogLeaf drives one `catalog...` invocation through NewRootCmd
// with the recording engine wired as the lazy factory result, mirroring
// runLeaf but typed to the local wrapper (runLeaf returns *fakeEngine).
func runCatalogLeaf(t *testing.T, eng engine.Engine, args ...string) (stdout, stderr string, err error) {
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

// --- catalog list ---

// TestCatalogList_JSON_EmitsSingleEnvelopeUnderAppsKey pins that
// `catalog list --json` writes exactly one wdm.v1 envelope on stdout whose
// data object carries the entries under the stable "apps" key — never a
// top-level JSON array (PRD §32 mandates envelope.data is an object) —
// mirroring the apps-list envelope precedent.
func TestCatalogList_JSON_EmitsSingleEnvelopeUnderAppsKey(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		availableAppsResult: []types.CatalogApp{
			{AppID: "freshrss", Name: "FreshRSS", Summary: "RSS reader", TemplateVersion: "2026-06-11", Channel: "stable"},
			sampleCatalogApp(),
		},
	}

	stdout, _, err := runLeaf(t, fake, "catalog", "list", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "catalog list --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	apps, ok := data["apps"].([]any)
	require.True(t, ok, "envelope data must carry the entries under the apps key as an array")
	require.Len(t, apps, 2, "both catalog entries must appear under apps")

	first, ok := apps[0].(map[string]any)
	require.True(t, ok, "each apps entry must be a JSON object")
	assert.Equal(t, "freshrss", first["app_id"])
	assert.Equal(t, "FreshRSS", first["name"])
}

// TestCatalogList_JSON_EmptyResultNormalizesToEmptyArray pins the nil ->
// []types.CatalogApp normalization: an empty catalog must emit "apps": [],
// not "apps": null, so an NDJSON/jq consumer iterates a real empty array.
func TestCatalogList_JSON_EmptyResultNormalizesToEmptyArray(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{availableAppsResult: nil}

	stdout, _, err := runLeaf(t, fake, "catalog", "list", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "catalog list --json must emit exactly one envelope even with no entries")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")
	assert.NotContains(t, lines[0], `"apps":null`, "a nil list must normalize to an empty array, not null")

	data := decodeEnvelopeData(t, lines[0])
	apps, ok := data["apps"].([]any)
	require.True(t, ok, "apps key must decode to an array, not null")
	assert.Empty(t, apps, "an empty catalog must emit an empty apps array")
}

// TestCatalogList_Plain_EmitsTabSeparatedLines pins the plain-mode
// contract: one entry per line as
// "<app_id>\t<name>\t<template_version>\t<summary>", tab-separated so
// cut(1)/awk(1) parse the leading fields, and no envelope bytes.
func TestCatalogList_Plain_EmitsTabSeparatedLines(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		availableAppsResult: []types.CatalogApp{
			{AppID: "freshrss", Name: "FreshRSS", Summary: "RSS reader", TemplateVersion: "2026-06-11"},
			{AppID: "uptime-kuma", Name: "Uptime Kuma", Summary: "Uptime monitor", TemplateVersion: "2026-06-10"},
		},
	}

	stdout, _, err := runLeaf(t, fake, "catalog", "list")
	require.NoError(t, err)

	assert.Equal(t,
		"freshrss\tFreshRSS\t2026-06-11\tRSS reader\n"+
			"uptime-kuma\tUptime Kuma\t2026-06-10\tUptime monitor\n",
		stdout,
		"plain list must emit one tab-separated line per catalog entry")
}

// TestCatalogList_Plain_EmptyEmitsNothing pins that an empty catalog exits
// 0 with empty stdout in plain mode (mirroring apps list on a fresh
// system).
func TestCatalogList_Plain_EmptyEmitsNothing(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{availableAppsResult: nil}

	stdout, _, err := runLeaf(t, fake, "catalog", "list")
	require.NoError(t, err)
	assert.Empty(t, stdout, "an empty catalog must emit nothing on stdout in plain mode")
}

// TestCatalogList_ChannelMapsOntoQuery pins that --channel maps verbatim
// onto CatalogQuery.Channel, and that an omitted flag leaves it empty (the
// engine's "use the configured default" signal).
func TestCatalogList_ChannelMapsOntoQuery(t *testing.T) {
	t.Parallel()

	t.Run("explicit channel", func(t *testing.T) {
		t.Parallel()

		rec := &recordingCatalogEngine{fakeEngine: &fakeEngine{}}
		_, _, err := runCatalogLeaf(t, rec, "catalog", "list", "--channel", "stable", "--json")
		require.NoError(t, err)
		assert.Equal(t, "stable", rec.gotListQuery.Channel, "--channel must map verbatim onto CatalogQuery.Channel")
	})

	t.Run("omitted channel", func(t *testing.T) {
		t.Parallel()

		rec := &recordingCatalogEngine{fakeEngine: &fakeEngine{}}
		_, _, err := runCatalogLeaf(t, rec, "catalog", "list", "--json")
		require.NoError(t, err)
		assert.Empty(t, rec.gotListQuery.Channel, "an omitted --channel must leave CatalogQuery.Channel empty")
	})
}

// TestCatalogList_ErrorPath_StdoutEmpty pins that a typed engine error
// propagates out of Execute with no envelope written, for both --json and
// plain mode.
func TestCatalogList_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeVerificationFailed, "catalog could not be verified", "refresh the catalog and retry")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"catalog", "list", "--json"}},
		{"plain", []string{"catalog", "list"}},
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

// --- catalog show ---

// TestCatalogShow_JSON_WrapsCatalogAppDirectly pins that
// `catalog show <app-id> --json` writes exactly one wdm.v1 envelope whose
// data IS the CatalogApp object directly (the apps-status direct-wrap
// precedent: no nesting key).
func TestCatalogShow_JSON_WrapsCatalogAppDirectly(t *testing.T) {
	t.Parallel()

	app := sampleCatalogApp()
	fake := &fakeEngine{availableAppResult: &app}

	stdout, _, err := runLeaf(t, fake, "catalog", "show", "uptime-kuma", "--json")
	require.NoError(t, err)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "catalog show --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "uptime-kuma", data["app_id"], "the CatalogApp must be the envelope data directly")
	assert.NotContains(t, data, "app", "CatalogApp must be data directly, not nested under an app key")
	assert.NotContains(t, data, "catalog_app", "CatalogApp must be data directly, not nested")
}

// TestCatalogShow_Plain_RendersDetailBlock pins the plain-mode detail
// rendering on a fully populated CatalogApp: the identity header, the
// per-section blocks, and — critically — that the secret placeholder reads
// as wdm-generated, never prompting the user to supply it.
func TestCatalogShow_Plain_RendersDetailBlock(t *testing.T) {
	t.Parallel()

	app := sampleCatalogApp()
	fake := &fakeEngine{availableAppResult: &app}

	stdout, _, err := runLeaf(t, fake, "catalog", "show", "uptime-kuma")
	require.NoError(t, err)

	// Identity + template + channel header.
	assert.Contains(t, stdout, "uptime-kuma\tUptime Kuma\n")
	assert.Contains(t, stdout, "template\tUptime Kuma 2026-06-11")
	assert.Contains(t, stdout, "channel\tstable")
	assert.Contains(t, stdout, "summary\tSelf-hosted uptime monitor")

	// Placeholders: required non-secret marked required; secret marked
	// generated (NOT required, NOT promptable); default surfaced.
	assert.Contains(t, stdout, "DOMAIN\tdomain (required)")
	assert.Contains(t, stdout, "DB_PASSWORD\tsecret (generated)")
	assert.NotContains(t, stdout, "DB_PASSWORD\tsecret (required)",
		"a secret placeholder must read as generated, never as a required user input")
	assert.Contains(t, stdout, "TIMEZONE\ttimezone [default: UTC]")

	// Ports, images, resources, risk.
	assert.Contains(t, stdout, "3008 -> 3001/tcp (app)")
	assert.Contains(t, stdout, "app\tlouislam/uptime-kuma:1.23.16")
	assert.Contains(t, stdout, "app\tmem 512m\tcpus 1.0")
	assert.Contains(t, stdout, "Risk: database")
}

// TestCatalogShow_ChannelMapsOntoQuery pins that the positional app-id and
// --channel both map verbatim onto CatalogAppQuery, and that an omitted
// channel leaves it empty.
func TestCatalogShow_ChannelMapsOntoQuery(t *testing.T) {
	t.Parallel()

	t.Run("explicit channel", func(t *testing.T) {
		t.Parallel()

		app := sampleCatalogApp()
		rec := &recordingCatalogEngine{fakeEngine: &fakeEngine{availableAppResult: &app}}
		_, _, err := runCatalogLeaf(t, rec, "catalog", "show", "uptime-kuma", "--channel", "stable", "--json")
		require.NoError(t, err)

		assert.Equal(t, "uptime-kuma", rec.gotShowQuery.AppID, "the positional app-id must map onto CatalogAppQuery.AppID")
		assert.Equal(t, "stable", rec.gotShowQuery.Channel, "--channel must map verbatim onto CatalogAppQuery.Channel")
	})

	t.Run("omitted channel", func(t *testing.T) {
		t.Parallel()

		app := sampleCatalogApp()
		rec := &recordingCatalogEngine{fakeEngine: &fakeEngine{availableAppResult: &app}}
		_, _, err := runCatalogLeaf(t, rec, "catalog", "show", "uptime-kuma", "--json")
		require.NoError(t, err)

		assert.Empty(t, rec.gotShowQuery.Channel, "an omitted --channel must leave CatalogAppQuery.Channel empty")
	})
}

// TestCatalogShow_UnknownApp_ErrorPath_StdoutEmpty pins the unknown-app
// refusal: the engine's typed usage-validation error propagates out of
// Execute with empty stdout, in both --json and plain mode.
func TestCatalogShow_UnknownApp_ErrorPath_StdoutEmpty(t *testing.T) {
	t.Parallel()

	engineErr := types.NewError(types.ErrCodeUsageValidation, "app not found in catalog", "run wdm catalog list")

	cases := []struct {
		name string
		args []string
	}{
		{"json", []string{"catalog", "show", "does-not-exist", "--json"}},
		{"plain", []string{"catalog", "show", "does-not-exist"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeEngine{err: engineErr}
			stdout, _, err := runLeaf(t, fake, tc.args...)

			require.Error(t, err, "an unknown app must propagate a typed engine error out of Execute")
			assert.ErrorIs(t, err, engineErr, "the leaf must return the engine error unchanged")
			assert.Empty(t, stdout, "no output may be written to stdout on the error path")
		})
	}
}

// TestCatalogShow_RequiresExactlyOneArg pins cobra.ExactArgs(1): zero or
// two positional args fail before RunE, so the engine is never consulted.
func TestCatalogShow_RequiresExactlyOneArg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"no args", []string{"catalog", "show"}},
		{"two args", []string{"catalog", "show", "a", "b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A panicking factory proves RunE (and the engine) is never
			// reached: ExactArgs rejects before construction.
			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, errors.New("factory must not be consulted on an arg-count failure")
			})
			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.Error(t, err, "catalog show must reject a wrong argument count")
			assert.Empty(t, outBuf.String(), "an arg-count failure must write nothing to stdout")
		})
	}
}

// --- shared invariants ---

// TestCatalog_FactoryNotInvokedOnHelp pins the PRD §14 self-update
// smoke-check invariant for both leaves: --help exits 0 and never
// constructs the engine (the factory is consulted only inside RunE).
func TestCatalog_FactoryNotInvokedOnHelp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"list help", []string{"catalog", "list", "--help"}},
		{"show help", []string{"catalog", "show", "--help"}},
		{"group help", []string{"catalog", "--help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd("test", func() (engine.Engine, error) {
				return nil, errors.New("factory must not be consulted for --help")
			})
			var outBuf, errBuf bytes.Buffer
			root.SetOut(&outBuf)
			root.SetErr(&errBuf)
			root.SetIn(&bytes.Buffer{})
			root.SetArgs(tc.args)
			root.SetContext(t.Context())

			err := root.Execute()
			require.NoError(t, err, "--help must exit 0 without constructing the engine")
		})
	}
}

// TestCatalogList_FactoryError_Propagates pins that a failed engine factory
// surfaces out of Execute and never produces output — the list leaf builds
// the engine inside RunE, so a construction failure is the first thing it
// can hit after the --json read.
func TestCatalogList_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"catalog", "list", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}

// TestCatalogShow_FactoryError_Propagates is the show-leaf mirror of
// TestCatalogList_FactoryError_Propagates: a failed engine factory
// surfaces out of Execute with no output, since show also builds the
// engine inside RunE after the --json read.
func TestCatalogShow_FactoryError_Propagates(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("engine factory failed")
	root := NewRootCmd("test", func() (engine.Engine, error) {
		return nil, factoryErr
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs([]string{"catalog", "show", "uptime-kuma", "--json"})
	root.SetContext(t.Context())

	err := root.Execute()
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr, "a factory failure must propagate out of Execute")
	assert.Empty(t, outBuf.String(), "no envelope may be written when the engine cannot be built")
}

// TestCatalogShow_PlainOmitsEmptySections pins the documented
// section-skip behavior of writeCatalogApp: a CatalogApp carrying no
// placeholders, ports, image pins, resources, or risk classification must
// render the identity header alone — every per-section header
// (writeCatalogPlaceholders/Ports/ImagePins/Resources/risk) is gated on
// content and must be absent from stdout. A second case with a populated
// Description covers the optional summary/description branch so the
// always-skipped headers are not confused with unconditionally-empty
// output.
func TestCatalogShow_PlainOmitsEmptySections(t *testing.T) {
	t.Parallel()

	// The exact section-header literals writeCatalogApp's helpers emit.
	// Each is skipped entirely when its slice is empty (or, for risk, when
	// the classification is empty).
	sectionHeaders := []string{
		"Placeholders:",
		"Ports:",
		"Images:",
		"Resources (recommended):",
		"Risk:",
	}

	t.Run("bare app omits every section", func(t *testing.T) {
		t.Parallel()

		app := types.CatalogApp{
			AppID:           "bare-app",
			Name:            "Bare App",
			TemplateName:    "Bare App",
			TemplateVersion: "2026-06-12",
			Channel:         "stable",
			// No Summary, Description, Placeholders, Ports, ImagePins,
			// Resources, or RiskClassification.
		}
		fake := &fakeEngine{availableAppResult: &app}

		stdout, _, err := runLeaf(t, fake, "catalog", "show", "bare-app")
		require.NoError(t, err)

		// Identity header present.
		assert.Contains(t, stdout, "bare-app\tBare App\n", "the identity header must always render")
		assert.Contains(t, stdout, "template\tBare App 2026-06-12", "the template line must always render")
		assert.Contains(t, stdout, "channel\tstable", "the channel line must always render")

		// Every content section absent.
		for _, header := range sectionHeaders {
			assert.NotContains(t, stdout, header,
				"an empty %s section must be skipped entirely", header)
		}

		// The optional summary/description lines are absent too when unset.
		assert.NotContains(t, stdout, "summary\t", "an empty summary must be skipped")
		assert.NotContains(t, stdout, "description\t", "an empty description must be skipped")
	})

	t.Run("populated description renders while sections stay omitted", func(t *testing.T) {
		t.Parallel()

		app := types.CatalogApp{
			AppID:           "bare-app",
			Name:            "Bare App",
			TemplateName:    "Bare App",
			TemplateVersion: "2026-06-12",
			Channel:         "stable",
			Description:     "A minimal app with only a description.",
		}
		fake := &fakeEngine{availableAppResult: &app}

		stdout, _, err := runLeaf(t, fake, "catalog", "show", "bare-app")
		require.NoError(t, err)

		assert.Contains(t, stdout, "description\tA minimal app with only a description.",
			"a populated description must render on its own line")
		for _, header := range sectionHeaders {
			assert.NotContains(t, stdout, header,
				"an empty %s section must be skipped even when a description is present", header)
		}
	})
}
