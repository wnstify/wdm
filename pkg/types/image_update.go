package types

import "time"

// ImageUpdateQuery selects the managed app whose service images
// Engine.CheckImageUpdates checks against the container registry (PRD
// §14, §20). The check is Go-native and read-only: it contacts the
// registry over Go HTTP, returns tag/digest candidates, and feeds the
// EXISTING app-update planning surface, never running Docker lifecycle
// commands and never applying. There is no apply
// counterpart by design — app updates apply only through Engine.Update.
type ImageUpdateQuery struct {
	// AppID identifies the managed stack to check registry images for.
	AppID string `json:"app_id"`
}

// ImageUpdateReport is the read-only result of
// Engine.CheckImageUpdates (PRD §14, §20). It lists the registry-derived
// tag/digest candidates per service so the existing app-update planning
// surface can show them (exit criterion "registry-derived tag/digest
// changes are shown when known"). No runtime.lock, no Confirmer, no
// ProgressFn, mirroring Status.
type ImageUpdateReport struct {
	// AppID is the app the report covers.
	AppID string `json:"app_id"`

	// Candidates lists the per-service registry findings.
	Candidates []ImageUpdateCandidate `json:"candidates,omitempty"`

	// CheckedAt is when the check contacted the registry.
	CheckedAt time.Time `json:"checked_at"`
}

// ImageUpdateCandidate is one service's registry finding — the current
// pinned image reference and the latest tag/digest the registry reports
// (PRD §14, §20). Digests may be empty when the registry response omits
// one or the current digest was never recorded; each is surfaced when
// known and omitted otherwise.
type ImageUpdateCandidate struct {
	// Service is the Compose service name the image backs.
	Service string `json:"service"`

	// Image is the image reference without tag or digest.
	Image string `json:"image"`

	// CurrentTag is the tag the stack currently pins.
	CurrentTag string `json:"current_tag,omitempty"`

	// CurrentDigest is the digest the stack currently records, empty when
	// unknown.
	CurrentDigest string `json:"current_digest,omitempty"`

	// LatestTag is the latest tag the registry reports for the image.
	LatestTag string `json:"latest_tag,omitempty"`

	// LatestDigest is the digest the registry reports for the latest tag,
	// empty when the registry response does not carry one.
	LatestDigest string `json:"latest_digest,omitempty"`

	// UpdateAvailable reports whether the registry tag/digest differs
	// from the currently pinned reference. It always serializes so a
	// "no change" finding is explicit.
	UpdateAvailable bool `json:"update_available"`
}
