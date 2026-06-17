//go:build unix

package catalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// Verified-catalog on-disk storage layout (PRD §22).
// The engine reads catalogs through os.DirFS rooted at
// <dataDir>/catalogs/ and resolves <channel>/catalog.yaml plus a
// shared templates/ tree whose paths (templates/<app>/...) are
// relative to that catalogs root (see internal/core/install.go and
// catalog_browse.go). The active layout this writer materializes MUST
// keep those exact read locations:
//
//	<catalogsRoot>/
//	  stable/
//	    catalog.yaml active manifest (engine reads)
//	    .versions/
//	      <version>/ immutable verified snapshot
//	        stable/catalog.yaml snapshot copy of the manifest
//	        templates/<app>/... snapshot copy of the templates
//	        SHA256SUMS downloaded provenance (optional)
//	        SHA256SUMS.sig downloaded provenance (optional)
//	        attestation.json downloaded provenance (optional)
//	  templates/<app>/... active templates (engine reads)
//
// the active manifest at catalogs/stable/catalog.yaml. The snapshot
// under stable/.versions/<version>/ holds the whole verified bundle
// plus the downloaded checksum/signature/attestation files, satisfying
// PRD §22's "downloaded catalog artifacts, signatures, checksums, and
// attestations must be stored... in a versioned subdirectory under the
// same channel path." An immutable snapshot lets a future explicit
// downgrade flow materialize a prior
// version without re-downloading.
// The bundle's templates/ is a channel-root SIBLING shared across channels because
// the manifest's template paths are catalogs-root relative, not
// channel relative. The active templates therefore live at
// <catalogsRoot>/templates/, not under the channel directory. v1 ships
// only the stable channel (PRD §22), so the active
// templates are unambiguously stable's; the snapshot keeps its own
// templates copy for provenance and future downgrade. When a second
// channel ever ships, active-templates ownership across channels must
// be re-adjudicated.
// rather than silently coupling templates to one version.
const (
	// activeManifestName is the active channel manifest file the engine
	// reads as <channel>/catalog.yaml.
	activeManifestName = "catalog.yaml"

	// activeTemplatesDirName is the shared active templates directory at
	// the catalogs root — the channel-directory sibling the manifest's
	// template paths resolve against.
	activeTemplatesDirName = "templates"

	// versionsDirName is the per-channel directory holding immutable
	// verified snapshots.
	versionsDirName = ".versions"

	// bundleChannelDirName / bundleTemplatesDirName are the archive-root
	// entries the verified bundle extracts to, mirroring
	// internal/release.CatalogBundleChannelDir /
	// CatalogBundleTemplatesDir. They are
	// re-declared rather than imported because internal/catalog is a
	// sibling of internal/release and must carry no release-package
	// dependency; a contract test pins byte equality against the
	// release constants.
	bundleChannelDirName   = "stable"
	bundleTemplatesDirName = "templates"

	// bundleManifestRelPath is the manifest path inside the extracted
	// bundle (mirrors internal/release.CatalogBundleManifestPath).
	bundleManifestRelPath = "stable/catalog.yaml"
)

// ProvenanceFile is one downloaded trust artifact (SHA256SUMS, its
// signature, or the attestation) the storage writer records alongside
// the verified manifest in the version snapshot per PRD §22. The bytes
// are already verified upstream (internal/release); this writer only
// persists them as immutable provenance.
type ProvenanceFile struct {
	// Name is the file name inside the version snapshot directory. It
	// MUST be a single path segment (no separators), typically one of
	// the release.Artifact* checksum/signature/attestation names.
	Name string

	// Data is the raw file content.
	Data []byte
}

// ErrCatalogStorage is returned (wrapped) when storing a verified
// catalog fails for a reason other than an invalid manifest (which
// wraps [ErrCatalogInvalid]). It rides inside a typed pkg/types error
// so cmd/wdm maps it to the right exit code; detect the class with
// [errors.Is].
var ErrCatalogStorage = errors.New("catalog: storage failed")

