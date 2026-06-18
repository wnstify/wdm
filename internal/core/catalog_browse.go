package core

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the AvailableApps and AvailableApp engine methods
// Both are read-only and local-filesystem only — no network, no
// download, no signature verification (the invariant, exit
// criterion "from the local FS with no network call"). They acquire no
// lock (neither the global runtime.lock nor a per-stack flock): catalog
// data is channel-global and read off the same catalog FS the install
// path reads, so there is no stack to contend with. They project
// internal/catalog shapes into pkg/types to keep the facade intact: a
// UI layer must never import internal/catalog, so the engine returns the
// projected [types.CatalogApp], not *catalog.App.
// Projection completeness is the confirmation rules: every field a generic TUI
// install form needs to render and validate is carried across — key,
// type, required, secret-ness, description, default, and pattern per
// placeholder, plus ports, image pins, resource bands, risk
// classification, and the channel / catalog-version / template identity
// per app. Two source-shape facts shape the projection:
//   - internal/catalog.Placeholder has no Description or Pattern field,
//     so the projected [types.CatalogPlaceholder.Description] and
//     .Pattern are always empty today; the omitempty json tags keep them
//     out of the envelope. They are projected as empty rather than
//     dropped so a later schema growth needs no projection change here.
//   - secret-ness is not a catalog field — it is derived from the
//     placeholder Type being "secret" (the closed enum value), the same
//     derivation the install path uses to mark a placeholder as
//     engine-generated and never user-supplied.
// Ordering and the Default stringification rule are documented at their
// projection helpers below.

// AvailableApps returns the installable catalog entries for the queried
// channel as [types.CatalogApp] projections (PRD §7, §8, §15,
// the invariant). An empty Channel selects the configured default channel
// ("stable" in v1); a non-empty Channel loads through the same catalog
// FS the install path reads, so an unknown or unavailable channel
// surfaces the loader's typed error unchanged.
// The result is sorted by AppID for an order independent of catalog
// authoring order (see [projectCatalogApps]). Every returned slice — the
// top-level slice and every nested placeholder, port, image-pin, and
// resource slice — is a fresh defensive copy sharing no backing array
// with the loader's parsed structures, so a caller may mutate it freely.
// The method is read-only: no lock, no.env read, no write, no network
// or Docker call. It returns [ErrClosed] after [Engine.Close] and
// propagates a canceled context.
func (e *Engine) AvailableApps(ctx context.Context, query types.CatalogQuery) ([]types.CatalogApp, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cat, err := e.loadBrowseCatalog(ctx, query.Channel)
	if err != nil {
		return nil, err
	}

	return projectCatalogApps(cat), nil
}

// AvailableApp returns the detail projection for one catalog entry as a
// [types.CatalogApp] (PRD §7, §8, §15). An empty Channel
// selects the configured default channel ("stable" in v1); a non-empty
// Channel loads through the same catalog FS the install path reads.
// An unknown app id is refused with a usage-validation error mirroring
// the install path's own unknown-app refusal (the shared
// [selectCatalogApp]); the hint points the user at apps list. An unknown
// or unavailable channel surfaces the loader's typed error unchanged.
// The returned pointer and every nested slice are fresh defensive
// copies. The method is read-only with the same no-lock, no-network,
// no-write posture as [Engine.AvailableApps]. It returns [ErrClosed]
// after [Engine.Close] and propagates a canceled context.
func (e *Engine) AvailableApp(ctx context.Context, query types.CatalogAppQuery) (*types.CatalogApp, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cat, err := e.loadBrowseCatalog(ctx, query.Channel)
	if err != nil {
		return nil, err
	}

	app, err := selectCatalogApp(cat, query.AppID)
	if err != nil {
		return nil, err
	}

	projected := projectCatalogApp(app, cat.Channel, catalogVersionOf(cat))
	return &projected, nil
}

