package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/wnstify/wdm/pkg/types"
)

// This file is the binary self-update staging primitive. It downloads the
// candidate release asset set through the
// trusted metadata [Client] and verifies it fail-closed BEFORE the bytes
// are usable, then writes the verified binary into a private staging path.
// It NEVER touches the currently-installed/running binary. The writability
// gate that decides WHETHER the install target is
// user-writable lives in internal/core (it knows os.Executable and the XDG
// layout); this file is the network+verify+stage half the trust boundary
// owns (internal-release-boundary; the invariant keeps heavy verifier deps
// out of the internal/security leaf).
// Network vs trust failure stays strictly distinct: every download fault is
// already a typed
// [types.ErrCodeNetworkFailure] (exit 8) from the [Client], and every
// verification fault is a typed [types.ErrCodeVerificationFailed] (exit 3)
// from the 0.2 verifier primitives. This file adds no new error class — it
// only propagates those two unchanged, plus a few usage-validation guards
// for caller misuse (a missing required asset, a blank staging dir) that
// are programmer/config errors, not trust or transport faults.

// Default download size caps for the self-update asset set. They bound
// memory against a hostile or misbehaving endpoint; a caller may override
// via [StageOptions]. The binary cap is generous for a stripped
// linux/amd64 Go binary; the metadata caps are tight because SHA256SUMS,
// its signature, and the attestation bundle are all small.
const (
	// defaultBinaryMaxBytes bounds the candidate binary download (128 MiB).
	defaultBinaryMaxBytes = 128 << 20

	// defaultChecksumsMaxBytes bounds the SHA256SUMS download (1 MiB).
	defaultChecksumsMaxBytes = 1 << 20

	// defaultSignatureMaxBytes bounds the SHA256SUMS.sig download (64 KiB).
	defaultSignatureMaxBytes = 64 << 10

	// defaultAttestationMaxBytes bounds the attestation.json download
	// (8 MiB). A Sigstore bundle with an inline cert chain and Rekor entry
	// is a few KiB to low tens of KiB; 8 MiB is generous.
	defaultAttestationMaxBytes = 8 << 20

	// stagingDirInsecureBits are the group- and world-write bits a
	// caller-created staging directory must not carry, so a
	// verified-but-staged binary cannot be swapped by another local user
	// before promotion. Enforced by [rejectInsecureStagingDir].
	stagingDirInsecureBits = 0o022

	// stagedBinaryMode is the mode the staged binary file is written with:
	// owner read/write/execute only, no group or world access.
	stagedBinaryMode os.FileMode = 0o700
)

