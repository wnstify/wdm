package security

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"

	"github.com/wnstify/wdm/pkg/types"
)

// Encoding selects the output form of a generated secret. The string
// underlying type matches the on-disk catalog representation
// #1 — `encoding: base64url | hex` on `type: secret` placeholders) so
// future catalog wiring can cast a parsed catalog value to an Encoding
// directly. The zero value ("") is deliberately not valid:
// [GenerateSecret] rejects it through the default arm of its switch so
// an uninitialized field surfaces as a programmer error at the
// security boundary rather than silently generating a secret in the
// wrong form.
type Encoding string

// EncodingBase64URL renders the 32-byte raw entropy as 43 characters
// of base64url without padding. The character set
// [A-Za-z0-9_-] is safe in `.env` values without quoting and free of
// the `+` / `/` / `=` characters that some consumers (URL fragments,
// shell here-strings) treat specially.
const EncodingBase64URL Encoding = "base64url"

// EncodingHex renders the 32-byte raw entropy as 64 characters of lowercase
// hex. The character set [0-9a-f] is safe for any consumer
// that expects a flat alphanumeric token.
const EncodingHex Encoding = "hex"

// EncodingBase64Std renders the 32-byte raw entropy as 44 characters of
// standard base64 with padding ([encoding/base64.StdEncoding] — alphabet
// [A-Za-z0-9+/] with a trailing `=`). Unlike [EncodingBase64URL], it uses
// the standard `+` / `/` alphabet because some consumers decode the value
// with a strict standard-base64 decoder that rejects the URL-safe `-` / `_`
// substitutions. The decoded width is exactly 32 bytes, which matters for
// consumers that require a precise key length: an AES-256 file-encryption
// key, for instance, needs exactly 32 bytes — hex would decode to 48 and
// the base64url alphabet would be rejected by the standard decoder.
const EncodingBase64Std Encoding = "base64std"

// EncodingArgon2id marks a placeholder whose generated value is an
// argon2id PHC hash string, not the secret itself. Unlike the
// reversible encodings above, this is a one-way derivation: wdm draws a
// strong random plaintext, persists only the PHC in `.env`, and surfaces
// the plaintext to the operator exactly once. The consumer
// (Vaultwarden's ADMIN_TOKEN) accepts either a plaintext token or an
// argon2id PHC; with a PHC the operator authenticates with the original
// plaintext. [GenerateSecret] deliberately does NOT handle this encoding
// — it has no single return value to give — so callers use
// [GenerateArgon2idCredential], which returns the plaintext and PHC as a
// pair.
const EncodingArgon2id Encoding = "argon2id"

// secretEntropyBytes is the raw entropy width drawn from [entropy]
// before encoding. the confirmation ruleslocks this at 32 bytes (256
// bits) for every generated secret regardless of encoding; 43 chars
// of base64url, 44 chars of standard base64 (with padding), and 64 chars
// of hex all decode back to this width.
// Changing it breaks the length invariants the tests assert against — the
// reversible encodings derive their output length from this value via
// `base64.RawURLEncoding.EncodedLen`, `base64.StdEncoding.EncodedLen`, and
// `hex.EncodedLen`.
const secretEntropyBytes = 32

// entropy is the package-private [io.Reader] seam [GenerateSecret]
// draws raw entropy from. Production code reads from
// [crypto/rand.Reader], which the runtime backs with the host kernel
// CSPRNG (PRD §11, §12). Tests inject a deterministic or
// failure-injecting reader via `SwapEntropyForTest` (defined in
// export_test.go — never compiled into the production binary, as the
// file carries the `_test.go` suffix). The variable is unexported so
// the test seam is the only legitimate mutation path; production
// callers never touch it.
// The type is inferred from [crypto/rand.Reader] (declared in the
// standard library as `var Reader io.Reader`) — an explicit io.Reader
// ascription would trip revive's var-declaration check. The seam's
// interface contract is still pinned by [SwapEntropyForTest], which
// takes an [io.Reader] parameter.
// failure: when [io.ReadFull] returns an error, [GenerateSecret]
// surfaces it as a typed `*types.Error` and NEVER retries with a
// non-cryptographic fallback (e.g. a seeded PRNG).
var entropy = rand.Reader

