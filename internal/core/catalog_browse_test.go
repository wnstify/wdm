package core_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// browsing, projecting internal/catalog shapes into pkg/types from the
// local FS only. These tests pin the projection
// completeness, the deterministic AppID ordering, the
// Default-any stringification rule, the
// defensive-copy contract, the unknown-app
// and channel refusals, and the closed-engine / canceled-context entry
// checks. They drive the public engine API only.

// catalogFSForChannel renders apps into a catalog manifest for the given
// channel and returns an fs.FS in the engine catalog-FS shape
// (<channel>/catalog.yaml), so a browse call with an explicit Channel can
// be exercised. It mirrors catalogFixtureFSWithFiles but lets the test
// pick the channel (that helper hardcodes "stable").
func catalogFSForChannel(t *testing.T, channel string, generatedAt time.Time, apps ...catalog.App) fstest.MapFS {
	t.Helper()

	manifest := catalog.Catalog{
		SchemaVersion: 1,
		Channel:       channel,
		GeneratedAt:   generatedAt,
		Apps:          apps,
	}
	raw, err := yaml.Marshal(manifest)
	require.NoError(t, err)

	return fstest.MapFS{
		filepath.Join(channel, "catalog.yaml"): &fstest.MapFile{Data: raw},
	}
}

// browseAppFixture builds a catalog app with the full nested shape
// populated (placeholders of several types including a secret, ports,
// image pins, and resource bands) so the projection can be checked
// field-by-field.
func browseAppFixture(appID string) catalog.App {
	regenerable := false
	return catalog.App{
		AppID:           appID,
		Name:            "App " + appID,
		Summary:         "summary of " + appID,
		Description:     "long description of " + appID,
		TemplateName:    "template-" + appID,
		TemplateVersion: "2026.05.29",
		ComposeTemplate: "templates/test/docker-compose.yml.tmpl",
		EnvTemplate:     "templates/test/.env.tmpl",
		Placeholders: []catalog.Placeholder{
			{
				Name:     "SITE_DOMAIN",
				Type:     "domain",
				Required: true,
			},
			{
				Name:     "WORKER_COUNT",
				Type:     "string",
				Required: false,
				Default:  200, // YAML integer scalar -> any(int)
			},
			{
				Name:        "DB_PASSWORD",
				Type:        "secret",
				Required:    false,
				Encoding:    "base64url",
				Regenerable: &regenerable,
			},
		},
		SupportedVersions: catalog.SupportedVersions{
			Docker:  ">=20.10",
			Compose: ">=2.0",
		},
		Ports: []catalog.Port{
			{Service: "app", Container: 8080, Host: 8080, Protocol: "tcp"},
			{Service: "app", Container: 9090, Host: 9090, Protocol: "udp"},
		},
		ImagePins: []catalog.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0", Digest: "sha256:" + strings.Repeat("a", 64)},
			{Service: "db", Image: "docker.io/library/postgres", Tag: "16"},
		},
		Resources: []catalog.ResourceProfile{
			{
				Service: "app",
				Memory:  catalog.MemoryBand{Min: "256m", Recommended: "512m", Max: "1g"},
				CPUs:    catalog.CPUBand{Min: "0.25", Recommended: "1.0", Max: "2.0"},
				PIDs:    catalog.PIDsBand{Default: 200, Max: 500},
			},
		},
		PangolinGuidance: catalog.PangolinGuidance{
			TargetURL: "http://127.0.0.1:8080",
		},
		RiskClassification: []string{"safe"},
	}
}

func newBrowseEngine(t *testing.T, fsys fstest.MapFS) *core.Engine {
	t.Helper()
	eng, _ := newTestEngine(t, core.WithCatalog(fsys))
	return eng
}

