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
	assert.Equal(t, "attestation.json", release.ArtifactAttestation)
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

// TestCatalogBundle_LayoutIsRealizable proves the layout contract is
// realizable as an actual gzip-compressed tar mirroring the engine's
// catalogs-root subtree: it builds a tiny catalog-stable.tar.gz in memory
// (the manifest at stable/catalog.yaml plus one app's compose template
// under the sibling templates/ directory), walks it back, reduces each
// member to its archive-root entry, and asserts the unique root entries
// match the layout contract (stable/ and templates/). This gives the
// workflow and storage writer a reference shape without committing a binary
// blob.
func TestCatalogBundle_LayoutIsRealizable(t *testing.T) {
	t.Parallel()

	bundle := buildCatalogBundle(t)

	rootEntries := walkBundleRootEntries(t, bundle)

	assert.ElementsMatch(t,
		[]string{release.CatalogBundleChannelDir, release.CatalogBundleTemplatesDir},
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
