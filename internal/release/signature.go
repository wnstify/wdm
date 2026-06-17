package release

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// ParseEd25519PublicKey parses an Ed25519 public key from PEM- or
// DER-encoded PKIX (SubjectPublicKeyInfo) bytes. A leading PEM block (such
// as a "-----BEGIN PUBLIC KEY-----" wrapper) is decoded first; non-PEM
// bytes are treated as raw DER.
// It fails closed (PRD §22, §23): empty input, undecodable bytes, a
// non-Ed25519 key, or a key of the wrong length return a typed
// [types.ErrCodeVerificationFailed] error. The algorithm is pinned to
// Ed25519 because the detached SHA256SUMS signature is an Ed25519 signature
// over the exact checksum-file bytes; accepting another key type would let
// a key of the wrong strength satisfy the policy.
func ParseEd25519PublicKey(keyBytes []byte) (ed25519.PublicKey, error) {
	if len(keyBytes) == 0 {
		return nil, verificationError("public key is empty", "", nil)
	}

	der := keyBytes
	if block, _ := pem.Decode(keyBytes); block != nil {
		der = block.Bytes
	}

	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, verificationError(
			"public key is not valid PKIX DER/PEM",
			"the signing public key is malformed",
			err,
		)
	}

	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, verificationError(
			"public key is not an ed25519 key",
			"the release signature scheme requires an ed25519 public key",
			nil,
		)
	}

	if len(pub) != ed25519.PublicKeySize {
		return nil, verificationError(
			"ed25519 public key has the wrong length",
			"the release signature scheme requires an ed25519 public key",
			nil,
		)
	}

	return pub, nil
}

// VerifyDetachedSignature verifies a detached Ed25519 signature over signed
// against the supplied public key. signed is the exact signed payload (the
// raw SHA256SUMS bytes); it is verified directly, with no prehash. signature
// is the raw 64-byte Ed25519 signature the on-disk SHA256SUMS.sig carries.
// The caller supplies the public key (production wires the long-lived
// release key; tests pass a generated key). No key is embedded in this
// primitive.
// It fails closed (PRD §22, §23): a nil key, an empty payload, a signature
// of the wrong length, or any signature that does not verify return a typed
// [types.ErrCodeVerificationFailed] error.
func VerifyDetachedSignature(pub ed25519.PublicKey, signed, signature []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return verificationError(
			"signing public key is not a valid ed25519 key",
			"the release signature scheme requires an ed25519 public key",
			nil,
		)
	}
	if len(signed) == 0 {
		return verificationError("signed payload is empty", "", nil)
	}
	if len(signature) != ed25519.SignatureSize {
		return verificationError(
			"signature has the wrong length for ed25519",
			"the release signature is invalid or was made with a different scheme",
			errSignatureInvalid,
		)
	}

	// ed25519.Verify returns false (never panics) on a bad signature; it
	// verifies over the exact signed bytes, with no prehash.
	if !ed25519.Verify(pub, signed, signature) {
		return verificationError(
			"checksum-file signature does not verify",
			"the release signature is invalid or was made with a different key",
			errSignatureInvalid,
		)
	}

	return nil
}

// errSignatureInvalid is the cause attached to a failed detached-signature
// verification, so callers can branch on the signature-specific failure
// via errors.Is while the surfaced *Error stays a generic verification
// failure.
var errSignatureInvalid = errors.New("release: detached ed25519 signature did not verify")
