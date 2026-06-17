package core_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/release"
)

// This file is the offline fake-binary-release fixture for the
// CheckSelfUpdate / ApplySelfUpdate tests. It mirrors the
// catalog fixture (catalog_update_fixture_test.go) but for the binary asset
// (release.ArtifactBinary) and drives BOTH the engine's WithReleaseDeps
// (metadata client) and WithSelfUpdateDeps (stageCandidate + smoke-exec)
// seams, so no test touches real github.com, the live Sigstore root, the real
// running executable, or the real test runner: every download lands on an
// httptest server, every verification chains to a virtual Sigstore, and every
// replacement/smoke step operates on an injected target path inside a
// t.TempDir.

const (
	selfUpdateReleaseTag      = "v1.5.0"
	selfUpdateAttestSentinel  = "WDM-SELFUPDATE-TEST-ATTESTATION"
	selfUpdateCandidateBinary = "the verified wdm-linux-amd64 candidate bytes\n"
)

// fakeBinaryRelease is a self-contained offline release fixture: a virtual
// Sigstore, an Ed25519 signing key, the candidate binary bytes, and an
// httptest server serving the trust-policy-matching asset set (SHA256SUMS,
// SHA256SUMS.sig, the binary, attestation.json).
type fakeBinaryRelease struct {
	vs          *ca.VirtualSigstore
	signingKey  ed25519.PrivateKey
	pubKeyPEM   []byte
	binary      []byte
	sums        []byte
	sig         []byte
	attestation []byte
	entity      verify.SignedEntity
	srv         *httptest.Server
	httpHits    atomic.Int64
	tag         string
}

// newFakeBinaryRelease builds the default happy-path fixture: a verified
// candidate binary tagged selfUpdateReleaseTag.
func newFakeBinaryRelease(t *testing.T) *fakeBinaryRelease {
	t.Helper()
	return newFakeBinaryReleaseWith(t, selfUpdateReleaseTag, []byte(selfUpdateCandidateBinary))
}

// newFakeBinaryReleaseWith builds a fixture for the supplied tag and binary
// bytes, so version/transport/verify tests vary the inputs.
func newFakeBinaryReleaseWith(t *testing.T, tag string, binary []byte) *fakeBinaryRelease {
	t.Helper()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	fr := &fakeBinaryRelease{
		vs:         vs,
		signingKey: key,
		pubKeyPEM:  pubPEM,
		binary:     binary,
		tag:        tag,
	}
	fr.sums = buildSums(t, map[string][]byte{release.ArtifactBinary: binary})
	fr.sig = signEd25519(t, key, fr.sums)
	fr.attestation = []byte(selfUpdateAttestSentinel)
	fr.entity = attestBinaryEntity(t, vs, tag, release.ArtifactBinary, binary)
	fr.startServer(t)
	return fr
}

// loadAttestation maps the served sentinel back to the in-memory minted
// entity and delegates everything else (empty / malformed) to the real
// production parser, so missing/malformed cases are proven through it.
func (fr *fakeBinaryRelease) loadAttestation(b []byte) (verify.SignedEntity, error) {
	if string(b) == selfUpdateAttestSentinel {
		return fr.entity, nil
	}
	return release.LoadAttestationBundle(b)
}

func (fr *fakeBinaryRelease) startServer(t *testing.T) {
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
	serve("/dl/"+release.ArtifactChecksums, func() []byte { return fr.sums })
	serve("/dl/"+release.ArtifactChecksumSignature, func() []byte { return fr.sig })
	serve("/dl/"+release.ArtifactBinary, func() []byte { return fr.binary })
	serve("/dl/"+release.ArtifactAttestation, func() []byte { return fr.attestation })
	mux.HandleFunc("/repos/wnstify/wdm/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fr.metadataJSON())
	})

	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fr.httpHits.Add(1)
		mux.ServeHTTP(w, r)
	})

	fr.srv = httptest.NewServer(counting)
	t.Cleanup(fr.srv.Close)
}

// httpRequests reports how many HTTP requests the fixture server has served,
// so a test can assert the release client never reached the network.
func (fr *fakeBinaryRelease) httpRequests() int64 {
	return fr.httpHits.Load()
}

// metadataJSON is the GitHub releases/latest JSON for this fixture, with
// asset browser_download_urls into the httptest server.
func (fr *fakeBinaryRelease) metadataJSON() []byte {
	url := func(name string) string { return fr.srv.URL + "/dl/" + name }
	return fmt.Appendf(nil,
		`{"tag_name":%q,"assets":[`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d}]}`,
		fr.tag,
		release.ArtifactBinary, url(release.ArtifactBinary), len(fr.binary),
		release.ArtifactChecksums, url(release.ArtifactChecksums), len(fr.sums),
		release.ArtifactChecksumSignature, url(release.ArtifactChecksumSignature), len(fr.sig),
		release.ArtifactAttestation, url(release.ArtifactAttestation), len(fr.attestation),
	)
}

// newReleaseClient returns a client pointed at this fixture's server.
func (fr *fakeBinaryRelease) newReleaseClient() (*release.Client, error) {
	return release.NewClient(
		release.DefaultTrustPolicy(),
		release.WithBaseURL(fr.srv.URL),
		release.WithHTTPClient(fr.srv.Client()),
	)
}

// stageCandidate drives the REAL release.StageCandidate through the fixture's
// virtual Sigstore + generated key into the supplied staging dir, so the
// production download/verify/stage chain is exercised end-to-end with no real
// network/root. It is the WithSelfUpdateDeps stage seam.
func (fr *fakeBinaryRelease) stageCandidate(
	ctx context.Context, client *release.Client, meta *release.Metadata, stagingDir string,
) (*release.StagedCandidate, error) {
	return release.StageCandidate(ctx, release.StageOptions{
		Client:           client,
		Metadata:         meta,
		Policy:           release.DefaultTrustPolicy(),
		TrustedRoot:      fr.vs,
		SigningPublicKey: fr.pubKeyPEM,
		StagingDir:       stagingDir,
		LoadAttestation:  fr.loadAttestation,
	})
}

// attestBinaryEntity mints an offline attestation entity binding name to the
// SHA-256 of artifact, under the production trust-policy SAN for the supplied
// tag.
func attestBinaryEntity(t *testing.T, vs *ca.VirtualSigstore, tag, name string, artifact []byte) verify.SignedEntity {
	t.Helper()
	san := release.DefaultTrustPolicy().TagCertificateIdentityPrefix + tag
	entity, err := vs.Attest(san, release.OIDCIssuer, binaryStatementFor(name, artifact))
	require.NoError(t, err)
	return entity
}

func binaryStatementFor(name string, artifact []byte) []byte {
	sum := sha256.Sum256(artifact)
	return fmt.Appendf(nil,
		`{"_type":"https://in-toto.io/Statement/v0.1",`+
			`"predicateType":"https://slsa.dev/provenance/v0.2",`+
			`"subject":[{"name":%q,"digest":{"sha256":%q}}],`+
			`"predicate":{}}`,
		name, hex.EncodeToString(sum[:]),
	)
}

// selfUpdateOption returns the option pair wiring the metadata client (via
// WithReleaseDeps, the verify half left nil → defaulted but never called on a
// self-update path) and the self-update stage seam to this fixture. Callers
// add their own WithSelfUpdateDeps executablePath/resolveSymlinks/runSmoke
// fields, so this only supplies the stageCandidate half via a dedicated
// option built by the test.
func (fr *fakeBinaryRelease) clientOption() core.Option {
	return core.WithReleaseDeps(fr.newReleaseClient, nil)
}
