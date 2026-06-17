package core_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// newCatalogUpdateEngine builds an engine wired to the offline fake-release
// fixture under its own data dir, so CheckCatalogUpdate / ApplyCatalogUpdate
// hit the httptest server and the virtual Sigstore, never real GitHub.
// WithCatalog is deliberately NOT used: the apply path writes to
// <dataDir>/catalogs and the read path must agree, so both go on disk.
func newCatalogUpdateEngine(t *testing.T, fr *fakeCatalogRelease, extra ...core.Option) (*core.Engine, string) {
	t.Helper()
	tmp := t.TempDir()
	require.NoError(t, os.Chmod(tmp, 0o700))
	stateDir := filepath.Join(tmp, "state")
	dataDir := filepath.Join(tmp, "data")
	stackBase := filepath.Join(tmp, "stacks")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))

	opts := []core.Option{
		core.WithStateDir(stateDir),
		core.WithDataDir(dataDir),
		core.WithStackBaseDir(stackBase),
		core.WithConfigPath(filepath.Join(tmp, "nonexistent.toml")),
		core.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fr.option(),
	}
	opts = append(opts, extra...)
	eng, err := core.New(opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })
	return eng, dataDir
}

// seedLocalCatalog writes a schema-valid active local catalog manifest at
// <dataDir>/catalogs/stable/catalog.yaml so the rollback / available /
// up-to-date paths have a "current" version to compare against.
func seedLocalCatalog(t *testing.T, dataDir, manifest string) {
	t.Helper()
	dir := filepath.Join(dataDir, "catalogs", "stable")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(manifest), 0o644))
}

// --- CheckCatalogUpdate ---

func TestCheckCatalogUpdate_NotInstalledReportsAvailable(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.NoError(t, err)
	require.NotNil(t, status)

	assert.Equal(t, "stable", status.Channel)
	assert.Empty(t, status.CurrentVersion, "no local catalog yet")
	assert.Equal(t, "2026-06-01T00:00:00Z", status.LatestVersion)
	assert.True(t, status.UpdateAvailable)
	assert.True(t, status.Verified)
	assert.NotEmpty(t, status.Changes, "a first install lists added apps")
	assert.False(t, status.CheckedAt.IsZero())
}

func TestCheckCatalogUpdate_AvailableWhenLocalOlder(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	// Local catalog older than the candidate, with only uptime-kuma at an
	// older template version so the diff reports "updated" + "added".
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtOlder, "2026-01-01", false))

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.NoError(t, err)

	assert.Equal(t, "2026-01-01T00:00:00Z", status.CurrentVersion)
	assert.Equal(t, "2026-06-01T00:00:00Z", status.LatestVersion)
	assert.True(t, status.UpdateAvailable)
	assert.True(t, status.Verified)

	kinds := map[string]string{}
	for _, c := range status.Changes {
		kinds[c.AppID] = c.Kind
	}
	assert.Equal(t, "updated", kinds["uptime-kuma"])
	assert.Equal(t, "added", kinds["freshrss"])
}

func TestCheckCatalogUpdate_UpToDateWhenLocalNewer(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	// Local catalog NEWER than the candidate: not an update.
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtNewer, candidateTemplateVer, true))

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.NoError(t, err)

	assert.Equal(t, "2026-09-01T00:00:00Z", status.CurrentVersion)
	assert.Equal(t, "2026-06-01T00:00:00Z", status.LatestVersion)
	assert.False(t, status.UpdateAvailable)
	assert.Empty(t, status.Changes, "no update -> no change summary")
	assert.True(t, status.Verified)
}

func TestCheckCatalogUpdate_UpToDateWhenLocalEqual(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtEqual, candidateTemplateVer, true))

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.NoError(t, err)
	assert.False(t, status.UpdateAvailable, "equal generated_at is not an update")
}

func TestCheckCatalogUpdate_TransportFailureMapsToExit8(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)
	fr.srv.Close() // metadata fetch now fails at the transport layer.

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.Nil(t, status)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)
}

func TestCheckCatalogUpdate_BadSignatureMapsToExit3(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	// Re-sign SHA256SUMS with a DIFFERENT key: the detached-signature check
	// rejects it as a verification failure, distinct from a transport fault.
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fr.sig = signEd25519(t, wrongKey, fr.sums)

	eng, _ := newCatalogUpdateEngine(t, fr)
	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.Nil(t, status)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", err)
}

func TestCheckCatalogUpdate_TamperedBundleMapsToExit3(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	// Serve a different bundle than SHA256SUMS/attestation cover.
	fr.bundle = append(fr.bundle, []byte("tampered tail")...)

	eng, _ := newCatalogUpdateEngine(t, fr)
	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.Nil(t, status)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", err)
}

func TestCheckCatalogUpdate_ClosedEngine(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)
	require.NoError(t, eng.Close())

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.Nil(t, status)
	require.ErrorIs(t, err, core.ErrClosed)
}

func TestCheckCatalogUpdate_CanceledContext(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	status, err := eng.CheckCatalogUpdate(ctx, types.CatalogUpdateQuery{})
	require.Nil(t, status)
	require.Error(t, err)
}

func TestCheckCatalogUpdate_InvalidChannel(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)

	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{Channel: "../evil"})
	require.Nil(t, status)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))
}

func TestCheckCatalogUpdate_NonStableChannelRefusedBeforeNetwork(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)

	// "beta" is a safe single path segment but not the only v1 channel
	// ("stable"). It must refuse with a usage-validation error (exit 2)
	// BEFORE any release-client construction or network call — not run the
	// full download + verify and surface a misleading exit-3/exit-8 error.
	status, err := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{Channel: "beta"})
	require.Nil(t, status)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a non-stable channel is a usage refusal (exit 2), got %v", err)
	assert.Equal(t, int64(0), fr.httpRequests(),
		"the HTTP doer must never be called for an unsupported channel")
}
