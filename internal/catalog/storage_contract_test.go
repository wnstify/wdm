//go:build unix

package catalog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wnstify/wdm/internal/release"
)

// TestBundleLayoutMatchesReleaseContract pins the catalog storage
// writer's view of the bundle layout to internal/release's 0.3 contract
// The catalog package re-declares these names rather than
// importing internal/release in production (sibling dependency
// hygiene), so this test guards against the two drifting. The release
// constants carry trailing slashes (tar directory-entry convention);
// the catalog package uses bare path segments for filepath.Join, so the
// comparison strips the trailing slash.
func TestBundleLayoutMatchesReleaseContract(t *testing.T) {
	t.Parallel()

	assert.Equal(t, strings.TrimSuffix(release.CatalogBundleChannelDir, "/"), bundleChannelDirName,
		"channel dir name must match the release bundle contract")
	assert.Equal(t, strings.TrimSuffix(release.CatalogBundleTemplatesDir, "/"), bundleTemplatesDirName,
		"templates dir name must match the release bundle contract")
	assert.Equal(t, release.CatalogBundleManifestPath, bundleManifestRelPath,
		"bundle manifest path must match the release bundle contract")

	// The active manifest file name is the last segment of the bundle
	// manifest path, and the engine reads <channel>/catalog.yaml.
	assert.Equal(t, "catalog.yaml", activeManifestName)
	assert.True(t, strings.HasSuffix(release.CatalogBundleManifestPath, "/"+activeManifestName))

	// The expected root entries the release contract exposes must be the
	// channel dir and templates dir this writer materializes.
	roots := release.ExpectedCatalogBundleRootEntries()
	assert.Contains(t, roots, release.CatalogBundleChannelDir)
	assert.Contains(t, roots, release.CatalogBundleTemplatesDir)
}
