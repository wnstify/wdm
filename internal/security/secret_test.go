package security_test

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	// base64UrlExpectedLen is the encoded length of 32 raw bytes under
	// [encoding/base64.RawURLEncoding]: 4 * ceil(32 / 3) - padding =
	// 44 - 1 = 43. Hard-coded (rather than computed) so a future change
	// to [secretEntropyBytes] fails this test loudly per
	base64UrlExpectedLen = 43

	// hexExpectedLen is the encoded length of 32 raw bytes under
	// [encoding/hex.EncodeToString]: 32 * 2 = 64. Hard-coded per the
	// same rationale as [base64UrlExpectedLen].
	hexExpectedLen = 64

	// base64StdExpectedLen is the encoded length of 32 raw bytes under
	// [encoding/base64.StdEncoding]: 4 * ceil(32 / 3) = 44 (padded).
	// Hard-coded per the same rationale as [base64UrlExpectedLen].
	base64StdExpectedLen = 44

	// secretDecodedBytes is the raw width a base64std secret must decode
	// back to. The consumer (Stoat's autumn AES-256 file-encryption key)
	// requires exactly this many bytes, so the test asserts it directly.
	secretDecodedBytes = 32

	// base64UrlCharset is the alphabet allowed in raw-base64url output
	// per RFC 4648 §5. Padding is excluded because GenerateSecret uses
	// [base64.RawURLEncoding].
	base64UrlCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	// hexCharset is the alphabet emitted by [encoding/hex.EncodeToString].
	hexCharset = "0123456789abcdef"
)

// TestGenerateSecret_Base64URLLength asserts that the production path
// (real `crypto/rand.Reader` seam) returns exactly 43 characters for
// the base64url encoding. The test runs without `SwapEntropyForTest`,
// so it can safely call `t.Parallel`.
func TestGenerateSecret_Base64URLLength(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingBase64URL)
	require.NoError(t, err)
	assert.Len(t, secret, base64UrlExpectedLen,
		"base64url secret must be exactly %d characters (32 bytes raw)", base64UrlExpectedLen)
}

// TestGenerateSecret_HexLength asserts that the production path
// returns exactly 64 characters for the hex encoding.
func TestGenerateSecret_HexLength(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingHex)
	require.NoError(t, err)
	assert.Len(t, secret, hexExpectedLen,
		"hex secret must be exactly %d characters (32 bytes raw)", hexExpectedLen)
}

// TestGenerateSecret_Base64URLCharset asserts that every byte of a
// generated base64url secret lies within the RFC 4648 §5 alphabet —
// no padding character (`=`), no standard-base64 substitutions (`+`,
// `/`). Runs against the production entropy source; the charset check
// is deterministic for any valid base64url output.
func TestGenerateSecret_Base64URLCharset(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingBase64URL)
	require.NoError(t, err)
	for i, r := range secret {
		assert.True(t, strings.ContainsRune(base64UrlCharset, r),
			"base64url secret contains disallowed rune %q at index %d (full output: %q)", r, i, secret)
	}
}

// TestGenerateSecret_HexCharset asserts that every byte of a generated
// hex secret is lowercase hex per [encoding/hex.EncodeToString] —
// uppercase letters are rejected so downstream consumers can rely on
// a single canonical form.
func TestGenerateSecret_HexCharset(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingHex)
	require.NoError(t, err)
	for i, r := range secret {
		assert.True(t, strings.ContainsRune(hexCharset, r),
			"hex secret contains disallowed rune %q at index %d (full output: %q)", r, i, secret)
	}
}

// TestGenerateSecret_Base64StdLength asserts the production path returns
// exactly 44 characters for the standard-base64 encoding (32 raw bytes
// padded). Runs against the real entropy source, so it can call t.Parallel.
func TestGenerateSecret_Base64StdLength(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingBase64Std)
	require.NoError(t, err)
	assert.Len(t, secret, base64StdExpectedLen,
		"base64std secret must be exactly %d characters (32 bytes raw, padded)", base64StdExpectedLen)
}

