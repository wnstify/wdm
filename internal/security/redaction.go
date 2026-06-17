package security

// Redactor removes sensitive substrings from text before that text
// reaches a log sink, the JSON envelope, an error detail line, or a
// TUI screen (PRD §11, §24). It is the single integration point used
// by internal/logging and by call sites that emit secrets-adjacent strings.
// [NewActiveRedactor] constructs the production pattern-and-literal scrubber;
// the concrete type is unexported so callers cannot bypass the constructor.
type Redactor interface {
	// Redact returns a copy of s with sensitive substrings replaced
	// by a stable placeholder. The returned string MUST be safe to
	// write to logs, JSON output, error details, and TUI screens
	// (PRD §11, §24). Implementations MUST be safe for concurrent use
	// by multiple goroutines.
	Redact(s string) string
}

// NoopRedactor is the passthrough [Redactor] retained for explicit no-op test
// callers. It returns its input unchanged. Production callers MUST obtain the
// active [Redactor] via [NewActiveRedactor].
// It is exported as a value, not a constructor, so test callers can
// write `redactor:= security.NoopRedactor` without an allocation.
var NoopRedactor Redactor = noopRedactor{}

// noopRedactor is the empty-struct backing for [NoopRedactor]. It
// carries no state and is safe to share across goroutines without
// synchronization.
type noopRedactor struct{}

// Redact returns s unchanged.
func (noopRedactor) Redact(s string) string { return s }