// GenerateSecret draws [secretEntropyBytes] bytes of entropy from the
// package-private [entropy] reader and encodes them according to enc.
// Returns:
//   - [EncodingBase64URL]: a 43-character raw-base64url string with no
//     padding; character set [A-Za-z0-9_-].
//   - [EncodingBase64Std]: a 44-character standard-base64 string with
//     padding; character set [A-Za-z0-9+/] plus a trailing `=`.
//   - [EncodingHex]: a 64-character lowercase hex string; character set
//     [0-9a-f].
//
// Ordering contract: the encoding is validated BEFORE any entropy is
// consumed, so an unknown [Encoding] always surfaces as the
// programmer-error path below even when the entropy seam would also
// fail. A caller-side typo can therefore never be masked by a
// transient CSPRNG outage and reported as an exit-code 1 runtime
// failure. If both failure modes coexist, the unknown-encoding error
// wins and entropy is never read.
// Errors:
//   - On an unknown encoding (including the zero value ""), returns a
//     plain [fmt.Errorf] error. This is a programmer error (callers
//     pass an [Encoding] constant from this package or, later, a cast
//     from a catalog-validated string), not a runtime failure, so it
//     deliberately carries no [types.ErrorCode]; the exit-code mapping
//     treats it as a generic failure if it ever escapes a test.
//     Validated first per the ordering contract above.
//   - On entropy read failure ([io.ReadFull] short read, kernel CSPRNG
//     unavailable, injected test failure such as [io.ErrUnexpectedEOF]),
//     returns a `*types.Error` with [types.ErrCodeGeneric] wrapping the
//     cause so cmd/wdm maps it to exit code 1 and the
//     JSON envelope carries a stable user-visible message. NEVER falls
//     back to a weaker entropy source.
//
// Concurrency: safe for concurrent use — [crypto/rand.Reader] is
// documented as safe for concurrent reads. Tests that swap the seam to
// a non-thread-safe reader (e.g. [bytes.Reader]) MUST NOT call
// `t.Parallel`.
func GenerateSecret(enc Encoding) (string, error) {
	// Validate the encoding (and select its encoder) BEFORE consuming
	// entropy — see the ordering contract above. One switch handles
	// both so the validation set and the encode set are visibly
	// identical; a dual-switch form would need an unreachable arm, and
	// `forbidigo` blocks a `panic` in this package.
	var encode func([]byte) string
	switch enc {
	case EncodingBase64URL:
		encode = base64.RawURLEncoding.EncodeToString
	case EncodingBase64Std:
		encode = base64.StdEncoding.EncodeToString
	case EncodingHex:
		encode = hex.EncodeToString
	default:
		return "", fmt.Errorf("security.GenerateSecret: unknown encoding %q", string(enc))
	}

	buf := make([]byte, secretEntropyBytes)
	if _, err := io.ReadFull(entropy, buf); err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"entropy source unavailable",
			"retry the operation; if it persists, ensure the host kernel CSPRNG (/dev/urandom on Linux, getentropy(2) on macOS) is reachable",
			err,
		)
	}
	return encode(buf), nil
}

// argon2id PHC parameters. These pin the cost/parallelism the PHC string
// advertises and the hash/salt widths drawn. They are fixed (not
// catalog-tunable) so every wdm-minted PHC is uniform and the format the
// finish-screen documents stays stable. The values match the PHC literal
// the catalog template embeds: m=65536 KiB (64 MiB) memory, t=3 passes,
// p=4 lanes, a 16-byte salt, and a 32-byte derived key.
const (
	argon2idTimeCost   uint32 = 3
	argon2idMemoryKiB  uint32 = 65536
	argon2idThreads    uint8  = 4
	argon2idKeyBytes   uint32 = 32
	argon2idSaltBytes         = 16
	argon2idPHCVersion        = argon2.Version // 19, the only version x/crypto emits
)

// GenerateArgon2idCredential mints a one-time admin credential: a strong
// random plaintext and the argon2id PHC hash of that plaintext. wdm
// persists only the PHC (a one-way derivation) in `.env` and surfaces the
// plaintext to the operator exactly once (PRD §24). The PHC follows the
// PHC string format:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<rawstd-b64 salt>$<rawstd-b64 hash>
//
// Both the salt and the derived hash are base64-encoded with
// [encoding/base64.RawStdEncoding] (standard alphabet, no padding) as the
// PHC specification and Vaultwarden's parser require — this is distinct
// from the base64url alphabet [GenerateSecret] uses for `.env` token
// values.
// The plaintext reuses the same 32-byte crypto/rand + base64url draw as
// [EncodingBase64URL] so it is a 43-character URL-safe token; the operator
// pastes it verbatim into the /admin login.
// Concurrency and entropy-failure behavior mirror [GenerateSecret]: both
// the plaintext and the salt are drawn from the package-private [entropy]
// seam, and a short read surfaces as a `*types.Error` with
// [types.ErrCodeGeneric] — wdm NEVER falls back to a weaker entropy
// source. The plaintext is drawn first so a salt-read failure cannot leave
// a usable plaintext stranded.
func GenerateArgon2idCredential() (plaintext, phc string, err error) {
	plaintext, err = GenerateSecret(EncodingBase64URL)
	if err != nil {
		return "", "", err
	}

	salt := make([]byte, argon2idSaltBytes)
	if _, err := io.ReadFull(entropy, salt); err != nil {
		return "", "", types.WrapError(
			types.ErrCodeGeneric,
			"entropy source unavailable",
			"retry the operation; if it persists, ensure the host kernel CSPRNG (/dev/urandom on Linux, getentropy(2) on macOS) is reachable",
			err,
		)
	}

	hash := argon2.IDKey([]byte(plaintext), salt, argon2idTimeCost, argon2idMemoryKiB, argon2idThreads, argon2idKeyBytes)
	phc = fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idPHCVersion,
		argon2idMemoryKiB,
		argon2idTimeCost,
		argon2idThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return plaintext, phc, nil
}
