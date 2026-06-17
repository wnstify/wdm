package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
)

const (
	testTag    = "v1.2.3"
	testIssuer = release.OIDCIssuer
)

// testPolicy returns the production trust policy; the SAN is composed from
// its TagCertificateIdentityPrefix + the release tag.
func testPolicy() release.TrustPolicy {
	return release.DefaultTrustPolicy()
}

// expectedSAN is the exact certificate Subject Alternative Name a valid
// release attestation for testTag must carry.
func expectedSAN() string {
	return testPolicy().TagCertificateIdentityPrefix + testTag
}

// statementFor builds an in-toto v0.1 statement whose single subject binds
// name to the SHA-256 of artifact — the shape GitHub/SLSA provenance uses.
func statementFor(name string, artifact []byte) []byte {
	sum := sha256.Sum256(artifact)
	return fmt.Appendf(nil,
		`{"_type":"https://in-toto.io/Statement/v0.1",`+
			`"predicateType":"https://slsa.dev/provenance/v0.2",`+
			`"subject":[{"name":%q,"digest":{"sha256":%q}}],`+
			`"predicate":{}}`,
		name, hex.EncodeToString(sum[:]),
	)
}

// attestEntity mints an offline attestation entity for artifact under the
// given virtual Sigstore, signed by identity san + issuer.
func attestEntity(t *testing.T, vs *ca.VirtualSigstore, san, issuer, name string, artifact []byte) verify.SignedEntity {
	t.Helper()

	entity, err := vs.Attest(san, issuer, statementFor(name, artifact))
	require.NoError(t, err)
	return entity
}

func TestVerifyAttestation_ValidIdentityAndArtifact(t *testing.T) {
	t.Parallel()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	artifact := []byte("the verified wdm-linux-amd64 bytes")
	entity := attestEntity(t, vs, expectedSAN(), testIssuer, "wdm-linux-amd64", artifact)

	res, err := release.VerifyAttestation(vs, entity, testPolicy(), testTag, artifact)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, testIssuer, res.VerifiedIssuer)
	assert.Equal(t, expectedSAN(), res.VerifiedSAN)

	require.Len(t, res.Subjects, 1)
	assert.Equal(t, "wdm-linux-amd64", res.Subjects[0].Name)

	// The verified subject digest binds to exactly the supplied artifact.
	assert.Equal(t, release.HexDigest(artifact), res.Subjects[0].Digests["sha256"])
}

func TestVerifyAttestation_WrongIssuerFails(t *testing.T) {
	t.Parallel()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	artifact := []byte("artifact signed under wrong issuer")

	// Mint with a SAN that matches the policy but a DIFFERENT issuer.
	entity := attestEntity(t, vs, expectedSAN(), "https://accounts.google.com", "wdm-linux-amd64", artifact)

	_, err = release.VerifyAttestation(vs, entity, testPolicy(), testTag, artifact)
	requireVerificationError(t, err)
}

func TestVerifyAttestation_WrongCertificateIdentityFails(t *testing.T) {
	t.Parallel()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	artifact := []byte("artifact signed by a neighboring repo workflow")

	tests := []struct {
		name string
		san  string
	}{
		{
			name: "wrong repository",
			san:  "https://github.com/evil/wdm/.github/workflows/release.yml@refs/tags/" + testTag,
		},
		{
			name: "wrong workflow file",
			san:  "https://github.com/wnstify/wdm/.github/workflows/evil.yml@refs/tags/" + testTag,
		},
		{
			name: "wrong tag",
			san:  testPolicy().TagCertificateIdentityPrefix + "v9.9.9",
		},
		{
			name: "branch ref instead of tag",
			san:  "https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/heads/main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entity := attestEntity(t, vs, tt.san, testIssuer, "wdm-linux-amd64", artifact)

			_, err := release.VerifyAttestation(vs, entity, testPolicy(), testTag, artifact)
			requireVerificationError(t, err)
		})
	}
}

func TestVerifyAttestation_TamperedArtifactFails(t *testing.T) {
	t.Parallel()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	signed := []byte("the genuinely attested artifact")
	entity := attestEntity(t, vs, expectedSAN(), testIssuer, "wdm-linux-amd64", signed)

	// The attestation is valid, but the artifact handed to the verifier is
	// not the one it covers: the digest binding must reject it.
	tampered := []byte("a substituted artifact with a different digest")

	_, err = release.VerifyAttestation(vs, entity, testPolicy(), testTag, tampered)
	requireVerificationError(t, err)
}

func TestVerifyAttestation_DifferentTrustedRootFails(t *testing.T) {
	t.Parallel()

	signerSigstore, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	// A completely independent Sigstore instance acts as the trusted root.
	// The entity was minted by signerSigstore, so its Fulcio chain does not
	// chain to this root and verification must fail.
	otherRoot, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	artifact := []byte("artifact attested under an untrusted root")
	entity := attestEntity(t, signerSigstore, expectedSAN(), testIssuer, "wdm-linux-amd64", artifact)

	_, err = release.VerifyAttestation(otherRoot, entity, testPolicy(), testTag, artifact)
	requireVerificationError(t, err)
}

func TestVerifyAttestation_MissingAndInvalidInputsFail(t *testing.T) {
	t.Parallel()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	artifact := []byte("artifact")
	entity := attestEntity(t, vs, expectedSAN(), testIssuer, "wdm-linux-amd64", artifact)

	t.Run("nil trusted root", func(t *testing.T) {
		t.Parallel()

		var nilRoot root.TrustedMaterial
		_, err := release.VerifyAttestation(nilRoot, entity, testPolicy(), testTag, artifact)
		requireVerificationError(t, err)
	})

	t.Run("nil entity (missing attestation)", func(t *testing.T) {
		t.Parallel()

		_, err := release.VerifyAttestation(vs, nil, testPolicy(), testTag, artifact)
		requireVerificationError(t, err)
	})

	t.Run("empty artifact", func(t *testing.T) {
		t.Parallel()

		_, err := release.VerifyAttestation(vs, entity, testPolicy(), testTag, nil)
		requireVerificationError(t, err)
	})

	t.Run("blank tag", func(t *testing.T) {
		t.Parallel()

		_, err := release.VerifyAttestation(vs, entity, testPolicy(), "   ", artifact)
		requireVerificationError(t, err)
	})

	t.Run("incomplete policy", func(t *testing.T) {
		t.Parallel()

		_, err := release.VerifyAttestation(vs, entity, release.TrustPolicy{}, testTag, artifact)
		requireVerificationError(t, err)
	})
}

func TestLoadAttestationBundle_FailsClosed(t *testing.T) {
	t.Parallel()

	t.Run("empty bytes", func(t *testing.T) {
		t.Parallel()

		_, err := release.LoadAttestationBundle(nil)
		requireVerificationError(t, err)
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()

		_, err := release.LoadAttestationBundle([]byte("{not a sigstore bundle}"))
		requireVerificationError(t, err)
	})

	t.Run("valid json but not a bundle", func(t *testing.T) {
		t.Parallel()

		_, err := release.LoadAttestationBundle([]byte(`{"hello":"world"}`))
		requireVerificationError(t, err)
	})
}
