package release_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
)

// fakeCatalog is the offline catalog-bundle analog of fakeRelease
// (selfupdate_test.go): a virtual Sigstore, signing key, the catalog
// bundle bytes, and an httptest server serving the asset set. It exercises
// VerifyCatalogBundle without touching real github.com or the live root.
type fakeCatalog struct {
	vs          *ca.VirtualSigstore
	signingKey  ed25519.PrivateKey
	pubKeyPEM   []byte
	bundle      []byte
	sums        []byte
	sig         []byte
	attestation []byte
	entity      verify.SignedEntity
	srv         *httptest.Server
	client      *release.Client
}

func newFakeCatalog(t *testing.T) *fakeCatalog {
	t.Helper()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	bundle := []byte("the verified catalog-stable.tar.gz bytes\n")

	fc := &fakeCatalog{vs: vs, signingKey: key, pubKeyPEM: pubPEM, bundle: bundle}
	fc.sums = buildSums(t, map[string][]byte{release.ArtifactCatalogBundle: bundle})
	fc.sig = signEd25519(t, key, fc.sums)
	fc.attestation = []byte(attestationSentinel)
	fc.entity = attestEntity(t, vs, expectedSAN(), testIssuer, release.ArtifactCatalogBundle, bundle)
	fc.startServer(t)
	return fc
}

func (fc *fakeCatalog) loadAttestation(b []byte) (verify.SignedEntity, error) {
	if string(b) == attestationSentinel {
		return fc.entity, nil
	}
	return release.LoadAttestationBundle(b)
}

func (fc *fakeCatalog) startServer(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	serve := func(p string, body func() []byte) {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			b := body()
			if b == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		})
	}
	serve("/dl/"+release.ArtifactChecksums, func() []byte { return fc.sums })
	serve("/dl/"+release.ArtifactChecksumSignature, func() []byte { return fc.sig })
	serve("/dl/"+release.ArtifactCatalogBundle, func() []byte { return fc.bundle })
	serve("/dl/"+release.ArtifactAttestation, func() []byte { return fc.attestation })

	fc.srv = httptest.NewServer(mux)
	t.Cleanup(fc.srv.Close)

	c, err := release.NewClient(
		release.DefaultTrustPolicy(),
		release.WithBaseURL(fc.srv.URL),
		release.WithHTTPClient(fc.srv.Client()),
	)
	require.NoError(t, err)
	fc.client = c
}

func (fc *fakeCatalog) metadata() *release.Metadata {
	url := func(name string) string { return fc.srv.URL + "/dl/" + name }
	return &release.Metadata{
		Tag: testTag,
		Assets: []release.ReleaseAsset{
			{Name: release.ArtifactCatalogBundle, DownloadURL: url(release.ArtifactCatalogBundle), Size: int64(len(fc.bundle))},
			{Name: release.ArtifactChecksums, DownloadURL: url(release.ArtifactChecksums), Size: int64(len(fc.sums))},
			{Name: release.ArtifactChecksumSignature, DownloadURL: url(release.ArtifactChecksumSignature), Size: int64(len(fc.sig))},
			{Name: release.ArtifactAttestation, DownloadURL: url(release.ArtifactAttestation), Size: int64(len(fc.attestation))},
		},
	}
}

func (fc *fakeCatalog) options() release.CatalogVerifyOptions {
	return release.CatalogVerifyOptions{
		Client:           fc.client,
		Metadata:         fc.metadata(),
		Policy:           release.DefaultTrustPolicy(),
		TrustedRoot:      fc.vs,
		SigningPublicKey: fc.pubKeyPEM,
		LoadAttestation:  fc.loadAttestation,
	}
}

func TestVerifyCatalogBundle_HappyPath(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, testTag, got.Tag)
	assert.Equal(t, fc.bundle, got.Bundle)
	assert.Equal(t, release.HexDigest(fc.bundle), got.BundleDigest)
	assert.Equal(t, release.DefaultTrustPolicy().TagCertificateIdentityPrefix+testTag, got.VerifiedSAN)

	// Provenance carries the three downloaded trust artifacts verbatim.
	prov := map[string][]byte{}
	for _, p := range got.Provenance {
		prov[p.Name] = p.Data
	}
	assert.Equal(t, fc.sums, prov[release.ArtifactChecksums])
	assert.Equal(t, fc.sig, prov[release.ArtifactChecksumSignature])
	assert.Equal(t, fc.attestation, prov[release.ArtifactAttestation])
}

func TestVerifyCatalogBundle_BadChecksumFailsClosed(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	// SHA256SUMS records a digest for a DIFFERENT bundle; re-sign so the sig
	// is valid and the checksum is the sole failure.
	fc.sums = buildSums(t, map[string][]byte{release.ArtifactCatalogBundle: []byte("a different bundle")})
	fc.sig = signEd25519(t, fc.signingKey, fc.sums)

	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.Nil(t, got)
	requireVerificationError(t, err)
}

