// Package catalog loads and validates wdm catalog manifests
// (catalog.yaml) read from the engine-resolved XDG path
// <XDG_DATA_HOME>/wdm/catalogs/<channel>/catalog.yaml per
// PRD §22.
//   - [LoadCatalog] / [LoadCatalogBytes] — read, parse, and
//     validate a catalog.yaml against the embedded JSON Schema
//     (draft 2020-12, single source of truth at
//     catalog/schema.json).
//   - Typed shapes — [Catalog], [App], [Placeholder], [Port],
//     [ImagePin], [SupportedVersions] — mirror the schema's
//     fields one-to-one, including narrowed leaf types such as the
//     placeholder.type enum.
//   - [ErrCatalogInvalid] — sentinel returned (wrapped) on any
//     YAML parse, schema validation, or typed decode failure.
//
// Engine methods discover installed stacks via internal/state.ScanStacks.
// The loader keeps catalog parsing and validation isolated so:
//   - the schema runs through the same code path callers
//     use, avoiding late integration surprises,
//   - the test suite can validate representative
//     catalog.yaml payloads against the schema,
//   - [StoreVerifiedCatalog] — the on-disk SINK for an
//     already-verified catalog bundle. Trust verification lives in
//     internal/release; this function only stores verified bytes.
//     It extracts the gzip-tar bundle (treated as hostile at the
//     filesystem level, every member path-contained by internal/state)
//     into an immutable per-channel version snapshot under
//     catalogs/stable/.versions/<version>/, records the downloaded
//     SHA256SUMS / signature / attestation provenance beside it
//     ([ProvenanceFile]), then atomically materializes the ACTIVE
//     layout the engine reads — catalogs/stable/catalog.yaml and the
//     shared catalogs/templates/ tree. On any failure it rolls back,
//     leaving the prior active catalog byte-identical.
//
// On-disk layout:
//
//	<catalogsRoot>/
//	  stable/
//	    catalog.yaml active manifest (engine reads)
//	    .versions/<version>/ immutable verified snapshot + provenance
//	  templates/<app>/... active templates (engine reads)
//
// templates/ is a channel-root sibling, not a child of the channel
// directory, because the manifest's compose_template paths are
// catalogs-root relative (internal/core/install.go). v1 ships only the
// stable channel, so the active templates are unambiguously stable's; a
// second channel would force re-adjudicating active-template ownership
// (recorded in storage.go as the lowest-surprise v1 default).
// Out of scope for:
//   - Real catalog content — ships the first stable
//     catalog.yaml under ~/.local/share/wdm/catalogs/stable/.
//   - Signature and checksum verification — (PRD §22, §23),
//     in internal/release; this package only stores verified bytes.
//   - Engine wiring of the catalog update check/apply.
//
// Import boundary: internal/catalog may import other internal/* siblings plus the standard library
// and the narrow set of third-party libraries needed for
// validation — gopkg.in/yaml.v3 (YAML parser) and
// github.com/santhosh-tekuri/jsonschema/v6 (JSON Schema
// validator, already pulled in by internal/state for config
// validation). The storage writer also imports
// internal/state (atomic, contained filesystem writes) and
// pkg/types (the typed verification-failure error). It MUST NOT
// depend on pkg/engine, internal/tui, internal/cli, or internal/core
// (enforced by the siblings-no-orchestrator depguard rule). The engine
// layer wraps [ErrCatalogInvalid] via pkg/types.WrapError with
// pkg/types.ErrCodeVerificationFailed for exit code 3 (PRD §27); this package
// exposes that sentinel and [ErrCatalogStorage].
// Platform: catalog loading is portable (no syscalls). The
// storage writer (storage.go) depends on
// internal/state's unix flock/atomic-write surface, so it is
// build-tagged //go:build unix, matching PRD §2's Linux-amd64 target
// (Darwin is also supported for local dev builds).
package catalog
