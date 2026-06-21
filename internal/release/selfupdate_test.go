package release_test

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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// fakeRelease is a self-contained, offline fake-release fixture: a virtual
// Sigstore, an Ed25519 signing key, the candidate binary bytes, and an
// httptest server that serves the trust-policy-matching asset set
// (SHA256SUMS, SHA256SUMS.sig, the binary, attestation.json). It NEVER
// touches real github.com — every download lands on the httptest server.
type fakeRelease struct {
	vs          *ca.VirtualSigstore
	signingKey  ed25519.PrivateKey
	pubKeyPEM   []byte
	binary      []byte
	sums        []byte
	sig         []byte
	attestation []byte // sentinel bytes served as attestation.json
	entity      verify.SignedEntity
	srv         *httptest.Server
	client      *release.Client
}

// attestationSentinel is the placeholder body served as attestation.json.
// The test loader (loadAttestation) maps these served bytes back to the
// in-memory minted entity, mirroring the attestation.go seam rationale
// (the test harness emits no serializable bundle JSON).
const attestationSentinel = "WDM-TEST-ATTESTATION"

// newFakeRelease builds the default happy-path fixture. Individual tamper
// tests mutate fields and rebuild the affected asset bytes before staging.
func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()

	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	binary := []byte("the verified wdm-linux-amd64 candidate bytes\n")

	fr := &fakeRelease{
		vs:         vs,
		signingKey: key,
		pubKeyPEM:  pubPEM,
		binary:     binary,
	}

	fr.sums = buildSums(t, map[string][]byte{
		release.ArtifactBinary: binary,
	})
	fr.sig = signEd25519(t, key, fr.sums)
	fr.attestation = []byte(attestationSentinel)
	fr.entity = attestEntity(t, vs, expectedSAN(), testIssuer, release.ArtifactBinary, binary)

	fr.startServer(t)
	return fr
}

// loadAttestation is the test attestation-parse seam. It returns the
// fixture's minted entity for the sentinel bytes and delegates empty or
// other bytes to the real production parser so the missing/malformed cases
// are proven through LoadAttestationBundle itself.
func (fr *fakeRelease) loadAttestation(b []byte) (verify.SignedEntity, error) {
	if string(b) == attestationSentinel {
		return fr.entity, nil
	}
	return release.LoadAttestationBundle(b)
}

// startServer (re)builds the httptest server and a client pointed at it.
// Asset download URLs are absolute URLs into this server, mirroring
// GitHub's browser_download_url shape.
func (fr *fakeRelease) startServer(t *testing.T) {
	t.Helper()

	mux := http.NewServeMux()
	serve := func(path string, body func() []byte) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
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

	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)

	c, err := release.NewClient(
		release.DefaultTrustPolicy(),
		release.WithBaseURL(fr.srv.URL),
		release.WithHTTPClient(fr.srv.Client()),
	)
	require.NoError(t, err)
	fr.client = c
}

// metadata returns release metadata whose assets point at this server.
func (fr *fakeRelease) metadata() *release.Metadata {
	url := func(name string) string { return fr.srv.URL + "/dl/" + name }
	return &release.Metadata{
		Tag: testTag,
		Assets: []release.ReleaseAsset{
			{Name: release.ArtifactBinary, DownloadURL: url(release.ArtifactBinary)},
			{Name: release.ArtifactChecksums, DownloadURL: url(release.ArtifactChecksums)},
			{Name: release.ArtifactChecksumSignature, DownloadURL: url(release.ArtifactChecksumSignature)},
			{Name: release.ArtifactAttestation, DownloadURL: url(release.ArtifactAttestation)},
		},
	}
}

// options assembles the default StageOptions against this fixture, using a
// freshly-created 0o700 staging directory.
func (fr *fakeRelease) options(t *testing.T) release.StageOptions {
	t.Helper()
	return release.StageOptions{
		Client:           fr.client,
		Metadata:         fr.metadata(),
		Policy:           release.DefaultTrustPolicy(),
		TrustedRoot:      fr.vs,
		SigningPublicKey: fr.pubKeyPEM,
		StagingDir:       newStagingDir(t),
		LoadAttestation:  fr.loadAttestation,
	}
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

// newStagingDir creates a private 0o700 staging directory. t.TempDir can
// be 0o775 on umask-0002 hosts, so the parent is pinned 0o700 to keep the
// staging dir's own restrictive-mode assertion meaningful.
func newStagingDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o700))
	dir := filepath.Join(parent, "staging")
	require.NoError(t, os.Mkdir(dir, 0o700))
	return dir
}

func requireNetworkError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)
}

func requireUsageError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"want ErrCodeUsageValidation (exit 2), got %v", err)
}

