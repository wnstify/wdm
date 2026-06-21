//go:build unix

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// storageManifestV1 is a schema-valid manifest used as the bundle's
// stable/catalog.yaml. Two distinct bodies let rollback tests prove the
// prior active manifest survives a failed second store.
const storageManifestV1 = `
schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: uptime-kuma
    name: Uptime Kuma
    summary: Status and uptime monitoring
    description: Self-hosted monitoring tool
    template_name: uptime-kuma
    template_version: "2026-05-01"
    compose_template: templates/uptime-kuma/docker-compose.yml.tmpl
    env_template: templates/uptime-kuma/.env.tmpl
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 3001
        host: 3008
        protocol: tcp
    image_pins:
      - service: app
        image: louislam/uptime-kuma
        tag: "1.23.0"
    local_target_url_template: "http://127.0.0.1:3008/"
    pangolin_guidance:
      target_url: "http://127.0.0.1:3008"
      recommended_subdomain: status
      notes:
        - Point DNS to your reverse proxy.
    first_run_notes:
      - Open the local URL and create the admin account.
    risk_classification: [database]
`

const storageManifestV2 = `
schema_version: 1
channel: stable
generated_at: "2026-06-01T09:14:33Z"
apps:
  - app_id: uptime-kuma
    name: Uptime Kuma
    summary: Status and uptime monitoring
    description: Self-hosted monitoring tool
    template_name: uptime-kuma
    template_version: "2026-06-01"
    compose_template: templates/uptime-kuma/docker-compose.yml.tmpl
    env_template: templates/uptime-kuma/.env.tmpl
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 3001
        host: 3008
        protocol: tcp
    image_pins:
      - service: app
        image: louislam/uptime-kuma
        tag: "1.24.0"
    local_target_url_template: "http://127.0.0.1:3008/"
    pangolin_guidance:
      target_url: "http://127.0.0.1:3008"
      recommended_subdomain: status
      notes:
        - Point DNS to your reverse proxy.
    first_run_notes:
      - Open the local URL and create the admin account.
    risk_classification: [database]
`

// bundleEntry is one member of a test catalog bundle.
type bundleEntry struct {
	Name string
	Body string
	Dir  bool
}

// makeCatalogBundle builds a gzip-tar with the release-contract root
// layout (stable/ + templates/) plus the supplied manifest and a sample
// template, so the result extracts to the engine-readable shape.
func makeCatalogBundle(t *testing.T, manifest, templateBody string, extra ...bundleEntry) []byte {
	t.Helper()

	entries := []bundleEntry{
		{Name: "stable/", Dir: true},
		{Name: "stable/catalog.yaml", Body: manifest},
		{Name: "templates/", Dir: true},
		{Name: "templates/uptime-kuma/", Dir: true},
		{Name: "templates/uptime-kuma/docker-compose.yml.tmpl", Body: templateBody},
		{Name: "templates/uptime-kuma/.env.tmpl", Body: "TZ=UTC\n"},
	}
	entries = append(entries, extra...)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name}
		if e.Dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = int64(len(e.Body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.Body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func catalogsRootDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	catalogs := filepath.Join(root, "catalogs")
	require.NoError(t, os.Mkdir(catalogs, 0o755))
	return catalogs
}

func TestStoreVerifiedCatalog_ProducesEngineReadableLayout(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	bundle := makeCatalogBundle(t, storageManifestV1, "services:\n  app: {}\n")
	provenance := []ProvenanceFile{
		{Name: "SHA256SUMS", Data: []byte("deadbeef  catalog-stable.tar.gz\n")},
		{Name: "SHA256SUMS.sig", Data: []byte("sig-bytes")},
		{Name: "attestation.json", Data: []byte(`{"_type":"x"}`)},
	}

	versionDir, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "2026-05-19", bundle, provenance,
	)
	require.NoError(t, err)

	// Active manifest at the exact engine-read location.
	activeManifest := filepath.Join(root, "stable", "catalog.yaml")
	got, err := os.ReadFile(activeManifest)
	require.NoError(t, err)
	assert.Equal(t, storageManifestV1, string(got))

	mi, err := os.Stat(activeManifest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), mi.Mode().Perm())

	// Active templates at the catalogs-root sibling location the engine reads.
	tmpl, err := os.ReadFile(filepath.Join(root, "templates", "uptime-kuma", "docker-compose.yml.tmpl"))
	require.NoError(t, err)
	assert.Equal(t, "services:\n  app: {}\n", string(tmpl))

	// Version snapshot under stable/.versions/<version>/ with provenance.
	assert.Equal(t, filepath.Join(root, "stable", ".versions", "2026-05-19"), versionDir)
	snapManifest, err := os.ReadFile(filepath.Join(versionDir, "stable", "catalog.yaml"))
	require.NoError(t, err)
	assert.Equal(t, storageManifestV1, string(snapManifest))
	for _, p := range provenance {
		data, readErr := os.ReadFile(filepath.Join(versionDir, p.Name))
		require.NoError(t, readErr)
		assert.Equal(t, p.Data, data)
	}

	// The active manifest re-parses through the loader (engine path).
	cat, err := LoadCatalogBytes(context.Background(), got)
	require.NoError(t, err)
	require.Len(t, cat.Apps, 1)
	assert.Equal(t, "uptime-kuma", cat.Apps[0].AppID)

	// No staging dir leaked.
	entries, err := os.ReadDir(filepath.Join(root, "stable", ".versions"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "2026-05-19", entries[0].Name())
}

