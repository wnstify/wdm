package security

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// RedactedPlaceholder is the stable substring the [Redactor] returned
// by [NewActiveRedactor] writes in place of every secret value it
// scrubs. It is exported so callers can assert against it without
// coupling to the redactor implementation — internal/logging tests
// checking that a known secret was scrubbed before reaching the log
// sink, internal/core install tests grepping the JSON envelope for
// leaks (PRD §11, §24). The literal value is
// fixed for the lifetime of the package and MUST NOT change across
// releases without a coordinated update to any test that grep-asserts
// against it.
const RedactedPlaceholder = "[REDACTED]"

// activeRedactor is the production [Redactor] introduced at
// obtain an instance — typed as the [Redactor] interface — through
// [NewActiveRedactor] only. Unexporting the type closes a zero-value
// footgun: an &activeRedactor{} would carry a nil *strings.Replacer
// and panic on the first Redact call. Construction through the
// constructor guarantees the replacer is initialized.
// Every instance scrubs two classes of sensitive text from any string
// before that string reaches a log sink, the JSON envelope, an error
// detail line, or a TUI screen (PRD §11, §24):
//   - Literal secret VALUES registered with [NewActiveRedactor] at
//     construction time. The secret-generation
//     surface hands its freshly minted values to
//     [NewActiveRedactor], so any later log line, error message, or
//     JSON envelope that echoes the value is scrubbed before the value
//     reaches any sink.
//   - Structural assignment FORMS — env-style KEY=VALUE pairs whose
//     KEY ends in a secret-typed word, JSON "key":"value" entries
//     under a secret-typed key, HTTP "Bearer <token>" headers, and
//     URL credential segments — that may carry secret-bearing values
//     even when the value is not pre-registered.
//
// Every match is replaced with [RedactedPlaceholder]. Instances are
// immutable after construction; both the compiled regular expressions
// and the *strings.Replacer they own are safe for concurrent use per
// the [regexp.Regexp] and [strings.Replacer] documentation.
// engine construction, replacing the [NoopRedactor] production
type activeRedactor struct {
	// patterns hold the structural-form scrubbers applied after the
	// literal-value pass. Order is fixed at construction; iteration is
	// deterministic across calls.
	patterns []redactionPattern

	// replacer holds the literal-secret substitutions, ordered
	// longest-secret-first so a secret that is a prefix of another in
	// the same set does not shadow the longer match — registering both
	// "abc" and "abcdef" still redacts "abcdef" as a single
	// [RedactedPlaceholder].
	replacer *strings.Replacer
}

// redactionPattern pairs a compiled regular expression with the
// template [regexp.Regexp.ReplaceAllString] applies on a match.
// Templates use Go's $1 / ${1} expansion to preserve the matched
// key / header / scheme prefix so the redacted output still tells the
// reader WHAT was scrubbed (e.g. POSTGRES_PASSWORD=[REDACTED] keeps
// "POSTGRES_PASSWORD=") without leaking the value.
type redactionPattern struct {
	rx          *regexp.Regexp
	replacement string
}

