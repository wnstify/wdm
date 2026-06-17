package security_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
)

func TestActiveRedactor_ImplementsRedactor(t *testing.T) {
	t.Parallel()

	r := security.NewActiveRedactor(nil)
	assert.Implements(t, (*security.Redactor)(nil), r)
}

func TestRedactedPlaceholder_IsStable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "[REDACTED]", security.RedactedPlaceholder)
}

// Literal-value pass: registered secrets are replaced by
// [security.RedactedPlaceholder] regardless of where they sit in the
// input. The constructor strips empty entries, deduplicates equal
// entries, and orders surviving secrets longest-first so a shorter
// secret cannot shadow a longer overlapping one.
func TestActiveRedactor_LiteralSecretRedaction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		secrets []string
		input   string
		want    string
	}{
		{
			name:    "single_secret_substring",
			secrets: []string{"hunter2"},
			input:   "the password is hunter2 here",
			want:    "the password is [REDACTED] here",
		},
		{
			name:    "repeated_secret_value",
			secrets: []string{"hunter2"},
			input:   "hunter2 hunter2 hunter2",
			want:    "[REDACTED] [REDACTED] [REDACTED]",
		},
		{
			name:    "multiple_distinct_secrets",
			secrets: []string{"alpha", "bravo"},
			input:   "alpha bravo charlie",
			want:    "[REDACTED] [REDACTED] charlie",
		},
		{
			name:    "longest_wins_input_order_short_first",
			secrets: []string{"abc", "abcdef"},
			input:   "abcdef",
			want:    "[REDACTED]",
		},
		{
			name:    "longest_wins_input_order_long_first",
			secrets: []string{"abcdef", "abc"},
			input:   "abcdef",
			want:    "[REDACTED]",
		},
		{
			name:    "disjoint_short_and_long_both_redact",
			secrets: []string{"abc", "abcdef"},
			input:   "abc abcdef",
			want:    "[REDACTED] [REDACTED]",
		},
		{
			name:    "empty_input_returns_empty",
			secrets: []string{"hunter2"},
			input:   "",
			want:    "",
		},
		{
			name:    "nil_secrets_no_change_when_unmatched",
			secrets: nil,
			input:   "no secret here",
			want:    "no secret here",
		},
		{
			name:    "empty_secret_string_dropped",
			secrets: []string{""},
			input:   "no secret here",
			want:    "no secret here",
		},
		{
			name:    "mixed_empty_and_real_only_real_applied",
			secrets: []string{"", "hunter2", ""},
			input:   "hunter2 stays",
			want:    "[REDACTED] stays",
		},
		{
			name:    "duplicate_secrets_deduplicated",
			secrets: []string{"hunter2", "hunter2", "hunter2"},
			input:   "hunter2",
			want:    "[REDACTED]",
		},
		{
			name:    "secret_at_string_boundaries",
			secrets: []string{"hunter2"},
			input:   "hunter2 middle hunter2",
			want:    "[REDACTED] middle [REDACTED]",
		},
		{
			name:    "secret_with_regex_special_chars_treated_literally",
			secrets: []string{"a.b*c"},
			input:   "value a.b*c then xax (literal)",
			want:    "value [REDACTED] then xax (literal)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := security.NewActiveRedactor(tc.secrets)
			assert.Equal(t, tc.want, r.Redact(tc.input))
		})
	}
}

