// Package catalog embeds the JSON Schema that validates wdm catalog
// manifests at load time. The bytes are embedded via //go:embed so the
// binary ships its own copy; internal/catalog uses
// them during catalog.yaml validation, mirroring config/schema.go
// catalog/schema.json stays the single source of truth; the package adds
// no Go-side logic.
// The package imports only [embed], so any package in the module —
// including pkg/* — can consume it without violating the depguard rules
// in .golangci.yml.
package catalog

import _ "embed"

// CatalogSchemaJSON is the JSON Schema (draft 2020-12) that validates
// ~/.local/share/wdm/catalogs/<channel>/catalog.yaml after internal/catalog's
// loader parses the YAML into a map. See PRD §22 and "Config &
// catalog schemas" for the field list and constraints.
// The schema accepts schema_version 1 or 2; version 2 adds the optional
// declaration fields (public-port intent, the container-privilege
// allow-list, socket-proxy, config generation, and network IPAM), each
// additive so a version-1 manifest still validates.
// The bytes are the raw contents of catalog/schema.json. Its $id is
// https://raw.githubusercontent.com/wnstify/wdm/main/catalog/schema.json;
// callers SHOULD register the schema under that URL so $id and $ref
// resolution matches the file.
//
//go:embed schema.json
var CatalogSchemaJSON []byte
