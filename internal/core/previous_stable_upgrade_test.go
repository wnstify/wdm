package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// This file is the previous-stable upgrade gate.
// convergence; PRD §22). It proves the named scenario the convergence batch
// requires before release: a deployed system whose ACTIVE local catalog is a
// previous stable can upgrade to a newer verified catalog without ever touching
// deployed app stacks, and only when the upgrade is explicitly approved.
// Synthetic-fixture provenance: no
// public v0 stable of wdm exists yet, so there is NO real previous-stable
// release artifact to upgrade from. The "previous stable" here is therefore a
// SYNTHETIC, in-code pre-release fixture minted at test time by the existing
// offline catalog fixture generators (candidateCatalogManifest, seedLocalCatalog)
// at previousStableGeneratedAt. It is NEVER a real stable and MUST NOT be
// committed as a release artifact or testdata blob — its provenance lives only
// in this file. The newer "verified catalog" is the standard offline fake
// release (newFakeCatalogRelease) at candidateGeneratedAt, which is strictly
// newer than previousStableGeneratedAt so the upgrade is a forward move, not a
// refused rollback.
// Scope discipline (no duplication): the byte-unchanged-deployed-apps invariant
// (TestApplyCatalogUpdate_DoesNotTouchDeployedApps) and the confirmer gating
// (TestApplyCatalogUpdate_NilConfirmerRefuses / _DeclinedConfirmerWritesNothing)
// are each proven in isolation on a clean slate. This file is additive because
// it ties them to ONE recognizable previous-stable→newer upgrade: an active,
// clearly-labeled synthetic previous-stable catalog is seeded first, a deployed
// stack sits outside the catalogs dir, and the SAME upgrade is exercised with an
// approving confirmer (advances + leaves the stack untouched) and with a
// declined/absent confirmer (writes nothing AND the active catalog stays pinned
// at the previous stable). Deterministic and offline (httptest + virtual
// Sigstore, no Docker, fixed generated_at constants); runs under `make test`.

// previousStableGeneratedAt is the generated_at of the SYNTHETIC previous-stable
// catalog this gate upgrades FROM. It is deliberately equal to the existing
// localGeneratedAtOlder constant (2026-01-01T00:00:00Z) so it is strictly older
// than candidateGeneratedAt (the verified upgrade target) — reused, not
// duplicated. It is a private pre-release fixture value, never a real shipped
// stable version.
const previousStableGeneratedAt = localGeneratedAtOlder

