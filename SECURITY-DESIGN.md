# Security Design

This document describes the actors, actions, trust boundaries, external interfaces, and main security risks for `wdm`.

## Scope

`wdm` is a local terminal application for managing a curated set of Docker Compose stacks. It has two user interfaces over one engine:

- an interactive Bubble Tea TUI;
- a scriptable Cobra CLI.

`wdm` does not run a server, expose an HTTP API, or accept remote users. Its main security boundary is the local machine: a normal user runs `wdm`, and `wdm` talks to Docker, the filesystem, registries, and GitHub release/catalog endpoints.

## Actors

| Actor | Role | Trust level |
| --- | --- | --- |
| End user | Runs `wdm`, chooses apps, confirms destructive operations, and owns local stack data. | Trusted local operator. |
| Local OS user account | Provides filesystem permissions, XDG paths, terminal, and Docker group access. | Trusted only as the invoking user. |
| Docker daemon and Compose plugin | Create networks, containers, volumes, and stack lifecycle operations. | Privileged local service reached through typed commands. |
| Managed application containers | Run third-party application images selected by the curated templates. | Untrusted workload after deployment. |
| Container registries | Provide image tags and metadata for update checks. | External service; responses are parsed and treated as untrusted input. |
| GitHub Releases | Hosts release assets, checksums, signatures, SBOM, and attestation bundles. | External service; artifacts must verify before use. |
| GitHub Actions | Builds release assets, signs manifests, produces provenance, and runs CI. | Trusted only through pinned workflows, scoped permissions, and release gates. |
| Sigstore services | Provide OIDC-backed signing identity, transparency log, and attestation verification material. | External trust service pinned by issuer and workflow identity. |
| Project maintainer | Reviews changes, controls repository settings, rotates signing material, and publishes releases. | Trusted project operator. |

## Main Actions

| Action | Initiator | Security controls |
| --- | --- | --- |
| Start TUI | End user | Refuses root/sudo execution; uses one engine API shared with CLI. |
| List catalog apps | End user | Reads verified local catalog data. |
| Install app | End user | Renders only curated templates, writes only under the selected stack directory, confirms required choices, and runs Docker through typed argv wrappers. |
| Update app | End user | Checks registry metadata, preserves volumes, creates config backups, and only touches managed stacks. |
| Restart app | End user | Runs Compose operations for a managed stack only. |
| Remove app | End user | Stops managed containers but never runs `docker compose down -v`; volumes are preserved. |
| View logs/status | End user | Reads Docker state and logs, with secret redaction before output sinks. |
| Backup/restore config | End user | Backs up generated configuration files, not application data volumes. |
| Update catalog | End user or CI-provided release asset | Requires signed/checksummed catalog artifacts and fails closed on verification failure. |
| Self-update binary | End user | Verifies checksums, detached Ed25519 signature, and attestation before staging a replacement binary. |
| Publish release | Maintainer through Git tag | Runs only on `push` to `refs/tags/v*` in `wnstify/wdm`; signs `SHA256SUMS`, emits provenance, and creates a GitHub Release. |
| Run CI | GitHub Actions | Uses pinned actions, read-only default token permissions, CodeQL, govulncheck, lint, tests, and protected-branch required checks. |

## External Interfaces

| Interface | Direction | Purpose |
| --- | --- | --- |
| Terminal stdin/stdout/stderr | user -> `wdm`, `wdm` -> user | TUI input, CLI flags, human output, and JSON output. |
| Local filesystem | `wdm` -> user-owned paths | XDG config/state/cache, verified catalog data, logs, backups, and managed stack directories under `~/docker/<app>/`. |
| Docker CLI / Compose plugin | `wdm` -> Docker daemon | Container, network, volume, and Compose lifecycle operations. |
| Docker Registry HTTP API | `wdm` -> registries | Anonymous image metadata checks for app updates. |
| GitHub Releases API/assets | `wdm` -> GitHub | Release and catalog artifact download. |
| Sigstore / Rekor / Fulcio material | `wdm` or human verifier -> trust services/artifacts | Keyless signing identity and transparency/provenance verification. |
| GitHub Actions | repository -> CI runners | Build, test, security scan, release asset creation, signing, and publication. |

`wdm` does not expose a network listener. Managed applications may expose their own ports according to their templates; by default templates bind to localhost unless a public listener is required by the application.

