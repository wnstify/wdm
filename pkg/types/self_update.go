package types

import "time"

// ConfirmationKindSelfUpdate is the [Confirmation.Kind] the
// ApplySelfUpdate flow carries (PRD §14). From the matching
// [SelfUpdateStatus] the TUI/CLI renders the current binary version, the
// verified candidate version, and the verification state before gating
// the download/replace on this prompt (the invariant — self-update keeps
// a rollback binary; the invariant — network actions are explicit). It is
// an exported const, matching the
// [ConfirmationKindDeleteDestructive] precedent.
const ConfirmationKindSelfUpdate = "self_update"

// SelfUpdateQuery carries the inputs for Engine.CheckSelfUpdate (PRD
// §14). The check is read-only: it reports the current binary version
// against the latest release without downloading or replacing anything
// (no runtime.lock, no Confirmer, no ProgressFn, mirroring Status). v1
// needs no selector — the binary self-update tracks the single published
// release line — so the query is empty today. A named struct, not a
// parameterless method, lets the surface add a channel or pre-release
// hint additively without a signature change.
type SelfUpdateQuery struct{}

// SelfUpdateStatus is the read-only result of Engine.CheckSelfUpdate
// (PRD §14). It reports the running binary version, the latest release
// version, and whether a newer verified candidate exists — the
// information the UI surfaces before any download or replace (exit
// criterion "reports current local version, latest stable release").
type SelfUpdateStatus struct {
	// CurrentVersion is the version of the running wdm binary.
	CurrentVersion string `json:"current_version"`

	// LatestVersion is the latest published release version, empty when
	// the release endpoint reports none.
	LatestVersion string `json:"latest_version,omitempty"`

	// UpdateAvailable reports whether a newer verified candidate exists.
	// It always serializes so a "no update" result is explicit.
	UpdateAvailable bool `json:"update_available"`

	// Verified reports whether the latest release metadata passed
	// checksum, signature, and attestation verification (PRD §14, §22,
	// §23, the invariant). It always serializes: an unverified candidate
	// is a fail-closed signal the UI must surface.
	Verified bool `json:"verified"`

	// CheckedAt is when the check contacted the release endpoint.
	CheckedAt time.Time `json:"checked_at"`

	// Notes carries optional operator guidance lines (for example a
	// note that the executable path is not user-writable).
	Notes []string `json:"notes,omitempty"`
}

// SelfUpdateRequest carries the inputs required by
// Engine.ApplySelfUpdate (PRD §14). The apply downloads and verifies the
// candidate BEFORE replacing anything, stages it, replaces
// the binary atomically where practical, retains the prior binary as
// wdm.previous, runs the exact-version `wdm --version` smoke check, and
// restores the previous binary if the smoke check fails.
type SelfUpdateRequest struct {
	// TargetVersion is the release version to apply — the version
	// surfaced by Engine.CheckSelfUpdate. The engine re-verifies the
	// downloaded artifact and the post-replace smoke check report this
	// version before keeping the new binary.
	TargetVersion string `json:"target_version,omitempty"`
}

// SelfUpdateResult summarizes a completed (or rolled-back) self-update
// (PRD §14, §31). It reports the version transition,
// whether the binary was replaced, whether the smoke check passed,
// whether the previous binary was restored, the wdm.previous path, and a
// user-readable message — so a failed smoke check that restores the
// previous binary reports the rollback (exit criterion "failed smoke
// restores the previous binary and reports the rollback result").
type SelfUpdateResult struct {
	// PreviousVersion is the binary version before the apply.
	PreviousVersion string `json:"previous_version"`

	// AppliedVersion is the candidate version the apply targeted.
	AppliedVersion string `json:"applied_version,omitempty"`

	// Replaced reports whether the new binary now occupies the
	// executable path. It always serializes so the outcome is explicit.
	Replaced bool `json:"replaced"`

	// SmokeOK reports whether the post-replacement `wdm --version` smoke
	// check exited 0 and reported the expected version. It always
	// serializes.
	SmokeOK bool `json:"smoke_ok"`

	// RolledBack reports whether a failed smoke check restored the
	// previous binary. It always serializes: a rollback is exactly when
	// rolled_back:true is the signal the UI must surface.
	RolledBack bool `json:"rolled_back"`

	// PreviousBinaryPath is the path of the retained prior binary
	// (wdm.previous) used for rollback.
	PreviousBinaryPath string `json:"previous_binary_path,omitempty"`

	// Message is a user-readable summary of the outcome.
	Message string `json:"message,omitempty"`
}
