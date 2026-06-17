package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the ApplyCatalogUpdate engine method (PRD §22). It is
// state-changing and mirrors
// [Engine.Update]'s posture: it holds the global runtime.lock, emits
// progress, and gates the consequential step on a Confirmer.
// Strict ordering — VERIFY BEFORE ANY WRITE:
//  1. Closed-flag + ctx.Err.
//  2. Acquire the global runtime.lock attributed "catalog-update"
//     (released on every path). It NEVER touches a per-stack .wdm.lock
//     and NEVER modifies deployed apps; the only write is
//     the verified catalog under <dataDir>/catalogs/<channel>/.
//  3. Resolve the latest release and download + verify the catalog asset
//     set fail-closed (checksum, detached signature, attestation). A
//     transport fault is exit 8, a verification fault exit 3
//     Nothing is written yet.
//  4. Refuse a signed rollback: a verified candidate whose
//     generated_at is older than OR equal to the active verified
//     catalog's is refused BEFORE any write. Authenticity is not
//     freshness.
//  5. Confirm via the catalog_update Confirmer kind. Nil confirmer ->
//     ErrCodeUsageValidation; decline -> ErrCodeUserCanceled; confirmer
//     error -> wrapped.
//  6. Write atomically via [catalog.StoreVerifiedCatalog] under
//     <dataDir>/catalogs/<channel>/ (the layout: active
//     catalog.yaml + templates/, immutable snapshot under
//     stable/.versions/<version>/). Rollback on a failed write is
//     StoreVerifiedCatalog's job.
//  7. Populate the full CatalogUpdateResult.
//
// catalogUpdateLockCommand is the runtime.lock attribution for the apply.
const catalogUpdateLockCommand = "catalog-update"

// ApplyCatalogUpdate downloads, verifies, and installs a newer catalog
// (PRD §22). See the file-level comment for the
// strict verify-before-write ordering. It never modifies deployed apps or
// any per-stack .wdm.lock.
func (e *Engine) ApplyCatalogUpdate(
	ctx context.Context,
	req types.CatalogUpdateRequest,
	onProgress types.ProgressFn,
	confirmer types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.ApplyCatalogUpdate: %w", err)
	}

	channel, err := e.resolveCatalogChannel(req.Channel)
	if err != nil {
		return nil, err
	}

	// Step 2: hold the global runtime.lock for the whole apply.
	handle, err := e.acquireRuntimeLock(ctx, catalogUpdateLockCommand)
	if err != nil {
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	if onProgress != nil {
		onProgress(types.StepCatalogUpdatePlanning, 5, "planning catalog update")
	}

	// The active local catalog (nil when none is installed yet). Read from
	// the on-disk catalogs root the write targets so the rollback
	// comparison and the write agree even when a test injects WithCatalog.
	catalogsRoot := e.catalogsRoot()
	localCat, err := e.readActiveCatalogOnDisk(ctx, catalogsRoot, channel)
	if err != nil {
		return nil, err
	}

	// Step 3: download + verify fail-closed.
	if onProgress != nil {
		onProgress(types.StepCatalogUpdateDownload, 25, "downloading catalog release")
	}
	verified, candidate, err := e.resolveVerifiedCandidate(ctx)
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepCatalogUpdateVerify, 50, "catalog release verified")
	}

	previousVersion := ""
	if localCat != nil {
		previousVersion = catalogVersionString(localCat.GeneratedAt)
	}
	appliedVersion := catalogVersionString(candidate.GeneratedAt)

	// Step 4a: if the caller pinned a target version, the verified candidate
	// must match it (the version CheckCatalogUpdate surfaced, i.e. the
	// candidate manifest's generated_at). A mismatch is refused BEFORE any
	// write so a TOCTOU between check and apply (the latest release moved)
	// never silently installs a version the user did not authorize.
	if req.TargetVersion != "" && req.TargetVersion != appliedVersion {
		return nil, usageValidationError(
			"the latest verified catalog does not match the requested target version",
			fmt.Sprintf("the latest verified catalog is %s; re-run the check and retry", appliedVersion),
			fmt.Errorf("requested target version %q, verified latest %q", req.TargetVersion, appliedVersion),
		)
	}

	// Step 4b: refuse a signed rollback BEFORE any write.
	if err := refuseCatalogRollback(localCat, candidate); err != nil {
		return nil, err
	}

	// Step 5: confirm the network-and-write action.
	if err := confirmCatalogUpdate(ctx, confirmer, channel, previousVersion, appliedVersion, candidate, onProgress); err != nil {
		return nil, err
	}

	// Step 6: atomic verified write (StoreVerifiedCatalog owns rollback).
	if onProgress != nil {
		onProgress(types.StepCatalogUpdateApply, 80, "installing verified catalog")
	}
	provenance := catalogProvenanceFromVerified(verified)
	if _, err := catalog.StoreVerifiedCatalog(ctx, catalogsRoot, channel, appliedVersion, verified.Bundle, provenance); err != nil {
		return nil, err
	}

	if onProgress != nil {
		onProgress(types.StepCatalogUpdateStatus, 100, "catalog update complete")
	}

	// Step 7: result.
	result := &types.CatalogUpdateResult{
		Channel:            channel,
		PreviousVersion:    previousVersion,
		AppliedVersion:     appliedVersion,
		VerificationDetail: catalogVerificationDetail(verified),
		Changes:            diffCatalogApps(localCat, candidate),
		AppliedAt:          time.Now().UTC(),
	}
	return result, nil
}