// TestGenerateSecret_Base64StdDecodesToThirtyTwoBytes is the core
// correctness assertion for the encoding: a generated base64std secret must
// be accepted by a strict standard-base64 decoder and decode back to
// exactly 32 bytes. This is what Stoat's autumn file server requires of
// FILES_ENCRYPTION_KEY (Rust's BASE64_STANDARD into a 32-byte AES-256 key);
// the URL-safe alphabet would be rejected and hex would decode to 48 bytes.
func TestGenerateSecret_Base64StdDecodesToThirtyTwoBytes(t *testing.T) {
	t.Parallel()

	secret, err := security.GenerateSecret(security.EncodingBase64Std)
	require.NoError(t, err)

	decoded, err := base64.StdEncoding.DecodeString(secret)
	require.NoError(t, err, "base64std secret must decode under the strict standard-base64 alphabet")
	assert.Len(t, decoded, secretDecodedBytes,
		"base64std secret must decode to exactly %d bytes for an AES-256 key", secretDecodedBytes)
}

// TestGenerateSecret_Base64StdConsumesFreshEntropy asserts each base64std
// call draws new entropy — two consecutive production-path reads must
// differ. Runs against the real CSPRNG; the collision probability for two
// 32-byte draws is negligible, so it can call t.Parallel.
func TestGenerateSecret_Base64StdConsumesFreshEntropy(t *testing.T) {
	t.Parallel()

	first, err := security.GenerateSecret(security.EncodingBase64Std)
	require.NoError(t, err)
	second, err := security.GenerateSecret(security.EncodingBase64Std)
	require.NoError(t, err)

	assert.NotEqual(t, first, second,
		"two consecutive base64std calls must consume fresh entropy and produce different outputs")
}

// errReader returns its configured error on every Read. Used to drive
// the entropy-failure surface of GenerateSecret.
type errReader struct {
	err error
}

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestGenerateSecret_EntropyFailureSurfacesAsTypedError asserts the
// [GenerateSecret] surfaces it as a `*types.Error` with
// [types.ErrCodeGeneric], NEVER falling back to a weaker source. The
// underlying cause MUST be reachable via [errors.Is] so cmd/wdm and
// log sinks can attribute the failure.
// MUST NOT call `t.Parallel` — swaps the package-global entropy
// seam via SwapEntropyForTest.
func TestGenerateSecret_EntropyFailureSurfacesAsTypedError(t *testing.T) {
	security.SwapEntropyForTest(t, errReader{err: io.ErrUnexpectedEOF})

	secret, err := security.GenerateSecret(security.EncodingBase64URL)
	assert.Empty(t, secret, "entropy failure must not yield a partial secret")
	require.Error(t, err)

	var typed *types.Error
	require.True(t, errors.As(err, &typed),
		"entropy failure must surface as *types.Error so cmd/wdm can map it to an exit code")
	assert.Equal(t, types.ErrCodeGeneric, typed.Code,
		"entropy failure must carry ErrCodeGeneric (PRD §27 row 1)")
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF),
		"the underlying entropy error must remain reachable via errors.Is for diagnostics")
}

// TestGenerateSecret_EntropyFailureForHexEncoding mirrors the
// base64url failure test for the hex encoding path — failure surface
// must be identical regardless of which switch arm would have run
// after a successful read.
// MUST NOT call `t.Parallel` — swaps the package-global entropy seam.
func TestGenerateSecret_EntropyFailureForHexEncoding(t *testing.T) {
	security.SwapEntropyForTest(t, errReader{err: io.ErrUnexpectedEOF})

	secret, err := security.GenerateSecret(security.EncodingHex)
	assert.Empty(t, secret)
	require.Error(t, err)

	var typed *types.Error
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
}

