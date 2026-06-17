package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/release"
)

func TestArtifactNames_PinExactLiterals(t *testing.T) {
	t.Parallel()

	// Each constant is pinned to its exact literal so any future rename
	// breaks this test rather than silently diverging from the workflow
	// or the verifier.
	assert.Equal(t, "wdm-linux-amd64", release.ArtifactBinary)
	assert.Equal(t, "catalog-stable.tar.gz", release.ArtifactCatalogBundle)
	assert.Equal(t, "SHA256SUMS", release.ArtifactChecksums)
	assert.Equal(t, "SHA256SUMS.sig", release.ArtifactChecksumSignature)
	assert.Equal(t, "SHA256SUMS.cosign.bundle", release.ArtifactCosignBundle)
	assert.Equal(t, "attestation.json", release.ArtifactAttestation)
	assert.Equal(t, "wdm-linux-amd64.spdx.json", release.ArtifactSBOM)
}

func TestCatalogBundleLayoutConstants_PinExactLiterals(t *testing.T) {
	t.Parallel()

	// The bundle mirrors the engine's catalogs-root subtree: the channel
	// directory and the shared templates directory at the archive root,
	// with the manifest one level deeper under the channel directory.
	assert.Equal(t, "stable/", release.CatalogBundleChannelDir)
	assert.Equal(t, "stable/catalog.yaml", release.CatalogBundleManifestPath)
	assert.Equal(t, "templates/", release.CatalogBundleTemplatesDir)
}

func TestAssetSet_CoversEverySevenAssetWithRole(t *testing.T) {
	t.Parallel()

	set := release.AssetSet()

	// The contract is exactly the seven settled assets, each with its role
	// Build name->role to assert membership without pinning order
	// here (ordering is asserted separately).
	roles := make(map[string]release.AssetRole, len(set))
	for _, asset := range set {
		assert.NotEmpty(t, asset.Name, "asset name must not be empty")
		_, dup := roles[asset.Name]
		assert.False(t, dup, "asset %q listed more than once", asset.Name)
		roles[asset.Name] = asset.Role
	}

	require.Len(t, roles, 7, "the release asset set must hold exactly seven assets")

	assert.Equal(t, release.RolePayload, roles[release.ArtifactBinary])
	assert.Equal(t, release.RolePayload, roles[release.ArtifactCatalogBundle])
	assert.Equal(t, release.RolePayload, roles[release.ArtifactAttestation])
	assert.Equal(t, release.RolePayload, roles[release.ArtifactSBOM])
	assert.Equal(t, release.RoleChecksumFile, roles[release.ArtifactChecksums])
	assert.Equal(t, release.RoleDetachedSignature, roles[release.ArtifactChecksumSignature])
	assert.Equal(t, release.RoleCosignBundle, roles[release.ArtifactCosignBundle])
}

func TestAssetSet_ReturnsFreshSliceEachCall(t *testing.T) {
	t.Parallel()

	// The canonical set must not be mutable through a returned slice: a
	// caller scribbling on one result cannot corrupt the contract for the
	// next caller.
	first := release.AssetSet()
	require.NotEmpty(t, first)
	first[0].Name = "tampered"
	first[0].Role = "tampered"

	second := release.AssetSet()
	assert.Equal(t, "wdm-linux-amd64", second[0].Name)
	assert.Equal(t, release.RolePayload, second[0].Role)
}

func TestChecksummedArtifactNames_AreExactlyThePayloadFiles(t *testing.T) {
	t.Parallel()

	covered := release.ChecksummedArtifactNames()

	// SHA256SUMS lists the four release payload files only.
	assert.ElementsMatch(t, []string{
		"wdm-linux-amd64",
		"catalog-stable.tar.gz",
		"attestation.json",
		"wdm-linux-amd64.spdx.json",
	}, covered)
}

func TestChecksummedArtifactNames_ExcludesChecksumFileAndSignatures(t *testing.T) {
	t.Parallel()

	// SHA256SUMS cannot list itself, and cannot list its own signatures —
	// those sign SHA256SUMS, so they are not inside it.
	covered := release.ChecksummedArtifactNames()

	assert.NotContains(t, covered, release.ArtifactChecksums)
	assert.NotContains(t, covered, release.ArtifactChecksumSignature)
	assert.NotContains(t, covered, release.ArtifactCosignBundle)
}

func TestChecksummedArtifactNames_MatchPayloadRoleInAssetSet(t *testing.T) {
	t.Parallel()

	// The coverage set must be derivable from the asset set by role so the
	// two cannot drift: every covered name is a RolePayload asset, and
	// every RolePayload asset is covered.
	covered := release.ChecksummedArtifactNames()

	var payloads []string
	for _, asset := range release.AssetSet() {
		if asset.Role == release.RolePayload {
			payloads = append(payloads, asset.Name)
		}
	}

	assert.ElementsMatch(t, payloads, covered)
}