func TestVerifyCatalogBundle_TamperedBundleFailsClosed(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	fc.bundle = append(fc.bundle, []byte("tampered")...)

	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.Nil(t, got)
	requireVerificationError(t, err)
}

func TestVerifyCatalogBundle_BadSignatureFailsClosed(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fc.sig = signEd25519(t, wrong, fc.sums)

	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.Nil(t, got)
	requireVerificationError(t, err)
}

func TestVerifyCatalogBundle_WrongIdentityAttestationFailsClosed(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	wrongSAN := "https://github.com/evil/wdm/.github/workflows/release.yml@refs/tags/" + testTag
	fc.entity = attestEntity(t, fc.vs, wrongSAN, testIssuer, release.ArtifactCatalogBundle, fc.bundle)

	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.Nil(t, got)
	requireVerificationError(t, err)
}

func TestVerifyCatalogBundle_MissingAttestationFailsClosed(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	fc.attestation = []byte{} // empty attestation: a verification failure.

	got, err := release.VerifyCatalogBundle(t.Context(), fc.options())
	require.Nil(t, got)
	requireVerificationError(t, err)
}

func TestVerifyCatalogBundle_TransportFailureMapsToNetwork(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	opts := fc.options()
	fc.srv.Close()

	got, err := release.VerifyCatalogBundle(t.Context(), opts)
	require.Nil(t, got)
	requireNetworkError(t, err)
}

func TestVerifyCatalogBundle_OverCapBundleMapsToNetwork(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	opts := fc.options()
	opts.BundleMaxBytes = 1

	got, err := release.VerifyCatalogBundle(t.Context(), opts)
	require.Nil(t, got)
	requireNetworkError(t, err)
}

func TestVerifyCatalogBundle_ContextCanceledBeforeWork(t *testing.T) {
	t.Parallel()

	fc := newFakeCatalog(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := release.VerifyCatalogBundle(ctx, fc.options())
	require.Nil(t, got)
	requireNetworkError(t, err)
}

func TestVerifyCatalogBundle_RejectsCallerMisuse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*release.CatalogVerifyOptions)
	}{
		{"nil client", func(o *release.CatalogVerifyOptions) { o.Client = nil }},
		{"nil metadata", func(o *release.CatalogVerifyOptions) { o.Metadata = nil }},
		{"blank tag", func(o *release.CatalogVerifyOptions) { o.Metadata.Tag = "  " }},
		{"nil signing key", func(o *release.CatalogVerifyOptions) { o.SigningPublicKey = nil }},
		{"nil trusted root", func(o *release.CatalogVerifyOptions) { o.TrustedRoot = nil }},
		{"incomplete policy", func(o *release.CatalogVerifyOptions) { o.Policy = release.TrustPolicy{} }},
		{"missing bundle asset", func(o *release.CatalogVerifyOptions) {
			o.Metadata = stripAsset(o.Metadata, release.ArtifactCatalogBundle)
		}},
		{"missing checksums asset", func(o *release.CatalogVerifyOptions) {
			o.Metadata = stripAsset(o.Metadata, release.ArtifactChecksums)
		}},
		{"missing attestation asset", func(o *release.CatalogVerifyOptions) {
			o.Metadata = stripAsset(o.Metadata, release.ArtifactAttestation)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := newFakeCatalog(t)
			opts := fc.options()
			tt.mutate(&opts)

			got, err := release.VerifyCatalogBundle(t.Context(), opts)
			require.Nil(t, got)
			requireUsageError(t, err)
		})
	}
}

func TestEmbeddedSigningPublicKey_IsProductionEd25519Key(t *testing.T) {
	t.Parallel()

	keyBytes := release.EmbeddedSigningPublicKey()
	require.NotEmpty(t, keyBytes)

	pub, err := release.ParseEd25519PublicKey(keyBytes)
	require.NoError(t, err)
	assert.Len(t, pub, ed25519.PublicKeySize)

	keyBytes[0] ^= 0xff
	assert.NotEqual(t, keyBytes, release.EmbeddedSigningPublicKey())
}

func TestVerifyCatalogBundleProduction_CanceledContextMapsToNetwork(t *testing.T) {
	t.Parallel()

	// A context already canceled before the production assembler does any
	// work returns a transport-class failure (exit 8) WITHOUT touching the
	// network — it never reaches root.FetchTrustedRoot. This exercises the
	// production path's fail-fast arm offline.
	fc := newFakeCatalog(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := release.VerifyCatalogBundleProduction(ctx, fc.client, fc.metadata())
	require.Nil(t, got)
	requireNetworkError(t, err)
}