// TestGenerateSecret_UnknownEncodingIsPlainError asserts that an
// unknown [security.Encoding] (including the zero value) returns a
// plain error built with [fmt.Errorf], NOT a `*types.Error`. Unknown
// encoding is a programmer error at the security boundary, not a
// runtime exit-code-bearing failure — typing it as `*types.Error`
// would mistakenly route it through cmd/wdm's exit-code mapping.
// Does NOT swap the entropy seam; can run in parallel.
func TestGenerateSecret_UnknownEncodingIsPlainError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		enc  security.Encoding
	}{
		{name: "zero value", enc: security.Encoding("")},
		{name: "bogus literal", enc: security.Encoding("base32")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret, err := security.GenerateSecret(tc.enc)
			assert.Empty(t, secret)
			require.Error(t, err)

			var typed *types.Error
			assert.False(t, errors.As(err, &typed),
				"unknown encoding must NOT surface as *types.Error — it is a programmer error, not an exit-code-bearing failure")
			assert.Contains(t, err.Error(), "unknown encoding",
				"error message must name the failure mode")
		})
	}
}

// TestGenerateSecret_UnknownEncodingTakesPriorityOverEntropyFailure
// asserts the ordering contract pinned in secret.go's [GenerateSecret]
// godoc — encoding validation runs BEFORE entropy is consumed. When
// both failure modes coexist (entropy seam returns an error AND the
// encoding is unknown), the unknown-encoding programmer error MUST win
// so a caller-side typo cannot be silently absorbed by a transient
// CSPRNG outage and reported as an exit-code 1 runtime failure
// (`*types.Error` with [types.ErrCodeGeneric]).
// Without this ordering, a caller writing `GenerateSecret(Encoding("base32"))`
// during a brief entropy stall would see "entropy source unavailable"
// instead of "unknown encoding %q" — the typo would surface only after
// the CSPRNG recovered, by which point logs and exit codes would have
// pointed at the wrong layer.
// MUST NOT call `t.Parallel` — swaps the package-global entropy seam.
func TestGenerateSecret_UnknownEncodingTakesPriorityOverEntropyFailure(t *testing.T) {
	security.SwapEntropyForTest(t, errReader{err: io.ErrUnexpectedEOF})

	secret, err := security.GenerateSecret(security.Encoding("base32"))
	assert.Empty(t, secret, "unknown encoding must not yield any output, even mid-entropy-failure")
	require.Error(t, err)

	var typed *types.Error
	assert.False(t, errors.As(err, &typed),
		"encoding validation must run before entropy read; unknown encoding must surface as a plain programmer error even when entropy would also fail")
	assert.Contains(t, err.Error(), "unknown encoding",
		"error message must name the encoding-validation failure, not the entropy-failure")
}

// TestGenerateSecret_ConsumesFreshEntropy asserts that each call draws
// new bytes from the entropy seam — the function must NOT cache or
// reuse a previously generated value. Uses a deterministic reader
// (`bytes.Reader` over 64 distinct bytes) so the two consecutive
// calls each consume their own 32-byte window and produce different
// outputs.
// MUST NOT call `t.Parallel` — swaps the package-global entropy seam.
func TestGenerateSecret_ConsumesFreshEntropy(t *testing.T) {
	stream := make([]byte, secretEntropyBytesForTest*2)
	for i := range stream {
		stream[i] = byte(i)
	}
	security.SwapEntropyForTest(t, bytes.NewReader(stream))

	first, err := security.GenerateSecret(security.EncodingBase64URL)
	require.NoError(t, err)

	second, err := security.GenerateSecret(security.EncodingBase64URL)
	require.NoError(t, err)

	assert.NotEqual(t, first, second,
		"two consecutive GenerateSecret calls must consume fresh entropy and produce different outputs")
	assert.Len(t, first, base64UrlExpectedLen)
	assert.Len(t, second, base64UrlExpectedLen)
}

// secretEntropyBytesForTest mirrors the unexported [secretEntropyBytes]
// constant from secret.go. Duplicated here because this is an external
// test package (`security_test`) and the production constant is
// unexported; the value MUST be kept in sync with the production
// definition. If the production constant ever changes,
// [TestGenerateSecret_Base64URLLength] /
// [TestGenerateSecret_HexLength] would fail loudly before this
// duplicate causes silent drift.
const secretEntropyBytesForTest = 32