func TestAvailableApps_ProjectsEveryFieldFromSyntheticCatalog(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	app := browseAppFixture("alpha")
	fsys := catalogFSForChannel(t, "stable", generatedAt, app)
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.NoError(t, err)
	require.Len(t, apps, 1)

	got := apps[0]
	assert.Equal(t, "alpha", got.AppID)
	assert.Equal(t, "App alpha", got.Name)
	assert.Equal(t, "summary of alpha", got.Summary)
	assert.Equal(t, "long description of alpha", got.Description)
	assert.Equal(t, "template-alpha", got.TemplateName)
	assert.Equal(t, "2026.05.29", got.TemplateVersion)
	assert.Equal(t, "stable", got.Channel)
	assert.Equal(t, []string{"safe"}, got.RiskClassification)

	require.Len(t, got.Placeholders, 3)
	assert.Equal(t, types.CatalogPlaceholder{
		Key:      "SITE_DOMAIN",
		Type:     "domain",
		Required: true,
		Secret:   false,
	}, got.Placeholders[0])
	assert.Equal(t, types.CatalogPlaceholder{
		Key:      "WORKER_COUNT",
		Type:     "string",
		Required: false,
		Secret:   false,
		Default:  "200",
	}, got.Placeholders[1])
	// Secret-ness is derived from Type=="secret"; Description and Pattern
	// have no catalog source and project empty.
	assert.Equal(t, types.CatalogPlaceholder{
		Key:      "DB_PASSWORD",
		Type:     "secret",
		Required: false,
		Secret:   true,
	}, got.Placeholders[2])

	require.Len(t, got.Ports, 2)
	assert.Equal(t, types.CatalogPort{Service: "app", Host: 8080, Container: 8080, Protocol: "tcp"}, got.Ports[0])
	assert.Equal(t, types.CatalogPort{Service: "app", Host: 9090, Container: 9090, Protocol: "udp"}, got.Ports[1])

	require.Len(t, got.ImagePins, 2)
	// Digest is intentionally not projected.
	assert.Equal(t, types.CatalogImagePin{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"}, got.ImagePins[0])
	assert.Equal(t, types.CatalogImagePin{Service: "db", Image: "docker.io/library/postgres", Tag: "16"}, got.ImagePins[1])

	require.Len(t, got.Resources, 1)
	assert.Equal(t, types.CatalogResource{
		Service:           "app",
		MemoryRecommended: "512m",
		CPUsRecommended:   "1.0",
	}, got.Resources[0])
}

func TestAvailableApps_EmptyNestedSlicesProjectNil(t *testing.T) {
	t.Parallel()

	// An app with no placeholders, no resources, and (after trimming) the
	// minimum required ports/pins must project nil for the empty nested
	// slices so the omitempty json tags drop them. appFixture declares one
	// port and one image pin (both schema-required) but no placeholders or
	// resources.
	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	app := appFixture("minimal", 8080)
	fsys := catalogFSForChannel(t, "stable", generatedAt, app)
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.NoError(t, err)
	require.Len(t, apps, 1)

	got := apps[0]
	assert.Nil(t, got.Placeholders, "no placeholders must project a nil slice")
	assert.Nil(t, got.Resources, "no resources must project a nil slice")
	require.Len(t, got.Ports, 1)
	require.Len(t, got.ImagePins, 1)
}

func TestAvailableApps_MalformedCatalogSurfacesVerificationError(t *testing.T) {
	t.Parallel()

	// A manifest that is syntactically present but fails schema validation
	// surfaces the loader's typed verification error unchanged.
	fsys := fstest.MapFS{
		"stable/catalog.yaml": &fstest.MapFile{Data: []byte("not: a valid catalog\n")},
	}
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.Error(t, err)
	assert.Nil(t, apps)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
}

func TestAvailableApps_OrdersByAppIDRegardlessOfCatalogOrder(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	// Catalog-file order is gamma, alpha, beta — sorted output must be
	// alpha, beta, gamma independent of authoring order.
	fsys := catalogFSForChannel(t, "stable", generatedAt,
		browseAppFixture("gamma"),
		browseAppFixture("alpha"),
		browseAppFixture("beta"),
	)
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.NoError(t, err)

	ids := make([]string, 0, len(apps))
	for _, a := range apps {
		ids = append(ids, a.AppID)
	}
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, ids)
}

func TestAvailableApps_DefaultStringification(t *testing.T) {
	t.Parallel()

	// The catalog stores a placeholder default as an any whose concrete
	// type mirrors the YAML scalar. Pin the stringification rule for every
	// admitted scalar form: nil -> "" (no default), bool -> true/false,
	// int -> base-10, float -> shortest round-trip, string -> verbatim.
	tests := []struct {
		name     string
		value    any
		expected string
	}{
		{name: "nil produces no default", value: nil, expected: ""},
		{name: "bool true", value: true, expected: "true"},
		{name: "bool false", value: false, expected: "false"},
		{name: "integer", value: 200, expected: "200"},
		{name: "zero integer", value: 0, expected: "0"},
		{name: "float", value: 1.5, expected: "1.5"},
		{name: "float trailing zero collapses", value: 1.0, expected: "1"},
		{name: "string verbatim", value: "3,33", expected: "3,33"},
		{name: "string octal-like verbatim", value: "022", expected: "022"},
		{name: "empty string", value: "", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
			app := browseAppFixture("alpha")
			app.Placeholders = []catalog.Placeholder{{
				Name:     "FIELD",
				Type:     "string",
				Required: false,
				Default:  tt.value,
			}}
			fsys := catalogFSForChannel(t, "stable", generatedAt, app)
			eng := newBrowseEngine(t, fsys)

			apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
			require.NoError(t, err)
			require.Len(t, apps, 1)
			require.Len(t, apps[0].Placeholders, 1)
			assert.Equal(t, tt.expected, apps[0].Placeholders[0].Default)
		})
	}
}