// StageOptions configures [StageCandidate]. The caller supplies the trust
// anchors (policy, trusted root, signing public key) and the resolved
// release metadata, so the primitive stays a pure verify+stage step with
// every input injected behind a seam ("Test through seams"): tests pass a
// [ca.VirtualSigstore] trusted root and a generated public key, production
// passes [root.FetchTrustedRoot] and the embedded release key.
type StageOptions struct {
	// Client is the trusted release metadata client used to download the
	// asset bytes. Required.
	Client *Client

	// Metadata is the resolved release metadata (tag + asset list) the
	// candidate is staged from, as returned by [Client.LatestRelease].
	// Required; its Tag completes the expected attestation certificate
	// identity and its assets resolve the download URLs.
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

	// StagingDir is the directory the verified binary is written into. It
	// MUST already exist and MUST NOT be group- or world-writable.
	// Required.
	StagingDir string

	// LoadAttestation parses the downloaded attestation bytes into a
	// verifiable entity. It is the attestation-parse seam mirroring the
	// [LoadAttestationBundle] / [VerifyAttestation] split (the attestation.go
	// rationale: tests hand an entity minted by a virtual Sigstore directly
	// rather than round-trip bundle JSON, which the test harness does not
	// emit). Nil defaults to [LoadAttestationBundle], the production parser.
	// A loader failure is propagated unchanged — [LoadAttestationBundle]
	// already fails closed (exit 3) on empty or malformed bytes, so a missing
	// attestation stays a verification failure (PRD §23).
	LoadAttestation func([]byte) (verify.SignedEntity, error)

	// BinaryMaxBytes overrides the candidate binary download cap. Zero
	// uses [defaultBinaryMaxBytes].
	BinaryMaxBytes int64

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

// StagedCandidate is the verified outcome of [StageCandidate]: the path of
// the staged binary and the metadata verified to put it there. It is
// returned ONLY after every verification step passes; a verification or
// transport failure returns a nil result and leaves no usable staged
// binary.
type StagedCandidate struct {
	// BinaryPath is the absolute path of the verified binary written into
	// the staging directory. The staging path never points at the live binary.
	BinaryPath string

	// Tag is the release tag the staged binary belongs to (the verified
	// attestation certificate identity is bound to this tag).
	Tag string

	// BinaryDigest is the lowercase hex SHA-256 of the staged binary — the
	// digest that matched SHA256SUMS and the attestation subject.
	BinaryDigest string

	// VerifiedSAN is the certificate Subject Alternative Name the
	// attestation verified against (the release workflow identity).
	VerifiedSAN string
}

// StageCandidate downloads the candidate release asset set, verifies it
// fail-closed, and writes the verified binary into the staging directory.
// It performs download → verify → stage, with NO replacement of the live binary.
// The verification order is strict and fail-closed (PRD §22, §23):
//  1. Download SHA256SUMS, SHA256SUMS.sig, the binary, and attestation.json
//     through the trusted [Client] (each capped). Any transport, DNS,
//     timeout, HTTP, size-cap, or context-cancel fault is the [Client]'s
//     typed [types.ErrCodeNetworkFailure] (exit 8), propagated unchanged.
//  2. Parse SHA256SUMS ([ParseSHA256SUMS]) and verify the binary's digest
//     against it ([VerifyChecksum]).
//  3. Verify the detached Ed25519 signature over SHA256SUMS against the
//     supplied public key ([ParseEd25519PublicKey] + [VerifyDetachedSignature]).
//  4. Verify the SLSA attestation ([LoadAttestationBundle] +
//     [VerifyAttestation]) against the trust-policy identity for the tag,
//     bound to the binary by digest.
//
// Any verification fault in steps 2-4 is a typed
// [types.ErrCodeVerificationFailed] (exit 3) from the verifier primitives,
// propagated unchanged; the staged binary is NEVER written, so a failure
// leaves no usable staged binary. Only after ALL checks pass is the
// verified binary written atomically into StagingDir with owner-only perms
// ([stagedBinaryMode]); the live binary is never touched.
// Caller-misuse guards (nil client/metadata, a missing required asset, a
// blank or insecure staging dir, an incomplete policy) return
// [types.ErrCodeUsageValidation] (exit 2): programmer or configuration
// errors, distinct from both trust (exit 3) and transport (exit 8)
// failures.
func StageCandidate(ctx context.Context, opts StageOptions) (*StagedCandidate, error) {
	if err := ctx.Err(); err != nil {
		// A context already canceled before any work is a transport-class
		// signal (the network step would observe the same), kept distinct
		// from a verification failure.
		return nil, networkError("self-update staging canceled", "", err)
	}
	if err := validateStageOptions(opts); err != nil {
		return nil, err
	}

	tag := strings.TrimSpace(opts.Metadata.Tag)

	// Resolve the four required assets from the metadata by their pinned
	// contract names before any download, so a release missing one fails as
	// a usage error rather than start a confusing partial download.
	binaryAsset, err := requireAsset(opts.Metadata, ArtifactBinary)
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
	binaryBytes, err := opts.Client.DownloadAsset(ctx, binaryAsset, capOrDefault(opts.BinaryMaxBytes, defaultBinaryMaxBytes))
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
	if err := VerifyChecksum(sums, ArtifactBinary, binaryBytes); err != nil {
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
	attResult, err := VerifyAttestation(opts.TrustedRoot, entity, opts.Policy, tag, binaryBytes)
	if err != nil {
		return nil, err
	}

	// --- Stage (only reached when every verification passed) ---

	binaryPath := filepath.Join(opts.StagingDir, ArtifactBinary)
	if err := writeStagedBinary(binaryPath, binaryBytes); err != nil {
		return nil, err
	}

	return &StagedCandidate{
		BinaryPath:   binaryPath,
		Tag:          tag,
		BinaryDigest: HexDigest(binaryBytes),
		VerifiedSAN:  attResult.VerifiedSAN,
	}, nil
}

// StageCandidateProduction is the production assembler for
// [StageCandidate]: it sources the trust anchors internal/core must NOT
// touch directly (the invariant keeps the sigstore-go verifier tree out of
// internal/core), then delegates to [StageCandidate]. internal/core passes
// only the release [Client] (its own download seam), the resolved
// [Metadata], and the staging directory it created beside the install
// target; this function supplies the production [DefaultTrustPolicy], the
// embedded signing key, and the live Sigstore trusted root. It mirrors
// [VerifyCatalogBundleProduction], differing only in the staged artifact
// ([ArtifactBinary]) and that it writes the verified binary into stagingDir.
// The trusted root is fetched from the Sigstore TUF root over the network
// ([root.FetchTrustedRoot]); a fetch failure is a transport-class fault
// mapped to [types.ErrCodeNetworkFailure] (exit 8) so it never masquerades
// as a verification failure. Every other failure is the
// download/verify/stage class [StageCandidate] already returns. The
// embedded signing key is passed through the same validation as
// caller-supplied keys: a nil/empty key is refused before any download or
// verification work, never skipping the signature check.
func StageCandidateProduction(ctx context.Context, client *Client, meta *Metadata, stagingDir string) (*StagedCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, networkError("self-update staging canceled", "", err)
	}
	trustedRoot, err := root.FetchTrustedRoot()
	if err != nil {
		return nil, networkError(
			"could not fetch the Sigstore trusted root",
			"check the network connection and try again",
			err,
		)
	}
	return StageCandidate(ctx, StageOptions{
		Client:           client,
		Metadata:         meta,
		Policy:           DefaultTrustPolicy(),
		TrustedRoot:      trustedRoot,
		SigningPublicKey: EmbeddedSigningPublicKey(),
		StagingDir:       stagingDir,
	})
}

// validateStageOptions rejects caller misuse before any network or
// filesystem work. These are programmer/configuration errors mapped to
// [types.ErrCodeUsageValidation] (exit 2), deliberately distinct from the
// trust (exit 3) and transport (exit 8) error classes.
func validateStageOptions(opts StageOptions) error {
	if opts.Client == nil {
		return usageError("self-update staging requires a release client", "")
	}
	if opts.Metadata == nil {
		return usageError("self-update staging requires release metadata", "")
	}
	if strings.TrimSpace(opts.Metadata.Tag) == "" {
		return usageError(
			"release metadata has no tag",
			"the release endpoint returned no version to stage",
		)
	}
	if len(opts.SigningPublicKey) == 0 {
		return usageError("self-update staging requires a signing public key", "")
	}
	if opts.TrustedRoot == nil {
		return usageError("self-update staging requires a trusted root", "")
	}
	if opts.Policy.OIDCIssuer == "" || opts.Policy.TagCertificateIdentityPrefix == "" {
		return usageError(
			"trust policy is incomplete for self-update staging",
			"issuer and certificate-identity prefix must be set",
		)
	}
	if strings.TrimSpace(opts.StagingDir) == "" {
		return usageError("self-update staging requires a staging directory", "")
	}
	return rejectInsecureStagingDir(opts.StagingDir)
}

// rejectInsecureStagingDir fails closed when the staging directory does
// not exist, is not a directory, carries group/world-write bits — any of
// which would let another local user replace the verified-but-staged binary
// before promotion — or is not owner-writable, which would make
// [writeStagedBinary] fail to create the staged binary at all. The directory
// is the caller's to create; this primitive only refuses an unusable or
// unsafe one (caller misuse, exit 2), distinct from a write fault
// discovered later (exit 1).
func rejectInsecureStagingDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usageError(
				"self-update staging directory does not exist",
				"create the staging directory before staging",
			)
		}
		return usageError("self-update staging directory is not accessible", "")
	}
	if !info.IsDir() {
		return usageError("self-update staging path is not a directory", "")
	}
	if info.Mode().Perm()&stagingDirInsecureBits != 0 {
		return usageError(
			"self-update staging directory is group- or world-writable",
			"use a private staging directory not writable by other users",
		)
	}
	if info.Mode().Perm()&0o200 == 0 {
		return usageError(
			"self-update staging directory is not writable",
			"use a staging directory wdm can write to",
		)
	}
	return nil
}

