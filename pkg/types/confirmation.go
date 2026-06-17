package types

// Confirmation is the payload an Engine write method passes to a
// Confirmer when it needs explicit user authorization for a consequence
// (PRD §37). defines the minimum shape the Confirmer interface
// needs to compile; lifecycle implementations populate it with
// richer context for the consequences they authorize.
type Confirmation struct {
	// Kind identifies the class of consequence — e.g. "destroy_data" or
	// "recreate_containers". Stable across releases for safe grouping in
	// telemetry and tests.
	Kind string `json:"kind"`

	// Title is a short headline shown by the TUI/CLI/GUI as the prompt
	// banner.
	Title string `json:"title"`

	// Message explains the consequence in user-readable terms.
	Message string `json:"message"`
}
