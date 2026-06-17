package types

// ConfirmationKindDeleteDestructive is the [Confirmation.Kind] the
// DeleteApp flow carries (PRD §19:444-455). The destructive-deletion
// modal renders the file/dir list (§19:449), the permanence warning
// (§19:450), and the remaining named volumes (§19:454) from this
// payload, and the UI gates it behind the typed-app-name challenge
// (§19:451).
// This is an exported const — an improvement over 's inline
// confirmation kinds (install_deploy, update_deploy, remove_safe,
// update_database_risk remain unexported literals in internal/core).
// Those kinds are NOT migrated here; only the new destructive-delete
// kind gets an exported name, so the CLI and TUI leaves can reference it
// without re-typing the literal.
const ConfirmationKindDeleteDestructive = "delete_destructive"

// DeleteRequest carries the inputs required by Engine.DeleteApp (PRD
// §19:444-455). Destructive deletion is a SEPARATE flow from the safe
// Remove; the safe-Remove contract is untouched.
type DeleteRequest struct {
	// AppID identifies the managed stack to delete.
	AppID string `json:"app_id"`

	// StackPath is an optional fail-closed cross-check: when set it must
	// match the AppID-resolved managed stack or the engine refuses
	// before any deletion, mirroring RemoveRequest's guard.
	StackPath string `json:"stack_path,omitempty"`

	// ConfirmationName is the typed-back app name proving stronger
	// intent (§19:451). The engine re-verifies it equals AppID and
	// refuses on mismatch BEFORE any deletion — a second
	// check alongside the Confirmer prompt. The CLI and TUI only collect
	// and pass it; verification is engine-side.
	ConfirmationName string `json:"confirmation_name"`

	// DeleteNamedVolumes is reserved and hard-refused in v1: when true,
	// the engine rejects the request with a usage-validation error
	// (§19:453) rather than defaulting to false. The field exists to make
	// the contract explicit; the approved volume-deletion flow is +.
	DeleteNamedVolumes bool `json:"delete_named_volumes,omitempty"`
}

// DeleteResult summarizes a completed destructive deletion (PRD
// §19:449, §19:454-455). Named volumes are never deleted, so the
// remaining-volumes list reports what survives.
type DeleteResult struct {
	// AppID is the app that was deleted.
	AppID string `json:"app_id"`

	// DeletedPaths lists the files and directories that were removed
	// (§19:449).
	DeletedPaths []string `json:"deleted_paths,omitempty"`

	// RemainingNamedVolumes lists named volumes that still exist after
	// deletion — v1 never deletes them (§19:454-455).
	RemainingNamedVolumes []string `json:"remaining_named_volumes,omitempty"`

	// RemainingNetworks lists networks left in place after deletion.
	RemainingNetworks []string `json:"remaining_networks,omitempty"`
}
