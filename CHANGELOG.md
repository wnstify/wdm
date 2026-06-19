# Changelog

All notable changes to this project are documented in this file. The format
follows Keep a Changelog, and the project follows Semantic Versioning.

## Unreleased

### Added
- Labeled every `wdm`-created Docker network at install with the Webnestify
  ownership labels `wdm.managed=true` and `wdm.app=<app-id>` (one `docker network
  create` per network, labels in a fixed order). Only newly-created networks are
  labeled; a network reached through the "already exists" reconciliation path is
  not re-labeled.
- `wdm apps delete` now removes the app's `wdm`-created Docker networks
  best-effort, after `docker compose down` and before the stack files are
  deleted. A network already gone counts as removed; one that cannot be removed
  is reported with the exact `docker network rm <name>` command and never aborts
  the deletion. Named volumes and all on-disk data are still never deleted, and a
  reinstall recreates the networks. Safe `wdm apps remove` is unchanged and still
  leaves the networks in place.
- Added an "Uninstall wdm" action to the dashboard and a top-level `wdm uninstall`
  command that tears down every managed app with `docker compose down --rmi all`
  (removing containers and images) and then removes `wdm`'s own
  footprint, including the binary. After every app is down it also removes the
  `wdm`-created Docker networks (declared `external` in the rendered compose, so
  `docker compose down` never removes them), best-effort: a network already gone
  counts as removed, and one that cannot be removed is reported with the exact
  `docker network rm <name>` command to run manually and never aborts the
  uninstall. Named volumes and every `~/docker/<app>/` stack directory are kept;
  this is never `docker compose down -v` and no user data is deleted. Scope is
  wdm-managed apps and wdm's footprint only. It is fail-closed: if any stack fails
  to tear down it aborts before removing anything, leaves `wdm` installed, lists
  the failed stacks, and exits nonzero. The `--yes` flag accepts the destructive
  confirmation without prompting.
- Added a "Stop all apps" action to the dashboard and a `wdm apps stop-all`
  command that runs `docker compose stop` against every running managed stack at
  once. It targets only apps with a running container, preserves all data
  (containers, networks, and named volumes stay defined), continues on error so
  one unreachable stack does not block the rest, and exits nonzero when any
  targeted stack fails.
- Added a `stopped` runtime state for apps whose managed containers all exist
  but are not running (for example after `docker compose stop`). It reports
  `needs_attention: false` so a cleanly stopped app is no longer shown as
  needing attention; `needs_attention` is reserved for genuine trouble.

### Changed
- `wdm apps stop-all` now stops only running apps. Stacks that are already
  stopped are skipped and reported as already stopped (in `--json` under
  `already_stopped` and as a short note in plain output). When no app is
  running, it prints "No running apps to stop." and exits `0` without prompting,
  instead of confirming and "stopping" apps that were already down.
- The dashboard "Check my apps" list and `wdm apps list` now report each app's
  live runtime state (running / stopped / needs attention / removed) from real
  Docker container state instead of always showing "running". `wdm apps list
  --json` entries gain a `state` field and a populated `needs_attention` flag,
  and the plain output gains a trailing tab-separated state column.

## v1.0.3 - 2026-06-18

### Fixed
- Treat catalog resource bands as guidance and Docker limit caps instead of
  blocking installs when the host is below the curated minimum profile.
- Fixed Stoat startup ordering so the `crond` service no longer waits on the
  `mongo-init` migration seeding job during install.

## v1.0.2 - 2026-06-18

### Fixed
- Advanced the stable catalog version so existing installs can receive the
  Stoat LiveKit UID/GID template fix through `wdm catalog update`.

### CI
- Added a catalog freshness guard so catalog or template changes must advance
  the stable catalog `generated_at` timestamp before release.

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
