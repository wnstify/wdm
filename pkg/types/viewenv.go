package types

// ViewEnvResult is the read-only, redaction-safe view behind
// `wdm view-env <app>` and the TUI view-env screen: the effective
// environment of a managed stack (base .env merged with the user
// overlay .env.user) with every secret value masked before it reaches
// this type. The engine builds it via ViewEnvRedacted, so callers may
// display or serialize it without risk of leaking secrets.
type ViewEnvResult struct {
	// AppID is the app whose environment the view describes.
	AppID string `json:"app_id"`

	// Entries is one EnvEntry per effective environment key, in the
	// order the engine resolved them.
	Entries []EnvEntry `json:"entries"`
}

// EnvEntry is a single environment key/value pair in a [ViewEnvResult].
type EnvEntry struct {
	// Key is the environment variable name.
	Key string `json:"key"`

	// Value is the variable's value as it is safe to display: the engine
	// has already redacted it, so a masked placeholder appears here in
	// place of any secret. It never carries a raw secret.
	Value string `json:"value"`

	// Secret reports whether this entry was treated as sensitive and so
	// has a masked Value (by secret-value match or secret-ish key name).
	Secret bool `json:"secret"`
}
