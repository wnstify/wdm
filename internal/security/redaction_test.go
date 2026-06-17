package security_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wnstify/wdm/internal/security"
)

func TestNoopRedactor_ReturnsInputUnchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"plain", "hello"},
		{"with_spaces", "hello world"},
		{"with_newline", "line1\nline2"},
		{"with_unicode", "héllo 世界"},
		{"looks_like_secret", "DB_PASSWORD=swordfish"},
		{"json_blob", `{"token":"abc123"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := security.NoopRedactor.Redact(tc.input)
			assert.Equal(t, tc.input, got)
		})
	}
}

// TestNoopRedactor_ImplementsRedactor is the runtime mirror of the
// `var NoopRedactor Redactor = noopRedactor{}` declaration in
// redaction.go. The compile-time check would already catch a broken
// interface, but the runtime assertion documents the contract for
// any reader who has not yet seen that line.
func TestNoopRedactor_ImplementsRedactor(t *testing.T) {
	t.Parallel()

	assert.Implements(t, (*security.Redactor)(nil), security.NoopRedactor)
}

// TestNoopRedactor_IsSafeForConcurrentUse exercises the redactor from
// many goroutines simultaneously. The implementation carries no state
// — the test exists so a future swap-in that accidentally
// introduces shared mutable state fails under `go test -race` rather
// than at the first production log call.
func TestNoopRedactor_IsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	const callsPer = 100

	done := make(chan struct{}, goroutines)
	for range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()
			for range callsPer {
				_ = security.NoopRedactor.Redact("payload")
			}
		}()
	}
	for range goroutines {
		<-done
	}
}