// Structural-pattern pass: assignment forms get scrubbed regardless of
// whether the value was pre-registered as a literal secret. Patterns
// cover env-style KEY=VALUE, JSON "key": "value" entries under a
// secret-typed key, HTTP "Bearer <token>" headers, and URL credential
// segments. The constructor is called with no secrets so only the
// structural pass fires.
func TestActiveRedactor_StructuralPatterns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		// env assignment — variations of KEY=VALUE.
		{"env_postgres_password", "POSTGRES_PASSWORD=hunter2", "POSTGRES_PASSWORD=[REDACTED]"},
		{"env_jwt_secret", "JWT_SECRET=eyJhbGciOiJI", "JWT_SECRET=[REDACTED]"},
		{"env_github_token", "GITHUB_TOKEN=ghp_abcdef", "GITHUB_TOKEN=[REDACTED]"},
		{"env_my_api_key_underscore", "MY_API_KEY=xyz-123", "MY_API_KEY=[REDACTED]"},
		{"env_apikey_no_separator", "APIKEY=xyz-123", "APIKEY=[REDACTED]"},
		{"env_lowercase_password", "password=hunter2", "password=[REDACTED]"},
		{"env_db_credential", "DB_CREDENTIAL=abc", "DB_CREDENTIAL=[REDACTED]"},
		{"env_aws_access_key", "AWS_ACCESS_KEY=AKIAXYZ", "AWS_ACCESS_KEY=[REDACTED]"},
		{"env_ssh_private_key", "SSH_PRIVATE_KEY=-----BEGIN", "SSH_PRIVATE_KEY=[REDACTED]"},
		{"env_encryption_key_n8n", "N8N_ENCRYPTION_KEY=abc123", "N8N_ENCRYPTION_KEY=[REDACTED]"},
		{"env_signing_key", "JWT_SIGNING_KEY=secretkey", "JWT_SIGNING_KEY=[REDACTED]"},
		{"env_authorization", "AUTHORIZATION=token123", "AUTHORIZATION=[REDACTED]"},
		{"env_auth_short_form", "auth=value", "auth=[REDACTED]"},
		{"env_double_quoted_value", `PASSWORD="hunter 2"`, `PASSWORD=[REDACTED]`},
		{"env_single_quoted_value", `PASSWORD='hunter 2'`, `PASSWORD=[REDACTED]`},
		{"env_spaces_around_equals", "PASSWORD = hunter2", "PASSWORD = [REDACTED]"},

		// JSON field — both spaced and compact, case-insensitive key.
		{"json_password", `{"password": "hunter2"}`, `{"password": "[REDACTED]"}`},
		{"json_token", `{"token": "abc.def.ghi"}`, `{"token": "[REDACTED]"}`},
		{"json_api_key", `{"api_key": "xyz"}`, `{"api_key": "[REDACTED]"}`},
		{"json_nested_in_object", `{"db": {"password": "secret"}}`, `{"db": {"password": "[REDACTED]"}}`},
		{"json_compact_no_space", `{"password":"hunter2"}`, `{"password":"[REDACTED]"}`},
		{"json_uppercase_key", `{"PASSWORD": "hunter2"}`, `{"PASSWORD": "[REDACTED]"}`},
		{"json_with_escaped_quote_in_value", `{"password": "hu\"nter"}`, `{"password": "[REDACTED]"}`},

		// HTTP Bearer — common Authorization-header pattern.
		{"http_bearer_simple", "Bearer eyJhbGc.def.ghi", "Bearer [REDACTED]"},
		{"http_bearer_with_auth_header", "Authorization: Bearer eyJhbGc", "Authorization: Bearer [REDACTED]"},
		{"http_bearer_lowercase", "bearer abc.def.ghi", "bearer [REDACTED]"},

		// URL credentials — multiple schemes.
		{"url_https_credentials", "https://user:pass@host.example.com", "https://user:[REDACTED]@host.example.com"},
		{"url_http_credentials", "http://u:p@host", "http://u:[REDACTED]@host"},
		{"url_postgres_credentials", "postgres://user:secret@db.example.com:5432/db", "postgres://user:[REDACTED]@db.example.com:5432/db"},
		{"url_redis_credentials", "redis://user:pass@redis.local:6379", "redis://user:[REDACTED]@redis.local:6379"},

		// Combined envelope — DB_URL is not a trigger word (URL is
		// not in the trigger set), so env_assignment does not fire on
		// the outer form. url_credentials redacts the embedded password
		// inside the URL while preserving the surrounding URL
		// structure, which is the more readable outcome.
		{
			name:  "url_inside_non_secret_env_redacts_credential_only",
			input: "DB_URL=postgres://user:secret@host/db",
			want:  "DB_URL=postgres://user:[REDACTED]@host/db",
		},
		// When the env KEY does carry a trigger word, env_assignment
		// captures the entire RHS (including any embedded URL) and the
		// url_credentials pattern never sees it.
		{
			name:  "url_inside_secret_env_outer_envelope_wins",
			input: "DB_PASSWORD=postgres://user:secret@host/db",
			want:  "DB_PASSWORD=[REDACTED]",
		},
	}

	r := security.NewActiveRedactor(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, r.Redact(tc.input))
		})
	}
}

// Unrelated words must NOT be redacted: trigger words appearing
// without an assignment delimiter, and identifiers containing trigger
// substrings in non-suffix positions, both must pass through
// unchanged.
func TestActiveRedactor_DoesNotOverRedactUnrelatedWords(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{"password_word_no_assignment", "the password is everywhere"},
		{"secret_word_no_assignment", "this is a secret message"},
		{"token_word_no_assignment", "a token of appreciation"},
		{"key_word_no_assignment", "the key to success"},
		{"authority_contains_auth_in_middle", "authority granted"},
		{"plain_url_no_credentials", "https://docs.example.com/path?q=1"},
		{"non_secret_env_user", "USER=alice"},
		{"non_secret_env_path", "PATH=/usr/bin:/usr/local/bin"},
		{"vault_key_bare_key_not_in_triggers", "VAULT_KEY=public-info"},
	}

	r := security.NewActiveRedactor(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.input, r.Redact(tc.input))
		})
	}
}

