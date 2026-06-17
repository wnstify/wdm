// Package config embeds the JSON schemas that validate user-facing
// configuration files. The bytes are embedded via //go:embed so the
// binary ships its own copy: internal/state
// consumes them during config validation, and internal/catalog
// will mirror the pattern for catalog.yaml.
// The package holds only embedded byte slices, keeping config/schema.json
// the single source of truth rather than duplicating its constraints in Go.
// It depends only on [embed], so any package in the module — including
// pkg/* — may import it without violating the depguard rules in
// .golangci.yml.
package config

import _ "embed"

// ConfigSchemaJSON is the JSON Schema (draft 2020-12) that validates
// ~/.config/wdm/config.toml after internal/state's loader parses it
// into a map. See PRD §34 for the field list and constraints.
// The bytes are the raw contents of config/schema.json. The schema's
// $id is the canonical URL
// https://raw.githubusercontent.com/wnstify/wdm/main/config/schema.json;
// callers SHOULD pass that same URL when registering the schema with a
// compiler so $id resolution stays consistent.
//
//go:embed schema.json
var ConfigSchemaJSON []byte
