# Security Policy

## Reporting

Please report security issues privately to the maintainers. Do not open public issues for active vulnerabilities until a fix is available.

Use one of these private channels:

- Email: `webnestify@webnestify.cloud`
- GitHub private vulnerability reporting, if available on the repository's **Security** tab.

Include the affected version, a clear description, reproduction steps or proof of concept if you have one, and any known impact. Do not include active exploit details in public issues, pull requests, or discussions before coordinated disclosure.

## Coordinated Vulnerability Disclosure

The maintainers follow coordinated vulnerability disclosure for confirmed security issues:

1. Acknowledge receipt within 7 calendar days.
2. Provide an initial assessment within 14 calendar days when enough information is available.
3. Work with the reporter on a fix, mitigation, and disclosure timeline based on severity and exploitability.
4. Publish vulnerability information after a fix or mitigation is available, unless there is a clear safety reason to delay disclosure.

Confirmed vulnerabilities are published through GitHub Security Advisories when appropriate. Release notes or changelog entries may reference the advisory after disclosure. If no vulnerabilities have been confirmed for a release, there may be no advisory to publish.

## Secrets and Credentials

Project secrets must stay out of the repository. Do not commit API tokens, passwords, private keys, signing keys, `.env` files, or other credentials. Local-only material belongs outside Git-tracked paths or in ignored local paths.

Repository and release credentials are stored in GitHub as repository or organization secrets. Access is limited to the maintainer account and to GitHub Actions jobs that need the credential for a specific task. The production Ed25519 release signing key is available only to the tag-gated signed-release job; branch builds, pull requests, and manual dry-runs do not receive it.

Rotate a project secret when it may have been exposed, when the maintainer account or repository settings change in a way that affects access, or when the secret's purpose changes. Release signing key rotation requires updating the GitHub Actions secret and committing the matching public key in `internal/release/signing_public_key.pem` with the next signed release.

## Destructive Operations

`wdm`'s destructive operations are destructive-but-fail-closed and never delete user data. `wdm uninstall` tears down every managed app with `docker compose down --rmi all`, removes the `wdm`-created Docker networks best-effort (declared `external` in the rendered compose, so `down` never removes them; an unremovable one is reported with the manual `docker network rm` command and never aborts the run), and then removes `wdm`'s own footprint (config, data, and state directories and the binary). It never runs `docker compose down -v`: named volumes and every `~/docker/<app>/` stack directory are preserved. Its scope is wdm-managed apps and wdm's own footprint only, never a system-wide Docker prune. If any stack fails to tear down, the operation aborts before removing any footprint, leaves `wdm` installed, and reports the failed stacks. The same no-`-v`, data-preserving guarantee applies to `wdm apps remove` and `wdm apps delete`: neither ever deletes named volumes or on-disk data. `wdm apps delete` additionally removes the app's `wdm`-created Docker networks best-effort (an unremovable one is reported with the manual `docker network rm` command and never aborts the deletion); `wdm apps remove` leaves the networks in place.

## Dependency and Static-Analysis Findings

The project treats Software Composition Analysis (SCA), Static Application Security Testing (SAST), and license findings as release-blocking inputs.

SCA inputs include `go mod verify`, `go mod tidy -diff`, Dependabot alerts, and `govulncheck`. A failing `go mod verify`, unexpected module drift, or a `govulncheck` finding in reachable code blocks merge and release until fixed or documented as non-exploitable.

SAST inputs include CodeQL code scanning and security-relevant linter findings. Critical SAST findings block merge and release. High and medium findings must be fixed, downgraded with written rationale, or tracked before the next stable release, based on exploitability and affected code path.

Dependency licenses must be compatible with the MIT-licensed project and with redistribution through GitHub Releases. A direct or runtime dependency with an incompatible or unknown license blocks release until resolved or replaced.

No official release may be cut while required security checks are failing or while an unresolved release-blocking SCA, SAST, malicious-dependency, or license finding affects the release. Suppressions must be narrow, documented, and tied to a specific finding.

If a vulnerability is present in a component but does not affect `wdm`, document the non-exploitability decision as a VEX statement. The VEX record must identify the component, version, vulnerability identifier, affected `wdm` version or commit range, status, and justification. VEX records may be published through a GitHub Security Advisory, release notes, or a repository security document when needed.

## Release Verification

