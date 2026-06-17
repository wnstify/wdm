package core_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// errConfirmerBackend is the sentinel a fakeConfirmer returns to prove a
// confirmer backend error propagates (errors.Is-reachable) out of
// ApplyCatalogUpdate.
var errConfirmerBackend = errors.New("confirmer backend failure")

// activeManifestPath is the on-disk active catalog manifest the engine reads.
func activeManifestPath(dataDir string) string {
	return filepath.Join(dataDir, "catalogs", "stable", "catalog.yaml")
}

// snapshotDirPath is the immutable version snapshot directory for version.
func snapshotDirPath(dataDir, version string) string {
	return filepath.Join(dataDir, "catalogs", "stable", ".versions", version)
}

func TestApplyCatalogUpdate_HappyPathWritesVerifiedCatalog(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{}

	var steps []string
	onProgress := func(step string, _ float64, _ string) { steps = append(steps, step) }

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, onProgress, confirmer)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "stable", result.Channel)
	assert.Empty(t, result.PreviousVersion, "no local catalog before apply")
	assert.Equal(t, "2026-06-01T00:00:00Z", result.AppliedVersion)
	assert.Contains(t, result.VerificationDetail, "attestation verified")
	assert.Contains(t, result.VerificationDetail, fakeReleaseTag)
	assert.NotEmpty(t, result.Changes)
	assert.False(t, result.AppliedAt.IsZero())

	// The verified catalog is now active on disk and re-checks up-to-date.
	active, readErr := os.ReadFile(activeManifestPath(dataDir))
	require.NoError(t, readErr)
	assert.Contains(t, string(active), "generated_at: \"2026-06-01T00:00:00Z\"")

	// The immutable snapshot exists under.versions/<version>/.
	_, statErr := os.Stat(filepath.Join(snapshotDirPath(dataDir, "2026-06-01T00:00:00Z"), "stable", "catalog.yaml"))
	require.NoError(t, statErr)

	// Provenance was stored alongside the snapshot.
	for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sig", "attestation.json"} {
		_, provErr := os.Stat(filepath.Join(snapshotDirPath(dataDir, "2026-06-01T00:00:00Z"), name))
		require.NoError(t, provErr, "provenance %s must be stored", name)
	}

	// The active templates are materialized at the catalogs-root sibling.
	_, tmplErr := os.Stat(filepath.Join(dataDir, "catalogs", "templates", "uptime-kuma", "docker-compose.yml.tmpl"))
	require.NoError(t, tmplErr)

	// Exactly one confirmation, of the catalog_update kind.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, types.ConfirmationKindCatalogUpdate, confirmer.calls[0].Kind)

	// Progress carried only the catalog-update step family, in order.
	assert.Equal(t, []string{
		types.StepCatalogUpdatePlanning,
		types.StepCatalogUpdateDownload,
		types.StepCatalogUpdateVerify,
		types.StepCatalogUpdateConfirm,
		types.StepCatalogUpdateApply,
		types.StepCatalogUpdateStatus,
	}, steps)

	// A follow-up check now reports up-to-date.
	status, checkErr := eng.CheckCatalogUpdate(t.Context(), types.CatalogUpdateQuery{})
	require.NoError(t, checkErr)
	assert.Equal(t, "2026-06-01T00:00:00Z", status.CurrentVersion)
	assert.False(t, status.UpdateAvailable)
}

func TestApplyCatalogUpdate_DoesNotTouchDeployedApps(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)

	// A managed stack with a sentinel.wdm.lock + files outside the catalogs
	// dir; the catalog apply must not read or write any of it.
	stackDir := t.TempDir()
	lockBytes := []byte(`{"schema_version":1,"app_id":"uptime-kuma"}`)
	composeBytes := []byte("services: {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, ".wdm.lock"), lockBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), composeBytes, 0o644))

	_, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	gotLock, _ := os.ReadFile(filepath.Join(stackDir, ".wdm.lock"))
	gotCompose, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
	assert.Equal(t, lockBytes, gotLock, ".wdm.lock must be byte-identical")
	assert.Equal(t, composeBytes, gotCompose, "compose must be byte-identical")
}

