//go:build unix

package catalog

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// Embedded catalog fallback for offline first-run (PRD §22).
// Precedence rule (PRD §22): a verified LOCAL catalog always takes
// precedence. The embedded fallback is consulted only for an offline
// first run, before any verified local catalog has been stored. Once
// [StoreVerifiedCatalog] has materialized an active manifest, the local
// copy wins and the fallback is never used.
// Embedding posture: embedded/fallback.json is a few hundred bytes of
// metadata describing the fallback contract. The full verified stable
// bundle bytes are deliberately not embedded unless a build supplies
// them through [embeddedFallbackBundle].
//
//go:embed embedded/fallback.json
var embeddedFallbackFS embed.FS

// embeddedFallbackBundle holds the embedded verified catalog bundle
// bytes once one is available. It is nil when no large blob is embedded.
// A build may assign it via //go:embed (e.g. embedded/catalog-stable.tar.gz);
// the rest of the precedence machinery needs no change.
var embeddedFallbackBundle []byte

// FallbackMetadata is the decoded embedded fallback descriptor
// (embedded/fallback.json). It documents the bundled-fallback contract
// and, once a real bundle is embedded, names its version and digest.
type FallbackMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	Channel       string `json:"channel"`
	BundlePresent bool   `json:"bundle_present"`
	Version       string `json:"version"`
	BundleSHA256  string `json:"bundle_sha256"`
}

// Source names which catalog the resolver selected.
type Source int

const (
	// SourceNone means neither a verified local catalog nor an embedded
	// fallback bundle is available.
	SourceNone Source = iota

	// SourceLocal means a verified local catalog manifest is present and
	// takes precedence (PRD §22).
	SourceLocal

	// SourceEmbeddedFallback means no verified local catalog exists yet
	// and an embedded fallback bundle is available for offline
	// first-run.
	SourceEmbeddedFallback
)

// String renders the source for diagnostics.
func (s Source) String() string {
	switch s {
	case SourceLocal:
		return "local"
	case SourceEmbeddedFallback:
		return "embedded_fallback"
	default:
		return "none"
	}
}

// ErrNoEmbeddedFallback is returned by [StoreEmbeddedFallback] when no
// embedded catalog bundle is available in this build. Detect with
// [errors.Is].
var ErrNoEmbeddedFallback = errors.New("catalog: no embedded fallback bundle is available")

// LocalCatalogPresent reports whether a verified local catalog manifest
// exists for channel under catalogsRoot — that is, whether
// <catalogsRoot>/<channel>/catalog.yaml is a regular file. A present
// local catalog takes precedence over the embedded fallback (PRD §22).
// catalogsRoot MUST be absolute and channel MUST be the supported v1
// channel; otherwise it reports false (malformed inputs claim no local
// catalog).
func LocalCatalogPresent(catalogsRoot, channel string) bool {
	if catalogsRoot == "" || !filepath.IsAbs(catalogsRoot) || !validChannel(channel) {
		return false
	}
	info, err := os.Lstat(filepath.Join(catalogsRoot, channel, activeManifestName))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// ResolveCatalogSource decides which catalog source the engine should
// use under catalogsRoot for channel, applying the PRD §22 precedence
// rule: verified local catalog first, embedded fallback only when no
// local catalog exists yet.
// It performs no writes and no network access. [SourceEmbeddedFallback]
// means [StoreEmbeddedFallback] can seed the local catalog for an
// offline first run; [SourceNone] means neither source is available
// (the offline-with-no-bundle case the caller must surface as "no
// catalog yet").
func ResolveCatalogSource(catalogsRoot, channel string) Source {
	if LocalCatalogPresent(catalogsRoot, channel) {
		return SourceLocal
	}
	if EmbeddedFallbackAvailable() {
		return SourceEmbeddedFallback
	}
	return SourceNone
}

// EmbeddedFallbackAvailable reports whether this build carries an
// embedded fallback bundle that [StoreEmbeddedFallback] could seed. It
// is false when this build does not embed a fallback bundle.
func EmbeddedFallbackAvailable() bool {
	return len(embeddedFallbackBundle) > 0
}

// readEmbeddedFallbackMeta decodes the embedded fallback descriptor. It
// is a function seam so tests can simulate a build that ships a real
// fallback bundle (BundlePresent + Version) without committing a blob.
// Production never reassigns it.
var readEmbeddedFallbackMeta = func() (FallbackMetadata, error) {
	raw, err := embeddedFallbackFS.ReadFile("embedded/fallback.json")
	if err != nil {
		return FallbackMetadata{}, fmt.Errorf("catalog: reading embedded fallback metadata: %w", err)
	}
	var meta FallbackMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return FallbackMetadata{}, fmt.Errorf("catalog: decoding embedded fallback metadata: %w", err)
	}
	return meta, nil
}

// EmbeddedFallback returns the decoded embedded fallback descriptor and
// the embedded bundle bytes (nil when none is available). The bytes are
// the full verified bundle [StoreEmbeddedFallback] would store; they
// are nil when this build does not embed a fallback bundle.
func EmbeddedFallback() (FallbackMetadata, []byte, error) {
	meta, err := readEmbeddedFallbackMeta()
	if err != nil {
		return FallbackMetadata{}, nil, err
	}
	return meta, embeddedFallbackBundle, nil
}

// StoreEmbeddedFallback seeds the local catalog from the embedded
// fallback bundle for an offline first run, deferring entirely to
// [StoreVerifiedCatalog] for atomic, contained, rollback-safe storage.
// It returns [ErrNoEmbeddedFallback] when no bundle is embedded in this
// build. When a verified local catalog
// already exists, the caller SHOULD NOT call this — the local catalog
// takes precedence; this function still stores the fallback under its
// own version directory but never overwrites an active manifest of a
// newer/equal version, because [StoreVerifiedCatalog] refuses an
// existing version snapshot. The embedded bundle is treated as
// already-verified (it shipped in the signed binary).
func StoreEmbeddedFallback(ctx context.Context, catalogsRoot string) (versionDir string, err error) {
	meta, bundle, err := EmbeddedFallback()
	if err != nil {
		return "", err
	}
	if len(bundle) == 0 || !meta.BundlePresent {
		return "", types.WrapError(
			types.ErrCodeVerificationFailed,
			"no bundled catalog is available for offline first-run",
			"connect to the network and run the catalog update",
			ErrNoEmbeddedFallback,
		)
	}
	channel := meta.Channel
	if channel == "" {
		channel = bundleChannelDirName
	}
	return StoreVerifiedCatalog(ctx, catalogsRoot, channel, meta.Version, bundle, nil)
}
