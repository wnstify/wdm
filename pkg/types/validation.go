package types

// ValidationResult is returned by Engine.ValidateConfig (PRD §18:418
// "Validate config", §18:427 compose-validation condition). A
// validation failure is reported as Valid false — a SUCCESS payload,
// NOT an error — so the caller can render the detail and still offer
// next actions, mirroring how Status returns a needs-attention stack at
// exit 0. The engine returns a non-nil error only for
// operational faults (unmanaged stack, busy stack, unreachable daemon),
// never for an invalid-but-readable Compose file.
type ValidationResult struct {
	// AppID identifies the validated stack.
	AppID string `json:"app_id"`

	// ComposeProject is the Compose project the stack deploys under.
	ComposeProject string `json:"compose_project,omitempty"`

	// ComposeFile is the on-disk Compose file path that was validated.
	ComposeFile string `json:"compose_file,omitempty"`

	// Valid reports whether docker compose config --quiet accepted the
	// on-disk Compose file. False is a success payload, not an error.
	Valid bool `json:"valid"`

	// Detail is a redactor-scrubbed, user-safe explanation when Valid is
	// false. Raw compose-config stdout is NEVER surfaced here — it
	// interpolates.env secrets — so the engine runs every byte through
	// the active redactor before populating this field.
	Detail string `json:"detail,omitempty"`
}