// argon2idExpectedPHCPrefix is the fixed cost/parallelism header every
// wdm-minted PHC must carry: argon2id, version 19, 64 MiB memory, 3
// passes, 4 lanes. Hard-coded so a change to the production parameters
// fails this test loudly rather than silently shifting the format the
// finish screen and the Vaultwarden parser depend on.
const argon2idExpectedPHCPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"

// TestGenerateArgon2idCredential_PHCWellFormedAndPlaintextVerifies is the
// core correctness assertion: the returned PHC is well-formed, parses into
// the pinned parameters, and re-hashing the returned PLAINTEXT with the
// PHC's own salt and parameters reproduces the embedded hash exactly. That
// round-trip proves the operator can later authenticate with the surfaced
// plaintext — the whole point of one-time surfacing. Runs against the
// production entropy source, so it can call t.Parallel.
func TestGenerateArgon2idCredential_PHCWellFormedAndPlaintextVerifies(t *testing.T) {
	t.Parallel()

	plaintext, phc, err := security.GenerateArgon2idCredential()
	require.NoError(t, err)

	assert.NotEmpty(t, plaintext, "plaintext must be non-empty")
	assert.NotEmpty(t, phc, "phc must be non-empty")
	assert.NotEqual(t, plaintext, phc, "plaintext and phc must differ — the PHC is a one-way hash, not the secret")
	assert.True(t, strings.HasPrefix(phc, argon2idExpectedPHCPrefix),
		"PHC must carry the pinned parameter header %q, got %q", argon2idExpectedPHCPrefix, phc)

	salt, hash, params := parseArgon2idPHC(t, phc)
	recomputed := argon2.IDKey([]byte(plaintext), salt, params.t, params.m, params.p, uint32(len(hash)))
	assert.Equal(t, 1, subtle.ConstantTimeCompare(hash, recomputed),
		"re-hashing the surfaced plaintext with the PHC salt+params must reproduce the embedded hash")
}

// TestGenerateArgon2idCredential_Deterministic pins both the plaintext and
// the PHC against a deterministic entropy reader, proving the draw order
// (plaintext first, then the 16-byte salt) and the encoding are stable.
// The same byte stream must always yield the same plaintext and PHC.
// MUST NOT call t.Parallel — swaps the package-global entropy seam.
func TestGenerateArgon2idCredential_Deterministic(t *testing.T) {
	// 32 bytes for the plaintext draw + 16 bytes for the salt = 48.
	stream := make([]byte, 48)
	for i := range stream {
		stream[i] = byte(i)
	}
	security.SwapEntropyForTest(t, bytes.NewReader(stream))
	plaintext, phc, err := security.GenerateArgon2idCredential()
	require.NoError(t, err)

	security.SwapEntropyForTest(t, bytes.NewReader(stream))
	plaintext2, phc2, err := security.GenerateArgon2idCredential()
	require.NoError(t, err)

	assert.Equal(t, plaintext, plaintext2, "identical entropy must yield an identical plaintext")
	assert.Equal(t, phc, phc2, "identical entropy must yield an identical PHC")

	salt, _, _ := parseArgon2idPHC(t, phc)
	assert.Equal(t, stream[secretEntropyBytesForTest:secretEntropyBytesForTest+16], salt,
		"the salt must be the 16 bytes drawn AFTER the 32-byte plaintext window")
}

// TestGenerateArgon2idCredential_PlaintextEntropyFailureSurfacesTypedError
// asserts that a failure on the plaintext draw surfaces as a typed error
// and yields no usable output — wdm never returns a half-formed credential.
// MUST NOT call t.Parallel — swaps the package-global entropy seam.
func TestGenerateArgon2idCredential_PlaintextEntropyFailureSurfacesTypedError(t *testing.T) {
	security.SwapEntropyForTest(t, errReader{err: io.ErrUnexpectedEOF})

	plaintext, phc, err := security.GenerateArgon2idCredential()
	assert.Empty(t, plaintext)
	assert.Empty(t, phc)
	require.Error(t, err)

	var typed *types.Error
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
}