// assertNoStagedBinary fails if a staged binary exists at the conventional
// path under dir — the fail-closed "no usable staged binary" guarantee.
func assertNoStagedBinary(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, release.ArtifactBinary))
	assert.True(t, os.IsNotExist(err),
		"expected no staged binary, stat err = %v", err)
	// No leftover temp file either.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp", "leftover temp file in staging dir: %s", e.Name())
	}
}

// --- happy path ---

func TestStageCandidate_HappyPath(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, staged)

	assert.Equal(t, filepath.Join(opts.StagingDir, release.ArtifactBinary), staged.BinaryPath)
	assert.Equal(t, testTag, staged.Tag)
	assert.Equal(t, release.HexDigest(fr.binary), staged.BinaryDigest)
	assert.Equal(t, fr.metadata().Tag, staged.Tag)
	assert.Equal(t, release.DefaultTrustPolicy().TagCertificateIdentityPrefix+testTag, staged.VerifiedSAN)

	// The staged binary is the exact verified bytes, owner-only mode.
	gotBytes, err := os.ReadFile(staged.BinaryPath)
	require.NoError(t, err)
	assert.Equal(t, fr.binary, gotBytes)

	info, err := os.Stat(staged.BinaryPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// --- verification failure modes (each: exit 3, no usable staged binary) ---

func TestStageCandidate_BadChecksumFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	// SHA256SUMS records a digest for a DIFFERENT binary than the one served.
	fr.sums = buildSums(t, map[string][]byte{
		release.ArtifactBinary: []byte("a different binary than the one served"),
	})
	fr.sig = signEd25519(t, fr.signingKey, fr.sums) // re-sign so the sig is valid; the checksum is the failure.
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireVerificationError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_TamperedBinaryFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	// The SHA256SUMS/sig/attestation cover the original binary; the server
	// then serves a tampered binary, so the checksum (and attestation
	// digest) cannot match.
	fr.binary = append(fr.binary, []byte("tampered tail")...)
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireVerificationError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_BadSignatureFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	// A valid Ed25519 signature, but made by a DIFFERENT key than the one in
	// SigningPublicKey: the detached-signature check must reject it.
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fr.sig = signEd25519(t, wrongKey, fr.sums)
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireVerificationError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_AbsentSignatureAssetFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// Drop the signature asset from the metadata: a missing required asset
	// is a usage error (the release is malformed for self-update).
	opts.Metadata = stripAsset(fr.metadata(), release.ArtifactChecksumSignature)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_WrongIdentityAttestationFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	// Re-mint the attestation under a SAN for a neighboring repo — valid
	// signature, wrong certificate identity. The served sentinel still maps
	// to fr.entity through the loader, so the verifier rejects it on identity.
	wrongSAN := "https://github.com/evil/wdm/.github/workflows/release.yml@refs/tags/" + testTag
	fr.entity = attestEntity(t, fr.vs, wrongSAN, testIssuer, release.ArtifactBinary, fr.binary)
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireVerificationError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_MissingAttestationFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	// An empty attestation body — missing attestation is a verification
	// failure, not a "skip it" path.
	fr.attestation = []byte{}
	opts := fr.options(t)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireVerificationError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_AbsentAttestationAssetFailsClosed(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	opts.Metadata = stripAsset(fr.metadata(), release.ArtifactAttestation)

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

// --- transport failure (exit 8, no usable staged binary) ---

func TestStageCandidate_TransportFailureMapsToNetwork(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// Close the server so the first download fails at the transport layer.
	fr.srv.Close()

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireNetworkError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_MissingChecksumAssetOnServerMapsToNetwork(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// The metadata advertises a SHA256SUMS URL, but the server returns 404
	// for it: an HTTP non-2xx is a transport-class failure (exit 8).
	fr.sums = nil

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireNetworkError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_OverCapBinaryMapsToNetwork(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// Cap the binary download below its real size: the client refuses the
	// over-cap body as a network failure rather than truncating.
	opts.BinaryMaxBytes = 1

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireNetworkError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

// --- context cancellation ---

func TestStageCandidate_ContextCanceledBeforeWork(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	staged, err := release.StageCandidate(ctx, opts)
	require.Nil(t, staged)
	requireNetworkError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_ContextCanceledDuringDownloadMapsToNetwork(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel after options are built but the download will observe it: a
	// canceled ctx surfaces through the client's transport as a network
	// failure (exit 8), never a verification failure.
	cancel()

	staged, err := release.StageCandidate(ctx, opts)
	require.Nil(t, staged)
	requireNetworkError(t, err)
}

// --- caller-misuse guards (exit 2) ---

func TestStageCandidate_RejectsCallerMisuse(t *testing.T) {
	t.Parallel()

	base := func(t *testing.T) (*fakeRelease, release.StageOptions) {
		fr := newFakeRelease(t)
		return fr, fr.options(t)
	}

	tests := []struct {
		name   string
		mutate func(*release.StageOptions)
	}{
		{"nil client", func(o *release.StageOptions) { o.Client = nil }},
		{"nil metadata", func(o *release.StageOptions) { o.Metadata = nil }},
		{"blank tag", func(o *release.StageOptions) { o.Metadata.Tag = "  " }},
		{"nil signing key", func(o *release.StageOptions) { o.SigningPublicKey = nil }},
		{"nil trusted root", func(o *release.StageOptions) { o.TrustedRoot = nil }},
		{"incomplete policy", func(o *release.StageOptions) { o.Policy = release.TrustPolicy{} }},
		{"blank staging dir", func(o *release.StageOptions) { o.StagingDir = "   " }},
		{"missing binary asset", func(o *release.StageOptions) {
			o.Metadata = stripAsset(o.Metadata, release.ArtifactBinary)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, opts := base(t)
			tt.mutate(&opts)

			staged, err := release.StageCandidate(t.Context(), opts)
			require.Nil(t, staged)
			requireUsageError(t, err)
			if strings.TrimSpace(opts.StagingDir) != "" {
				assertNoStagedBinary(t, opts.StagingDir)
			}
		})
	}
}

func TestStageCandidate_RejectsNonexistentStagingDir(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	opts.StagingDir = filepath.Join(t.TempDir(), "does-not-exist")

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
}

func TestStageCandidate_RejectsGroupWritableStagingDir(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// Make the staging dir group-writable: a verified-but-staged binary
	// must not be swappable by another local user before promotion.
	require.NoError(t, os.Chmod(opts.StagingDir, 0o770))

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
}

func TestStageCandidate_RejectsOwnerNonWritableStagingDir(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses the owner-write permission check")
	}

	fr := newFakeRelease(t)
	opts := fr.options(t)
	// Make the staging dir owner-non-writable (0o500): writeStagedBinary
	// could not create the staged binary, so an unusable staging dir is
	// caller misuse (exit 2), not a generic write fault (exit 1).
	require.NoError(t, os.Chmod(opts.StagingDir, 0o500))
	// Restore owner-write so t.TempDir teardown can remove the dir.
	t.Cleanup(func() { _ = os.Chmod(opts.StagingDir, 0o700) })

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
	assertNoStagedBinary(t, opts.StagingDir)
}

func TestStageCandidate_RejectsFileAsStagingDir(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	opts.StagingDir = file

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	requireUsageError(t, err)
}

// --- staging-write failure (exit 1, no usable staged binary) ---

func TestStageCandidate_StagingWriteFailureMapsToGeneric(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)
	opts := fr.options(t)

	// Pre-create the staging temp file so the O_EXCL atomic write refuses
	// to reuse it: a local I/O fault during staging is a generic error
	// (exit 1) — distinct from trust (exit 3) and transport (exit 8) — and
	// leaves no usable staged binary. This also reaches the verify-passed
	// path, proving the generic class triggers only AFTER verification.
	collision := filepath.Join(opts.StagingDir, release.ArtifactBinary+".tmp")
	require.NoError(t, os.WriteFile(collision, []byte("squatting temp"), 0o600))

	staged, err := release.StageCandidate(t.Context(), opts)
	require.Nil(t, staged)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"want ErrCodeGeneric (exit 1), got %v", err)

	// The final staged binary was never produced.
	_, statErr := os.Stat(filepath.Join(opts.StagingDir, release.ArtifactBinary))
	assert.True(t, os.IsNotExist(statErr), "expected no staged binary, stat err = %v", statErr)
}

// --- the live binary is never touched ---

func TestStageCandidate_NeverTouchesAPreexistingLiveBinary(t *testing.T) {
	t.Parallel()

	fr := newFakeRelease(t)

	// A separate "install" directory holding the current live binary. The
	// staging primitive must not read, rename, or overwrite it — it only
	// writes into the staging dir.
	installDir := newStagingDir(t)
	livePath := filepath.Join(installDir, release.ArtifactBinary)
	liveBytes := []byte("the CURRENT installed binary - must be untouched")
	require.NoError(t, os.WriteFile(livePath, liveBytes, 0o755))

	opts := fr.options(t) // staging dir is a different directory
	staged, err := release.StageCandidate(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, staged)

	// The live binary is byte-identical and the staged path is elsewhere.
	gotLive, err := os.ReadFile(livePath)
	require.NoError(t, err)
	assert.Equal(t, liveBytes, gotLive)
	assert.NotEqual(t, livePath, staged.BinaryPath)
}

// stripAsset returns a copy of meta with the named asset removed.
func stripAsset(meta *release.Metadata, name string) *release.Metadata {
	out := &release.Metadata{Tag: meta.Tag}
	for _, a := range meta.Assets {
		if a.Name != name {
			out.Assets = append(out.Assets, a)
		}
	}
	return out
}