func TestStoreVerifiedCatalog_SecondVersionReplacesActiveAndKeepsBothSnapshots(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)

	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services:\n  app: {image: a}\n"), nil,
	)
	require.NoError(t, err)

	_, err = StoreVerifiedCatalog(
		context.Background(), root, "stable", "v2",
		makeCatalogBundle(t, storageManifestV2, "services:\n  app: {image: b}\n"), nil,
	)
	require.NoError(t, err)

	active, err := os.ReadFile(filepath.Join(root, "stable", "catalog.yaml"))
	require.NoError(t, err)
	assert.Equal(t, storageManifestV2, string(active))

	tmpl, err := os.ReadFile(filepath.Join(root, "templates", "uptime-kuma", "docker-compose.yml.tmpl"))
	require.NoError(t, err)
	assert.Equal(t, "services:\n  app: {image: b}\n", string(tmpl))

	// Both immutable snapshots survive.
	for _, v := range []string{"v1", "v2"} {
		_, statErr := os.Stat(filepath.Join(root, "stable", ".versions", v, "stable", "catalog.yaml"))
		assert.NoError(t, statErr, "snapshot %s must survive", v)
	}

	// No stash dir leaked next to the active templates.
	siblings, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, s := range siblings {
		assert.NotContains(t, s.Name(), ".prev-", "no leftover templates stash")
	}
}

