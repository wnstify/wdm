package core_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/release"
)

// This file is the offline fake-catalog-release fixture for the
// CheckCatalogUpdate / ApplyCatalogUpdate tests. It mirrors
// internal/release's fakeRelease (selfupdate_test.go) but for the catalog
// bundle asset (ArtifactCatalogBundle) and drives the engine's
// WithReleaseDeps seam, so no test touches real github.com or the live
// Sigstore root: every download lands on an httptest server and every
// verification chains to a virtual Sigstore.

const (
	fakeReleaseTag         = "v1.2.3"
	attestationSentinel    = "WDM-CATALOG-TEST-ATTESTATION"
	candidateGeneratedAt   = "2026-06-01T00:00:00Z"
	candidateTemplateVer   = "2026-06-01"
	localGeneratedAtNewer  = "2026-09-01T00:00:00Z"
	localGeneratedAtOlder  = "2026-01-01T00:00:00Z"
	localGeneratedAtEqual  = candidateGeneratedAt
	candidateExtraAppID    = "freshrss"
	candidateBaseAppID     = "uptime-kuma"
	candidateBaseAppNewVer = candidateTemplateVer
)

// fakeCatalogRelease is a self-contained offline release fixture: a virtual
// Sigstore, an Ed25519 signing key, the catalog bundle bytes, and an
// httptest server serving the trust-policy-matching asset set (SHA256SUMS,
// SHA256SUMS.sig, the catalog bundle, attestation.json).
type fakeCatalogRelease struct {
	vs          *ca.VirtualSigstore
	signingKey  ed25519.PrivateKey
	pubKeyPEM   []byte
	bundle      []byte
	sums        []byte
	sig         []byte
	attestation []byte
	entity      verify.SignedEntity
	srv         *httptest.Server
	httpHits    atomic.Int64
}

// newFakeCatalogRelease builds the default happy-path fixture: a verified
// catalog bundle whose manifest's generated_at is candidateGeneratedAt.
func newFakeCatalogRelease(t *testing.T) *fakeCatalogRelease {
	t.Helper()
	return newFakeCatalogReleaseWith(t, candidateCatalogManifest(candidateGeneratedAt, candidateTemplateVer, true))
}

// newFakeCatalogReleaseWith builds a fixture whose catalog bundle carries
// the supplied manifest body, so version/rollback tests vary generated_at.
func newFakeCatalogReleaseWith(t *testing.T, manifest string) *fakeCatalogRelease {
	t.Helper()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	bundle := makeFakeCatalogBundle(t, manifest)

	fr := &fakeCatalogRelease{
		vs:         vs,
		signingKey: key,
		pubKeyPEM:  pubPEM,
		bundle:     bundle,
	}
	fr.sums = buildSums(t, map[string][]byte{release.ArtifactCatalogBundle: bundle})
	fr.sig = signEd25519(t, key, fr.sums)
	fr.attestation = []byte(attestationSentinel)
	fr.entity = attestCatalogEntity(t, vs, release.ArtifactCatalogBundle, bundle)
	fr.startServer(t)
	return fr
}

// loadAttestation maps the served sentinel back to the in-memory minted
// entity and delegates everything else (empty / malformed) to the real
// production parser, so missing/malformed cases are proven through it.
func (fr *fakeCatalogRelease) loadAttestation(b []byte) (verify.SignedEntity, error) {
	if string(b) == attestationSentinel {
		return fr.entity, nil
	}
	return release.LoadAttestationBundle(b)
}

func (fr *fakeCatalogRelease) startServer(t *testing.T) {
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
	serve("/dl/"+release.ArtifactCatalogBundle, func() []byte { return fr.bundle })
	serve("/dl/"+release.ArtifactAttestation, func() []byte { return fr.attestation })
	// The metadata endpoint returns the latest release pointing at /dl/*.
	mux.HandleFunc("/repos/wnstify/wdm/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fr.metadataJSON())
	})

	// Count every request so a test can prove the HTTP doer was NEVER called
	// (e.g. a usage refusal that must short-circuit before any network call).
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fr.httpHits.Add(1)
		mux.ServeHTTP(w, r)
	})

	fr.srv = httptest.NewServer(counting)
	t.Cleanup(fr.srv.Close)
}