func TestAvailableApps_EmptyChannelUsesConfiguredDefault(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	// Engine default channel is "stable" (defaultSettings). An empty query
	// channel must read the stable manifest.
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{Channel: ""})
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "stable", apps[0].Channel)
}

func TestAvailableApps_ExplicitChannelIsHonored(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	// v1's catalog schema enum admits only "stable", so an explicit channel
	// is exercised by naming "stable" verbatim — the per-request channel is
	// threaded to the loader rather than ignored in favor of the default
	// (the per-request-vs-default split is the load-bearing behavior; the
	// no-such-channel test below proves a different channel reaches a
	// different FS subdir).
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{Channel: "stable"})
	require.NoError(t, err)
	require.Len(t, apps, 1)
	assert.Equal(t, "stable", apps[0].Channel)
}

func TestAvailableApps_UnknownChannelSurfacesLoaderError(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	// Only stable/catalog.yaml exists; querying "nightly" reads
	// nightly/catalog.yaml (a different FS subdir), which is absent — so the
	// per-request channel reaches the FS path and the loader's read error
	// surfaces unchanged. This is the proof that a per-request channel is
	// not silently coerced to the default.
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{Channel: "nightly"})
	require.Error(t, err)
	assert.Nil(t, apps)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
}

func TestAvailableApps_InvalidChannelRefusesWithUsageValidation(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	// A slash-bearing channel is rejected before any FS read.
	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{Channel: "../escape"})
	require.Error(t, err)
	assert.Nil(t, apps)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
}

func TestAvailableApp_ReturnsSingleProjection(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt,
		browseAppFixture("alpha"),
		browseAppFixture("beta"),
	)
	eng := newBrowseEngine(t, fsys)

	app, err := eng.AvailableApp(context.Background(), types.CatalogAppQuery{AppID: "beta"})
	require.NoError(t, err)
	require.NotNil(t, app)
	assert.Equal(t, "beta", app.AppID)
	assert.Equal(t, "stable", app.Channel)
	require.Len(t, app.Placeholders, 3)
	assert.True(t, app.Placeholders[2].Secret)
}

func TestAvailableApp_UnknownAppRefusesWithUsageValidation(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	app, err := eng.AvailableApp(context.Background(), types.CatalogAppQuery{AppID: "does-not-exist"})
	require.Error(t, err)
	assert.Nil(t, app)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
	assert.NotEmpty(t, typed.Hint, "unknown-app refusal must carry a hint pointing at apps list")
}

func TestAvailableApp_EmptyChannelUsesConfiguredDefault(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	app, err := eng.AvailableApp(context.Background(), types.CatalogAppQuery{AppID: "alpha"})
	require.NoError(t, err)
	require.NotNil(t, app)
	assert.Equal(t, "alpha", app.AppID)
}