// loadBrowseCatalog loads and verifies the catalog manifest for the
// browse-requested channel. An empty channel falls back to the
// configured default (e.settings.CatalogChannel, "stable" in v1), so a
// browse call with no channel reads the same manifest the install path
// would.
// The channel is validated and the manifest read and parsed through the
// same catalog FS, path shape, and loader the install path uses
// ([Engine.installCatalogFS], [catalog.LoadCatalogBytes]), so browse and
// install resolve channels identically: an invalid channel, an
// unreadable manifest, or a manifest that fails schema verification
// surfaces the same typed error. Unlike [Engine.loadInstallCatalog] it
// takes the channel as a parameter, since browse channels are
// per-request while install always uses the configured channel.
func (e *Engine) loadBrowseCatalog(ctx context.Context, channel string) (*catalog.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if channel == "" {
		channel = e.settings.CatalogChannel
	}
	if !validCatalogChannel(channel) {
		return nil, usageValidationError(
			"catalog channel is invalid",
			"set catalog_channel to stable in config.toml",
			fmt.Errorf("invalid catalog channel %q", channel),
		)
	}

	catalogPath := path.Join(channel, "catalog.yaml")
	raw, err := fs.ReadFile(e.installCatalogFS(), catalogPath)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be read",
			"install the stable catalog before browsing apps",
			err,
		)
	}
	cat, err := catalog.LoadCatalogBytes(ctx, raw)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be verified",
			"refresh the catalog and retry",
			err,
		)
	}
	return cat, nil
}

// validCatalogChannel reports whether channel is a safe single path
// segment usable as a catalog FS subdirectory. It mirrors the channel
// guard in [Engine.loadInstallCatalog] (non-empty, slash-free, a single
// valid path element) so browse and install reject the same malformed
// channels before any FS read.
func validCatalogChannel(channel string) bool {
	if channel == "" || channel == "." {
		return false
	}
	if strings.Contains(channel, "/") {
		return false
	}
	return fs.ValidPath(channel)
}

// projectCatalogApps projects every app in the loaded catalog into a
// fresh []types.CatalogApp ordered by AppID ascending.
// The loader preserves catalog-file (YAML) order without sorting, so
// projecting in source order would make output order an artifact of
// catalog authoring — a manifest reorder would silently reorder the
// picker. Sorting by the stable AppID makes the order independent of
// authoring order, testable, and consistent with the "choose one of the
// listed app ids" model the unknown-app hint uses. AppIDs are expected
// unique per channel but the loader does not enforce it: a duplicate
// projects twice in this list view while the detail path
// ([selectCatalogApp]) refuses it. The sort stays deterministic for a
// fixed catalog either way.
func projectCatalogApps(cat *catalog.Catalog) []types.CatalogApp {
	channel := cat.Channel
	catalogVersion := catalogVersionOf(cat)
	apps := make([]types.CatalogApp, 0, len(cat.Apps))
	for _, app := range cat.Apps {
		apps = append(apps, projectCatalogApp(app, channel, catalogVersion))
	}
	slices.SortFunc(apps, func(a, b types.CatalogApp) int {
		return cmp.Compare(a.AppID, b.AppID)
	})
	return apps
}

// catalogVersionOf renders the catalog manifest's version string the
// same way the install path records it in.wdm.lock — GeneratedAt
// formatted as UTC RFC 3339 (install.go's catalogVersion).
// internal/catalog has no string version field, so this timestamp is the
// catalog version; deriving it identically keeps the browse detail view
// consistent with the version the install path pins.
func catalogVersionOf(cat *catalog.Catalog) string {
	return cat.GeneratedAt.UTC().Format(time.RFC3339)
}

// projectCatalogApp projects one catalog app into a [types.CatalogApp].
// The channel and catalog version are passed in because they are
// catalog-level fields internal/catalog.App does not carry, so the
// caller reads them off the loaded manifest. Every nested slice is a
// fresh copy (see the per-shape helpers) so the result aliases no
// catalog-loader memory.
func projectCatalogApp(app catalog.App, channel, catalogVersion string) types.CatalogApp {
	return types.CatalogApp{
		AppID:              app.AppID,
		Name:               app.Name,
		Summary:            app.Summary,
		Description:        app.Description,
		TemplateName:       app.TemplateName,
		TemplateVersion:    app.TemplateVersion,
		Channel:            channel,
		CatalogVersion:     catalogVersion,
		Placeholders:       projectCatalogPlaceholders(app.Placeholders),
		Ports:              projectCatalogPorts(app.Ports),
		ImagePins:          projectCatalogImagePins(app.ImagePins),
		Resources:          projectCatalogResources(app.Resources),
		RiskClassification: slices.Clone(app.RiskClassification),
	}
}