// httpRequests reports how many HTTP requests the fixture server has served,
// so a test can assert the release client never reached the network.
func (fr *fakeCatalogRelease) httpRequests() int64 {
	return fr.httpHits.Load()
}

// metadataJSON is the GitHub releases/latest JSON for this fixture, with
// asset browser_download_urls into the httptest server.
func (fr *fakeCatalogRelease) metadataJSON() []byte {
	url := func(name string) string { return fr.srv.URL + "/dl/" + name }
	return fmt.Appendf(nil,
		`{"tag_name":%q,"assets":[`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d},`+
			`{"name":%q,"browser_download_url":%q,"size":%d}]}`,
		fakeReleaseTag,
		release.ArtifactCatalogBundle, url(release.ArtifactCatalogBundle), len(fr.bundle),
		release.ArtifactChecksums, url(release.ArtifactChecksums), len(fr.sums),
		release.ArtifactChecksumSignature, url(release.ArtifactChecksumSignature), len(fr.sig),
		release.ArtifactAttestation, url(release.ArtifactAttestation), len(fr.attestation),
	)
}

// newReleaseClient returns a client pointed at this fixture's server.
func (fr *fakeCatalogRelease) newReleaseClient() (*release.Client, error) {
	return release.NewClient(
		release.DefaultTrustPolicy(),
		release.WithBaseURL(fr.srv.URL),
		release.WithHTTPClient(fr.srv.Client()),
	)
}

// verifyCatalogBundle drives the REAL release.VerifyCatalogBundle through
// the fixture's virtual Sigstore + generated key, so the production verify
// chain is exercised end-to-end with no real network/root.
func (fr *fakeCatalogRelease) verifyCatalogBundle(
	ctx context.Context, client *release.Client, meta *release.Metadata,
) (*release.VerifiedCatalogBundle, error) {
	return release.VerifyCatalogBundle(ctx, release.CatalogVerifyOptions{
		Client:           client,
		Metadata:         meta,
		Policy:           release.DefaultTrustPolicy(),
		TrustedRoot:      fr.vs,
		SigningPublicKey: fr.pubKeyPEM,
		LoadAttestation:  fr.loadAttestation,
	})
}

// option returns the WithReleaseDeps option wiring both seam halves to this
// fixture.
func (fr *fakeCatalogRelease) option() core.Option {
	return core.WithReleaseDeps(fr.newReleaseClient, fr.verifyCatalogBundle)
}

// --- fixture builders ---

func buildSums(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b strings.Builder
	for name, data := range files {
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

// signEd25519 signs the exact payload bytes with key, returning the raw
// 64-byte Ed25519 signature served as SHA256SUMS.sig. There is no prehash.
func signEd25519(t *testing.T, key ed25519.PrivateKey, payload []byte) []byte {
	t.Helper()
	return ed25519.Sign(key, payload)
}

// attestCatalogEntity mints an offline attestation entity binding name to
// the SHA-256 of artifact, under the production trust-policy SAN for the
// fixture tag.
func attestCatalogEntity(t *testing.T, vs *ca.VirtualSigstore, name string, artifact []byte) verify.SignedEntity {
	t.Helper()
	san := release.DefaultTrustPolicy().TagCertificateIdentityPrefix + fakeReleaseTag
	entity, err := vs.Attest(san, release.OIDCIssuer, catalogStatementFor(name, artifact))
	require.NoError(t, err)
	return entity
}

func catalogStatementFor(name string, artifact []byte) []byte {
	sum := sha256.Sum256(artifact)
	return fmt.Appendf(nil,
		`{"_type":"https://in-toto.io/Statement/v0.1",`+
			`"predicateType":"https://slsa.dev/provenance/v0.2",`+
			`"subject":[{"name":%q,"digest":{"sha256":%q}}],`+
			`"predicate":{}}`,
		name, hex.EncodeToString(sum[:]),
	)
}

// makeFakeCatalogBundle builds a gzip-tar with the release-contract root
// layout (stable/ + templates/) carrying the supplied manifest, so the
// bundle extracts to the engine-readable shape and ReadBundleManifest can
// read its generated_at.
func makeFakeCatalogBundle(t *testing.T, manifest string) []byte {
	t.Helper()
	type entry struct {
		name string
		body string
		dir  bool
	}
	entries := []entry{
		{name: "stable/", dir: true},
		{name: "stable/catalog.yaml", body: manifest},
		{name: "templates/", dir: true},
		{name: "templates/uptime-kuma/", dir: true},
		{name: "templates/uptime-kuma/docker-compose.yml.tmpl", body: "services:\n  app:\n    image: louislam/uptime-kuma:1.23.0\n"},
		{name: "templates/uptime-kuma/.env.tmpl", body: "TZ=UTC\n"},
		{name: "templates/freshrss/", dir: true},
		{name: "templates/freshrss/docker-compose.yml.tmpl", body: "services:\n  app:\n    image: freshrss/freshrss:1.24.0\n"},
		{name: "templates/freshrss/.env.tmpl", body: "TZ=UTC\n"},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// candidateCatalogManifest renders a schema-valid stable manifest with the
// supplied generated_at and uptime-kuma template_version; when withExtra is
// true it also carries the freshrss app, so app-set-diff tests can assert
// an "added"/"removed" change.
func candidateCatalogManifest(generatedAt, templateVersion string, withExtra bool) string {
	apps := uptimeKumaApp(templateVersion)
	if withExtra {
		apps += freshrssApp()
	}
	return fmt.Sprintf(`schema_version: 1
