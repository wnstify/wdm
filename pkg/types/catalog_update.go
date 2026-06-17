package types

import "time"

// ConfirmationKindCatalogUpdate is the [Confirmation.Kind] the
// ApplyCatalogUpdate flow carries (PRD §22). From the matching
// [CatalogUpdateStatus] the TUI/CLI renders the current local catalog
// version, the verified latest version, the change summary, and the
// verification state before gating the download/apply on this prompt
// (the invariant — network actions are explicit; the invariant — trust
// substrate precedes apply). It is an exported const, matching the
const ConfirmationKindCatalogUpdate = "catalog_update"

// CatalogUpdateQuery selects which channel Engine.CheckCatalogUpdate
// inspects for an available catalog update (PRD §22). v1 ships the
// "stable" channel only (PRD §22); an empty Channel means
// the configured default channel ("stable" in v1).
type CatalogUpdateQuery struct {
	// Channel is the catalog channel to check. Empty selects the
	// configured default ("stable" in v1).
	Channel string `json:"channel,omitempty"`
}

// CatalogChange is one entry in a catalog update's change summary — the
// per-app delta a user reviews before authorizing the download/apply
// (PRD §22). A struct rather than a free-form line, so the UI can render
// and group changes consistently across CLI, TUI, and the future GUI.
type CatalogChange struct {
	// AppID is the catalog identifier the change applies to.
	AppID string `json:"app_id"`

	// Kind classifies the change ("added", "updated", or "removed").
	Kind string `json:"kind"`

	// Summary is the one-line human-readable description of the change.
	Summary string `json:"summary,omitempty"`
}

// CatalogUpdateStatus is the read-only result of
// Engine.CheckCatalogUpdate (PRD §22). It reports the current local
// catalog version, the latest available version, whether an update is
// available, the change summary, and the verification state — what the
// UI surfaces before any download or apply (exit criterion "reports
// current local version, latest stable release, and a change summary").
// The check is read-only: no runtime.lock, no Confirmer, no ProgressFn,
// mirroring Status.
type CatalogUpdateStatus struct {
	// Channel is the catalog channel that was checked.
	Channel string `json:"channel"`

	// CurrentVersion is the version of the verified local catalog under
	// the channel, empty when no local catalog has been installed yet.
	CurrentVersion string `json:"current_version,omitempty"`

	// LatestVersion is the latest catalog version available from the
	// configured catalog endpoint for the channel.
	LatestVersion string `json:"latest_version,omitempty"`

	// UpdateAvailable reports whether LatestVersion is newer than
	// CurrentVersion. It always serializes so a "no update" result is
	// explicit rather than inferred from absent fields.
	UpdateAvailable bool `json:"update_available"`

	// Changes is the per-app change summary between the local and latest
	// catalog, empty when no update is available.
	Changes []CatalogChange `json:"changes,omitempty"`

	// Verified reports whether the latest catalog metadata passed
	// checksum, signature, and attestation verification (PRD §22, §23,
	// the invariant). It always serializes: an unverified latest is a
	// fail-closed signal the UI must surface.
	Verified bool `json:"verified"`

	// CheckedAt is when the check contacted the catalog endpoint.
	CheckedAt time.Time `json:"checked_at"`
}

// CatalogUpdateRequest carries the inputs required by
// Engine.ApplyCatalogUpdate (PRD §22). The apply downloads, verifies
// (checksum, signature, attestation) BEFORE writing any catalog file,
// writes atomically under the runtime.lock, refuses a signed rollback
// and never modifies deployed apps.
type CatalogUpdateRequest struct {
	// Channel is the catalog channel to apply the update to. Empty
	// selects the configured default ("stable" in v1).
	Channel string `json:"channel,omitempty"`

	// TargetVersion is the catalog version to apply — the version
	// surfaced by Engine.CheckCatalogUpdate. The engine re-verifies the
	// downloaded artifact matches it before any write.
	TargetVersion string `json:"target_version,omitempty"`
}

// CatalogUpdateResult summarizes a completed catalog update (PRD §22).
// It reports the channel, the version before and after the apply, the
// verification detail, what changed, and when the apply completed. The
// apply never touches deployed apps or per-stack .wdm.lock files
type CatalogUpdateResult struct {
	// Channel is the catalog channel that was updated.
	Channel string `json:"channel"`

	// PreviousVersion is the local catalog version before the apply,
	// empty when no local catalog existed.
	PreviousVersion string `json:"previous_version,omitempty"`

	// AppliedVersion is the catalog version now installed locally.
	AppliedVersion string `json:"applied_version"`

	// VerificationDetail is a short, user-safe description of the
	// verification that passed before the write (PRD §22, §23).
	VerificationDetail string `json:"verification_detail,omitempty"`

	// Changes is the per-app change summary the apply realized.
	Changes []CatalogChange `json:"changes,omitempty"`

	// AppliedAt is when the apply finished writing the verified catalog.
	AppliedAt time.Time `json:"applied_at"`
}
