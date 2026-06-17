//go:build unix

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadBundleManifest_ParsesGeneratedAt(t *testing.T) {
	t.Parallel()

	bundle := makeCatalogBundle(t, storageManifestV1, "services: {}\n")
	cat, err := ReadBundleManifest(context.Background(), bundle)
	require.NoError(t, err)
	require.NotNil(t, cat)

	assert.Equal(t, "2026-05-19T09:14:33Z", cat.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	require.Len(t, cat.Apps, 1)
	assert.Equal(t, "uptime-kuma", cat.Apps[0].AppID)
}

func TestReadBundleManifest_EmptyBundle(t *testing.T) {
	t.Parallel()

	_, err := ReadBundleManifest(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
}

func TestReadBundleManifest_MissingManifest(t *testing.T) {
	t.Parallel()

	// A bundle with templates but no stable/catalog.yaml.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "templates/", Typeflag: tar.TypeDir, Mode: 0o755}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	_, err := ReadBundleManifest(context.Background(), buf.Bytes())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
	assert.ErrorContains(t, err, "missing")
}

func TestReadBundleManifest_InvalidManifestSurfacesCatalogInvalid(t *testing.T) {
	t.Parallel()

	bundle := makeCatalogBundle(t, "not: [valid: catalog", "services: {}\n")
	_, err := ReadBundleManifest(context.Background(), bundle)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogInvalid, "loader rejection must surface ErrCatalogInvalid")
}

func TestReadBundleManifest_NotGzip(t *testing.T) {
	t.Parallel()

	_, err := ReadBundleManifest(context.Background(), []byte("this is not a gzip stream at all"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
}

func TestReadBundleManifest_CanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadBundleManifest(ctx, makeCatalogBundle(t, storageManifestV1, "services: {}\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
