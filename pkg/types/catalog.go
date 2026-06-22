package types

// CatalogQuery selects which channel Engine.AvailableApps enumerates
// (PRD §7, §8 step 3). v1 ships the "stable" channel only (PRD §22);
// an empty Channel means the settings/default channel.
type CatalogQuery struct {
	// Channel is the catalog channel to browse. Empty selects the
	// configured default ("stable" in v1).
	Channel string `json:"channel,omitempty"`
}

// CatalogAppQuery selects one catalog entry for Engine.AvailableApp
// (PRD §7, §8, §15).
type CatalogAppQuery struct {
	// AppID is the catalog identifier to fetch.
	AppID string `json:"app_id"`

	// Channel is the catalog channel to look the app up in. Empty
	// selects the configured default ("stable" in v1).
	Channel string `json:"channel,omitempty"`
}

// CatalogApp is the UI-facing projection of a catalog entry, returned
// by Engine.AvailableApps (list view) and Engine.AvailableApp (detail
// view) for the install picker and app-detail / install-form screens
// (PRD §7, §8, §15). It mirrors the internal catalog app shape
// field-for-field but lives in pkg/types so the engine can return it
// across the facade: a UI layer must never import internal/catalog
// (PRD §29).
type CatalogApp struct {
	// AppID is the stable catalog identifier (for example "uptime-kuma").
	AppID string `json:"app_id"`

	// Name is the human-readable app name shown in the picker and on
	// the install finish screen.
	Name string `json:"name"`

	// Summary is the one-line description rendered in list views.
	Summary string `json:"summary"`

	// Description is the long-form description shown on the app detail
	// screen. May contain Markdown.
	Description string `json:"description,omitempty"`

	// TemplateName is the human-readable template label the app's
	// Compose /.env files derive from.
	TemplateName string `json:"template_name"`

	// TemplateVersion is the template version recorded in the catalog
	// manifest.
	TemplateVersion string `json:"template_version"`

	// Channel is the catalog channel this entry was read from.
	Channel string `json:"channel"`

	// Placeholders enumerates the templated values the install form must
	// collect or the engine generates. This projection lets a generic
	// TUI form render and validate without per-app special-casing
	Placeholders []CatalogPlaceholder `json:"placeholders,omitempty"`

	// Ports lists the local ports the rendered stack binds, for the
	// detail view.
	Ports []CatalogPort `json:"ports,omitempty"`

	// ImagePins lists the per-service image references the catalog pins,
	// for the detail view.
	ImagePins []CatalogImagePin `json:"image_pins,omitempty"`

	// Resources lists the per-service recommended resource bands, for
	// the detail / sizing view.
	Resources []CatalogResource `json:"resources,omitempty"`

	// RiskClassification carries the PRD §20 risk tags surfaced on the
	// detail screen and gating the database-risk confirmation
	// ("safe", "major", "database", "complex").
	RiskClassification []string `json:"risk_classification,omitempty"`
}

// CatalogPlaceholder is the projection of one catalog placeholder — the
// metadata a generic install form needs to render and validate an input
// field.
type CatalogPlaceholder struct {
	// Key is the placeholder identifier as it appears in templates
	// (uppercase snake, for example "DB_PASSWORD").
	Key string `json:"key"`

	// Type is the placeholder leaf type from the closed catalog enum:
	// "string", "domain", "port", "secret", "timezone", "path", or
	// "bool". The form picks its widget and validation from this.
	Type string `json:"type"`

	// Required indicates whether the installer must supply a value.
	Required bool `json:"required"`

	// Secret is true for engine-generated secret placeholders. wdm
	// generates the value at install time, so the form MUST NOT render an
	// input for it and any caller-supplied value is ignored. Surfaced so
	// the form can hide or annotate the field.
	Secret bool `json:"secret"`

	// Description is optional help text for the field.
	Description string `json:"description,omitempty"`

	// Default is the optional default value the form may pre-fill, as a
	// string projection of the catalog scalar.
	Default string `json:"default,omitempty"`

	// Pattern is the optional validation regular expression the form may
	// apply to user input.
	Pattern string `json:"pattern,omitempty"`
}

// CatalogPort is the projection of one catalog port declaration, for
// the app detail view.
type CatalogPort struct {
	// Service is the Compose service name that exposes the port.
	Service string `json:"service,omitempty"`

	// Host is the default host-side port (may be overridden at install
	// to resolve collisions).
	Host int `json:"host"`

	// Container is the in-container port the service listens on.
	Container int `json:"container"`

	// Protocol is the transport protocol ("tcp" or "udp").
	Protocol string `json:"protocol,omitempty"`
}

// CatalogImagePin is the projection of one catalog image pin, for the
// app detail view.
type CatalogImagePin struct {
	// Service is the Compose service name the image backs.
	Service string `json:"service"`

	// Image is the image reference without tag or digest.
	Image string `json:"image"`

	// Tag is the pinned image tag.
	Tag string `json:"tag,omitempty"`
}

// CatalogResource is the projection of one catalog per-service resource
// band, for the detail / sizing view. The recommended values document
// what the install path selects when the host guidance budget allows.
type CatalogResource struct {
	// Service is the Compose service name this band sizes.
	Service string `json:"service"`

	// MemoryRecommended is the recommended memory limit (Docker
	// "<integer><b|k|m|g>" form, for example "512m").
	MemoryRecommended string `json:"memory_recommended,omitempty"`

	// CPUsRecommended is the recommended CPU quota (decimal string, for
	// example "1.0").
	CPUsRecommended string `json:"cpus_recommended,omitempty"`
}