// TestGenerateArgon2idCredential_SaltEntropyFailureSurfacesTypedError
// asserts that a failure on the salt draw (after a successful plaintext
// draw) still yields no output and a typed error — a usable plaintext must
// never be stranded without its PHC.
// MUST NOT call t.Parallel — swaps the package-global entropy seam.
func TestGenerateArgon2idCredential_SaltEntropyFailureSurfacesTypedError(t *testing.T) {
	// Exactly enough entropy for the 32-byte plaintext, none for the salt.
	security.SwapEntropyForTest(t, io.LimitReader(bytes.NewReader(make([]byte, secretEntropyBytesForTest)), secretEntropyBytesForTest))

	plaintext, phc, err := security.GenerateArgon2idCredential()
	assert.Empty(t, plaintext)
	assert.Empty(t, phc)
	require.Error(t, err)

	var typed *types.Error
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, types.ErrCodeGeneric, typed.Code)
}

// argon2idParams holds the parsed cost/parallelism a PHC advertises.
type argon2idParams struct {
	m uint32
	t uint32
	p uint8
}

// parseArgon2idPHC parses a PHC string of the form
// $argon2id$v=19$m=...,t=...,p=...$<b64salt>$<b64hash> and returns the
// decoded salt, decoded hash, and the advertised parameters. It fails the
// test on any malformed field, so it doubles as a well-formedness check.
func parseArgon2idPHC(t *testing.T, phc string) (salt, hash []byte, params argon2idParams) {
	t.Helper()

	fields := strings.Split(phc, "$")
	// Leading "$" yields an empty first field: ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash].
	require.Len(t, fields, 6, "PHC must have six $-delimited fields, got %q", phc)
	require.Empty(t, fields[0], "PHC must begin with $")
	assert.Equal(t, "argon2id", fields[1], "PHC algorithm must be argon2id")

	var version int
	_, err := fmt.Sscanf(fields[2], "v=%d", &version)
	require.NoError(t, err, "PHC version field must parse, got %q", fields[2])
	assert.Equal(t, 19, version, "PHC version must be 19")

	_, err = fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.m, &params.t, &params.p)
	require.NoError(t, err, "PHC parameter field must parse, got %q", fields[3])

	salt, err = base64.RawStdEncoding.DecodeString(fields[4])
	require.NoError(t, err, "PHC salt must be valid raw-std base64, got %q", fields[4])
	require.Len(t, salt, 16, "PHC salt must be 16 bytes")

	hash, err = base64.RawStdEncoding.DecodeString(fields[5])
	require.NoError(t, err, "PHC hash must be valid raw-std base64, got %q", fields[5])
	require.Len(t, hash, 32, "PHC hash must be 32 bytes")

	return salt, hash, params
}

// TestNoMathRandImportInSecurityOrRender enforces —
// generated secrets MUST come from `crypto/rand` only; no
// production-path file in `internal/security` or `internal/render`
// may import `math/rand` or `math/rand/v2`. The check parses every
// non-`_test.go` Go file under the two packages and asserts the
// import set is clean. Test files are excluded because they may
// legitimately import a math-rand variant for non-cryptographic fixture data.
func TestNoMathRandImportInSecurityOrRender(t *testing.T) {
	t.Parallel()

	dirs := []string{
		filepath.Join("..", "..", "internal", "security"),
		filepath.Join("..", "..", "internal", "render"),
	}
	banned := map[string]struct{}{
		`"math/rand"`:    {},
		`"math/rand/v2"`: {},
	}

	fset := token.NewFileSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err, "reading %s", dir)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			require.NoError(t, err, "parsing %s", path)
			for _, imp := range file.Imports {
				if _, bad := banned[imp.Path.Value]; bad {
					t.Errorf("forbidden import %s in production file %s (crypto/rand only)",
						imp.Path.Value, path)
				}
			}
		}
	}
}
