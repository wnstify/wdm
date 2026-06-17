package catalog_test

import (
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/catalog"
)

// fuzzCatalogSeeds collects the YAML payloads handed to the loader as
// the starting corpus. The first entry is the minimal manifest that
// satisfies every required field; the remainder are malformed,
// hostile, or boundary inputs the loader must reject without panicking.
var fuzzCatalogSeeds = []string{
	validCatalogYAML,
	"",
	"\x00",
	"schema_version: 1",
	"not: yaml: : :",
	"[1, 2, 3]",
	"%YAML 1.1\n---\n",
	"&anchor *anchor",
	"schema_version: 1\nchannel: stable\ngenerated_at: not-a-date\napps: []\n",
	"schema_version: 999\nchannel: bogus\ngenerated_at: \"2026-05-19T09:14:33Z\"\napps: []\n",
	"apps:\n  - app_id: \"\"\n",
	"\xff\xfe\x00bad utf16 marker",
}

// FuzzLoadCatalogBytes drives the real byte-level catalog entry point
// [catalog.LoadCatalogBytes] — the YAML parse, JSON-Schema validation,
// and typed re-decode pipeline — against arbitrary input. It enforces
// three invariants that hold for every input:
//   - The loader never panics (the property a fuzz target exists to
//     prove for a parser fed untrusted bytes).
//   - On failure, the error wraps [catalog.ErrCatalogInvalid] OR a
//     context error; it is never a bare nil-with-nil-catalog. The
//     loader's documented contract is that every YAML/schema/decode
//     failure is detectable via errors.Is(err, ErrCatalogInvalid), so
//     a failure that bypasses the sentinel is a contract break the
//     engine's exit-code-3 mapping would silently miss.
//   - On success, the returned catalog is non-nil and every app it
//     reports carries a non-empty AppID — the schema's app_id pattern
//     (lowercase ASCII, length 1-63) forecloses an empty identifier,
//     so an accepted-yet-empty AppID would mean validation and decode
//     disagreed about which keys were present.
func FuzzLoadCatalogBytes(f *testing.F) {
	for _, seed := range fuzzCatalogSeeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		cat, err := catalog.LoadCatalogBytes(t.Context(), raw)

		if err != nil {
			if cat != nil {
				t.Fatalf("LoadCatalogBytes returned both a catalog and an error: %v", err)
			}
			if !errors.Is(err, catalog.ErrCatalogInvalid) && t.Context().Err() == nil {
				t.Fatalf("rejection error does not wrap ErrCatalogInvalid: %v", err)
			}
			return
		}

		if cat == nil {
			t.Fatal("LoadCatalogBytes returned nil catalog with nil error")
		}
		for i, app := range cat.Apps {
			if app.AppID == "" {
				t.Fatalf("accepted catalog has app %d with empty AppID", i)
			}
		}
	})
}