// writeStagedBinary writes the verified binary into the staging path
// atomically: a same-directory temp file created O_EXCL at
// [stagedBinaryMode], written, fsync'd, closed, then renamed into place,
// with the parent directory fsync'd so the rename is durable. A partial
// write is removed so no half-written staged binary is ever observable.
// internal/release cannot import internal/state (depguard
// internal-release-boundary), so this is a stdlib-only mirror of the
// state-layer atomic-write pattern scoped to the single staged binary.
func writeStagedBinary(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"

	// O_EXCL so a leftover or attacker-planted temp file is refused rather
	// than silently reused.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, stagedBinaryMode) //nolint:gosec // G304: path is a fixed staged-binary name under the caller-validated staging dir, not user input.
	if err != nil {
		return genericError("could not create the staging temp file", "", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()      //nolint:errcheck // primary error is the failed write; best-effort cleanup follows.
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file.
		return genericError("could not write the staged binary", "", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()      //nolint:errcheck // primary error is the failed fsync; best-effort cleanup follows.
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file.
		return genericError("could not flush the staged binary", "", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file.
		return genericError("could not close the staged binary", "", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort removal of the partial temp file.
		return genericError("could not finalize the staged binary", "", err)
	}

	if err := syncDir(dir); err != nil {
		// The rename succeeded but the directory entry is not yet durable;
		// remove the staged binary so a crash cannot leave an unverified-
		// looking partial state and fail closed.
		_ = os.Remove(path) //nolint:errcheck // best-effort removal after a non-durable rename.
		return genericError("could not flush the staging directory", "", err)
	}

	return nil
}

// syncDir fsyncs a directory so a preceding rename into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the caller-validated staging directory, not user input.
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() //nolint:errcheck // primary error is the failed sync; best-effort close follows.
		return err
	}
	return d.Close()
}

// requireAsset resolves a required asset by its pinned contract name. A
// release that does not carry the asset is a usage error (the release is
// malformed for self-update), distinct from a transport or trust failure.
func requireAsset(meta *Metadata, name string) (ReleaseAsset, error) {
	asset, ok := meta.FindAsset(name)
	if !ok {
		return ReleaseAsset{}, usageError(
			"release is missing a required self-update asset",
			"the release does not provide "+name,
		)
	}
	return asset, nil
}

// capOrDefault returns override when positive, otherwise def.
func capOrDefault(override, def int64) int64 {
	if override > 0 {
		return override
	}
	return def
}

// usageError wraps a caller-misuse fault with [types.ErrCodeUsageValidation]
// (exit 2). It is a separate class from the verifier's [verificationError]
// (exit 3) and the client's [networkError] (exit 8) so the three failure
// kinds never conflate.
func usageError(message, hint string) error {
	return types.NewError(types.ErrCodeUsageValidation, message, hint)
}

// genericError wraps an unexpected filesystem fault during staging with
// [types.ErrCodeGeneric] (exit 1). Staging-write faults are local I/O
// errors, not trust or transport failures, so they stay out of those
// classes.
func genericError(message, hint string, cause error) error {
	return types.WrapError(types.ErrCodeGeneric, message, hint, cause)
}
