package core_test

import (
	"strings"
	"testing"

	"github.com/wnstify/wdm/internal/core"
)

// fuzzDomainSeeds is the starting corpus for the domain validator. It
// pairs known-good hostnames (mixed case, trailing dot, single label,
// punycode) with known-rejected inputs: localhost (explicitly refused
// by normalizeDomain), injection payloads, URLs, unicode, IP literals,
// and oversized labels.
var fuzzDomainSeeds = []string{
	"app.example.com",
	"App.Example.COM",
	"app.example.com.",
	"localhost",
	"a",
	"xn--e1afmkfd.xn--p1ai",
	"sub.domain.co.uk",
	"a-b-c.example.org",
	"",
	".",
	"*.example.com",
	"-leading.example.com",
	"trailing-.example.com",
	"app..example.com",
	"http://example.com",
	"user:pass@example.com",
	"example.com/path",
	"example.com:8080",
	"127.0.0.1",
	"::1",
	"münchen.de",
	"app\nexample.com",
	"app example.com",
	strings.Repeat("a", 64) + ".example.com",
	strings.Repeat("a.", 130) + "com",
}

// FuzzNormalizeDomain drives the real RFC-1123 domain normalizer
// (internal/core's normalizeDomain, reached via NormalizeDomainForTest)
// against arbitrary input. It enforces the validator's accept-side
// contract — when it accepts, the output MUST be a canonical lowercase
// ASCII hostname (PRD §17 domain handling; "domain
// validator" fuzz fence):
//   - Never panics.
//   - On rejection, the returned host is the empty string (callers must
//     never receive a half-normalized value alongside an error).
//   - On acceptance, the host is non-empty, all-ASCII, fully lowercase,
//     free of "/", ":", "@", and "*", carries no trailing dot, is at
//     most 253 bytes, and every dot-separated label is 1-63 bytes of
//     [a-z0-9-] with no leading or trailing hyphen.
func FuzzNormalizeDomain(f *testing.F) {
	for _, seed := range fuzzDomainSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		host, err := core.NormalizeDomainForTest(value)

		if err != nil {
			if host != "" {
				t.Fatalf("rejected domain %q returned non-empty host %q", value, host)
			}
			return
		}
		assertNormalizedDomainAccepted(t, value, host)
	})
}

func assertNormalizedDomainAccepted(t *testing.T, value, host string) {
	t.Helper()

	if host == "" {
		t.Fatalf("accepted domain %q normalized to empty host", value)
	}
	if host != strings.ToLower(host) {
		t.Fatalf("accepted host %q (from %q) is not lowercase", host, value)
	}
	if strings.HasSuffix(host, ".") {
		t.Fatalf("accepted host %q (from %q) carries a trailing dot", host, value)
	}
	if strings.ContainsAny(host, "/:@*") {
		t.Fatalf("accepted host %q (from %q) contains a forbidden character", host, value)
	}
	if len(host) > 253 {
		t.Fatalf("accepted host %q (from %q) exceeds 253 bytes", host, value)
	}
	assertNormalizedDomainASCII(t, value, host)
	for _, label := range strings.Split(host, ".") {
		assertNormalizedDomainLabel(t, value, host, label)
	}
}

func assertNormalizedDomainASCII(t *testing.T, value, host string) {
	t.Helper()

	for _, r := range host {
		if r > 127 {
			t.Fatalf("accepted host %q (from %q) contains non-ASCII rune %q", host, value, r)
		}
	}
}

func assertNormalizedDomainLabel(t *testing.T, value, host, label string) {
	t.Helper()

	if len(label) == 0 || len(label) > 63 {
		t.Fatalf("accepted host %q (from %q) has invalid label length %q", host, value, label)
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		t.Fatalf("accepted host %q (from %q) has hyphen-bounded label %q", host, value, label)
	}
	for _, r := range label {
		isLetter := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		isHyphen := r == '-'
		if !isLetter && !isDigit && !isHyphen {
			t.Fatalf("accepted host %q (from %q) label %q has invalid character %q", host, value, label, r)
		}
	}
}
