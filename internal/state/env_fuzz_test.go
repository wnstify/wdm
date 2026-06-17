//go:build unix

package state_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/wnstify/wdm/internal/state"
)

// fuzzEnvSeeds is the starting corpus for the .env parser. It mixes the
// well-formed shapes the parser must accept (values containing "=",
// empty values, comments, blank lines, surrounding whitespace) with the
// malformed shapes it must reject (missing "=", empty key, duplicate
// key) and a few hostile byte sequences.
var fuzzEnvSeeds = []string{
	"KEY=value\n",
	"KEY=value",
	"A=1\nB=2\nC=3\n",
	"URL=postgres://user:pass@host:5432/db?sslmode=require\n",
	"EMPTY=\n",
	"  PADDED  =  spaced value  \n",
	"# a comment\n   # indented comment\nKEY=value\n",
	"\n\n\nKEY=value\n\n",
	"no_equals_here\n",
	"=novalue\n",
	"DUP=1\nDUP=2\n",
	"KEY=line with = signs = everywhere\n",
	"KEY=\x00binary\xffvalue\n",
	"",
}

// FuzzReadStackEnv drives the real file-based parser
// [state.ReadStackEnv] by writing each fuzzed input to <tmp>/.env and
// parsing it. It enforces the parser's documented KEY=VALUE contract
//   - Never panics on arbitrary bytes.
//   - On success, every key is non-empty, keys are unique, and each
//     parsed value byte-equals the substring after the FIRST "=" on its
//     source line (a slow reference walk re-derives the expected map and
//     the two must agree exactly — the parser must preserve "=" inside
//     values and never trim the value side).
//   - A malformed input (a non-comment, non-blank line with no "=", an
//     empty key, or a duplicate key) is rejected, never silently
//     accepted.
//
// The body is hermetic: file I/O happens only under t.TempDir.
func FuzzReadStackEnv(f *testing.F) {
	for _, seed := range fuzzEnvSeeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		stackPath := t.TempDir()
		envPath := filepath.Join(stackPath, ".env")
		if err := os.WriteFile(envPath, raw, 0o600); err != nil {
			t.Fatalf("writing fuzz .env: %v", err)
		}

		parsed, err := state.ReadStackEnv(stackPath)

		expected, wantErr := referenceParseEnv(raw)
		if wantErr {
			if err == nil {
				t.Fatalf("parser accepted malformed input the reference walk rejected: %q", raw)
			}
			return
		}
		if err != nil {
			// The reference walk accepted the input but the parser
			// rejected it. The only legitimate divergence is an
			// unreadable file, which cannot happen for a regular file we
			// just wrote, so surface every mismatch.
			t.Fatalf("parser rejected input the reference walk accepted (%q): %v", raw, err)
		}

		if len(parsed) != len(expected) {
			t.Fatalf("key count mismatch for %q: parser=%d reference=%d", raw, len(parsed), len(expected))
		}
		for key, value := range parsed {
			if key == "" {
				t.Fatalf("parser produced an empty key for %q", raw)
			}
			want, ok := expected[key]
			if !ok {
				t.Fatalf("parser produced unexpected key %q for input %q", key, raw)
			}
			if value != want {
				t.Fatalf("value mismatch for key %q: parser=%q reference=%q", key, value, want)
			}
		}
	})
}

// referenceParseEnv is an independent, deliberately slow reimplementation
// of ReadStackEnv's documented line rules used purely to cross-check the
// production parser inside the fuzz body. It returns the expected map and
// whether the input should be rejected as malformed.
func referenceParseEnv(raw []byte) (map[string]string, bool) {
	expected := make(map[string]string)
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "#") {
			continue
		}
		separator := strings.IndexByte(line, '=')
		if separator < 0 {
			return nil, true
		}
		key := strings.TrimSpace(line[:separator])
		if key == "" {
			return nil, true
		}
		if _, exists := expected[key]; exists {
			return nil, true
		}
		expected[key] = line[separator+1:]
	}
	return expected, false
}
