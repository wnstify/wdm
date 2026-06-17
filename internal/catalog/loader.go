package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	catalogschema "github.com/wnstify/wdm/catalog"
)

// ErrCatalogInvalid is returned (wrapped) by [LoadCatalog] and
// [LoadCatalogBytes] when the catalog.yaml fails YAML parse, JSON
// Schema validation, or typed decode. Detect with [errors.Is].
// Engine.List does NOT consult the catalog. Catalog reads are wired into the engine
// (install/update flows), where the engine layer wraps this via
// pkg/types.WrapError with pkg/types.ErrCodeVerificationFailed for
// exit code 3 (PRD §27 — "release, catalog, or signature
// verification failure").
// The sentinel lives in internal/catalog rather than pkg/types
// because no public surface needs to detect it across the
// internal-public boundary; the engine forwards catalog errors as
// the typed code above.
var ErrCatalogInvalid = errors.New("catalog: invalid")

// catalogSchemaID is the canonical $id of catalog/schema.json. The
// loader registers and compiles the embedded schema under this URL so
// its self-declared identity matches the file's $id, keeping $ref
// resolution consistent between the embedded copy and any future
// externally fetched one.
const catalogSchemaID = "https://raw.githubusercontent.com/wnstify/wdm/main/catalog/schema.json"

// LoadCatalog reads, parses, and validates a catalog.yaml file at
// path, returning a populated [*Catalog] or an error.
// path must be the engine-resolved XDG location
// <XDG_DATA_HOME>/wdm/catalogs/<channel>/catalog.yaml per
// resolved and absolute. The engine expands ~/ → $HOME upstream.
// Validation failures (YAML parse, schema validation, typed
// decode) wrap [ErrCatalogInvalid] so callers detect the class
// with [errors.Is]. Missing files return a wrapped
// [os.ErrNotExist] — NOT ErrCatalogInvalid — so the engine layer
// can tell "no catalog yet" from "broken catalog".
// forward-compat only.
// Validation steps mirror internal/state's config loader
//  1. Read the file from disk.
//  2. Parse YAML into a generic map that records which keys were
//     present, so the schema's required and
//     additionalProperties:false checks are meaningful.
//  3. Validate the map against catalog/schema.json (draft
//     2020-12).
//  4. Re-decode YAML into [*Catalog] via yaml tags.
//
// path MUST be absolute; relative paths are rejected before any
// file I/O so callers cannot read from the process working
// directory.
func LoadCatalog(ctx context.Context, path string) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalog: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("catalog.LoadCatalog: path must be absolute, got %q", path)
	}

	// G304 is suppressed: path is engine-controlled per
	// "On-disk layout" (<XDG_DATA_HOME>/wdm/catalogs/<channel>/
	// catalog.yaml, expanded upstream) and the absolute-path check
	// above forecloses relative re-injection. Same engine-XDG-path
	// rationale as runtime_lock.go, config.go,
	// and lock.go; a centralized helper in
	raw, err := os.ReadFile(path) //nolint:gosec // G304: engine-controlled XDG path, validated absolute
	if err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalog: reading %q: %w", path, err)
	}

	cat, err := LoadCatalogBytes(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalog %q: %w", path, err)
	}
	return cat, nil
}

// LoadCatalogBytes parses and validates an in-memory YAML payload
// with the same semantics as [LoadCatalog] but without touching
// disk. Used by unit tests and any caller that already
// has the bytes.
// Errors wrap [ErrCatalogInvalid] via multi-%w so [errors.Is]
// matches the sentinel across all failure modes; the underlying
// cause (a *yaml.TypeError or a santhosh-tekuri
// *jsonschema.ValidationError) stays reachable via
// [errors.Unwrap] / [errors.As] so cmd/wdm can compose
// multi-line hint messages from the engine-forwarded error.
func LoadCatalogBytes(ctx context.Context, raw []byte) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w", err)
	}

	// Step 1: decode into a generic value, then normalize through
	// JSON to strip non-JSON types like time.Time (yaml.v3
	// auto-decodes RFC 3339 scalars to time.Time in interface{}
	// mode, which santhosh-tekuri/jsonschema rejects with "invalid
	// jsonType time.Time"). Decoding into a generic map first
	// preserves key presence; decoding straight into *Catalog would
	// zero missing fields and drop unknown ones, defeating the
	// schema's required + additionalProperties:false checks.
	// json.Marshal(time.Time) emits an RFC 3339 string, so the
	// schema's format:"date-time" check on generated_at sees the
	// value it expects.
	var intermediate any
	if err := yaml.Unmarshal(raw, &intermediate); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: yaml parse: %w", ErrCatalogInvalid, err)
	}
	jsonBytes, err := json.Marshal(intermediate)
	if err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: yaml->json normalize: %w", ErrCatalogInvalid, err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(jsonBytes, &rawMap); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: json normalize: %w", ErrCatalogInvalid, err)
	}

	// Step 2: validate the map against the embedded schema.
	schema, err := catalogSchema()
	if err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: schema compile: %w", ErrCatalogInvalid, err)
	}
	if err := schema.Validate(rawMap); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: schema validation: %w", ErrCatalogInvalid, err)
	}

	// Step 3: re-decode into the typed Catalog. The schema has
	// already confirmed key presence, types, enums, patterns, the
	// const on schema_version, and the date-time format on
	// generated_at, so this pass only populates the struct via the
	// yaml tags. yaml.v3 parses the generated_at string into
	// time.Time here based on the field type.
	var cat Catalog
	if err := yaml.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("catalog.LoadCatalogBytes: %w: yaml decode: %w", ErrCatalogInvalid, err)
	}
	return &cat, nil
}

// catalogSchema returns the compiled catalog schema, compiled
// lazily on first call. [sync.OnceValues] (Go 1.21+) caches the
// result and any compile error so the cost is paid once per
// process, and so the project's forbidigo "no panic" rule holds —
// a malformed embedded schema surfaces as a returned error, not a
// panic at package init.
var catalogSchema = sync.OnceValues(compileCatalogSchema)

// compileCatalogSchema parses the embedded JSON Schema bytes
// ([catalogschema.CatalogSchemaJSON]) and compiles them under the
// schema's canonical $id so $ref / $id resolution stays consistent
// with the file's own declarations.
func compileCatalogSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(catalogschema.CatalogSchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse embedded catalog schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(catalogSchemaID, doc); err != nil {
		return nil, fmt.Errorf("add embedded catalog schema: %w", err)
	}
	s, err := c.Compile(catalogSchemaID)
	if err != nil {
		return nil, fmt.Errorf("compile embedded catalog schema: %w", err)
	}
	return s, nil
}