channel: stable
generated_at: %q
apps:
%s`, generatedAt, apps)
}

func uptimeKumaApp(templateVersion string) string {
	return fmt.Sprintf(`  - app_id: uptime-kuma
    name: Uptime Kuma
    summary: Status and uptime monitoring
    description: Self-hosted monitoring tool
    template_name: uptime-kuma
    template_version: %q
    compose_template: templates/uptime-kuma/docker-compose.yml.tmpl
    env_template: templates/uptime-kuma/.env.tmpl
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 3001
        host: 3008
        protocol: tcp
    image_pins:
      - service: app
        image: louislam/uptime-kuma
        tag: "1.23.0"
    local_target_url_template: "http://127.0.0.1:3008/"
    pangolin_guidance:
      target_url: "http://127.0.0.1:3008"
      recommended_subdomain: status
      notes:
        - Point DNS to your reverse proxy.
    first_run_notes:
      - Open the local URL and create the admin account.
    risk_classification: [database]
`, templateVersion)
}

func freshrssApp() string {
	return `  - app_id: freshrss
    name: FreshRSS
    summary: RSS aggregator
    description: Self-hosted feed reader
    template_name: freshrss
    template_version: "2026-06-01"
    compose_template: templates/freshrss/docker-compose.yml.tmpl
    env_template: templates/freshrss/.env.tmpl
    placeholders:
      - name: ADMIN_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 80
        host: 8088
        protocol: tcp
    image_pins:
      - service: app
        image: freshrss/freshrss
        tag: "1.24.0"
    local_target_url_template: "http://127.0.0.1:8088/"
    pangolin_guidance:
      target_url: "http://127.0.0.1:8088"
      recommended_subdomain: rss
      notes:
        - Point DNS to your reverse proxy.
    first_run_notes:
      - Open the local URL and create the admin account.
    risk_classification: [safe]
`
}