// storageNowUTC is the clock seam for the staging directory's unique
// suffix; tests swap it for determinism.
var storageNowUTC = func() time.Time { return time.Now().UTC() }

// storageWriteManifest is the active-manifest commit-point seam. It
// defaults to the atomic state writer; tests swap it to inject a
// commit-point failure and prove the prior active layout is restored
// Production never reassigns it.
var storageWriteManifest = state.WriteFileAtomic

// StoreVerifiedCatalog atomically installs an already-verified catalog
// bundle into the on-disk layout under catalogsRoot for the given
// channel and version (PRD §22).
// The bundle MUST be the gzip-tar produced by the release workflow
// (root entries stable/ and templates/, manifest at
// stable/catalog.yaml). It is treated as hostile bytes at the
// filesystem level even though its authenticity was proven upstream:
// every archive member is path-contained by internal/state, and the
// extracted manifest is re-validated against the embedded schema as a
// structural sanity check before it can become active.
// catalogsRoot MUST be the absolute <dataDir>/catalogs directory.
// channel MUST be "stable" (the only v1 channel). version MUST be a
// safe single path segment used as the snapshot directory name; a
// version whose snapshot already exists is rejected so existing
// provenance is never clobbered (idempotent, fail-closed).
// Ordering and rollback: the bundle extracts into
// a temporary staging directory, then publishes atomically to the
// immutable version snapshot. Only once the snapshot exists is the
// ACTIVE layout updated — templates first, then the manifest as the
// commit point. A failure updating the active layout restores the prior
// active templates and leaves the prior active manifest byte-identical,
// so a partway failure never leaves the engine reading a half-updated
// catalog. On success the returned versionDir is the absolute snapshot
// path.
func StoreVerifiedCatalog(
	ctx context.Context,
	catalogsRoot, channel, version string,
	bundle []byte,
	provenance []ProvenanceFile,
) (versionDir string, err error) {
	if err := ctx.Err(); err != nil {
		return "", storageError("catalog storage canceled", "retry the catalog update", err)
	}
	if err := validateStorageInputs(catalogsRoot, channel, version, bundle); err != nil {
		return "", err
	}
	if err := validateProvenance(provenance); err != nil {
		return "", err
	}

	channelDir := filepath.Join(catalogsRoot, channel)
	versionsRoot := filepath.Join(channelDir, versionsDirName)
	// snapshotDir is the working path every cleanup defer below uses; the
	// named versionDir return is set only on success, so a
	// "return "", err" failure can never clobber the path the defers
	// must remove.
	snapshotDir := filepath.Join(versionsRoot, version)

	if _, statErr := os.Lstat(snapshotDir); statErr == nil {
		return "", storageError(
			"catalog version already stored",
			"remove the existing version snapshot or store a different version",
			fmt.Errorf("%w: version snapshot %q already exists", ErrCatalogStorage, snapshotDir),
		)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", storageIOError("catalog version snapshot could not be checked", statErr)
	}

	if dirErr := ensureDir(versionsRoot); dirErr != nil {
		return "", dirErr
	}

	// Extract into a staging dir, then publish atomically to snapshotDir.
	staging := filepath.Join(versionsRoot, fmt.Sprintf(".staging-%d", storageNowUTC().UnixNano()))
	if extractErr := state.ExtractTarGzToDir(ctx, bundle, staging); extractErr != nil {
		return "", storageError("catalog bundle could not be extracted", "the catalog bundle is malformed; retry the update", extractErr)
	}
	// From here on, a failure must remove the staging tree. Every error
	// return below assigns the named err so this and the snapshot-removal
	// defer fire — a shadowed local err would leave the named return nil
	// and skip them.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeStorageTree(staging))
		}
	}()

	manifestBytes, structErr := validateStagedBundle(ctx, staging)
	if structErr != nil {
		err = structErr
		return "", err
	}
	if provErr := writeProvenance(staging, provenance); provErr != nil {
		err = provErr
		return "", err
	}

	if renameErr := os.Rename(staging, snapshotDir); renameErr != nil {
		err = storageIOError("catalog version snapshot could not be published", renameErr)
		return "", err
	}
	// Snapshot is now published; on later failure remove the version dir,
	// not the (already-renamed-away) staging dir.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeStorageTree(snapshotDir))
		}
	}()
	if syncErr := state.SyncDirectory(versionsRoot); syncErr != nil {
		err = storageIOError("catalog version snapshot could not be synced", syncErr)
		return "", err
	}

	if activateErr := activateVerifiedSnapshot(channelDir, catalogsRoot, snapshotDir, manifestBytes); activateErr != nil {
		err = activateErr
		return "", err
	}
	return snapshotDir, nil
}