// projectCatalogPlaceholders projects the catalog placeholder slice into
// fresh [types.CatalogPlaceholder] values, nil when empty so the
// omitempty json tag drops the field.
// Secret-ness is derived from the placeholder Type being
// [render.TypeSecret] — the same canonical constant the install path's
// secret detection uses, so browse and install cannot drift — rather
// than read from a catalog field (the catalog shape has no Secret
// field). Description and Pattern have no catalog source today and
// project as empty.
func projectCatalogPlaceholders(placeholders []catalog.Placeholder) []types.CatalogPlaceholder {
	if len(placeholders) == 0 {
		return nil
	}
	out := make([]types.CatalogPlaceholder, 0, len(placeholders))
	for _, ph := range placeholders {
		out = append(out, types.CatalogPlaceholder{
			Key:      ph.Name,
			Type:     ph.Type,
			Required: ph.Required,
			Secret:   render.Type(ph.Type) == render.TypeSecret,
			Default:  projectPlaceholderDefault(ph.Default),
		})
	}
	return out
}

// projectPlaceholderDefault stringifies a catalog placeholder default
// for the UI form. The catalog stores the default as an [any] whose
// concrete type mirrors the placeholder's leaf type — YAML unmarshaling
// preserves the scalar as a string, int, float64, bool, or nil
// (internal/catalog/types.go [Placeholder.Default]).
// It reuses the project-shared [stringDefault]: a nil default becomes
// the empty string (no default), and every non-nil scalar goes through
// fmt.Sprint, deterministic for each admitted form — a bool renders
// "true"/"false", an integer in base 10, a float in its shortest
// round-trippable form (strconv 'g'), a string verbatim. Sharing
// [stringDefault] with the install path keeps the browse-time default
// the form pre-fills from drifting from the install-time default the
// engine resolves.
func projectPlaceholderDefault(value any) string {
	projected, _ := stringDefault(value)
	return projected
}

// projectCatalogPorts projects the catalog port slice into fresh
// [types.CatalogPort] values, nil when empty.
func projectCatalogPorts(ports []catalog.Port) []types.CatalogPort {
	if len(ports) == 0 {
		return nil
	}
	out := make([]types.CatalogPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, types.CatalogPort{
			Service:   port.Service,
			Host:      port.Host,
			Container: port.Container,
			Protocol:  port.Protocol,
		})
	}
	return out
}

// projectCatalogImagePins projects the catalog image-pin slice into
// fresh [types.CatalogImagePin] values, nil when empty. The optional
// catalog digest is not projected: the detail view shows the
// maintainer-pinned image:tag, and the resolved digest is an
// install-time artifact recorded in.wdm.lock, not catalog metadata.
func projectCatalogImagePins(pins []catalog.ImagePin) []types.CatalogImagePin {
	if len(pins) == 0 {
		return nil
	}
	out := make([]types.CatalogImagePin, 0, len(pins))
	for _, pin := range pins {
		out = append(out, types.CatalogImagePin{
			Service: pin.Service,
			Image:   pin.Image,
			Tag:     pin.Tag,
		})
	}
	return out
}

// projectCatalogResources projects the catalog resource profiles into
// fresh [types.CatalogResource] values, nil when empty. Only the
// recommended memory and CPU values are projected: the sizing view shows
// what the install path selects when the host guidance budget allows, and the
// min/max band and pids cap are install-planning internals the form does
// not surface.
func projectCatalogResources(resources []catalog.ResourceProfile) []types.CatalogResource {
	if len(resources) == 0 {
		return nil
	}
	out := make([]types.CatalogResource, 0, len(resources))
	for _, profile := range resources {
		out = append(out, types.CatalogResource{
			Service:           profile.Service,
			MemoryRecommended: profile.Memory.Recommended,
			CPUsRecommended:   profile.CPUs.Recommended,
		})
	}
	return out
}