## Trust Boundaries

The important trust boundaries are:

- CLI/TUI input crossing into the engine;
- template variables crossing into rendered Compose files;
- stack paths crossing into filesystem writes;
- Docker command arguments crossing into Docker/Compose;
- registry responses crossing into update decisions;
- release/catalog artifacts crossing into local execution or catalog state;
- CI jobs crossing into release signing and publication.

Each boundary has a narrow control:

- typed request structs and engine APIs for UI input;
- curated templates and schema validation for template data;
- symlink/path checks before writes;
- typed argv wrappers instead of shell command strings;
- parsed registry responses with conservative update behavior;
- Ed25519, SHA-256, Sigstore, and attestation verification before trusting artifacts;
- tag-only release gates and scoped GitHub Actions permissions.

## Threat Modeling and Attack Surface Analysis

This model identifies the main actors, actions, external interfaces, trust boundaries, and security controls for `wdm`. The assessment focuses on critical paths: local command execution, filesystem writes, Docker operations, template rendering, release verification, catalog updates, self-update, and release publishing.

Maintainers update this analysis when the project adds a new external interface, changes the release or verification path, adds a privileged operation, or changes how templates, filesystem paths, Docker commands, or release artifacts cross trust boundaries.

The most likely and impactful risks are:

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Malicious or tampered release asset | User runs attacker-controlled binary or catalog. | `SHA256SUMS` is signed, payload hashes are verified, provenance is checked, and verification fails closed. |
| Compromised release workflow path | Attacker publishes assets or mints misleading provenance. | Release job runs only for `refs/tags/v*` in `wnstify/wdm`, actions are pinned, token permissions are scoped, and protected tag rules require signed tags. |
| Command injection through Docker operations | Local command execution outside intended Docker operation. | Docker and system operations are built as typed argv arrays, not shell strings. |
| Path traversal or symlink write outside stack directory | Overwrite unrelated user files. | Paths are resolved and scoped before writes; managed operations stay under the selected stack directory. |
| Accidental application-data deletion | Loss of user volumes. | Remove operations never run `docker compose down -v`; volume deletion is not part of normal removal. |
| Secret disclosure in logs or JSON | Credentials exposed to terminal, logs, or automation. | Secrets are generated and redacted before output sinks. |
| Untrusted pull request gaining privileged CI credentials | Secret or release-key exposure. | The release signing secret is only used in the tag-gated release job; pull request workflows do not run the release publisher. |
| Vulnerable third-party dependency or container image | Known vulnerability reaches users. | Go dependencies are tracked in `go.mod`/`go.sum`, Dependabot is enabled, govulncheck and CodeQL run in CI, and app image updates are surfaced through update checks. |

Security assessment is continuous: CI runs unit tests, linting, govulncheck, CodeQL, and Scorecard; releases require signed artifacts and provenance; repository rules protect `main` with required checks.

## Maintainer Responsibilities

The project maintainer is responsible for:

- reviewing code and release workflow changes;
- keeping branch and tag protection active;
- rotating release signing material when needed;
- triaging private vulnerability reports;
- publishing GitHub Security Advisories for confirmed vulnerabilities when disclosure is appropriate.

## Collaborator Access Review

Before granting write, admin, release, secret, or ruleset access, the maintainer reviews the collaborator's identity, need, requested permission level, and expected duration of access. Access must use the least privilege that allows the work to be done.

Escalated access is removed when it is no longer needed. Release signing secrets and repository security settings remain maintainer-controlled unless a future governance change documents a different owner and review process.

## Two-Maintainer Review Posture

The project has two human maintainers with Write access (the `webnestify-dev` team). Independent human approval is required: branch-protection rules on `dev` and `main` require one approving review from the `webnestify-dev` team, and self-approval does not count, so the non-author maintainer reviews every pull request. The latest reviewable push must be approved by someone other than the person who pushed it. The same rules apply to the owner's own pull requests, and merges into `dev` and `main` are restricted to the repository owner. The project does not use alternate accounts to simulate independent review.

Controls are independent two-person review, public pull requests, signed commits, DCO signoff, required CI (`test`, `lint`, `govulncheck`), CodeQL, protected `main`, protected `dev`, protected release tags, signed release artifacts, and public issue/security reporting.