// defaultPatterns is the curated set of structural redaction patterns
// applied by every [Redactor] returned from [NewActiveRedactor].
// Patterns target common secret-bearing assignment forms wdm
// encounters in Docker stderr output, env-file content, JSON
// envelopes, and URL credentials.
// Compilation happens once at package init via [regexp.MustCompile];
// a bad expression is a programming error that surfaces at program
// start, not at the first Redact call.
// Adding a pattern is the canonical way to expand the structural scrub
// surface without changing the redactor type; each pattern stays
// self-contained and order-independent for the cases that matter.
var defaultPatterns = []redactionPattern{
	// env_assignment: KEY=VALUE where KEY ends in a secret-typed word.
	// Value may be double-quoted, single-quoted, or unquoted (anything
	// non-whitespace). The second capture group preserves whitespace
	// around the "=" so "FOO = bar" round-trips as "FOO = [REDACTED]"
	// without normalizing the operator spacing. Common forms covered:
	// POSTGRES_PASSWORD=, JWT_SECRET=, GITHUB_TOKEN=, MY_API_KEY=,
	// N8N_ENCRYPTION_KEY=, SSH_PRIVATE_KEY=, AWS_ACCESS_KEY=,
	// AUTHORIZATION=, AUTH=, DB_CREDENTIAL=.
	{
		rx: regexp.MustCompile(
			`(?i)\b([A-Za-z0-9_]*(?:password|passwd|secret|token|api[_-]?key|apikey|encryption[_-]?key|signing[_-]?key|private[_-]?key|access[_-]?key|auth(?:orization)?|credential))(\s*=\s*)(?:"[^"]*"|'[^']*'|\S+)`,
		),
		replacement: "${1}${2}" + RedactedPlaceholder,
	},
	// json_field: "key": "value" where key contains a secret-typed
	// word. Handles both spaced and compact JSON; value matching is
	// escape-aware ((?:[^"\\]|\\.)* preserves quoted-escape pairs like
	// \" inside the value).
	{
		rx: regexp.MustCompile(
			`(?i)("(?:[^"\\]|\\.)*(?:password|passwd|secret|token|api[_-]?key|apikey|encryption[_-]?key|signing[_-]?key|private[_-]?key|access[_-]?key|auth(?:orization)?|credential)(?:[^"\\]|\\.)*"\s*:\s*)"(?:[^"\\]|\\.)*"`,
		),
		replacement: `${1}"` + RedactedPlaceholder + `"`,
	},
	// http_bearer: "Bearer <token>" in HTTP Authorization headers. The
	// trailing \S+ matches any token character set (base64, base64url,
	// opaque) without identifying the shape.
	{
		rx:          regexp.MustCompile(`(?i)(\bbearer\s+)\S+`),
		replacement: "${1}" + RedactedPlaceholder,
	},
	// url_credentials: scheme://user:password@host across http(s),
	// postgres, redis, mysql, mongodb, ftp, and any other scheme
	// that follows the credential-userinfo URL form.
	{
		rx:          regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.\-]*://[^/\s:@]+:)([^@/\s]+)(@)`),
		replacement: "${1}" + RedactedPlaceholder + "${3}",
	},
}

// Compile-time confirmation that *activeRedactor satisfies the
// [Redactor] interface, mirroring the var-typed declaration
// [NoopRedactor] uses to bind to the same interface.
var _ Redactor = (*activeRedactor)(nil)

// NewActiveRedactor builds a [Redactor] that scrubs every entry of
// secrets from any string it sees, plus the structural assignment
// forms named by the package-private default pattern set. Empty
// entries are dropped and duplicates removed; surviving secrets are
// stored longest-first (stable on tie) so a shorter secret cannot
// shadow a longer overlapping one.
// The caller's slice is not retained: mutating secrets[i] after this
// call does not change the redactor, which stays pinned to the values
// seen at construction time. Go strings are immutable, so no defensive
// copy is needed.
// The returned [Redactor] is safe for concurrent use and produces
// deterministic output: identical (s, secrets) inputs yield identical
// [Redactor.Redact] results across calls and goroutines. The concrete
// type behind the interface is unexported, so this constructor is the
// only construction path — guaranteeing the underlying
// *strings.Replacer is initialized and the package never hands out a
// nil-replacer instance that would panic on the first Redact call.
func NewActiveRedactor(secrets []string) Redactor {
	return &activeRedactor{
		patterns: defaultPatterns,
		replacer: buildSecretReplacer(secrets),
	}
}

// Redact returns a copy of s with every registered literal secret and
// every match against the structural assignment patterns replaced by
// [RedactedPlaceholder]. The empty string is returned unchanged. For
// any non-empty s, the result is a new value; Go strings are
// immutable, so the caller's input is never mutated.
// Redact is safe for concurrent use: every field on *activeRedactor is
// either an immutable compiled regular expression or a
// *strings.Replacer, both documented as concurrency-safe by the
// standard library.
func (r *activeRedactor) Redact(s string) string {
	if s == "" {
		return s
	}
	out := r.replacer.Replace(s)
	for _, p := range r.patterns {
		out = p.rx.ReplaceAllString(out, p.replacement)
	}
	return out
}

// buildSecretReplacer constructs the literal-value *strings.Replacer
// the [Redactor] returned by [NewActiveRedactor] applies first. Empty
// entries are dropped, duplicates removed, and surviving secrets
// sorted longest-first (stable on tie) so the Replacer honors the
// longest-match-wins contract documented on [NewActiveRedactor]. When
// no usable secrets remain, it returns the no-op Replacer.
func buildSecretReplacer(secrets []string) *strings.Replacer {
	seen := make(map[string]struct{}, len(secrets))
	cleaned := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		cleaned = append(cleaned, s)
	}
	if len(cleaned) == 0 {
		return strings.NewReplacer()
	}
	slices.SortStableFunc(cleaned, func(a, b string) int {
		return cmp.Compare(len(b), len(a))
	})
	pairs := make([]string, 0, len(cleaned)*2)
	for _, s := range cleaned {
		pairs = append(pairs, s, RedactedPlaceholder)
	}
	return strings.NewReplacer(pairs...)
}