// Literal and structural passes compose cleanly: a value scrubbed by
// the literal pass first stays redacted; a structural match that
// contains a registered secret remains redacted with the structural
// envelope preserved.
func TestActiveRedactor_LiteralAndStructuralPassesCompose(t *testing.T) {
	t.Parallel()

	r := security.NewActiveRedactor([]string{"hunter2", "secret-token-xyz"})

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "registered_secret_inside_env_assignment",
			input: "PASSWORD=hunter2",
			want:  "PASSWORD=[REDACTED]",
		},
		{
			name:  "registered_secret_inside_json_field",
			input: `{"password": "hunter2"}`,
			want:  `{"password": "[REDACTED]"}`,
		},
		{
			name:  "registered_secret_outside_envelope_still_redacts",
			input: "the value hunter2 appears in plain text",
			want:  "the value [REDACTED] appears in plain text",
		},
		{
			name:  "two_secrets_in_mixed_envelopes",
			input: "PASSWORD=hunter2 token=secret-token-xyz",
			want:  "PASSWORD=[REDACTED] token=[REDACTED]",
		},
		{
			name:  "registered_secret_bare_redacts_via_literal_pass",
			input: "secret-token-xyz alone",
			want:  "[REDACTED] alone",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, r.Redact(tc.input))
		})
	}
}

// Determinism: the same input redacted many times produces
// byte-identical output. Sequential application of the patterns is
// order-stable; the secret replacer is deterministic in both content
// and ordering.
func TestActiveRedactor_DeterministicAcrossManyCalls(t *testing.T) {
	t.Parallel()

	r := security.NewActiveRedactor([]string{"hunter2", "swordfish"})
	input := `PASSWORD=hunter2 and {"token": "swordfish"} via Bearer eyJabc.def`
	want := r.Redact(input)

	for i := range 100 {
		got := r.Redact(input)
		require.Equal(t, want, got, "iteration %d diverged", i)
	}
}

// Constructor determinism: two redactors built from the same secrets
// slice (in different orders) produce identical output on the same
// input. The constructor's dedupe + sort is order-stable.
func TestActiveRedactor_ConstructorIsOrderInsensitive(t *testing.T) {
	t.Parallel()

	a := security.NewActiveRedactor([]string{"alpha", "bravo", "charlie"})
	b := security.NewActiveRedactor([]string{"charlie", "bravo", "alpha"})

	input := "alpha bravo charlie"
	assert.Equal(t, a.Redact(input), b.Redact(input))
}

// Caller-side mutation of the secrets slice after construction must
// not alter the redactor's behavior. Go strings are immutable; the
// redactor retains the values it saw at construction time even if the
// caller rewrites slice elements afterwards.
func TestActiveRedactor_CallerSliceMutationDoesNotAffectRedactor(t *testing.T) {
	t.Parallel()

	secrets := []string{"hunter2", "swordfish"}
	r := security.NewActiveRedactor(secrets)
	secrets[0] = "tampered"
	secrets[1] = "altered"

	// Original secrets still redact.
	assert.Equal(t, "[REDACTED]", r.Redact("hunter2"))
	assert.Equal(t, "[REDACTED]", r.Redact("swordfish"))

	// Post-construction values were never registered — no literal-pass
	// match. They contain no trigger substring + no assignment form,
	// so no structural pattern fires either.
	assert.Equal(t, "tampered", r.Redact("tampered"))
	assert.Equal(t, "altered", r.Redact("altered"))
}

// Concurrency safety: many goroutines hitting the same redactor with
// distinct inputs must produce stable per-input output and must not
// race. Run under -race to detect any shared-state regression in
// future refactors.
func TestActiveRedactor_IsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r := security.NewActiveRedactor([]string{"hunter2", "swordfish"})
	inputs := []struct {
		in   string
		want string
	}{
		{"PASSWORD=hunter2", "PASSWORD=[REDACTED]"},
		{`{"token": "swordfish"}`, `{"token": "[REDACTED]"}`},
		{"Bearer eyJabc.def", "Bearer [REDACTED]"},
		{"https://u:p@host", "https://u:[REDACTED]@host"},
		{"hunter2 hunter2 swordfish", "[REDACTED] [REDACTED] [REDACTED]"},
		{"plain text with no triggers", "plain text with no triggers"},
	}

	const goroutines = 50
	const callsPer = 100

	done := make(chan struct{}, goroutines)
	for i := range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()
			tc := inputs[i%len(inputs)]
			for range callsPer {
				if got := r.Redact(tc.in); got != tc.want {
					t.Errorf("goroutine %d: got %q want %q for input %q", i, got, tc.want, tc.in)
					return
				}
			}
		}()
	}
	for range goroutines {
		<-done
	}
}

// NoopRedactor remains the passthrough implementation it has been
// since; the active redactor is the counterpart introduced
// at. Both satisfy the [security.Redactor] interface
// (interface compliance is asserted by [TestActiveRedactor_ImplementsRedactor]
// and pinned at compile time by the `var _ Redactor = (*activeRedactor)(nil)`
// line in active_redactor.go); this test pins the behavioral
// divergence on a single input that exercises both the literal and
// structural scrub paths.
func TestRedactors_NoopAndActiveProduceDifferentOutput(t *testing.T) {
	t.Parallel()

	active := security.NewActiveRedactor([]string{"hunter2"})

	const sample = "PASSWORD=hunter2"
	assert.Equal(t, sample, security.NoopRedactor.Redact(sample))
	assert.Equal(t, "PASSWORD=[REDACTED]", active.Redact(sample))
}
