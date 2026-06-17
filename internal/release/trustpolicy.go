// Package release does Go-native verification of wdm release and catalog
// artifacts plus the self-update helpers built on it. It is the
// dependency-isolation boundary that keeps heavy verifier/attestation
// libraries out of the internal/security leaf: security
// stays a small generic leaf, release carries the trust policy and the
// verification machinery.
// identity every later signing and verification path checks against
// It is identity data only — no checksum,
// signature, attestation, crypto, network, or filesystem behavior. The
// verifier primitives that consume it follow in.2.
package release

// GitHub Actions keyless (Sigstore/Fulcio) trust anchors for the wdm
// release identity. The product verifies against the public wnstify/wdm
// repository. The repo stays
// private through and mints no real signature under the temporary
// private name, but these final anchors are pinned from the start so the
// verifier never has to be re-pointed at the v1 public move (PRD §12
// signing-and-trust-anchor, §14).
const (
	// OIDCIssuer is the GitHub Actions OIDC token issuer. It is the
	// certificate-oidc-issuer value a keyless verification pins for any
	// GitHub-Actions-minted Sigstore signature (PRD §12: prefer keyless
	// signing through GitHub Actions OIDC).
	OIDCIssuer = "https://token.actions.githubusercontent.com"

	// RepositoryURL is the canonical source repository for wdm releases.
	// SourceRepository is its owner/name short form.
	RepositoryURL    = "https://github.com/wnstify/wdm"
	SourceRepository = "wnstify/wdm"

	// ReleaseWorkflowPath is the repository-relative path of the workflow
	// permitted to produce signed releases. It anchors the workflow
	// identity half of the trust policy (PRD §12: pin the expected
	// repository identity, issuer, and release workflow).
	ReleaseWorkflowPath = ".github/workflows/release.yml"

	// tagCertificateIdentityPrefix is the certificate-identity (SAN)
	// prefix for a tag release: the keyless Fulcio cert binds the SAN to
	// "<repo-url>/<workflow-path>@<git-ref>", and a tag release completes
	// it with "refs/tags/<tag>". The concrete <tag> and whether the match
	// is exact or a pattern are the verifier's job in.2, not this
	// record's.
	tagCertificateIdentityPrefix = RepositoryURL + "/" + ReleaseWorkflowPath + "@refs/tags/"
)

// TrustPolicy pins the release identity the product verifies against. It
// is data only — no verification logic, network, or I/O — so production
// builds it via [DefaultTrustPolicy] and tests supply their own against
// fake fixtures (the invariant,; "Trust policy is data, not
// prose").
type TrustPolicy struct {
	// OIDCIssuer is the expected certificate OIDC issuer.
	OIDCIssuer string

	// RepositoryURL is the expected source repository URL;
	// SourceRepository is its owner/name short form.
	RepositoryURL    string
	SourceRepository string

	// ReleaseWorkflowPath is the repository-relative path of the workflow
	// permitted to sign releases.
	ReleaseWorkflowPath string

	// TagCertificateIdentityPrefix is the keyless certificate-identity
	// (SAN) prefix for tag releases. The verifier appends the concrete
	// tag ref to complete the expected identity.
	TagCertificateIdentityPrefix string
}

// DefaultTrustPolicy returns the pinned production trust policy for the
// public wnstify/wdm release identity. The verifier in
// a different one ("Test through seams"); production wiring calls this
// constructor rather than a mutable global.
func DefaultTrustPolicy() TrustPolicy {
	return TrustPolicy{
		OIDCIssuer:                   OIDCIssuer,
		RepositoryURL:                RepositoryURL,
		SourceRepository:             SourceRepository,
		ReleaseWorkflowPath:          ReleaseWorkflowPath,
		TagCertificateIdentityPrefix: tagCertificateIdentityPrefix,
	}
}
