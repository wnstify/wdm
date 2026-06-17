package core_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// This file is a trust-and-distribution convergence harness for PRD §31/§38.
// It proves, through the PUBLIC engine methods
// (CheckSelfUpdate/ApplySelfUpdate and CheckCatalogUpdate/ApplyCatalogUpdate),
// that the full release-verification + self-update-rollback chain is
// fail-closed end-to-end against the offline fake-release fixtures
// (self_update_fixture_test.go, catalog_update_fixture_test.go). It is
// deterministic and offline: every download lands on an httptest server, every
// signature chains to a virtual Sigstore, no Docker and no real binary are
// touched. It runs under plain `go test` (no build tag) so it is part of
// `make test`.
// Scope discipline (no duplication): the primitive-level negatives are already
// proven at the release layer (internal/release/selfupdate_test.go,
// catalog_verify_test.go via StageCandidate/VerifyCatalogBundle), and several
// engine-level negatives already live in self_update_test.go /
// catalog_update_apply_test.go (bad signature, tampered artifact, transport,
// version mismatch, failed-smoke→rollback). This harness adds ONLY the
// engine-level negatives that no existing test exercises through the public
// method — bad checksum, wrong-identity attestation, missing attestation, and
// malformed attestation — plus a single consolidated rollback-chain assertion,
// presented as one cohesive convergence suite. Cases already covered
// elsewhere are intentionally not re-added.

// --- self-update convergence: trust negatives fail closed via ApplySelfUpdate ---

// TestSelfUpdateConvergence_TrustNegativesFailClosed drives ApplySelfUpdate
// against the offline fake-binary-release fixture with one trust fault injected
// per case and proves the full check+apply path fails closed: every fault maps
// to ErrCodeVerificationFailed (exit 3), the live binary is NOT replaced, no
// wdm.previous is written, and neither the confirmer nor the smoke seam ever
// runs (verification precedes both). The bad-checksum, wrong-identity,
// missing-attestation, and malformed-attestation faults are proven here at the
// engine level for the first time; bad-signature and tampered-binary are
// already covered by
// TestApplySelfUpdate_BadSignatureMapsToExit3ReplacesNothing and
// TestCheckSelfUpdate_TamperedBinaryMapsToExit3 and are not re-added.
func TestSelfUpdateConvergence_TrustNegativesFailClosed(t *testing.T) {
	t.Parallel()

	const oldBinary = "OLD BINARY CONTENTS -- must survive every trust fault"

	tests := []struct {
		name   string
		tamper func(t *testing.T, fr *fakeBinaryRelease)
	}{
		{
			name: "bad checksum",
			tamper: func(t *testing.T, fr *fakeBinaryRelease) {
				// SHA256SUMS records a digest for DIFFERENT bytes than the
				// binary served; re-sign so the signature is valid and the
				// checksum is the sole fault.
				fr.sums = buildSums(t, map[string][]byte{
					release.ArtifactBinary: []byte("a different binary than the one served"),
				})
				fr.sig = signEd25519(t, fr.signingKey, fr.sums)
			},
		},
		{
			name: "wrong-identity attestation",
			tamper: func(t *testing.T, fr *fakeBinaryRelease) {
				// A validly-signed attestation minted under a neighboring repo's
				// SAN: the verifier must reject it on certificate identity.
				wrongSAN := "https://github.com/evil/wdm/.github/workflows/release.yml@refs/tags/" + selfUpdateReleaseTag
				entity, err := fr.vs.Attest(wrongSAN, release.OIDCIssuer, binaryStatementFor(release.ArtifactBinary, fr.binary))
				require.NoError(t, err)
				fr.entity = entity
			},
		},
		{
			name: "missing attestation",
			tamper: func(_ *testing.T, fr *fakeBinaryRelease) {
				// An empty attestation body — missing attestation is a
				// verification failure, never a skip.
				fr.attestation = []byte{}
			},
		},
		{
			name: "malformed attestation",
			tamper: func(_ *testing.T, fr *fakeBinaryRelease) {
				// A non-empty, corrupt attestation body — a malformed bundle is
				// a verification failure, never a skip; the
				// checksum (over the unchanged binary) and signature (over the
				// unchanged SHA256SUMS, which never covers the attestation) still
				// pass, so the parse is the sole fault.
				fr.attestation = []byte("{not a valid in-toto attestation bundle}")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fr := newFakeBinaryRelease(t)
			tt.tamper(t, fr)

			target := fakeTarget(t, oldBinary)
			smoke := &smokeStub{version: selfUpdateReleaseTag}
			confirmer := &fakeConfirmer{}
			eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

			result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, nil, confirmer)
			require.Error(t, err)
			assert.Nil(t, result)
			assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
				"want ErrCodeVerificationFailed (exit 3), got %v", err)

			// Fail-closed: verification precedes confirm and replace, so the
			// confirmer and smoke seam never ran.
			assert.Empty(t, confirmer.calls, "confirmer must not run before verification passes")
			assert.Empty(t, smoke.calls, "smoke must not run when verification fails")

			// The live binary is unchanged and no wdm.previous was written.
			got, readErr := os.ReadFile(target)
			require.NoError(t, readErr)
			assert.Equal(t, oldBinary, string(got), "the live binary must not be replaced on a trust fault")
			_, statErr := os.Stat(target + ".previous")
			assert.True(t, os.IsNotExist(statErr), "no wdm.previous must be retained on a trust fault")
		})
	}
}