func TestApplyCatalogUpdate_VerifyBeforeWrite_BadSignatureWritesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fr.sig = signEd25519(t, wrongKey, fr.sums)

	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{}

	result, applyErr := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
	require.Nil(t, result)
	require.Error(t, applyErr)
	assert.True(t, types.IsCode(applyErr, types.ErrCodeVerificationFailed),
		"want ErrCodeVerificationFailed (exit 3), got %v", applyErr)

	// Verify-before-write: nothing was written and the confirmer never ran.
	assertNoCatalogWritten(t, dataDir)
	assert.Empty(t, confirmer.calls, "confirmer must not run before verification passes")
}

func TestApplyCatalogUpdate_VerifyBeforeWrite_TamperedBundleWritesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	fr.bundle = append(fr.bundle, []byte("tampered")...)

	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{}

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed))
	assertNoCatalogWritten(t, dataDir)
	assert.Empty(t, confirmer.calls)
}

func TestApplyCatalogUpdate_TransportFailureMapsToExit8WritesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	fr.srv.Close()

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeNetworkFailure),
		"want ErrCodeNetworkFailure (exit 8), got %v", err)
	assertNoCatalogWritten(t, dataDir)
}

func TestApplyCatalogUpdate_RejectsSignedRollback_Older(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	// Local is NEWER than the verified candidate: a downgrade is refused
	// BEFORE any write.
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtNewer, candidateTemplateVer, true))
	before, _ := os.ReadFile(activeManifestPath(dataDir))
	confirmer := &fakeConfirmer{}

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a stale-catalog refusal is a usage refusal (exit 2), got %v", err)
	assert.Contains(t, err.Error(), "not newer")

	// No write happened and the confirmer never ran (refusal is pre-confirm).
	after, _ := os.ReadFile(activeManifestPath(dataDir))
	assert.Equal(t, before, after, "active catalog must be byte-identical after a refused rollback")
	assert.Empty(t, confirmer.calls)
	// No new snapshot for the candidate version was created.
	_, statErr := os.Stat(snapshotDirPath(dataDir, "2026-06-01T00:00:00Z"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestApplyCatalogUpdate_RejectsSignedRollback_Equal(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	// Local generated_at EQUAL to the candidate: equal is not newer -> refuse.
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtEqual, candidateTemplateVer, true))

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))
}

func TestApplyCatalogUpdate_TargetVersionMismatchRefusesBeforeWrite(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{}

	result, err := eng.ApplyCatalogUpdate(
		t.Context(),
		types.CatalogUpdateRequest{TargetVersion: "2099-01-01T00:00:00Z"},
		nil, confirmer,
	)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a target-version mismatch is a usage refusal (exit 2), got %v", err)
	assertNoCatalogWritten(t, dataDir)
	assert.Empty(t, confirmer.calls, "mismatch is refused before the confirmer runs")
}

func TestApplyCatalogUpdate_TargetVersionMatchApplies(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)

	result, err := eng.ApplyCatalogUpdate(
		t.Context(),
		types.CatalogUpdateRequest{TargetVersion: "2026-06-01T00:00:00Z"},
		nil, &fakeConfirmer{},
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "2026-06-01T00:00:00Z", result.AppliedVersion)

	_, statErr := os.Stat(activeManifestPath(dataDir))
	require.NoError(t, statErr)
}

func TestApplyCatalogUpdate_NilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, nil)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"nil confirmer must refuse with usage-validation, got %v", err)
	assertNoCatalogWritten(t, dataDir)
}

func TestApplyCatalogUpdate_DeclinedConfirmerWritesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, nil
	}}

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUserCanceled),
		"a decline maps to ErrCodeUserCanceled (exit 7), got %v", err)
	assertNoCatalogWritten(t, dataDir)
}

