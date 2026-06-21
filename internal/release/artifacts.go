package release

// This file locks the release artifact naming and catalog-bundle layout
// contract (, settled 2026-06-13; the invariant: trust substrate
// first). It is the single source of truth for the file names the release
// workflow produces and the product verifier / apply paths
// consume.
// It is naming/layout data with small pure helpers — no checksum,
// signature, attestation, crypto, network, or filesystem behavior. The
// 0.2 verifier primitives stay byte/digest-oriented and filename-agnostic
// (they operate on supplied bytes, digests, and keys, not on locating
// artifacts by filename); this is the naming layer beside them, never
// inside them (PRD §22, §23).

// Release artifact file names. These are the exact GitHub release asset
// names the workflow uploads and the verifier resolves; renaming any of
// them breaks the contract, which the artifacts tests catch.
const (
	// ArtifactBinary is the linux/amd64 wdm binary.
	ArtifactBinary = "wdm-linux-amd64"

	// ArtifactCatalogBundle is the stable-channel catalog bundle: a
	// gzip-compressed tar mirroring the engine's catalogs-root subtree
	// (the channel directory stable/ plus the shared templates/ tree), so
	// extracting it into the verified catalogs root reproduces exactly what
	// the catalog filesystem reads. See the catalog-bundle layout block
	// below.
	ArtifactCatalogBundle = "catalog-stable.tar.gz"

	// ArtifactChecksums is the GNU-coreutils SHA256SUMS file: one
	// "<sha256-hex> <name>" line per release payload file. It is the
	// integrity anchor every payload artifact is checked against.
	ArtifactChecksums = "SHA256SUMS"

	// ArtifactChecksumSignature is the product-verifiable detached
	// signature over ArtifactChecksums: a raw 64-byte Ed25519 signature
	// over the exact SHA256SUMS bytes.
	ArtifactChecksumSignature = "SHA256SUMS.sig"

	// ArtifactAttestation is the SLSA provenance attestation
	// (in-toto/DSSE), verified in-product.
	ArtifactAttestation = "attestation.json"
)

// Catalog-bundle layout. The bundle (ArtifactCatalogBundle) is a
// gzip-compressed tar mirroring the engine's catalogs-root subtree, so the
// release workflow tars that subtree directly and the apply path extracts it
// directly into the verified catalogs root, reproducing
// the layout the catalog filesystem reads.
// The engine's catalog filesystem is rooted at <dataDir>/catalogs/
// (internal/core/install.go: installCatalogFS = os.DirFS over that path).
// It reads the channel manifest at <channel>/catalog.yaml (so
// stable/catalog.yaml) and reads templates by each manifest entry's
// compose_template = "templates/<app>/..." path, relative to the catalogs
// root — making templates/ a SIBLING of the channel directory, not a child
// of it. tests/e2e/install_uptime_kuma_test.go's seedStableCatalog is the
// authoritative on-disk reference: it seeds
// <dataDir>/catalogs/stable/catalog.yaml plus
// <dataDir>/catalogs/templates/<app>/*.tmpl.
// The bundle's archive root therefore holds the channel directory and the
// shared templates directory as siblings, with the manifest one level
// deeper at stable/catalog.yaml (stable is the only v1
// channel).
const (
	// CatalogBundleChannelDir is the channel directory at the bundle root.
	// The trailing slash marks a directory entry, matching the tar header
	// name convention.
	CatalogBundleChannelDir = "stable/"

	// CatalogBundleManifestPath is the channel manifest one level under the
	// channel directory, the path the catalog filesystem reads as
	// <channel>/catalog.yaml.
	CatalogBundleManifestPath = "stable/catalog.yaml"

	// CatalogBundleTemplatesDir is the shared templates directory at the
	// bundle root, a sibling of the channel directory (the catalogs-root
	// level the manifest's compose_template paths are relative to). The
	// trailing slash marks a directory entry.
	CatalogBundleTemplatesDir = "templates/"
)