// TestSelfUpdateConvergence_FailedSmokeRollsBackAndIsByteIdentical is the
// headline rollback test. It exercises the full ApplySelfUpdate chain
// against the verified fixture, then drives the injected smoke seam to fail.
// The engine must restore the previous binary BYTE-IDENTICAL from wdm.previous,
// report RolledBack=true and Replaced=false (the new binary was renamed away by
// the restore), consume wdm.previous so PreviousBinaryPath is empty, and surface
// ErrCodeGeneric (exit 1) — a rolled-back self-update is a failure. This is the
// integrated convergence of the verify→replace→smoke→rollback path proven via
// the public method; the existing TestApplySelfUpdate_FailedSmokeExitRollsBack /
// _RollbackRestoresPreviousByteForByte assert the same behavior on narrower
// fronts and are kept, but this single test ties the full result contract
// together as the convergence headline.
func TestSelfUpdateConvergence_FailedSmokeRollsBackAndIsByteIdentical(t *testing.T) {
	t.Parallel()

	const oldBinary = "the precise old binary bytes -- e3b0c44298fc1c149afbf4c8996fb924\n"

	fr := newFakeBinaryRelease(t)
	target := fakeTarget(t, oldBinary)
	// The new binary reports the WRONG version: smoke fails → rollback.
	smoke := &smokeStub{version: "v9.9.9-impostor"}
	confirmer := &fakeConfirmer{}
	eng := selfUpdateEngine(t, fr, "v1.0.0", target, smoke.run)

	var steps []string
	onProgress := func(step string, _ float64, _ string) { steps = append(steps, step) }

	result, err := eng.ApplySelfUpdate(t.Context(), types.SelfUpdateRequest{}, onProgress, confirmer)

	// A rolled-back self-update is a generic failure (exit 1).
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a rolled-back self-update is a generic failure (exit 1), got %v", err)

	// The result reports the rollback honestly.
	require.NotNil(t, result)
	assert.Equal(t, "v1.0.0", result.PreviousVersion)
	assert.Equal(t, selfUpdateReleaseTag, result.AppliedVersion)
	assert.False(t, result.Replaced, "a successful rollback renamed the new binary away")
	assert.False(t, result.SmokeOK)
	assert.True(t, result.RolledBack)
	assert.Empty(t, result.PreviousBinaryPath,
		"a successful rollback consumes wdm.previous, so PreviousBinaryPath is empty")

	// The confirmer ran exactly once (verification passed, so the replace was
	// authorized) and the smoke seam ran against the NEW target binary.
	require.Len(t, confirmer.calls, 1)
	assert.Equal(t, types.ConfirmationKindSelfUpdate, confirmer.calls[0].Kind)
	require.Len(t, smoke.calls, 1)
	assert.Equal(t, target, smoke.calls[0], "smoke must run the NEW target binary, never the test runner")

	// The live binary is restored byte-identical to the old one — NOT the
	// verified candidate that briefly occupied the target before the smoke check.
	gotTarget, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, oldBinary, string(gotTarget), "the rollback must restore the exact previous binary")
	assert.NotEqual(t, selfUpdateCandidateBinary, string(gotTarget))

	// wdm.previous was consumed by the restore rename, so it no longer exists.
	_, statErr := os.Stat(target + ".previous")
	assert.True(t, os.IsNotExist(statErr), "wdm.previous must be consumed by a successful rollback")

	// The progress stream reached the rollback step.
	assert.Contains(t, steps, types.StepSelfUpdateRollback)
}

