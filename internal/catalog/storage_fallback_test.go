//go:build unix

package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

func TestEmbeddedFallback_MechanismOnlyByDefault(t *testing.T) {
	t.Parallel()

	meta, bundle, err := EmbeddedFallback()
	require.NoError(t, err)
	assert.Equal(t, "stable", meta.Channel)
	assert.False(t, meta.BundlePresent, "no real bundle is embedded in this commit")
	assert.Nil(t, bundle, "no embedded bundle bytes in this commit")
	assert.False(t, EmbeddedFallbackAvailable())
}

func TestResolveCatalogSource_LocalTakesPrecedence(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)

	// No local catalog and no embedded bundle -> none.
	assert.Equal(t, SourceNone, ResolveCatalogSource(root, "stable"))

	// Store a verified local catalog -> local wins.
	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
	)
	require.NoError(t, err)
	assert.True(t, LocalCatalogPresent(root, "stable"))
	assert.Equal(t, SourceLocal, ResolveCatalogSource(root, "stable"))
}

func TestResolveCatalogSource_FallbackOnlyWhenNoLocal(t *testing.T) {
	// Not parallel: swaps the package-level embedded bundle.
	root := catalogsRootDir(t)

	// Install a non-empty embedded bundle.
	prev := embeddedFallbackBundle
	embeddedFallbackBundle = makeCatalogBundle(t, storageManifestV1, "services: {}\n")
	t.Cleanup(func() { embeddedFallbackBundle = prev })

	assert.True(t, EmbeddedFallbackAvailable())
	// No local catalog yet -> fallback.
	assert.Equal(t, SourceEmbeddedFallback, ResolveCatalogSource(root, "stable"))

	// Once a local catalog is stored, local takes precedence over the
	// still-available embedded fallback (PRD §22).
	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV2, "services: {}\n"), nil,
	)
	require.NoError(t, err)
	assert.True(t, EmbeddedFallbackAvailable(), "fallback is still embedded")
	assert.Equal(t, SourceLocal, ResolveCatalogSource(root, "stable"),
		"verified local catalog must win over the embedded fallback")
}

func TestLocalCatalogPresent_RejectsMalformedInputs(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	assert.False(t, LocalCatalogPresent("relative", "stable"))
	assert.False(t, LocalCatalogPresent(root, "verified"))
	assert.False(t, LocalCatalogPresent(root, ""))

	// A directory at the manifest path is not a regular file.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "stable", "catalog.yaml"), 0o755))
	assert.False(t, LocalCatalogPresent(root, "stable"))
}

func TestStoreEmbeddedFallback_NoBundleReturnsTypedError(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	_, err := StoreEmbeddedFallback(context.Background(), root)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbeddedFallback)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
	assert.False(t, LocalCatalogPresent(root, "stable"))
}

func TestStoreEmbeddedFallback_SeedsLocalWhenBundlePresent(t *testing.T) {
	// Not parallel: swaps both embedded seams.
	root := catalogsRootDir(t)

	prevBundle := embeddedFallbackBundle
	embeddedFallbackBundle = makeCatalogBundle(t, storageManifestV1, "services: {}\n")
	t.Cleanup(func() { embeddedFallbackBundle = prevBundle })

	// Simulate a build whose descriptor advertises a present, versioned
	// fallback bundle.
	prevMeta := readEmbeddedFallbackMeta
	readEmbeddedFallbackMeta = func() (FallbackMetadata, error) {
		return FallbackMetadata{
			SchemaVersion: 1,
			Channel:       "stable",
			BundlePresent: true,
			Version:       "bundled-2026-05-19",
		}, nil
	}
	t.Cleanup(func() { readEmbeddedFallbackMeta = prevMeta })

	versionDir, err := StoreEmbeddedFallback(context.Background(), root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "stable", ".versions", "bundled-2026-05-19"), versionDir)
	assert.True(t, LocalCatalogPresent(root, "stable"))
}

func TestCatalogSourceString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "local", SourceLocal.String())
	assert.Equal(t, "embedded_fallback", SourceEmbeddedFallback.String())
	assert.Equal(t, "none", SourceNone.String())
	assert.Equal(t, "none", Source(99).String())
}

// errNoEmbeddedFallbackSentinelStable pins the sentinel value so an
// accidental rename surfaces in review.
func TestErrNoEmbeddedFallbackSentinel(t *testing.T) {
	t.Parallel()
	assert.True(t, errors.Is(ErrNoEmbeddedFallback, ErrNoEmbeddedFallback))
	assert.Equal(t, "catalog: no embedded fallback bundle is available", ErrNoEmbeddedFallback.Error())
}

func TestEmbeddedFallback_PropagatesMetadataReadError(t *testing.T) {
	// Not parallel: swaps the metadata seam.
	sentinel := errors.New("induced metadata read failure")
	prev := readEmbeddedFallbackMeta
	readEmbeddedFallbackMeta = func() (FallbackMetadata, error) { return FallbackMetadata{}, sentinel }
	t.Cleanup(func() { readEmbeddedFallbackMeta = prev })

	_, _, err := EmbeddedFallback()
	require.ErrorIs(t, err, sentinel)

	// StoreEmbeddedFallback surfaces the same error before any storage.
	root := catalogsRootDir(t)
	_, storeErr := StoreEmbeddedFallback(context.Background(), root)
	require.ErrorIs(t, storeErr, sentinel)
	assert.False(t, LocalCatalogPresent(root, "stable"))
}

func TestValidVersionSegment(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"2026-05-19":     true,
		"v1":             true,
		"SHA256SUMS":     true,
		"SHA256SUMS.sig": true,
		"":               false,
		".":              false,
		"..":             false,
		"a/b":            false,
		"sub/dir/leaf":   false,
		"with\x00null":   false,
		"trailing/":      false,
		"./relative":     false,
	}
	for in, want := range cases {
		assert.Equalf(t, want, validVersionSegment(in), "validVersionSegment(%q)", in)
	}
}