// catalogsRoot is the absolute <dataDir>/catalogs directory the verified
// catalog is stored under. It mirrors the os.DirFS root
// [installCatalogFS] reads when no catalog FS is injected, but as a
// concrete path because StoreVerifiedCatalog writes to disk.
func (e *Engine) catalogsRoot() string {
	return filepath.Join(e.dataDir, "catalogs")
}

// readActiveCatalogOnDisk reads and parses the active local catalog
// manifest directly from the on-disk catalogs root (NOT the injected
// catalog FS), so the apply path's rollback comparison and write target
// agree. Returns (nil, nil) when no local catalog is installed yet. A
// present-but-corrupt manifest is a typed verification failure (exit 3);
// a pure local read fault (EACCES, I/O) is an operational generic error
// (exit 1), neither network nor trust.
func (e *Engine) readActiveCatalogOnDisk(ctx context.Context, catalogsRoot, channel string) (*catalog.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(catalogsRoot, channel, "catalog.yaml")
	raw, err := os.ReadFile(manifestPath) //nolint:gosec // G304: engine-controlled XDG catalogs path, channel validated as a safe single segment
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		// A pure local read fault (permission denied, I/O error) on the
		// already-installed catalog is neither a network nor a trust failure:
		// it is an operational generic error (exit 1), not a verification
		// failure (watch-item-1 principle).
		return nil, genericError(
			"local catalog could not be read",
			"check the catalogs directory permissions and retry",
			err,
		)
	}
	cat, err := catalog.LoadCatalogBytes(ctx, raw)
	if err != nil {
		return nil, catalogVerificationError(
			"local catalog could not be verified",
			"refresh the catalog and retry",
			err,
		)
	}
	return cat, nil
}

