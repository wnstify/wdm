package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/pkg/types"
)

// This file hosts the CheckCatalogUpdate engine method and the catalog-
// update helpers shared with the apply path in catalog_update_apply.go
// (PRD §22).
// CheckCatalogUpdate is read-only and mirrors Status's posture: no
// runtime.lock, no Confirmer, no ProgressFn. It reports the current local
// catalog version, the latest verified release, whether an update is
// available, the change summary, and the verification state — what the UI
// surfaces before any download or apply gates on it (the invariant:
// network actions explicit; the invariant: verify before apply).
// Network vs verification failures stay strictly distinct:
// a transport fault is [types.ErrCodeNetworkFailure] (exit 8) from the
// release client, a trust fault is [types.ErrCodeVerificationFailed]
// (exit 3) from the verifier. No telemetry is emitted on any path.
// Catalog version identity: the catalog manifest's
// generated_at (RFC 3339, a time.Time with a total ordering) is the
// canonical version for display, for the available/up-to-date decision,
// and for the signed-rollback refusal in the apply path. The GitHub
// release tag binds the attestation certificate identity and appears in
// the verification detail, but it is NOT the freshness key — a tag is an
// opaque string with no safe ordering, while generated_at orders cleanly.

// catalogVersionString renders a catalog manifest's generated_at as the
// canonical version string: RFC 3339 in UTC. It serves both as the
// display version and as the immutable version-snapshot directory name
// (StoreVerifiedCatalog validates it as a safe single path segment —
// RFC 3339 contains no path separator). Mirrors the catalogVersion
// formatting in update.go's planUpdateCheck so the two surfaces agree.
func catalogVersionString(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// CheckCatalogUpdate reports whether a newer verified catalog exists for
// the queried channel (PRD §22). It is read-only: no
// runtime.lock, no Confirmer, no ProgressFn, mirroring [Engine.Status].
// It reads the local catalog version (the active manifest's generated_at,
// empty when none is installed yet), resolves the latest release via the
// trusted release client (a transport fault is exit 8), downloads and
// verifies the latest catalog bundle fail-closed (a trust fault is exit
// 3), and compares the verified candidate's generated_at against the
// local version to decide whether an update is available. The change
// summary is the per-app delta between the local and candidate manifests.
// Verified is true only when the full checksum/signature/attestation
// chain passed; a release whose verification fails surfaces as an exit-3
// error rather than an unverified "update available" — fail closed
// (PRD §22, §23).
func (e *Engine) CheckCatalogUpdate(ctx context.Context, query types.CatalogUpdateQuery) (*types.CatalogUpdateStatus, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.CheckCatalogUpdate: %w", err)
	}

	channel, err := e.resolveCatalogChannel(query.Channel)
	if err != nil {
		return nil, err
	}

	localCat, err := e.readLocalCatalog(ctx, channel)
	if err != nil {
		return nil, err
	}

	// Verify the latest bundle fail-closed: a trust failure
	// surfaces as exit 3 here, not an unverified "update available". The
	// verified bytes/provenance are the apply path's concern, so the check
	// takes only the candidate manifest.
	_, candidate, err := e.resolveVerifiedCandidate(ctx)
	if err != nil {
		return nil, err
	}

	status := &types.CatalogUpdateStatus{
		Channel:       channel,
		LatestVersion: catalogVersionString(candidate.GeneratedAt),
		Verified:      true,
		CheckedAt:     time.Now().UTC(),
	}
	if localCat != nil {
		status.CurrentVersion = catalogVersionString(localCat.GeneratedAt)
	}
	status.UpdateAvailable = catalogUpdateAvailable(localCat, candidate)
	if status.UpdateAvailable {
		status.Changes = diffCatalogApps(localCat, candidate)
	}
	return status, nil
}

// resolveCatalogChannel resolves the requested channel against the
// configured default and validates it, mirroring
// [Engine.loadBrowseCatalog]'s channel guard so check/apply and browse
// reject the same malformed channels (PRD §22). An empty request defaults
// to the configured catalog_channel (normally "stable").
// Beyond the safe-single-path-segment shape check, an effective channel
// other than "stable" is refused up front with a usage-validation error
// (exit 2) — BEFORE any release-client construction or network call —
// because "stable" is the only v1 channel and a usage
// refusal must not masquerade as a verification failure:
// a non-stable channel like "beta" would otherwise run the full download +
// verify and fail only at the store step with a misleading exit-3 error,
// and CheckCatalogUpdate would report an "update available" for a channel
// ApplyCatalogUpdate can never install. The "verified" channel name is
// reserved for a future release and is not accepted yet.
func (e *Engine) resolveCatalogChannel(requested string) (string, error) {
	channel := requested
	if channel == "" {
		channel = e.settings.CatalogChannel
	}
	if !validCatalogChannel(channel) {
		return "", usageValidationError(
			"catalog channel is invalid",
			"set catalog_channel to stable in config.toml",
			fmt.Errorf("invalid catalog channel %q", channel),
		)
	}
	if channel != catalogChannelStable {
		return "", usageValidationError(
			"catalog channel is not supported",
			"only the stable catalog channel is available; set catalog_channel to stable",
			fmt.Errorf("unsupported catalog channel %q", channel),
		)
	}
	return channel, nil
}

