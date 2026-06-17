// Package registry is the Go-native OCI / Docker Registry v2 metadata
// client for app-image update checks (PRD §14, §20, §35). Given an
// image reference (registry/repository:tag) it resolves
// read-only registry metadata — a tag's manifest digest and the
// repository's available tags so the app-update planner can show
// tag/digest update candidates through the EXISTING app-update
// surface. It applies no update and mutates no app or Docker state (PRD §35
// "no app mutation through catalog update").
// # Go-native only
// Every registry request is a Go [net/http] call, with JSON parsed by
// [encoding/json] and digests computed with [crypto/sha256]. The package
// NEVER shells out to docker, crane, skopeo, or any external binary
// ("Trust architecture conventions": "No Docker registry shell-outs",
// "Go-native product verification"). The internal-registry-boundary
// depguard rule in .golangci.yml enforces this by denying internal/docker
// and every other sibling import.
// # Anonymous, public posture
// This client performs anonymous public metadata checks ONLY. It stores no
// registry credentials and never sends a caller-supplied Authorization
// header. When a registry answers a metadata request with 401 and a Bearer
// WWW-Authenticate challenge, the client runs the anonymous token dance: it
// requests a token from the challenge realm WITHOUT any username or password
// (the public-pull flow Docker Hub, GHCR, and other v2 registries grant
// unauthenticated clients) and retries once. Private registries that require
// credentials are out of scope for and surface as a network failure,
// not a credential prompt.
// # Failure classes are distinct
// Per the "Network and trust failures are distinct" convention,
// EVERY transport, DNS, timeout, HTTP-status (including 429 rate-limit and
// 5xx), token-endpoint, size-cap, and malformed-response failure maps to a
// single typed [types.ErrCodeNetworkFailure] (exit 8) via [networkError].
// Bad CALLER input — a malformed or unsupported image reference — maps to
// [types.ErrCodeUsageValidation] (exit 2) via [usageError]. This client does
// NO trust verification, so it NEVER emits
// [types.ErrCodeVerificationFailed] (exit 3); checksum, signature, and
// attestation verification lives in internal/release.
// # Redaction
// The client carries no secrets, but the anonymous token dance holds a
// short-lived bearer token in memory. That token is never logged and never
// placed in an error message, [Error.Hint], or [Error.Cause]: error text
// names only the operation kind and HTTP status, never response bodies,
// header values, or token strings (PRD §24 redaction spirit).
package registry