// activateVerifiedSnapshot updates the ACTIVE layout from a published
// immutable snapshot: it materializes the active templates tree, then
// writes the active manifest as the commit point, restoring the prior
// active templates if the manifest write fails.
func activateVerifiedSnapshot(channelDir, catalogsRoot, versionDir string, manifestBytes []byte) (err error) {
	activeManifest := filepath.Join(channelDir, activeManifestName)
	activeTemplates := filepath.Join(catalogsRoot, activeTemplatesDirName)
	snapshotTemplates := filepath.Join(versionDir, bundleTemplatesDirName)

	// Stash the prior active templates so they can be restored if the
	// manifest commit fails. A rename (not a copy) keeps the stash cheap
	// and atomic; missing prior templates is fine.
	stash := activeTemplates + fmt.Sprintf(".prev-%d", storageNowUTC().UnixNano())
	hadTemplates := false
	if _, statErr := os.Lstat(activeTemplates); statErr == nil {
		if renameErr := os.Rename(activeTemplates, stash); renameErr != nil {
			return storageIOError("prior catalog templates could not be set aside", renameErr)
		}
		hadTemplates = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return storageIOError("prior catalog templates could not be checked", statErr)
	}

	// Restore the stashed templates on any failure below; on success,
	// discard the stash best-effort.
	restoreTemplates := func() error {
		if !hadTemplates {
			return removeStorageTree(activeTemplates)
		}
		if rmErr := removeStorageTree(activeTemplates); rmErr != nil {
			return rmErr
		}
		return os.Rename(stash, activeTemplates)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, restoreTemplates())
			return
		}
		// Success: drop the stash. A leftover stash is cosmetic, not a
		// correctness fault, so a removal error is non-fatal.
		_ = removeStorageTree(stash) //nolint:errcheck // best-effort stash cleanup after a successful commit; a leftover stash is cosmetic, not a correctness fault
	}()

	if copyErr := state.CopyTree(snapshotTemplates, activeTemplates); copyErr != nil {
		return storageIOError("catalog templates could not be activated", copyErr)
	}

	// Commit point: the engine keys on catalog.yaml, so it is written
	// last via the atomic temp+rename path. WriteFileAtomic is
	// all-or-nothing, so a failure here leaves the prior manifest
	// byte-identical while the deferred restore reverts the templates.
	if writeErr := storageWriteManifest(activeManifest, manifestBytes, catalogManifestMode); writeErr != nil {
		return storageIOError("catalog manifest could not be activated", writeErr)
	}
	return nil
}

// catalogManifestMode is the restrictive mode for the active and
// snapshot catalog manifests. Catalog manifests are non-secret config.
const catalogManifestMode os.FileMode = 0o644