func TestApplyCatalogUpdate_ConfirmerErrorPropagates(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, errConfirmerBackend
	}}

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
	require.Nil(t, result)
	require.Error(t, err)
	assert.ErrorIs(t, err, errConfirmerBackend)
	assertNoCatalogWritten(t, dataDir)
}

func TestApplyCatalogUpdate_NonStableChannelRefusedBeforeNetworkWritesNothing(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	confirmer := &fakeConfirmer{}

	// "beta" is a safe single path segment but not the only v1 channel
	// ("stable"). Apply must refuse with a usage-validation error (exit 2)
	// BEFORE any release-client construction, network call, or write — not
	// run the full download + verify and fail at the store step with a
	// misleading exit-3 verification error.
	result, err := eng.ApplyCatalogUpdate(
		t.Context(),
		types.CatalogUpdateRequest{Channel: "beta"},
		nil, confirmer,
	)
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation),
		"a non-stable channel is a usage refusal (exit 2), got %v", err)
	assert.Equal(t, int64(0), fr.httpRequests(),
		"the HTTP doer must never be called for an unsupported channel")
	assert.Empty(t, confirmer.calls, "the confirmer must not run for an unsupported channel")
	assertNoCatalogWritten(t, dataDir)
}

func TestApplyCatalogUpdate_LocalReadFaultMapsToExit1(t *testing.T) {
	t.Parallel()

	// A read-permission fault on the already-installed local catalog is a
	// pure FS fault — neither network nor trust — so it must surface as a
	// generic error (exit 1), NOT a verification failure (exit 3).
	if os.Geteuid() == 0 {
		t.Skip("read-permission fault is bypassed by root; skipping under euid 0")
	}

	fr := newFakeCatalogRelease(t)
	eng, dataDir := newCatalogUpdateEngine(t, fr)
	seedLocalCatalog(t, dataDir, candidateCatalogManifest(localGeneratedAtOlder, "2026-01-01", false))

	manifest := activeManifestPath(dataDir)
	require.NoError(t, os.Chmod(manifest, 0o000))
	t.Cleanup(func() { _ = os.Chmod(manifest, 0o644) })

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.Nil(t, result)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a local read fault is a generic error (exit 1), got %v", err)
}

func TestApplyCatalogUpdate_ClosedEngine(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)
	require.NoError(t, eng.Close())

	result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.Nil(t, result)
	require.ErrorIs(t, err, core.ErrClosed)
}

func TestApplyCatalogUpdate_SecondApplyIsUpToDateRefusal(t *testing.T) {
	t.Parallel()

	fr := newFakeCatalogRelease(t)
	eng, _ := newCatalogUpdateEngine(t, fr)

	// First apply installs the verified catalog.
	_, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	// Second apply of the SAME release: now a no-op rollback refusal
	// (equal generated_at), so the snapshot-already-exists path is never
	// reached and the engine refuses cleanly with exit 2.
	result, secondErr := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, &fakeConfirmer{})
	require.Nil(t, result)
	require.Error(t, secondErr)
	assert.True(t, types.IsCode(secondErr, types.ErrCodeUsageValidation))
}

// assertNoCatalogWritten asserts neither an active manifest nor any version
// snapshot exists under the catalogs root — the fail-closed
// verify-before-write / refuse-before-write guarantee.
func assertNoCatalogWritten(t *testing.T, dataDir string) {
	t.Helper()
	_, err := os.Stat(activeManifestPath(dataDir))
	assert.True(t, os.IsNotExist(err), "no active catalog must be written, stat err = %v", err)
	versionsDir := filepath.Join(dataDir, "catalogs", "stable", ".versions")
	entries, readErr := os.ReadDir(versionsDir)
	if readErr == nil {
		assert.Empty(t, entries, "no version snapshot must be written")
	}
}