// refuseCatalogRollback enforces the invariant (authenticity is not
// freshness): a verified candidate whose generated_at is older than OR
// equal to the active local catalog's is refused BEFORE any write. A nil
// local catalog (first install) can never be a rollback.
// The ordering key is the manifest generated_at (a time.Time with a total
// ordering), NOT the release tag, an opaque string with no safe ordering.
// The refusal is a usage-validation error (exit 2): the downloaded
// catalog is authentic, so it is neither a transport (exit 8) nor a trust
// (exit 3) failure, but a refusal to install a stale catalog.
func refuseCatalogRollback(local, candidate *catalog.Catalog) error {
	if local == nil {
		return nil
	}
	if candidate.GeneratedAt.After(local.GeneratedAt) {
		return nil
	}
	return usageValidationError(
		"the latest verified catalog is not newer than the installed catalog",
		"the installed catalog is already current; a catalog downgrade is refused",
		fmt.Errorf(
			"candidate generated_at %s is not after installed generated_at %s",
			catalogVersionString(candidate.GeneratedAt),
			catalogVersionString(local.GeneratedAt),
		),
	)
}

// confirmCatalogUpdate gates the apply on the catalog_update Confirmer
// kind after verification and before the write. The
// consequence payload names the channel, the version transition, and what
// will be written. A nil confirmer refuses with ErrCodeUsageValidation
// (the install/update posture), a decline maps to ErrCodeUserCanceled,
// and a confirmer error propagates wrapped — none of which leaves an
// on-disk side effect, since the write has not run yet.
func confirmCatalogUpdate(
	ctx context.Context,
	confirmer types.Confirmer,
	channel, previousVersion, appliedVersion string,
	candidate *catalog.Catalog,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required for a catalog update",
			"pass a confirmer that can authorize the catalog download and write",
		)
	}
	if onProgress != nil {
		onProgress(types.StepCatalogUpdateConfirm, 65, "confirming catalog update")
	}

	confirmed, err := confirmer.Confirm(ctx, catalogUpdateConfirmation(channel, previousVersion, appliedVersion, candidate))
	if err != nil {
		return fmt.Errorf("core.ApplyCatalogUpdate: confirming catalog update: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"catalog update canceled before applying",
			"re-run the catalog update and confirm the prompt to install the verified catalog",
		)
	}
	return nil
}

// catalogUpdateConfirmation builds the consequence payload surfaced
// before the verified catalog is written (PRD §22,
// ConfirmationKindCatalogUpdate). It names the channel, the version
// transition, and the storage location — never any secret (catalog
// content is non-secret config).
func catalogUpdateConfirmation(channel, previousVersion, appliedVersion string, candidate *catalog.Catalog) types.Confirmation {
	from := previousVersion
	if from == "" {
		from = "(none installed)"
	}
	msg := fmt.Sprintf(
		"Install the verified %s catalog %s (currently %s)?\n"+
			"This writes the verified catalog under the local catalogs directory and does not change any deployed app.\n"+
			"Apps in the catalog: %d.",
		channel, appliedVersion, from, len(candidate.Apps),
	)
	return types.Confirmation{
		Kind:    types.ConfirmationKindCatalogUpdate,
		Title:   fmt.Sprintf("update %s catalog to %s", channel, appliedVersion),
		Message: msg,
	}
}

// catalogProvenanceFromVerified maps the release-side verified provenance
// (the downloaded SHA256SUMS / signature / attestation bytes) onto
// [catalog.ProvenanceFile] so they are stored immutably alongside the
// verified snapshot (PRD §22). The bytes are already verified upstream;
// storage only persists them.
func catalogProvenanceFromVerified(verified *release.VerifiedCatalogBundle) []catalog.ProvenanceFile {
	out := make([]catalog.ProvenanceFile, 0, len(verified.Provenance))
	for _, p := range verified.Provenance {
		out = append(out, catalog.ProvenanceFile{Name: p.Name, Data: p.Data})
	}
	return out
}

// catalogVerificationDetail is a short, user-safe description of the
// verification that passed before the write (PRD §22, §23). It names the
// verified release identity (the attestation SAN) and bundle digest — no
// secrets, no internal paths.
func catalogVerificationDetail(verified *release.VerifiedCatalogBundle) string {
	return fmt.Sprintf(
		"checksum, detached signature, and attestation verified for release %s (bundle sha256 %s)",
		verified.Tag, verified.BundleDigest,
	)
}