func validateStorageInputs(catalogsRoot, channel, version string, bundle []byte) error {
	if catalogsRoot == "" || !filepath.IsAbs(catalogsRoot) {
		return storageError(
			"catalog root path is invalid",
			"the catalogs root must be an absolute path",
			fmt.Errorf("%w: catalogs root %q is not absolute", ErrCatalogStorage, catalogsRoot),
		)
	}
	if !validChannel(channel) {
		return storageError(
			"catalog channel is invalid",
			"v1 supports only the stable channel",
			fmt.Errorf("%w: invalid channel %q", ErrCatalogStorage, channel),
		)
	}
	if !validVersionSegment(version) {
		return storageError(
			"catalog version is invalid",
			"the catalog version must be a single safe path segment",
			fmt.Errorf("%w: invalid version %q", ErrCatalogStorage, version),
		)
	}
	if len(bundle) == 0 {
		return storageError(
			"catalog bundle is empty",
			"download the catalog bundle before storing it",
			fmt.Errorf("%w: empty bundle", ErrCatalogStorage),
		)
	}
	return nil
}

func validateProvenance(provenance []ProvenanceFile) error {
	seen := make(map[string]struct{}, len(provenance))
	for _, p := range provenance {
		if !validVersionSegment(p.Name) {
			return storageError(
				"catalog provenance file name is invalid",
				"provenance file names must be single safe path segments",
				fmt.Errorf("%w: invalid provenance name %q", ErrCatalogStorage, p.Name),
			)
		}
		if p.Name == activeManifestName || p.Name == bundleChannelDirName || p.Name == bundleTemplatesDirName {
			return storageError(
				"catalog provenance file name collides with a bundle entry",
				"choose a provenance file name distinct from the bundle's own entries",
				fmt.Errorf("%w: provenance name %q collides with a bundle entry", ErrCatalogStorage, p.Name),
			)
		}
		if _, dup := seen[p.Name]; dup {
			return storageError(
				"duplicate catalog provenance file name",
				"each provenance file name must be unique",
				fmt.Errorf("%w: duplicate provenance name %q", ErrCatalogStorage, p.Name),
			)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}

// validateStagedBundle confirms the extracted bundle has the expected
// root layout and that its manifest validates against the embedded
// schema, returning the manifest bytes for activation. A structural
// failure fails closed before anything becomes active.
func validateStagedBundle(ctx context.Context, staging string) ([]byte, error) {
	for _, want := range []string{bundleChannelDirName, bundleTemplatesDirName} {
		info, statErr := os.Lstat(filepath.Join(staging, want))
		if statErr != nil || !info.IsDir() {
			return nil, storageError(
				"catalog bundle layout is invalid",
				"the catalog bundle is missing its expected stable/ and templates/ entries",
				fmt.Errorf("%w: bundle missing %q directory at root", ErrCatalogStorage, want),
			)
		}
	}

	manifestPath := filepath.Join(staging, filepath.FromSlash(bundleManifestRelPath))
	// G304: manifestPath is built from the absolute staging dir created
	// by ExtractTarGzToDir; bundleManifestRelPath is a fixed constant.
	manifestBytes, readErr := os.ReadFile(manifestPath) //nolint:gosec // G304: path is under the absolute staging dir, fixed relative component
	if readErr != nil {
		return nil, storageError(
			"catalog bundle manifest could not be read",
			"the catalog bundle is missing stable/catalog.yaml",
			fmt.Errorf("%w: %w", ErrCatalogStorage, readErr),
		)
	}
	if _, validateErr := LoadCatalogBytes(ctx, manifestBytes); validateErr != nil {
		// LoadCatalogBytes wraps ErrCatalogInvalid; surface it under the
		// verification-failure code, keeping the cause
		// reachable.
		return nil, catalogManifestError(validateErr)
	}
	return manifestBytes, nil
}

func writeProvenance(staging string, provenance []ProvenanceFile) error {
	for _, p := range provenance {
		// validateProvenance already proved p.Name is a safe single
		// segment; security.SafeJoin is the canonical containment
		// primitive and a defense-in-depth backstop.
		dest, joinErr := security.SafeJoin(staging, p.Name)
		if joinErr != nil {
			return storageError(
				"catalog provenance path is invalid",
				"provenance file names must be safe single segments",
				fmt.Errorf("%w: rejecting provenance %q: %w", ErrCatalogStorage, p.Name, joinErr),
			)
		}
		if err := state.WriteFileAtomic(dest, p.Data, catalogManifestMode); err != nil {
			return storageIOError(
				"catalog provenance file could not be written",
				fmt.Errorf("%w: writing provenance %q: %w", ErrCatalogStorage, p.Name, err),
			)
		}
	}
	return nil
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, state.GeneratedDirMode); err != nil {
		return storageIOError("catalog storage directory could not be created", err)
	}
	if err := os.Chmod(dir, state.GeneratedDirMode); err != nil {
		return storageIOError("catalog storage directory mode could not be set", err)
	}
	return nil
}

// removeStorageTree rolls back a partial storage write by removing
// treePath. Destructive removal goes through internal/state's single
// sanctioned site (state.RemoveContainedTree) so internal/catalog holds
// no os.RemoveAll call of its own.
func removeStorageTree(treePath string) error {
	if err := state.RemoveContainedTree(treePath); err != nil {
		return fmt.Errorf("%w: removing %q: %w", ErrCatalogStorage, treePath, err)
	}
	return nil
}

// validChannel reports whether channel is the only supported v1
// channel as a safe single path segment.
func validChannel(channel string) bool {
	return channel == bundleChannelDirName
}

// validVersionSegment reports whether s is a safe single path segment
// usable as a directory or file name: non-empty, slash-free, neither
// "." nor "..", and accepted by fs.ValidPath.
func validVersionSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if path.Base(s) != s {
		return false
	}
	for i := range len(s) {
		if s[i] == '/' || s[i] == 0 {
			return false
		}
	}
	return fs.ValidPath(s)
}

