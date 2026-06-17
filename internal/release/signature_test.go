package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// newEd25519Key generates a fresh Ed25519 key pair for a test. No
// production key is ever embedded — the verifier takes the public key as a
// parameter.
func newEd25519Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

// signEd25519 signs the exact payload bytes with key, returning the raw
// 64-byte signature the on-disk SHA256SUMS.sig carries. There is no prehash.
func signEd25519(t *testing.T, key ed25519.PrivateKey, payload []byte) []byte {
	t.Helper()

	return ed25519.Sign(key, payload)
}

// pubKeyPEM marshals a public key to PKIX PEM ("BEGIN PUBLIC KEY").
func pubKeyPEM(t *testing.T, pub any) []byte {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestParseEd25519PublicKey_AcceptsPEMAndDER(t *testing.T) {
	t.Parallel()

	pub, _ := newEd25519Key(t)

	pemBytes := pubKeyPEM(t, pub)
	derBytes, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)

	fromPEM, err := release.ParseEd25519PublicKey(pemBytes)
	require.NoError(t, err)
	assert.True(t, fromPEM.Equal(pub))

	fromDER, err := release.ParseEd25519PublicKey(derBytes)
	require.NoError(t, err)
	assert.True(t, fromDER.Equal(pub))
}

func TestParseEd25519PublicKey_FailsClosed(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()

		_, err := release.ParseEd25519PublicKey(nil)
		requireVerificationError(t, err)
	})

	t.Run("garbage bytes", func(t *testing.T) {
		t.Parallel()

		_, err := release.ParseEd25519PublicKey([]byte("not a key at all"))
		requireVerificationError(t, err)
	})

	t.Run("rsa key is rejected", func(t *testing.T) {
		t.Parallel()

		// An RSA PKIX key parses but is not Ed25519.
		rsaPEM := generateRSAPublicKeyPEM(t)
		_, err := release.ParseEd25519PublicKey(rsaPEM)
		requireVerificationError(t, err)
	})
}

func TestVerifyDetachedSignature_Valid(t *testing.T) {
	t.Parallel()

	pub, priv := newEd25519Key(t)
	signed := []byte("SHA256SUMS file content over every release asset")

	sig := signEd25519(t, priv, signed)

	assert.NoError(t, release.VerifyDetachedSignature(pub, signed, sig))
}

func TestVerifyDetachedSignature_FailsClosed(t *testing.T) {
	t.Parallel()

	pub, priv := newEd25519Key(t)
	signed := []byte("the canonical SHA256SUMS payload")
	sig := signEd25519(t, priv, signed)

	t.Run("nil key", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyDetachedSignature(nil, signed, sig)
		requireVerificationError(t, err)
	})

	t.Run("empty payload", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyDetachedSignature(pub, nil, sig)
		requireVerificationError(t, err)
	})

	t.Run("wrong-length signature", func(t *testing.T) {
		t.Parallel()

		// A signature that is not exactly ed25519.SignatureSize bytes is
		// rejected before any verification attempt.
		err := release.VerifyDetachedSignature(pub, signed, sig[:len(sig)-1])
		requireVerificationError(t, err)
	})

	t.Run("tampered payload", func(t *testing.T) {
		t.Parallel()

		err := release.VerifyDetachedSignature(pub, []byte("tampered payload"), sig)
		requireVerificationError(t, err)
	})

	t.Run("tampered signature", func(t *testing.T) {
		t.Parallel()

		bad := make([]byte, len(sig))
		copy(bad, sig)
		bad[0] ^= 0xff
		err := release.VerifyDetachedSignature(pub, signed, bad)
		requireVerificationError(t, err)
	})

	t.Run("wrong key", func(t *testing.T) {
		t.Parallel()

		other, _ := newEd25519Key(t)
		err := release.VerifyDetachedSignature(other, signed, sig)
		requireVerificationError(t, err)
	})
}

// requireVerificationError asserts err is a non-nil typed verification
// failure (exit 3).
func requireVerificationError(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	assert.True(
		t,
		types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed, got %v", err,
	)
}

// generateRSAPublicKeyPEM produces a PKIX PEM-encoded RSA public key so the
// parser's "not Ed25519" rejection arm is exercised with a real, parseable
// non-Ed25519 key.
func generateRSAPublicKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return pubKeyPEM(t, &key.PublicKey)
}
