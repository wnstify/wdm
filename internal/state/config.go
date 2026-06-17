//go:build unix

package state

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/santhosh-tekuri/jsonschema/v6"

	cfgschema "github.com/wnstify/wdm/config"
	"github.com/wnstify/wdm/pkg/types"
)

// configSchemaID is the canonical $id of config/schema.json. We
// register and compile under this URL so the resolved schema matches
// the file's self-declared identity (the schema's first three lines
// pin this string).
const configSchemaID = "https://raw.githubusercontent.com/wnstify/wdm/main/config/schema.json"

// LoadConfig reads, parses, and validates the wdm configuration
// at path, returning a populated [*types.Settings] on success or a
// non-nil error otherwise.
// All validation failures (TOML parse, schema violation, decode into
// the typed struct) wrap [types.ErrConfigInvalid] so callers detect
// the class with [errors.Is]; cmd/wdm maps the wrapped sentinel
// onto pkg/types.ErrCodeUsageValidation for exit code 2 (PRD §27).
// A missing file is returned as a wrapped [os.ErrNotExist]
// (NOT ErrConfigInvalid), so callers can distinguish "no config"
// from "bad config" with errors.Is and fall back to defaults if
// desired.
// Validation steps follow:
//  1. Read the file from disk.
//  2. Parse TOML into a generic map preserving which keys were
//     actually present (this is what makes the schema's required
//     and additionalProperties checks meaningful).
//  3. Validate the map against config/schema.json (draft 2020-12).
//  4. Re-decode TOML into [types.Settings] via toml tags.
//
// Path expansion (~/ → $HOME) is upstream of this function — see
// layer. The path argument MUST be absolute; relative paths are
// rejected before any file I/O so callers cannot accidentally read
// from the process working directory.
func LoadConfig(ctx context.Context, path string) (*types.Settings, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.LoadConfig: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state.LoadConfig: path must be absolute, got %q", path)
	}

	// G304 is suppressed: path is engine-controlled per "On-disk
	// layout" (XDG-clean ~/.config/wdm/config.toml, expanded upstream)
	// and the absolute-path check above forecloses relative re-injection.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: engine-controlled XDG path, validated absolute
	if err != nil {
		return nil, fmt.Errorf("state.LoadConfig: reading %q: %w", path, err)
	}

	settings, err := LoadConfigBytes(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("state.LoadConfig %q: %w", path, err)
	}
	return settings, nil
}

// LoadConfigBytes parses and validates an in-memory TOML payload with
// the same semantics as [LoadConfig] but without touching disk. Used
// from unit tests and from any caller that already holds
// the bytes (e.g. an editor-driven validate-then-save flow).
// Errors wrap [types.ErrConfigInvalid] so [errors.Is] matches the
// sentinel across all failure modes; the underlying cause — a
// TOML position error from BurntSushi/toml or a santhosh-tekuri
// validation tree — remains reachable via [errors.Unwrap] /
// [errors.As] so cmd/wdm can compose multi-line hint messages.
func LoadConfigBytes(ctx context.Context, raw []byte) (*types.Settings, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.LoadConfigBytes: %w", err)
	}

	// Step 1: decode into a generic map so missing/required and
	// additionalProperties checks see what the TOML actually contained.
	// Decoding directly into *types.Settings would silently zero
	// missing fields and silently drop unknown ones.
	var rawMap map[string]any
	if _, err := toml.Decode(string(raw), &rawMap); err != nil {
		return nil, fmt.Errorf("state.LoadConfigBytes: %w: toml parse: %w", types.ErrConfigInvalid, err)
	}

	// Step 2: validate the map against the embedded schema.
	schema, err := configSchema()
	if err != nil {
		return nil, fmt.Errorf("state.LoadConfigBytes: %w: schema compile: %w", types.ErrConfigInvalid, err)
	}
	if err := schema.Validate(rawMap); err != nil {
		return nil, fmt.Errorf("state.LoadConfigBytes: %w: schema validation: %w", types.ErrConfigInvalid, err)
	}

	// Step 3: re-decode into the typed Settings. The schema has already
	// confirmed key presence, types, enums, patterns, and the const on
	// schema_version — this pass purely populates the struct via the
	// toml tags on pkg/types.Settings.
	var settings types.Settings
	if _, err := toml.Decode(string(raw), &settings); err != nil {
		return nil, fmt.Errorf("state.LoadConfigBytes: %w: toml decode: %w", types.ErrConfigInvalid, err)
	}
	return &settings, nil
}

// configSchema returns the compiled config schema, lazily compiled
// on first call. [sync.OnceValues] (Go 1.21+) caches the result and
// any compile error so the cost is paid exactly once per process,
// and so the project's forbidigo "no panic" rule is respected — a
// malformed embedded schema surfaces as a returned error rather
// than a panic at init time.
var configSchema = sync.OnceValues(compileConfigSchema)

// compileConfigSchema parses the embedded JSON Schema bytes
// ([cfgschema.ConfigSchemaJSON]) and compiles them under the
// schema's canonical $id so $ref / $id resolution remains consistent
// with what the file declares about itself.
func compileConfigSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(cfgschema.ConfigSchemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse embedded config schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(configSchemaID, doc); err != nil {
		return nil, fmt.Errorf("add embedded config schema: %w", err)
	}
	s, err := c.Compile(configSchemaID)
	if err != nil {
		return nil, fmt.Errorf("compile embedded config schema: %w", err)
	}
	return s, nil
}