// storageError wraps cause in the typed verification-failure error
// carrying the storage sentinel so callers
// detect the class with errors.Is while cmd/wdm maps the exit code.
// It is reserved for genuine contract/verification faults: a malformed
// bundle layout, a path-traversal/containment rejection, an
// already-stored version snapshot, an invalid channel/version/empty
// bundle, or a bad provenance descriptor. A PURE local filesystem I/O
// fault (mkdir, rename, sync, copy, chmod, atomic-write) is NOT a trust
// failure and routes through [storageIOError] (exit 1) instead, so a
// disk-full or permission error never falsely implies a verification
// problem.
func storageError(message, hint string, cause error) error {
	return wrapStorage(types.ErrCodeVerificationFailed, message, hint, cause)
}

// storageIOError wraps cause in the typed generic error (PRD §27,
// exit 1) carrying the storage sentinel. It is the FS-I/O-fault arm of
// the storage exit-code split: a failed mkdir, rename, fsync, tree copy,
// chmod, or atomic write while installing an already-verified bundle is
// a local infrastructure fault, neither a network (exit 8) nor a trust
// (exit 3) failure. [ErrCatalogStorage] stays reachable via errors.Is
// on both arms. Every FS-fault site shares
// one retry hint, so it is fixed here rather than per-call.
func storageIOError(message string, cause error) error {
	return wrapStorage(types.ErrCodeGeneric, message, "retry the catalog update", cause)
}

// wrapStorage is the shared body for [storageError] / [storageIOError]:
// it ensures the [ErrCatalogStorage] sentinel is in the cause chain
// (reachable via errors.Is regardless of the exit code) and attaches the
// supplied typed exit code.
func wrapStorage(code types.ErrorCode, message, hint string, cause error) error {
	if cause == nil {
		cause = ErrCatalogStorage
	} else if !errors.Is(cause, ErrCatalogStorage) {
		cause = fmt.Errorf("%w: %w", ErrCatalogStorage, cause)
	}
	return types.WrapError(code, message, hint, cause)
}

// catalogManifestError wraps a LoadCatalogBytes failure (which carries
// ErrCatalogInvalid) in the typed verification-failure error.
func catalogManifestError(cause error) error {
	return types.WrapError(
		types.ErrCodeVerificationFailed,
		"catalog bundle manifest is invalid",
		"the catalog bundle is corrupt; retry the update",
		cause,
	)
}