// TestAvailableApps_ReturnsDefensiveCopies mutates every returned slice
// and nested slice, then re-fetches and asserts the second result is
// pristine — pinning the defensive-copy exit criterion through
// the public API: mutating a result cannot corrupt engine state.
// Honesty note on what this can and cannot prove: the engine caches raw
// catalog bytes, not a parsed manifest, so every call re-parses and the
// second fetch shares no memory with the first regardless of how the
// projection is built. The mutate-and-refetch shape therefore pins the
// public-API contract (no retained state a caller can corrupt) but
// cannot detect aliasing of a single parse's backing arrays; that
// deep-copy property is held by construction in the projection helpers
// (fresh make+append per slice, slices.Clone for RiskClassification).
func TestAvailableApps_ReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)
	ctx := context.Background()

	first, err := eng.AvailableApps(ctx, types.CatalogQuery{})
	require.NoError(t, err)
	require.Len(t, first, 1)

	// Mutate the top-level slice element and every nested slice.
	first[0].AppID = "MUTATED"
	first[0].Name = "MUTATED"
	first[0].RiskClassification[0] = "MUTATED"
	first[0].Placeholders[0].Key = "MUTATED"
	first[0].Placeholders[1].Default = "MUTATED"
	first[0].Ports[0].Host = -1
	first[0].ImagePins[0].Image = "MUTATED"
	first[0].Resources[0].MemoryRecommended = "MUTATED"

	second, err := eng.AvailableApps(ctx, types.CatalogQuery{})
	require.NoError(t, err)
	require.Len(t, second, 1)

	assert.Equal(t, "alpha", second[0].AppID)
	assert.Equal(t, "App alpha", second[0].Name)
	assert.Equal(t, []string{"safe"}, second[0].RiskClassification)
	assert.Equal(t, "SITE_DOMAIN", second[0].Placeholders[0].Key)
	assert.Equal(t, "200", second[0].Placeholders[1].Default)
	assert.Equal(t, 8080, second[0].Ports[0].Host)
	assert.Equal(t, "docker.io/example/app", second[0].ImagePins[0].Image)
	assert.Equal(t, "512m", second[0].Resources[0].MemoryRecommended)
}

func TestAvailableApps_RejectsCanceledContext(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	apps, err := eng.AvailableApps(ctx, types.CatalogQuery{})
	require.Error(t, err)
	assert.Nil(t, apps)
	assert.ErrorIs(t, err, context.Canceled)

	app, err := eng.AvailableApp(ctx, types.CatalogAppQuery{AppID: "alpha"})
	require.Error(t, err)
	assert.Nil(t, app)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestAvailableApps_RejectsClosedEngine(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC)
	fsys := catalogFSForChannel(t, "stable", generatedAt, browseAppFixture("alpha"))
	eng := newBrowseEngine(t, fsys)
	require.NoError(t, eng.Close())

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, apps)

	app, err := eng.AvailableApp(context.Background(), types.CatalogAppQuery{AppID: "alpha"})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, app)
}

// TestAvailableApps_RealStableCatalog drives the real stable catalog
// (catalog/stable/catalog.yaml) through the projection so the
// curated apps' real metadata is proven to survive the projection.
// This complements the synthetic edge-shape tests with a real-data
// sanity check.
func TestAvailableApps_RealStableCatalog(t *testing.T) {
	t.Parallel()

	abs, err := filepath.Abs(realCatalogPath)
	require.NoError(t, err)
	cat, err := catalog.LoadCatalog(context.Background(), abs)
	require.NoError(t, err)

	raw, err := yaml.Marshal(cat)
	require.NoError(t, err)
	fsys := fstest.MapFS{"stable/catalog.yaml": &fstest.MapFile{Data: raw}}
	eng := newBrowseEngine(t, fsys)

	apps, err := eng.AvailableApps(context.Background(), types.CatalogQuery{})
	require.NoError(t, err)

	ids := make([]string, 0, len(apps))
	for _, a := range apps {
		ids = append(ids, a.AppID)
		assert.Equal(t, "stable", a.Channel)
		assert.NotEmpty(t, a.Name)
		assert.NotEmpty(t, a.ImagePins, "curated app %s must project image pins", a.AppID)
	}
	// Sorted by AppID: appflowy, authentik, baserow, dockhand, docuseal, freshrss, jellyfin, meshcentral, mira, n8n, navidrome, nextcloud, openwebui, qbittorrent, serpbear, stoat, syncthing, uptime-kuma, vaultwarden, wg-adguard, zulip.
	assert.Equal(t, []string{"appflowy", "authentik", "baserow", "dockhand", "docuseal", "freshrss", "jellyfin", "meshcentral", "mira", "n8n", "navidrome", "nextcloud", "openwebui", "qbittorrent", "serpbear", "stoat", "syncthing", "uptime-kuma", "vaultwarden", "wg-adguard", "zulip"}, ids)

	// Spot-check the secret-ness derivation on a real app: uptime-kuma
	// carries secret placeholders.
	uptime, err := eng.AvailableApp(context.Background(), types.CatalogAppQuery{AppID: "uptime-kuma"})
	require.NoError(t, err)
	var sawSecret bool
	for _, ph := range uptime.Placeholders {
		if ph.Type == "secret" {
			assert.True(t, ph.Secret, "secret-typed placeholder %s must project Secret=true", ph.Key)
			sawSecret = true
		}
	}
	assert.True(t, sawSecret, "uptime-kuma must declare at least one secret placeholder")
}