`wdm` release artifacts are signed with [cosign](https://github.com/sigstore/cosign)/Sigstore using keyless signing through GitHub Actions OIDC. In-product verification is Go-native; the human verification command below uses `cosign` and pins the same trust anchors the product code pins.

### Trust anchors

Verification pins the expected repository identity, issuer, and release workflow identity. These anchors are the human-readable quotation of the policy pinned in code at `internal/release/trustpolicy.go` — the constants there are authoritative and these values must match them verbatim:

| Anchor | Value |
|---|---|
| OIDC issuer | `https://token.actions.githubusercontent.com` |
| Source repository | `wnstify/wdm` (`https://github.com/wnstify/wdm`) |
| Release workflow | `.github/workflows/release.yml` |
| Certificate identity (tag releases) | `https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/tags/<tag>` |

The certificate identity for a tagged release is the repository URL and release workflow path joined to the tag ref, for example `https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/tags/v1.0.0`.

### Scope

Real signed releases are produced only by pushing a `v*` tag to the public `wnstify/wdm` repository. Manual `workflow_dispatch` runs and branch builds are non-publishing verification runs: they do not mint release signatures and do not create GitHub Releases. The commands below describe how to verify the public release artifacts of `wnstify/wdm`.

### Artifacts

A release publishes the following assets (names locked in `internal/release/artifacts.go`):

| Artifact | Purpose |
|---|---|
| `wdm-linux-amd64` | the linux/amd64 binary (payload) |
| `catalog-stable.tar.gz` | the stable-channel catalog bundle (payload) |
| `attestation.json` | SLSA provenance attestation, multi-subject (payload) |
| `wdm-linux-amd64.intoto.jsonl` | same SLSA provenance as a genuine in-toto JSONL Statement (additive; not in `SHA256SUMS`) |
| `wdm-linux-amd64.spdx.json` | SPDX 2.3 JSON SBOM of the binary (payload) |
| `SHA256SUMS` | GNU-coreutils checksums over the payload files only |
| `SHA256SUMS.sig` | detached Ed25519 signature over `SHA256SUMS` (in-product) |
| `SHA256SUMS.cosign.bundle` | keyless cosign bundle over `SHA256SUMS` (human/CI) |

Trust chains from the signed `SHA256SUMS` outward: verify `SHA256SUMS` once, then the checksums verify every payload (binary, catalog bundle, attestation, SBOM). `SHA256SUMS` never lists itself or its own signatures. `wdm-linux-amd64.intoto.jsonl` is published additively as the SLSA provenance in the standard in-toto JSONL form supply-chain tooling expects; it carries the same signed Statement as `attestation.json` (which is in `SHA256SUMS`), so it is intentionally not a `SHA256SUMS` payload and is not consumed by the in-product verifier.

### Human verification (cosign)

Download the release assets into one directory, then verify in this order. Set `TAG` to the release tag (e.g. `v1.0.0`).

**1. Verify `SHA256SUMS` with the keyless cosign bundle.** Pin the trust anchors above:

```sh
cosign verify-blob \
  --bundle SHA256SUMS.cosign.bundle \
  --certificate-identity "https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  SHA256SUMS
```

The `--certificate-identity` value is `RepositoryURL + "/" + ReleaseWorkflowPath + "@refs/tags/" + <tag>` — the tag-form certificate SAN. The `--certificate-oidc-issuer` value is the `OIDCIssuer` constant. Both are the anchors in the table above and the constants in `internal/release/trustpolicy.go`.

**2. Verify the payloads against the now-trusted `SHA256SUMS`.** This covers the binary, catalog bundle, attestation, and SBOM in one step:

```sh
sha256sum -c SHA256SUMS
```

**3. Verify the SLSA provenance attestation** of the binary and catalog bundle against the same workflow identity and issuer. `attestation.json` is a multi-subject statement covering both `wdm-linux-amd64` and `catalog-stable.tar.gz`:

```sh
cosign verify-blob-attestation wdm-linux-amd64 \
  --bundle attestation.json \
  --new-bundle-format \
  --type slsaprovenance1 \
  --certificate-identity "https://github.com/wnstify/wdm/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

`--new-bundle-format` is required to parse the new-format Sigstore bundle that `actions/attest-build-provenance` emits (cosign v2.6+ accepts it; it is the default and a deprecated no-op in cosign v3). `--type slsaprovenance1` selects the SLSA **v1.0** predicate (`slsaprovenance` without the suffix is the v0.2 shorthand and does not match).

Repeat with `catalog-stable.tar.gz` to verify the catalog bundle against the same attestation.

### In-product verification

`wdm` verifies releases itself, Go-native, with no external verifier (decision #56). Instead of the keyless cosign path, the binary verifies a **detached Ed25519 signature** (`SHA256SUMS.sig`, raw 64-byte signature) over the exact `SHA256SUMS` bytes against a **long-lived public key embedded in `internal/release`**, then verifies each payload against the SHA-256 checksums and verifies `attestation.json` by digest. This embedded-key path is independent of the keyless cosign identity above; both cover the same `SHA256SUMS`, so the human and the product reach the same trust decision by different routes.

### Key and identity rotation

- **Trust identity (keyless / cosign path).** The pinned anchors — issuer, source repository, and release workflow path — are the constants in `internal/release/trustpolicy.go`. Any change to the repository, the OIDC issuer, or the release workflow path is an identity rotation: update the trust policy constants and this document in the same change, and the new anchors take effect with the next signed release.
- **Embedded signing key (in-product path).** The production Ed25519 private key lives as a CI secret and the matching public key is embedded from `internal/release/signing_public_key.pem`. Rotation replaces the embedded public key in a signed release. Non-publishing verification runs sign only with ephemeral in-run keys, never the production key.
