# Changelog

All notable changes to this project are documented in this file. The format
follows Keep a Changelog, and the project follows Semantic Versioning.

## v1.0.1 - 2026-06-18

### Added
- Added the verified one-line installer script and installer tests.
- Seeded the stable catalog during installer bootstrap so a fresh install can
  start with a local verified catalog.

### Fixed
- Fixed Stoat installs on systems where the installing user's UID/GID is not
  `1000:1000`; LiveKit now runs as the installing user so it can read the
  generated `0600` `livekit.yml` config.
- Preserved existing catalog data when the installer seeds a catalog, with
  rollback on partial promotion failures.
- Preserved existing installer directory permissions.

### Security
- Added DCO enforcement, CodeQL analysis, OpenSSF Scorecard, Codecov reporting,
  and public security governance documentation.

## v1.0.0 - 2026-06-17

The first public release of `wdm` (Webnestify Docker Manager), a Go terminal
application combining a Bubble Tea TUI and a CLI for installing, updating, and
checking a curated set of Docker Compose self-hosting templates. It targets
Linux amd64 (Debian 12/13 and Ubuntu 24.04/26.04) with Docker 20.10+ and
Compose V2.

### Added
- Install, update, status, remove, and self-update workflows for curated
  Docker Compose stacks, from both the TUI and the CLI.
- Per-stack locking plus generation of each stack's `.env` and Compose files.
- Automatic secret generation with redaction: secrets never reach logs or
  JSON output.
- Pre-change backups and a cancellation-safe rollback when an install fails.
- Signed-and-verified catalog and release artifacts that fail closed on a
  missing or invalid signature, checksum, or attestation.
- Runs without root or sudo, and never destroys volumes on remove.

### Catalog
Nineteen curated apps ship at launch (catalog schema version 2): Uptime Kuma,
FreshRSS, Jellyfin, n8n, Navidrome, Open WebUI, SerpBear, qBittorrent,
Syncthing, Baserow, Nextcloud, DocuSeal, Vaultwarden, Authentik, MeshCentral,
WireGuard + AdGuard Home, Zulip, Dockhand, and Stoat.

### Security and verification
Each release publishes seven assets:

- `wdm-linux-amd64` — the linux/amd64 binary.
- `catalog-stable.tar.gz` — the stable-channel catalog bundle.
- `attestation.json` — multi-subject SLSA provenance attestation.
- `wdm-linux-amd64.spdx.json` — SPDX 2.3 JSON SBOM of the binary.
- `SHA256SUMS` — checksums over the payload files.
- `SHA256SUMS.sig` — detached Ed25519 signature over `SHA256SUMS` for
  in-product verification.
- `SHA256SUMS.cosign.bundle` — keyless cosign/Sigstore bundle over
  `SHA256SUMS` for human and CI verification.

See `SECURITY.md` for the full verification procedure.

### Known issues
The following end-to-end behaviors are not yet covered by the automated install
smoke matrix and are scheduled for validation after v1:

- WireGuard + AdGuard Home: the public peer (VPN) tunnel path.
- Open WebUI: live model conversation.
- Stoat: voice, gifbox, and web-push features.