func TestChecksummedArtifactNames_ReturnsFreshSliceEachCall(t *testing.T) {
	t.Parallel()

	first := release.ChecksummedArtifactNames()
	require.NotEmpty(t, first)
	first[0] = "tampered"

	second := release.ChecksummedArtifactNames()
	assert.NotContains(t, second, "tampered")
}

func TestExpectedCatalogBundleRootEntries_AreChannelAndTemplatesDir(t *testing.T) {
	t.Parallel()

	entries := release.ExpectedCatalogBundleRootEntries()

	// Archive-root entries mirror the catalogs-root subtree: the channel
	// directory and the shared templates directory as siblings. The
	// manifest is one level deeper at stable/catalog.yaml, not at the root.
	assert.Equal(t, []string{"stable/", "templates/"}, entries)
	assert.Equal(t, "stable/catalog.yaml", release.CatalogBundleManifestPath)
}

func TestExpectedCatalogBundleRootEntries_ReturnsFreshSliceEachCall(t *testing.T) {
	t.Parallel()

	first := release.ExpectedCatalogBundleRootEntries()
	require.NotEmpty(t, first)
	first[0] = "tampered"

	second := release.ExpectedCatalogBundleRootEntries()
	assert.Equal(t, "stable/", second[0])
}

// TestCatalogBundle_LayoutIsRealizable proves the layout contract is
// realizable as an actual gzip-compressed tar mirroring the engine's
// catalogs-root subtree: it builds a tiny catalog-stable.tar.gz in memory
// (the manifest at stable/catalog.yaml plus one app's compose template
// under the sibling templates/ directory), walks it back, reduces each
// member to its archive-root entry, and asserts the unique root entries
// match ExpectedCatalogBundleRootEntries (stable/ and templates/). This
// gives the workflow and storage writer a reference shape without committing
// a binary blob.
func TestCatalogBundle_LayoutIsRealizable(t *testing.T) {
	t.Parallel()

	bundle := buildCatalogBundle(t)

	rootEntries := walkBundleRootEntries(t, bundle)

	assert.ElementsMatch(t,
		release.ExpectedCatalogBundleRootEntries(),
		rootEntries,
		"the realizable bundle's root entries must match the layout contract",
	)
}

// buildCatalogBundle assembles a minimal but well-formed
// catalog-stable.tar.gz in memory, mirroring the engine's catalogs-root
// subtree: the channel directory header, the manifest one level under it
// (stable/catalog.yaml), the sibling templates/ directory header, and one
// nested template file under it.
func buildCatalogBundle(t *testing.T) []byte {
	t.Helper()

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	tw := tar.NewWriter(gzw)

	manifest := []byte("schema_version: 1\napps: []\n")
	template := []byte("services:\n  uptime-kuma:\n    image: louislam/uptime-kuma\n")

	writeTarDir(t, tw, release.CatalogBundleChannelDir)
	writeTarFile(t, tw, release.CatalogBundleManifestPath, manifest)
	writeTarDir(t, tw, release.CatalogBundleTemplatesDir)
	writeTarFile(t, tw, "templates/uptime-kuma/docker-compose.yml.tmpl", template)

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	return gzBuf.Bytes()
}

func writeTarFile(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
	}))
	_, err := tw.Write(body)
	require.NoError(t, err)
}

func writeTarDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     name,
		Mode:     0o755,
	}))
}

// walkBundleRootEntries unpacks a gzip-compressed tar and returns the
// distinct archive-root entry names, normalizing each member path to its
// first segment so a directory header ("templates/") and a nested file
// ("templates/app/...") collapse to one root entry "templates/".
func walkBundleRootEntries(t *testing.T, bundle []byte) []string {
	t.Helper()

	gzr, err := gzip.NewReader(bytes.NewReader(bundle))
	require.NoError(t, err)
	defer func() { require.NoError(t, gzr.Close()) }()

	tr := tar.NewReader(gzr)
	seen := map[string]struct{}{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		seen[rootEntry(hdr.Name)] = struct{}{}
	}

	roots := make([]string, 0, len(seen))
	for entry := range seen {
		roots = append(roots, entry)
	}
	return roots
}

// rootEntry reduces a tar member name to its archive-root entry: a
// top-level file keeps its name, while anything that is or lives under a
// directory collapses to "<dir>/" so it matches the trailing-slash
// directory entry in the layout contract. The trailing-slash convention is
// preserved deliberately — path.Clean would strip it and let a directory
// header ("templates/") and a nested file ("templates/app/...") disagree.
func rootEntry(name string) string {
	clean := strings.TrimPrefix(name, "./")
	first, _, nested := strings.Cut(clean, "/")
	if nested {
		return first + "/"
	}
	return clean
}