func TestStoreVerifiedCatalog_FailedActivationRollsBackToPriorState(t *testing.T) {
	// Not parallel: swaps the package-level commit-point seam.
	root := catalogsRootDir(t)

	// First store establishes the prior active state.
	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services:\n  app: {image: a}\n"), nil,
	)
	require.NoError(t, err)

	// Inject a commit-point failure for the second store.
	sentinel := errors.New("induced manifest write failure")
	prev := storageWriteManifest
	storageWriteManifest = func(string, []byte, os.FileMode) error { return sentinel }
	t.Cleanup(func() { storageWriteManifest = prev })

	_, err = StoreVerifiedCatalog(
		context.Background(), root, "stable", "v2",
		makeCatalogBundle(t, storageManifestV2, "services:\n  app: {image: b}\n"), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.ErrorIs(t, err, ErrCatalogStorage)

	// Prior active manifest is byte-identical (engine still reads v1).
	active, readErr := os.ReadFile(filepath.Join(root, "stable", "catalog.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, storageManifestV1, string(active))

	// Prior active templates restored to the v1 image line.
	tmpl, readErr := os.ReadFile(filepath.Join(root, "templates", "uptime-kuma", "docker-compose.yml.tmpl"))
	require.NoError(t, readErr)
	assert.Equal(t, "services:\n  app: {image: a}\n", string(tmpl))

	// The failed v2 snapshot was rolled back (removed).
	_, statErr := os.Lstat(filepath.Join(root, "stable", ".versions", "v2"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "failed v2 snapshot must be removed")

	// No leftover stash dir.
	siblings, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	for _, s := range siblings {
		assert.NotContains(t, s.Name(), ".prev-", "no leftover templates stash after rollback")
	}
}

func TestStoreVerifiedCatalog_FirstStoreFailureLeavesNoActiveCatalog(t *testing.T) {
	root := catalogsRootDir(t)

	sentinel := errors.New("induced manifest write failure")
	prev := storageWriteManifest
	storageWriteManifest = func(string, []byte, os.FileMode) error { return sentinel }
	t.Cleanup(func() { storageWriteManifest = prev })

	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)

	// No active manifest, no active templates, no leftover snapshot/stash.
	_, manifestErr := os.Lstat(filepath.Join(root, "stable", activeManifestName))
	assert.True(t, errors.Is(manifestErr, os.ErrNotExist))
	_, statErr := os.Lstat(filepath.Join(root, "templates"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
	_, statErr = os.Lstat(filepath.Join(root, "stable", ".versions", "v1"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestStoreVerifiedCatalog_RejectsHostileBundleMemberWithoutTouchingActive(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	// First store a good catalog.
	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services:\n  app: {image: a}\n"), nil,
	)
	require.NoError(t, err)

	// Build a bundle carrying a traversal member.
	hostile := makeCatalogBundle(t, storageManifestV2, "services: {}\n",
		bundleEntry{Name: "../escape.yaml", Body: "pwned"})

	_, err = StoreVerifiedCatalog(context.Background(), root, "stable", "v2", hostile, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
	assert.ErrorIs(t, err, security.ErrPathEscape)

	// Active state still v1; no escape file written next to catalogs root.
	active, readErr := os.ReadFile(filepath.Join(root, "stable", "catalog.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, storageManifestV1, string(active))
	_, statErr := os.Lstat(filepath.Join(filepath.Dir(root), "escape.yaml"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
	_, statErr = os.Lstat(filepath.Join(root, "stable", ".versions", "v2"))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestStoreVerifiedCatalog_RejectsExistingVersionSnapshot(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	bundle := makeCatalogBundle(t, storageManifestV1, "services: {}\n")

	_, err := StoreVerifiedCatalog(context.Background(), root, "stable", "v1", bundle, nil)
	require.NoError(t, err)

	_, err = StoreVerifiedCatalog(context.Background(), root, "stable", "v1", bundle, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
	assert.ErrorContains(t, err, "already")
}

func TestStoreVerifiedCatalog_RejectsInvalidManifestInBundle(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	bad := makeCatalogBundle(t, "not: [valid: catalog", "services: {}\n")

	_, err := StoreVerifiedCatalog(context.Background(), root, "stable", "v1", bad, nil)
	require.Error(t, err)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
	assert.ErrorIs(t, err, ErrCatalogInvalid)

	// Nothing became active.
	_, statErr := os.Lstat(filepath.Join(root, "stable", activeManifestName))
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestStoreVerifiedCatalog_RejectsBundleMissingRootEntries(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)

	// A bundle with only the manifest, no templates/ dir.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := storageManifestV1
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "stable/catalog.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	_, err = StoreVerifiedCatalog(context.Background(), root, "stable", "v1", buf.Bytes(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCatalogStorage)
	assert.ErrorContains(t, err, "templates")
}

func TestStoreVerifiedCatalog_RejectsBadInputs(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	bundle := makeCatalogBundle(t, storageManifestV1, "services: {}\n")

	cases := []struct {
		name              string
		catalogsRoot      string
		channel           string
		version           string
		bundle            []byte
		wantErrContains   string
		wantVerifyTypedID types.ErrorCode
	}{
		{"relative root", "relative/catalogs", "stable", "v1", bundle, "absolute", types.ErrCodeVerificationFailed},
		{"non-stable channel", root, "verified", "v1", bundle, "channel", types.ErrCodeVerificationFailed},
		{"empty version", root, "stable", "", bundle, "version", types.ErrCodeVerificationFailed},
		{"traversal version", root, "stable", "../evil", bundle, "version", types.ErrCodeVerificationFailed},
		{"slash version", root, "stable", "a/b", bundle, "version", types.ErrCodeVerificationFailed},
		{"empty bundle", root, "stable", "v1", nil, "empty", types.ErrCodeVerificationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := StoreVerifiedCatalog(context.Background(), tc.catalogsRoot, tc.channel, tc.version, tc.bundle, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCatalogStorage)
			assert.ErrorContains(t, err, tc.wantErrContains)
			var typed *types.Error
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, tc.wantVerifyTypedID, typed.Code)
		})
	}
}

func TestStoreVerifiedCatalog_RejectsHostileProvenanceNames(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	bundle := makeCatalogBundle(t, storageManifestV1, "services: {}\n")

	cases := []struct {
		name        string
		provenance  []ProvenanceFile
		errContains string
	}{
		{"traversal name", []ProvenanceFile{{Name: "../evil", Data: []byte("x")}}, "invalid"},
		{"slash name", []ProvenanceFile{{Name: "a/b", Data: []byte("x")}}, "invalid"},
		{"collides with manifest", []ProvenanceFile{{Name: "catalog.yaml", Data: []byte("x")}}, "collides"},
		{"collides with stable dir", []ProvenanceFile{{Name: "stable", Data: []byte("x")}}, "collides"},
		{"duplicate name", []ProvenanceFile{
			{Name: "SHA256SUMS", Data: []byte("x")},
			{Name: "SHA256SUMS", Data: []byte("y")},
		}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := StoreVerifiedCatalog(context.Background(), root, "stable", "v-"+tc.name, bundle, tc.provenance)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCatalogStorage)
			assert.ErrorContains(t, err, tc.errContains)
		})
	}
}

func TestStoreVerifiedCatalog_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	root := catalogsRootDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := StoreVerifiedCatalog(
		ctx, root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestStoreVerifiedCatalog_FilesystemFaultMapsToGeneric pins the FS-fault
// arm of the exit-code split: a pure local I/O
// fault at the commit-point manifest write is a generic error (exit 1),
// NOT a verification failure (exit 3) — a disk-full or permission error
// must not falsely imply a trust problem. ErrCatalogStorage stays
// reachable via errors.Is on this arm.
func TestStoreVerifiedCatalog_FilesystemFaultMapsToGeneric(t *testing.T) {
	// Not parallel: swaps the package-level commit-point seam.
	root := catalogsRootDir(t)

	sentinel := errors.New("induced disk full")
	prev := storageWriteManifest
	storageWriteManifest = func(string, []byte, os.FileMode) error { return sentinel }
	t.Cleanup(func() { storageWriteManifest = prev })

	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
	)
	require.Error(t, err)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeGeneric, typed.Code,
		"a filesystem write fault is generic (exit 1), not verification (exit 3)")
	assert.ErrorIs(t, err, ErrCatalogStorage, "the storage sentinel stays reachable on the FS-fault arm")
	assert.ErrorIs(t, err, sentinel)
}

// TestStoreVerifiedCatalog_ExitCodeSplit pins BOTH arms side by side: a
// genuine contract/verification fault (an invalid manifest in the bundle)
// stays exit 3, while a pure FS write fault is exit 1.
func TestStoreVerifiedCatalog_ExitCodeSplit(t *testing.T) {
	// Not parallel: the FS-fault subtest swaps the commit-point seam.
	t.Run("contract fault stays verification (exit 3)", func(t *testing.T) {
		root := catalogsRootDir(t)
		bad := makeCatalogBundle(t, "not: [valid: catalog", "services: {}\n")
		_, err := StoreVerifiedCatalog(context.Background(), root, "stable", "v1", bad, nil)
		require.Error(t, err)
		var typed *types.Error
		require.ErrorAs(t, err, &typed)
		assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
		// An invalid bundle manifest is a contract fault; it surfaces via
		// ErrCatalogInvalid (the loader sentinel), still at exit 3.
		assert.ErrorIs(t, err, ErrCatalogInvalid)
	})

	t.Run("missing-root-entry contract fault stays verification (exit 3)", func(t *testing.T) {
		// A structural contract fault that flows through storageError
		// (ErrCatalogStorage), distinct from the loader-sentinel manifest
		// case above, both at exit 3.
		root := catalogsRootDir(t)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := storageManifestV1
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: "stable/catalog.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}))
		_, wErr := tw.Write([]byte(body))
		require.NoError(t, wErr)
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())

		_, err := StoreVerifiedCatalog(context.Background(), root, "stable", "v1", buf.Bytes(), nil)
		require.Error(t, err)
		var typed *types.Error
		require.ErrorAs(t, err, &typed)
		assert.Equal(t, types.ErrCodeVerificationFailed, typed.Code)
		assert.ErrorIs(t, err, ErrCatalogStorage)
	})

	t.Run("mkdir fault is generic (exit 1)", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission checks")
		}
		// A catalogs root that is owner-non-writable makes ensureDir's
		// MkdirAll of.versions fail with a permission error: a pure FS
		// fault, exit 1.
		root := catalogsRootDir(t)
		require.NoError(t, os.Chmod(root, 0o500))
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		_, err := StoreVerifiedCatalog(
			context.Background(), root, "stable", "v1",
			makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
		)
		require.Error(t, err)
		var typed *types.Error
		require.ErrorAs(t, err, &typed)
		assert.Equal(t, types.ErrCodeGeneric, typed.Code,
			"a mkdir permission fault is generic (exit 1), got %v", err)
		assert.ErrorIs(t, err, ErrCatalogStorage)
	})
}

func TestStoreVerifiedCatalog_StagingSuffixSeamKeepsDeterminism(t *testing.T) {
	// Not parallel: swaps the package-level clock seam.
	root := catalogsRootDir(t)

	calls := 0
	prev := storageNowUTC
	storageNowUTC = func() time.Time { calls++; return prev() }
	t.Cleanup(func() { storageNowUTC = prev })

	_, err := StoreVerifiedCatalog(
		context.Background(), root, "stable", "v1",
		makeCatalogBundle(t, storageManifestV1, "services: {}\n"), nil,
	)
	require.NoError(t, err)
	assert.Positive(t, calls, "the clock seam must be consulted for the staging suffix")
}
