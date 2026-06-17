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

	// ArtifactCosignBundle is the keyless cosign bundle (signature, Fulcio
	// cert, Rekor entry) over ArtifactChecksums, for human/CI
	// `cosign verify-blob`.
	ArtifactCosignBundle = "SHA256SUMS.cosign.bundle"

	// ArtifactAttestation is the SLSA provenance attestation
	// (in-toto/DSSE), verified in-product.
	ArtifactAttestation = "attestation.json"

	// ArtifactSBOM is the SPDX 2.3 JSON SBOM for the binary.
	ArtifactSBOM = "wdm-linux-amd64.spdx.json"
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

// AssetRole names the role a release asset plays in the trust contract.
// Roles are data, not prose:
// the workflow producing assets and the verifier consuming them agree on
// roles through these values rather than filename heuristics.
type AssetRole string

// The closed set of release asset roles.
const (
	// RolePayload is a release payload file whose digest appears in
	// SHA256SUMS (binary, catalog bundle, attestation, SBOM).
	RolePayload AssetRole = "payload"

	// RoleChecksumFile is the SHA256SUMS file itself: it lists the payload
	// digests and is therefore never listed in itself.
	RoleChecksumFile AssetRole = "checksum_file"

	// RoleDetachedSignature is the product-verifiable detached signature
	// over SHA256SUMS.
	RoleDetachedSignature AssetRole = "detached_signature"

	// RoleCosignBundle is the keyless cosign bundle over SHA256SUMS, for
	// human/CI verification.
	RoleCosignBundle AssetRole = "cosign_bundle"
)

// Asset is one named release asset and its trust role. It is data only:
// the workflow emits exactly this set of names and the verifier resolves
// against them.
type Asset struct {
	// Name is the release asset file name (one of the Artifact* constants).
	Name string

	// Role is the part the asset plays in the trust contract.
	Role AssetRole
}

// AssetSet returns the full, ordered release asset set the workflow
// produces and the verifier consumes. A fresh slice is returned
// on every call so callers cannot mutate the canonical set.
// The ordering — payload files, then the checksum file, then the
// signatures over it — mirrors the producer/consumer flow: payloads are
// hashed into SHA256SUMS, then SHA256SUMS is signed.
func AssetSet() []Asset {
	return []Asset{
		{Name: ArtifactBinary, Role: RolePayload},
		{Name: ArtifactCatalogBundle, Role: RolePayload},
		{Name: ArtifactAttestation, Role: RolePayload},
		{Name: ArtifactSBOM, Role: RolePayload},
		{Name: ArtifactChecksums, Role: RoleChecksumFile},
		{Name: ArtifactChecksumSignature, Role: RoleDetachedSignature},
		{Name: ArtifactCosignBundle, Role: RoleCosignBundle},
	}
}

// ChecksummedArtifactNames returns the asset names whose digests must
// appear in SHA256SUMS: the release payload files only (binary, catalog
// bundle, attestation, SBOM).
// SHA256SUMS lists neither itself nor its own signatures (SHA256SUMS.sig,
// SHA256SUMS.cosign.bundle): those sign SHA256SUMS, so they cannot be
// inside it. The set is derived from [AssetSet] by role ([RolePayload]) so
// the coverage rule cannot drift from the asset set. A fresh slice is
// returned on every call.
func ChecksummedArtifactNames() []string {
	var names []string
	for _, asset := range AssetSet() {
		if asset.Role == RolePayload {
			names = append(names, asset.Name)
		}
	}
	return names
}

// ExpectedCatalogBundleRootEntries returns the required archive-root entry
// names inside ArtifactCatalogBundle: the channel directory (stable/) and
// the shared templates directory (templates/), mirroring the engine's
// catalogs-root subtree so extraction reproduces the catalog filesystem
// layout. The channel manifest lives one level deeper at
// CatalogBundleManifestPath (stable/catalog.yaml), not at the archive root.
// This is the layout *contract*, not a parser. A fresh slice is returned on
// every call.
func ExpectedCatalogBundleRootEntries() []string {
	return []string{CatalogBundleChannelDir, CatalogBundleTemplatesDir}
}