// --- catalog convergence: trust negatives fail closed via ApplyCatalogUpdate ---

// TestCatalogUpdateConvergence_TrustNegativesWriteNothing drives
// ApplyCatalogUpdate against the offline fake-catalog-release fixture with one
// trust fault injected per case and proves verify-before-write fails closed:
// every fault maps to ErrCodeVerificationFailed (exit 3), nothing is written
// under the catalogs root (assertNoCatalogWritten), and the confirmer never
// runs. The bad-checksum, wrong-identity, missing-attestation, and
// malformed-attestation faults are proven here at the engine level for the
// first time; bad-signature and tampered-bundle are already covered by
// TestApplyCatalogUpdate_VerifyBeforeWrite_BadSignatureWritesNothing /
// _TamperedBundleWritesNothing and are not re-added.
func TestCatalogUpdateConvergence_TrustNegativesWriteNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tamper func(t *testing.T, fr *fakeCatalogRelease)
	}{
		{
			name: "bad checksum",
			tamper: func(t *testing.T, fr *fakeCatalogRelease) {
				// SHA256SUMS records a digest for a DIFFERENT bundle; re-sign so
				// the signature is valid and the checksum is the sole fault.
				fr.sums = buildSums(t, map[string][]byte{
					release.ArtifactCatalogBundle: []byte("a different bundle than the one served"),
				})
				fr.sig = signEd25519(t, fr.signingKey, fr.sums)
			},
		},
		{
			name: "wrong-identity attestation",
			tamper: func(t *testing.T, fr *fakeCatalogRelease) {
				wrongSAN := "https://github.com/evil/wdm/.github/workflows/release.yml@refs/tags/" + fakeReleaseTag
				entity, err := fr.vs.Attest(wrongSAN, release.OIDCIssuer, catalogStatementFor(release.ArtifactCatalogBundle, fr.bundle))
				require.NoError(t, err)
				fr.entity = entity
			},
		},
		{
			name: "missing attestation",
			tamper: func(_ *testing.T, fr *fakeCatalogRelease) {
				fr.attestation = []byte{}
			},
		},
		{
			name: "malformed attestation",
			tamper: func(_ *testing.T, fr *fakeCatalogRelease) {
				// A non-empty, corrupt attestation body — a malformed bundle is
				// a verification failure, never a skip; the
				// checksum and signature still pass (neither covers the
				// attestation), so the parse is the sole fault.
				fr.attestation = []byte("{not a valid in-toto attestation bundle}")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fr := newFakeCatalogRelease(t)
			tt.tamper(t, fr)

			eng, dataDir := newCatalogUpdateEngine(t, fr)
			confirmer := &fakeConfirmer{}

			result, err := eng.ApplyCatalogUpdate(t.Context(), types.CatalogUpdateRequest{}, nil, confirmer)
			require.Error(t, err)
			assert.Nil(t, result)
			assert.True(t, types.IsCode(err, types.ErrCodeVerificationFailed),
				"want ErrCodeVerificationFailed (exit 3), got %v", err)

			// Verify-before-write: nothing was written and the confirmer never ran.
			assertNoCatalogWritten(t, dataDir)
			assert.Empty(t, confirmer.calls, "confirmer must not run before verification passes")
		})
	}
}