// seedManagedStack writes a managed app stack (sentinel.wdm.lock +
// docker-compose.yml) under its own directory OUTSIDE the catalogs dir and
// returns the lock and compose bytes so a test can assert byte-identity after a
// catalog upgrade. It mirrors the deployed-stack seam in
// TestApplyCatalogUpdate_DoesNotTouchDeployedApps.
func seedManagedStack(t *testing.T) (stackDir string, lock, compose []byte) {
	t.Helper()
	stackDir = t.TempDir()
	lock = []byte(`{"schema_version":1,"app_id":"uptime-kuma"}`)
	compose = []byte("services: {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, ".wdm.lock"), lock, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), compose, 0o644))
	return stackDir, lock, compose
}

// TestPreviousStableUpgradeGate_DoesNotModifyDeployedAppsWithoutApproval is the
// Headline upgrade gate. With the synthetic previous-stable catalog active and a
// deployed stack outside the catalogs dir, it proves: (a) an APPROVED upgrade to
// the newer verified catalog advances the active manifest and writes the
// candidate snapshot while leaving the deployed stack byte-identical; and (b)
// the SAME upgrade with a declined or nil confirmer writes nothing and leaves
// the active catalog pinned at the previous stable — the upgrade is gated on
// explicit approval.
func TestPreviousStableUpgradeGate_DoesNotModifyDeployedAppsWithoutApproval(t *testing.T) {
	t.Parallel()

	// previousStableManifest is the active local catalog the gate upgrades from:
	// the synthetic previous-stable, carrying only uptime-kuma at an older
	// template version so the forward upgrade reports a real app-set change.
	previousStableManifest := candidateCatalogManifest(previousStableGeneratedAt, "2026-01-01", false)

	// Guard the fixture invariant: the previous stable MUST be strictly older
	// than the candidate so this gate exercises a forward upgrade, not a refused
	// rollback. String comparison is valid for these RFC-3339 UTC constants.
	require.Less(t, previousStableGeneratedAt, candidateGeneratedAt,
		"fixture must be strictly older than the candidate so this is a forward upgrade")

	t.Run("approved upgrade advances and leaves deployed apps untouched", func(t *testing.T) {
		t.Parallel()

		fr := newFakeCatalogRelease(t)
		eng, dataDir := newCatalogUpdateEngine(t, fr)

		// Seed the SYNTHETIC previous-stable as the active local catalog.
		seedLocalCatalog(t, dataDir, previousStableManifest)
		// A deployed stack lives outside the catalogs dir; the upgrade must not
		// read or write any of it.
		stackDir, wantLock, wantCompose := seedManagedStack(t)

		confirmer := &fakeConfirmer{}

		result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
		require.NoError(t, err)
		require.NotNil(t, result)

		// The upgrade is a forward move FROM the previous stable TO the candidate.
		assert.Equal(t, previousStableGeneratedAt, result.PreviousVersion,
			"the upgrade must report the synthetic previous-stable as the previous version")
		assert.Equal(t, candidateGeneratedAt, result.AppliedVersion)
		assert.NotEmpty(t, result.Changes, "a previous-stable→newer upgrade reports app-set changes")

		// The catalog upgrade was authorized exactly once, as a catalog update.
		require.Len(t, confirmer.calls, 1)
		assert.Equal(t, types.ConfirmationKindCatalogUpdate, confirmer.calls[0].Kind)

		// The active catalog manifest advanced to the candidate, and the
		// immutable candidate snapshot was written.
		active, readErr := os.ReadFile(activeManifestPath(dataDir))
		require.NoError(t, readErr)
		assert.Contains(t, string(active), "generated_at: \""+candidateGeneratedAt+"\"")
		_, snapErr := os.Stat(filepath.Join(snapshotDirPath(dataDir, candidateGeneratedAt), "stable", "catalog.yaml"))
		require.NoError(t, snapErr, "the candidate snapshot must be written on an approved upgrade")

		// The deployed stack is byte-identical: the catalog upgrade never touched it.
		gotLock, _ := os.ReadFile(filepath.Join(stackDir, ".wdm.lock"))
		gotCompose, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
		assert.Equal(t, wantLock, gotLock, ".wdm.lock must be byte-identical after the catalog upgrade")
		assert.Equal(t, wantCompose, gotCompose, "docker-compose.yml must be byte-identical after the catalog upgrade")
	})

	gatedCases := []struct {
		name      string
		confirmFn func(context.Context, types.Confirmation) (bool, error)
		wantCode  types.ErrorCode
	}{
		{
			name: "declined confirmer",
			confirmFn: func(context.Context, types.Confirmation) (bool, error) {
				return false, nil
			},
			wantCode: types.ErrCodeUserCanceled,
		},
		{
			name:      "absent confirmer",
			confirmFn: nil, // a nil *fakeConfirmer is passed as the Confirmer
			wantCode:  types.ErrCodeUsageValidation,
		},
	}

	for _, tc := range gatedCases {
		t.Run("unapproved upgrade is refused and pins previous stable: "+tc.name, func(t *testing.T) {
			t.Parallel()

			fr := newFakeCatalogRelease(t)
			eng, dataDir := newCatalogUpdateEngine(t, fr)

			seedLocalCatalog(t, dataDir, previousStableManifest)
			stackDir, wantLock, wantCompose := seedManagedStack(t)

			before, readErr := os.ReadFile(activeManifestPath(dataDir))
			require.NoError(t, readErr)

			var confirmer types.Confirmer
			if tc.confirmFn != nil {
				confirmer = &fakeConfirmer{confirmFn: tc.confirmFn}
			}

			result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
			require.Nil(t, result)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, tc.wantCode),
				"want %v, got %v", tc.wantCode, err)

			// The active catalog stays pinned at the synthetic previous stable:
			// the upgrade wrote nothing without approval.
			after, afterErr := os.ReadFile(activeManifestPath(dataDir))
			require.NoError(t, afterErr)
			assert.Equal(t, before, after,
				"the active catalog must remain the previous stable when the upgrade is not approved")
			assert.Contains(t, string(after), "generated_at: \""+previousStableGeneratedAt+"\"")

			// No candidate snapshot was written.
			_, snapErr := os.Stat(snapshotDirPath(dataDir, candidateGeneratedAt))
			assert.True(t, os.IsNotExist(snapErr),
				"no candidate snapshot must be written when the upgrade is not approved")

			// The deployed stack is byte-identical regardless of the refused upgrade.
			gotLock, _ := os.ReadFile(filepath.Join(stackDir, ".wdm.lock"))
			gotCompose, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml"))
			assert.Equal(t, wantLock, gotLock, ".wdm.lock must be byte-identical after a refused upgrade")
			assert.Equal(t, wantCompose, gotCompose, "docker-compose.yml must be byte-identical after a refused upgrade")
		})
	}
}
