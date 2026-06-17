package types

// Settings captures the user-configurable knobs persisted at
// ~/.config/wdm/config.toml (PRD §34). JSON tags drive the --json
// envelope and any IPC consumers (PRD §32, §37); TOML tags map the
// on-disk snake_case keys to these fields when internal/state's loader
// decodes the file. The two tag sets MUST stay in
// sync so a round-trip (TOML → Settings → JSON) preserves field names
// verbatim.
type Settings struct {
	// SchemaVersion is the on-disk config schema version. Locked to 1 in
	SchemaVersion int `json:"schema_version" toml:"schema_version"`

	// BaseStackPath is the directory under which managed stacks are
	// written (default ~/docker, PRD §9). Path expansion (~/ → $HOME)
	// happens in the engine per; this field
	// holds the raw string as loaded from disk.
	BaseStackPath string `json:"base_stack_path" toml:"base_stack_path"`

	// Timezone is an IANA timezone name (e.g. "Europe/Bratislava"). An
	// empty string means: detect from the host at install time.
	Timezone string `json:"timezone" toml:"timezone"`

	// DefaultDockerNetwork is the external Docker network attached to
	// managed stacks; created on demand the first time a stack needs it.
	DefaultDockerNetwork string `json:"default_docker_network" toml:"default_docker_network"`

	// CatalogChannel selects the catalog channel directory under
	// ~/.local/share/wdm/catalogs/. Locked to "stable" in v1
	// (PRD §22); "verified" is reserved for a future premium channel.
	CatalogChannel string `json:"catalog_channel" toml:"catalog_channel"`

	// UpdateCheckPreference controls automatic update checks. One of
	// "manual", "daily-on-launch", "disabled" (PRD §34).
	UpdateCheckPreference string `json:"update_check_preference" toml:"update_check_preference"`
}
