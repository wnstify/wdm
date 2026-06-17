package release

import (
	"context"
	_ "embed"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// embeddedSigningPublicKey is the long-lived Ed25519 public key the
// product verifies the detached SHA256SUMS signature against (:
// dual-sign — the embedded-key path is the in-product verification half).
// A nil or empty key still fails closed in [validateCatalogVerifyOptions]
// with a usage-validation error (exit 2): the verifier refuses it before
// any network or verification work, NEVER skipping the signature check. (A
// NON-EMPTY but malformed key is the verification-failure case, caught
// later at [ParseEd25519PublicKey] -> exit 3.)
//
//go:embed signing_public_key.pem
var embeddedSigningPublicKey []byte

// EmbeddedSigningPublicKey returns the embedded production signing public
// key bytes (PEM). The catalog/self-update apply paths pass it as the
// SigningPublicKey for verification. A defensive nil guard remains so a
// malformed build still fails closed downstream rather than skip the
// signature check.
func EmbeddedSigningPublicKey() []byte {
	if embeddedSigningPublicKey == nil {
		return nil
	}
	// Defensive copy so a caller cannot mutate the package-level key.
	out := make([]byte, len(embeddedSigningPublicKey))
	copy(out, embeddedSigningPublicKey)
	return out
}

// This file is the catalog-bundle verification primitive: verify before
// apply. It downloads the catalog release
// asset set through the trusted metadata [Client] and verifies it
// fail-closed BEFORE the bytes are usable, returning the verified bundle
// bytes and provenance to the caller (internal/core's ApplyCatalogUpdate)
// for storage. It performs NO filesystem write — extraction and atomic
// storage are internal/catalog's job (StoreVerifiedCatalog); this file is
// the network+verify half the trust boundary owns.
// It mirrors [StageCandidate]'s verify chain step for step —
// ParseSHA256SUMS -> VerifyChecksum -> ParseEd25519PublicKey +
// VerifyDetachedSignature -> LoadAttestationBundle + VerifyAttestation —
// so the binary and catalog trust substrates can never diverge. The only
// differences are the verified artifact ([ArtifactCatalogBundle] instead
// of [ArtifactBinary]) and that nothing is staged: the result carries the
// verified bytes rather than a staged path.
// Network vs trust failure stays strictly distinct: every
// download fault is a typed [types.ErrCodeNetworkFailure] (exit 8) from
// the [Client], and every verification fault is a typed
// [types.ErrCodeVerificationFailed] (exit 3) from the 0.2 verifier
// primitives. This file adds no new error class — it propagates those two
// unchanged plus the caller-misuse usage-validation guards (exit 2).

// Default download size caps for the catalog asset set. They bound memory
// against a hostile or misbehaving endpoint; a caller may override via
// [CatalogVerifyOptions]. The bundle cap is generous for a gzip-tar of
// catalog YAML and templates; the metadata caps mirror [StageCandidate]'s.
const (
	// defaultCatalogBundleMaxBytes bounds the catalog bundle download
	// (64 MiB). The stable bundle is tens of KiB; 64 MiB is generous and
	// matches internal/state's MaxBundleTotalBytes extraction ceiling.
	defaultCatalogBundleMaxBytes = 64 << 20
)

// CatalogVerifyOptions configures [VerifyCatalogBundle]. The caller
// supplies every trust anchor (policy, trusted root, signing public key)
// and the resolved release metadata, so the primitive stays a pure
// download+verify step with every input injected behind a seam ("Test
// through seams"): tests pass a [ca.VirtualSigstore] trusted root and a
// generated public key, production passes [root.FetchTrustedRoot] and the
// embedded release key.
type CatalogVerifyOptions struct {
	// Client is the trusted release metadata client used to download the
	// asset bytes. Required.
	Client *Client

	// Metadata is the resolved release metadata (tag + asset list) the
	// catalog bundle is verified from, as returned by
	// [Client.LatestRelease]. Required; its Tag completes the expected
	// attestation certificate identity and its assets resolve the
	// download URLs.
	Metadata *Metadata

	// Policy is the trust policy the attestation identity is checked
	// against. Required and must be complete (issuer + identity prefix).
	Policy TrustPolicy

	// TrustedRoot is the Sigstore trust anchor the attestation chains to.
	// Required. Production passes [root.FetchTrustedRoot]; offline tests
	// pass a virtual Sigstore implementing the same interface.
	TrustedRoot root.TrustedMaterial

	// SigningPublicKey is the PEM- or DER-encoded Ed25519 public key the
	// detached SHA256SUMS signature is verified against. Required.
	// Production passes [EmbeddedSigningPublicKey]; tests pass a generated
	// key. It is supplied so this primitive stays injectable.
	SigningPublicKey []byte

	// LoadAttestation parses the downloaded attestation bytes into a
	// verifiable entity. It is the attestation-parse seam mirroring
	// [StageOptions.LoadAttestation]; nil defaults to
	// [LoadAttestationBundle], the production parser. A loader failure is
	// propagated unchanged — the production parser already fails closed
	// (exit 3) on empty or malformed bytes, so a missing attestation stays a
	// verification failure (PRD §23).
	LoadAttestation func([]byte) (verify.SignedEntity, error)

	// BundleMaxBytes overrides the catalog bundle download cap. Zero uses
	// [defaultCatalogBundleMaxBytes].
	BundleMaxBytes int64

	// ChecksumsMaxBytes overrides the SHA256SUMS download cap. Zero uses
	// [defaultChecksumsMaxBytes].
	ChecksumsMaxBytes int64

	// SignatureMaxBytes overrides the SHA256SUMS.sig download cap. Zero
	// uses [defaultSignatureMaxBytes].
	SignatureMaxBytes int64

	// AttestationMaxBytes overrides the attestation.json download cap.
	// Zero uses [defaultAttestationMaxBytes].
	AttestationMaxBytes int64
}

// VerifiedCatalogBundle is the verified outcome of [VerifyCatalogBundle]:
// the catalog bundle bytes that passed every check plus the trust metadata
// and the provenance the caller persists alongside the verified manifest.
// It is returned ONLY after every verification step passes; a verification
// or transport failure returns a nil result.
type VerifiedCatalogBundle struct {
	// Tag is the release tag the verified catalog belongs to (the verified
	// attestation certificate identity is bound to this tag). The apply
	// path uses it as the immutable version-snapshot name.
	Tag string

	// Bundle is the verified gzip-tar catalog bundle bytes — exactly the
	// bytes whose digest matched SHA256SUMS and the attestation subject.
	// The caller extracts and stores these via StoreVerifiedCatalog.
	Bundle []byte

	// BundleDigest is the lowercase hex SHA-256 of the verified bundle —
	// the digest that matched SHA256SUMS and the attestation subject.
	BundleDigest string

	// VerifiedSAN is the certificate Subject Alternative Name the
	// attestation verified against (the release workflow identity).
	VerifiedSAN string

	// Provenance carries the downloaded trust artifacts (SHA256SUMS, its
	// detached signature, and the attestation) so the caller can persist
	// them immutably alongside the verified snapshot (PRD §22). Each entry
	// is {name, raw bytes}; the caller maps them to its provenance type.
	Provenance []CatalogProvenanceFile
}

// CatalogProvenanceFile is one downloaded, already-verified trust artifact
// the apply path persists alongside the verified manifest. It is the
// release-side mirror of internal/catalog.ProvenanceFile (kept here so
// internal/catalog needs no internal/release dependency); the caller maps
// the slice across the package boundary.
type CatalogProvenanceFile struct {
	// Name is the release artifact file name (one of the Artifact*
	// checksum/signature/attestation constants).
	Name string

	// Data is the raw, verified file content.
	Data []byte
}

// VerifyCatalogBundle downloads the catalog release asset set and verifies
// it fail-closed, returning the verified bundle bytes: download -> verify,
// with NO filesystem write.
// The verification order is strict and fail-closed (PRD §22, §23),
// mirroring [StageCandidate]:
//  1. Download SHA256SUMS, SHA256SUMS.sig, the catalog bundle, and
//     attestation.json through the trusted [Client] (each capped). Any
//     transport, DNS, timeout, HTTP, size-cap, or context-cancel fault is
//     the [Client]'s typed [types.ErrCodeNetworkFailure] (exit 8),
//     propagated unchanged.
//  2. Parse SHA256SUMS ([ParseSHA256SUMS]) and verify the bundle's digest
//     against it ([VerifyChecksum]).
//  3. Verify the detached Ed25519 signature over SHA256SUMS against
//     the supplied public key ([ParseEd25519PublicKey] +
//     [VerifyDetachedSignature]).
//  4. Verify the SLSA attestation ([LoadAttestationBundle] +
//     [VerifyAttestation]) against the trust-policy identity for the tag,
//     bound to the bundle by digest.
//
// Any verification fault in steps 2-4 is a typed
// [types.ErrCodeVerificationFailed] (exit 3) from the verifier primitives,
// propagated unchanged. Only after ALL checks pass is the verified bundle
// returned; nothing is written, so a failure leaves the caller with no
// usable bundle to store.
// Caller-misuse guards (nil client/metadata, a missing required asset, an
// incomplete policy, a missing key or trusted root) return
// [types.ErrCodeUsageValidation] (exit 2): programmer or configuration
// errors, distinct from both trust (exit 3) and transport (exit 8).
func VerifyCatalogBundle(ctx context.Context, opts CatalogVerifyOptions) (*VerifiedCatalogBundle, error) {
	if err := ctx.Err(); err != nil {
		// A context already canceled before any work is a transport-class
		// signal (the network step would observe the same), kept distinct
		// from a verification failure; mirrors StageCandidate.
		return nil, networkError("catalog bundle verification canceled", "", err)
	}
	if err := validateCatalogVerifyOptions(opts); err != nil {
		return nil, err
	}

	tag := strings.TrimSpace(opts.Metadata.Tag)

	// Resolve the four required assets by their pinned contract names
	// before any download, so a release missing one fails as a usage error
	// rather than start a confusing partial download.
	bundleAsset, err := requireAsset(opts.Metadata, ArtifactCatalogBundle)
	if err != nil {
		return nil, err
	}
	sumsAsset, err := requireAsset(opts.Metadata, ArtifactChecksums)
	if err != nil {
		return nil, err
	}
	sigAsset, err := requireAsset(opts.Metadata, ArtifactChecksumSignature)
	if err != nil {
		return nil, err
	}
	attestationAsset, err := requireAsset(opts.Metadata, ArtifactAttestation)
	if err != nil {
		return nil, err
	}

	// --- Download (transport failures -> exit 8, propagated unchanged) ---

	sumsBytes, err := opts.Client.DownloadAsset(ctx, sumsAsset, capOrDefault(opts.ChecksumsMaxBytes, defaultChecksumsMaxBytes))
	if err != nil {
		return nil, err
	}
	sigBytes, err := opts.Client.DownloadAsset(ctx, sigAsset, capOrDefault(opts.SignatureMaxBytes, defaultSignatureMaxBytes))
	if err != nil {
		return nil, err
	}
	bundleBytes, err := opts.Client.DownloadAsset(ctx, bundleAsset, capOrDefault(opts.BundleMaxBytes, defaultCatalogBundleMaxBytes))
	if err != nil {
		return nil, err
	}
	attestationBytes, err := opts.Client.DownloadAsset(ctx, attestationAsset, capOrDefault(opts.AttestationMaxBytes, defaultAttestationMaxBytes))
	if err != nil {
		return nil, err
	}

	// --- Verify (trust failures -> exit 3, propagated unchanged) ---

	sums, err := ParseSHA256SUMS(sumsBytes)
	if err != nil {
		return nil, err
	}
	if err := VerifyChecksum(sums, ArtifactCatalogBundle, bundleBytes); err != nil {
		return nil, err
	}

	pub, err := ParseEd25519PublicKey(opts.SigningPublicKey)
	if err != nil {
		return nil, err
	}
	if err := VerifyDetachedSignature(pub, sumsBytes, sigBytes); err != nil {
		return nil, err
	}

	loadAttestation := opts.LoadAttestation
	if loadAttestation == nil {
		loadAttestation = LoadAttestationBundle
	}
	entity, err := loadAttestation(attestationBytes)
	if err != nil {
		return nil, err
	}
	attResult, err := VerifyAttestation(opts.TrustedRoot, entity, opts.Policy, tag, bundleBytes)
	if err != nil {
		return nil, err
	}

	// --- Verified: hand the bytes + provenance back (no write here) ---

	return &VerifiedCatalogBundle{
		Tag:          tag,
		Bundle:       bundleBytes,
		BundleDigest: HexDigest(bundleBytes),
		VerifiedSAN:  attResult.VerifiedSAN,
		Provenance: []CatalogProvenanceFile{
			{Name: ArtifactChecksums, Data: sumsBytes},
			{Name: ArtifactChecksumSignature, Data: sigBytes},
			{Name: ArtifactAttestation, Data: attestationBytes},
		},
	}, nil
}

// VerifyCatalogBundleProduction is the production assembler for
// [VerifyCatalogBundle]: it sources the trust anchors internal/core must
// NOT touch directly (the invariant keeps the sigstore-go verifier tree out
// of internal/core), then delegates to [VerifyCatalogBundle]. internal/core
// passes only the release [Client] (its own download seam) and the resolved
// [Metadata]; this function supplies the production [DefaultTrustPolicy],
// the embedded signing key, and the live Sigstore trusted root.
// The trusted root is fetched from the Sigstore TUF root over the network
// ([root.FetchTrustedRoot]); a fetch failure is a transport-class fault
// mapped to [types.ErrCodeNetworkFailure] (exit 8) so it never masquerades
// as a verification failure. Every other failure is the
// download/verify class [VerifyCatalogBundle] already returns. The embedded
// signing key is passed through the same validation as caller-supplied keys:
// a nil/empty key is refused before any download or verification work,
// never skipping the signature check; a non-empty malformed key is caught at
// [ParseEd25519PublicKey] as a verification failure (exit 3).
func VerifyCatalogBundleProduction(ctx context.Context, client *Client, meta *Metadata) (*VerifiedCatalogBundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, networkError("catalog bundle verification canceled", "", err)
	}
	trustedRoot, err := root.FetchTrustedRoot()
	if err != nil {
		return nil, networkError(
			"could not fetch the Sigstore trusted root",
			"check the network connection and try again",
			err,
		)
	}
	return VerifyCatalogBundle(ctx, CatalogVerifyOptions{
		Client:           client,
		Metadata:         meta,
		Policy:           DefaultTrustPolicy(),
		TrustedRoot:      trustedRoot,
		SigningPublicKey: EmbeddedSigningPublicKey(),
	})
}

// validateCatalogVerifyOptions rejects caller misuse before any network or
// verification work. These are programmer/configuration errors mapped to
// [types.ErrCodeUsageValidation] (exit 2), distinct from the trust (exit 3)
// and transport (exit 8) error classes; it mirrors [validateStageOptions]
// minus the staging-dir guards (this primitive never writes).
func validateCatalogVerifyOptions(opts CatalogVerifyOptions) error {
	if opts.Client == nil {
		return usageError("catalog verification requires a release client", "")
	}
	if opts.Metadata == nil {
		return usageError("catalog verification requires release metadata", "")
	}
	if strings.TrimSpace(opts.Metadata.Tag) == "" {
		return usageError(
			"release metadata has no tag",
			"the release endpoint returned no version to verify",
		)
	}
	if len(opts.SigningPublicKey) == 0 {
		return usageError("catalog verification requires a signing public key", "")
	}
	if opts.TrustedRoot == nil {
		return usageError("catalog verification requires a trusted root", "")
	}
	if opts.Policy.OIDCIssuer == "" || opts.Policy.TagCertificateIdentityPrefix == "" {
		return usageError(
			"trust policy is incomplete for catalog verification",
			"issuer and certificate-identity prefix must be set",
		)
	}
	return nil
}
