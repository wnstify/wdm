package release

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// AttestationResult is the verified outcome of [VerifyAttestation]: the
// signer identity that passed the trust policy. It is returned only after
// the signature, certificate-identity, OIDC-issuer, transparency-log, and
// timestamp checks all pass.
type AttestationResult struct {
	// VerifiedSAN is the certificate Subject Alternative Name (the workflow
	// identity) required by the policy for this tag and matched against the
	// signing certificate.
	VerifiedSAN string
}

// LoadAttestationBundle parses a Sigstore attestation bundle from raw JSON
// bytes into a [verify.SignedEntity] for [VerifyAttestation]. Splitting the
// byte-parse seam from the trust-verification seam keeps the verifier pure
// and lets offline tests hand [VerifyAttestation] an entity minted by a
// virtual Sigstore directly ("Test through seams").
// It fails closed (PRD §22, §23): empty input or a bundle that does not
// parse and self-validate returns a typed [types.ErrCodeVerificationFailed]
// error. sigstore-go exposes LoadJSONFromPath but no public byte-loader, so
// this mirrors that helper over an in-memory UnmarshalJSON to keep the
// loader off the filesystem (the caller supplies bytes, not a path).
func LoadAttestationBundle(bundleJSON []byte) (verify.SignedEntity, error) {
	if len(bundleJSON) == 0 {
		return nil, verificationError(
			"attestation bundle is empty",
			"a release with no attestation cannot be verified",
			nil,
		)
	}

	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, verificationError(
			"attestation bundle is not a valid Sigstore bundle",
			"the attestation file is malformed",
			err,
		)
	}

	return &b, nil
}

// VerifyAttestation verifies a parsed Sigstore attestation entity carrying
// a SLSA / in-toto DSSE statement against (a) a supplied trusted root and
// (b) an identity policy derived from the supplied [TrustPolicy] for the
// given release tag. It is the in-product attestation check PRD §22 step 7
// / §23 require, done in Go with sigstore-go, never by shelling
// out to cosign.
// trustedMaterial is the trust anchor: production passes the real Sigstore
// [root.TrustedRoot]; offline tests pass a virtual Sigstore implementing
// the same interface ("Test through seams"). entity is the parsed bundle
// from [LoadAttestationBundle] (or a test entity). policy supplies the
// expected OIDC issuer and the tag-certificate-identity prefix; tag is the
// concrete release tag (such as "v1.2.3") completing the expected
// certificate SAN. artifact is the bytes the attestation must cover, bound
// by its SHA-256 digest so verification proves coverage of exactly this
// artifact.
// It fails closed (PRD §22, §23; "missing attestation is a
// verification failure"): a nil entity or trusted root, an empty artifact,
// a blank tag, an incomplete policy, a wrong OIDC issuer, a wrong
// certificate identity (wrong repository or workflow), an untrusted root,
// or any failed signature / timestamp / inclusion check return a typed
// [types.ErrCodeVerificationFailed] error (exit 3). There is no network
// here: failures map to verification, never to
// [types.ErrCodeNetworkFailure].
func VerifyAttestation(
	trustedMaterial root.TrustedMaterial,
	entity verify.SignedEntity,
	policy TrustPolicy,
	tag string,
	artifact []byte,
) (*AttestationResult, error) {
	if trustedMaterial == nil {
		return nil, verificationError("no trusted root supplied for attestation", "", nil)
	}
	if entity == nil {
		return nil, verificationError(
			"no attestation supplied",
			"a release with no attestation cannot be verified",
			nil,
		)
	}
	if len(artifact) == 0 {
		return nil, verificationError("artifact is empty", "", nil)
	}
	if strings.TrimSpace(tag) == "" {
		return nil, verificationError("no release tag supplied for attestation", "", nil)
	}
	if policy.OIDCIssuer == "" || policy.TagCertificateIdentityPrefix == "" {
		return nil, verificationError(
			"trust policy is incomplete for attestation",
			"issuer and certificate-identity prefix must be set",
			nil,
		)
	}

	// Require a transparency-log inclusion and an observer timestamp, the
	// posture real GitHub-Actions keyless attestations carry. This rejects
	// a bundle that drops the tlog proof or the timestamp.
	verifier, err := verify.NewVerifier(
		trustedMaterial,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return nil, verificationError("could not build attestation verifier", "", err)
	}

	// Expected signer identity: exact OIDC issuer and exact certificate SAN
	// (the workflow identity for this tag). Exact matching is stricter than
	// a regex and cannot admit a neighboring repo or workflow.
	expectedSAN := policy.TagCertificateIdentityPrefix + tag
	certID, err := verify.NewShortCertificateIdentity(policy.OIDCIssuer, "", expectedSAN, "")
	if err != nil {
		return nil, verificationError("could not build attestation identity policy", "", err)
	}

	// Bind to the supplied artifact by digest so a valid attestation for a
	// different artifact does not pass.
	digest := sha256.Sum256(artifact)

	result, err := verifier.Verify(entity, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest[:]),
		verify.WithCertificateIdentity(certID),
	))
	if err != nil {
		return nil, verificationError(
			"release attestation did not verify",
			"the attestation is missing, untrusted, or does not match the release identity",
			err,
		)
	}

	if result.Statement == nil {
		return nil, verificationError("attestation carries no in-toto statement", "", nil)
	}

	return &AttestationResult{VerifiedSAN: expectedSAN}, nil
}

// HexDigest returns the lowercase hex SHA-256 of data, the form digests
// take across the verifier surface (SHA256SUMS lines and in-toto subject
// digests alike). It lets a caller cross-check a verified subject digest
// against a locally computed one without re-importing crypto/sha256.
func HexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
