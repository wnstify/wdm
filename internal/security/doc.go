// Package security provides the security primitives shared by other
// internal packages: a [Redactor] interface for stripping sensitive
// substrings before they reach logs, JSON envelopes, or error messages
// (PRD §11, §24); path-safety helpers that validate untrusted file
// paths before any I/O (PRD §12, §13); cryptographic secret generation
// backed by crypto/rand (PRD §11, §12); and file-mode helpers
// enforcing owner-only permissions on secret-bearing artifacts
// (PRD §11, §38).
// [NoopRedactor], and the path validators ([RejectUnsafeRoot],
// [EnsureWithinRoot], [SafeJoin]). added the active
// pattern-and-literal redactor, constructible via [NewActiveRedactor]
// (the concrete type is unexported so a zero-value bypass of the
// constructor is structurally impossible). wired that
// redactor into internal/logging and engine construction per
// before any secret-generation path landed. added
// [GenerateSecret] backed by crypto/rand with a package-private
// io.Reader seam swappable from tests via the SwapEntropyForTest
// helper in export_test.go (test-only, never linked into the
// production binary); the encoder is selected and validated BEFORE
// entropy is read so an unknown [Encoding] always surfaces the
// programmer-error path even when entropy would also fail.
// ([SecretFileMode] = 0o600) on every secret-bearing write, plus
// [RejectInsecureParent] for parent-directory hardening.
// PACKAGE INVARIANT — NO ONE-SHOT WRITE HELPER:
// This package deliberately exposes no one-shot
// WriteSecretFile(path, data) error helper. [CreateSecretFile] returns
// an open *os.File for caller-driven write+fsync+close composition and
// does NOT write/fsync/close itself, because atomic write composition
// belongs to
// internal/state at, its first production consumer.
// The absence of the helper IS the contract, enforced at the AST level
// by an internal package test that rejects any top-level
// WriteSecretFile function declaration.
// The release-verification surface
// is deferred to (PRD §22, §23).
// Import boundary: per and the
// internal-security-leaf depguard rule at .golangci.yml:188-194,
// internal/security imports only the standard library and pkg/types
// (used to wrap returns from [GenerateSecret], [CreateSecretFile], and
// [RejectInsecureParent] with the typed *types.Error /
// [types.ErrorCode] contract). It must not depend on pkg/engine,
// cmd/wdm, internal/tui, internal/cli, or any other internal/*
// sibling — the package is a leaf.
// Public surface roll-call:
//   - [Redactor] — interface for sensitive-substring removal
//   - [NoopRedactor] — passthrough Redactor retained for explicit
//     no-op test callers
//   - [RedactedPlaceholder] — stable substring substituted for every
//     redacted value
//   - [NewActiveRedactor] — constructor returning a [Redactor] that
//     scrubs registered literal secrets plus structural assignment
//     forms (env / JSON / HTTP Bearer / URL credentials); the concrete
//     type is unexported so this is the only construction path
//   - [Encoding] — string enum naming a secret encoding format
//   - [EncodingBase64URL] — 43-char raw-URL-safe base64, no padding
//   - [EncodingBase64Std] — 44-char standard base64 with padding
//   - [EncodingHex] — 64-char lowercase hexadecimal
//   - [GenerateSecret] — draws 32 raw bytes from crypto/rand
//     (swappable via the test-only entropy seam in export_test.go) and
//     encodes per the supplied [Encoding]
//   - [SecretFileMode] — canonical Unix mode bits for secret-bearing
//     files (0o600 — owner read+write, no group, no world)
//   - [ValidateSecretFileMode] — strict-equality check against
//     [SecretFileMode]
//   - [CreateSecretFile] — opens a path with O_CREATE|O_WRONLY|O_EXCL
//     at [SecretFileMode] plus post-open Chmod for umask defense;
//     returns the open *os.File for caller-driven write composition
//     (does NOT write/fsync/close — see the no-one-shot-write
//     invariant above)
//   - [RejectInsecureParent] — rejects parent directories with
//     group-write (0o020) or world-write (0o002) bits, including
//     sticky-on-world-writable like /tmp at 1777
//   - [ErrUnsafePath] — sentinel returned when a path is rejected outright
//   - [ErrPathEscape] — sentinel returned when a path escapes its root
//   - [RejectUnsafeRoot] — rejects /, common system directories, and
//     their descendants
//   - [EnsureWithinRoot] — verifies a candidate path lies under a root
//   - [SafeJoin] — joins a trusted root with an untrusted relative
//     subpath, rejecting parent traversal and absolute re-injection
//   - [ResolveContainedPath] — resolves root and candidate through
//     EvalSymlinks on both sides and confirms containment, the seam
//     destructive verbs run before os.RemoveAll
//   - [IsSuspiciouslyShallowPath] — reports a near-root path, the
//     defense-in-depth backstop those verbs apply before removal
package security