// catalogChannelStable is the only catalog channel v1 ships;
// "verified" is reserved for a future release.
const catalogChannelStable = "stable"

// readLocalCatalog reads and parses the active local catalog manifest for
// channel, returning (nil, nil) when none is installed yet so the check
// can honestly report "not installed" (PRD §22, the invariant: a verified
// local catalog wins, but its absence is not an error). A
// present-but-corrupt manifest surfaces as a typed verification failure
// (exit 3); a pure local read fault (EACCES, I/O) is an operational
// generic error (exit 1), neither network nor trust.
func (e *Engine) readLocalCatalog(ctx context.Context, channel string) (*catalog.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	catalogPath := path.Join(channel, "catalog.yaml")
	raw, err := fs.ReadFile(e.installCatalogFS(), catalogPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No local catalog yet — an honest "not installed" state, not a
			// fault. The embedded-fallback first-run path is separate.
			return nil, nil
		}
		// A pure local read fault (permission denied, I/O) on the
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

// resolveVerifiedCandidate resolves the latest release, downloads and
// verifies its catalog bundle fail-closed, and parses the verified
// candidate manifest. It is the shared check/apply trust step: the
// returned *release.VerifiedCatalogBundle carries the verified bytes and
// provenance the apply path stores; the returned *catalog.Catalog is the
// candidate manifest both paths read the version from.
// A transport fault (release-metadata fetch or asset download) is exit 8;
// a verification fault (checksum / signature / attestation) is exit 3
// A verified bundle whose embedded manifest fails the
// schema is also exit 3.
func (e *Engine) resolveVerifiedCandidate(
	ctx context.Context,
) (*release.VerifiedCatalogBundle, *catalog.Catalog, error) {
	client, err := e.releaseDeps.newReleaseClient()
	if err != nil {
		return nil, nil, err
	}

	meta, err := client.LatestRelease(ctx)
	if err != nil {
		return nil, nil, err
	}

	verified, err := e.releaseDeps.verifyCatalogBundle(ctx, client, meta)
	if err != nil {
		return nil, nil, err
	}

	candidate, err := catalog.ReadBundleManifest(ctx, verified.Bundle)
	if err != nil {
		return nil, nil, catalogVerificationError(
			"verified catalog bundle manifest is invalid",
			"the catalog release is malformed; retry later",
			err,
		)
	}
	return verified, candidate, nil
}

// catalogUpdateAvailable reports whether candidate is newer than the
// local catalog by the generated_at ordering. A nil local
// catalog (none installed) always counts as an available update; an equal
// or older candidate is NOT an update.
func catalogUpdateAvailable(local, candidate *catalog.Catalog) bool {
	if local == nil {
		return true
	}
	return candidate.GeneratedAt.After(local.GeneratedAt)
}

// diffCatalogApps computes the per-app change summary between the local
// and candidate catalogs (PRD §22 change summary). Apps only in the
// candidate are "added", apps only local are "removed", and apps whose
// template_version differs are "updated". The result is ordered by app id
// for deterministic rendering across CLI/TUI/GUI. A nil local catalog
// yields an "added" entry per candidate app (first install).
func diffCatalogApps(local, candidate *catalog.Catalog) []types.CatalogChange {
	localByID := map[string]catalog.App{}
	if local != nil {
		for _, app := range local.Apps {
			localByID[app.AppID] = app
		}
	}
	candidateByID := map[string]catalog.App{}
	for _, app := range candidate.Apps {
		candidateByID[app.AppID] = app
	}

	// Union of app ids, sorted for determinism.
	ids := map[string]struct{}{}
	for id := range localByID {
		ids[id] = struct{}{}
	}
	for id := range candidateByID {
		ids[id] = struct{}{}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	slices.Sort(sorted)

	changes := make([]types.CatalogChange, 0, len(sorted))
	for _, id := range sorted {
		localApp, hasLocal := localByID[id]
		candApp, hasCand := candidateByID[id]
		switch {
		case !hasLocal && hasCand:
			changes = append(changes, types.CatalogChange{
				AppID:   id,
				Kind:    "added",
				Summary: fmt.Sprintf("new app at template version %s", candApp.TemplateVersion),
			})
		case hasLocal && !hasCand:
			changes = append(changes, types.CatalogChange{
				AppID:   id,
				Kind:    "removed",
				Summary: "no longer offered in the catalog",
			})
		case localApp.TemplateVersion != candApp.TemplateVersion:
			changes = append(changes, types.CatalogChange{
				AppID:   id,
				Kind:    "updated",
				Summary: fmt.Sprintf("template version %s -> %s", localApp.TemplateVersion, candApp.TemplateVersion),
			})
		}
	}
	return changes
}
