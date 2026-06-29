# Changelog

All notable changes to this project are documented in this file. The format
follows Keep a Changelog, and the project follows Semantic Versioning.

## v1.4.0 - 2026-06-29

### Added
- `wdm apps install --port HOST=NEW` (repeatable) remaps a conflicting loopback
  host port before the availability probe, so an install can proceed on a free
  port instead of aborting. Only single `127.0.0.1` ports are remappable; port
  ranges and public ports stay refused, and a remap never changes a binding's
  host IP. On a host-port conflict the install stays fail-closed by default, and
  under `--json` the error now carries the conflicting service, the busy port,
  and a deterministic suggested free port.

### Changed
- Updated build and CI dependencies: `golang.org/x/mod` 0.36.0 to 0.37.0 and the
  pinned GitHub Actions (`codeql-action` to v4.36.2, `setup-go` to 6.5.0,
  `attest-build-provenance` to 4.1.1).

## v1.3.2 - 2026-06-26

### Security
- Refuse to operate against a rootful Docker daemon. Before any state-changing
  mutation `wdm` now fails closed when the active daemon is running in rootful
  mode or its mode cannot be determined, with no override flag.

### Changed
- Render the dockhand socket-proxy source from the rootless Docker socket so the
  generated stack points at the correct per-user `docker.sock`.

### Fixed
- Share a single backup-history appender across the install, update, and
  reconfigure apply paths so backup records are written consistently.

## v1.3.1 - 2026-06-25

### Changed
- Internal-only maintenance release: no change to installed stacks or commands.
  The install pipeline was refactored into named, individually testable phases
  behind a thin orchestrator, and install, update, and reconfigure now freeze
  their plan into a read-only value before the shared write and deploy helpers
  run. Behavior is unchanged; the change adds substantial unit-test coverage and
  hardens the code against future regressions.

### Fixed
- An optional `bool` or `port` placeholder with no default and no supplied value
  now resolves to an empty value instead of failing, matching the existing
  string placeholder behavior. No catalog template currently uses such a
  placeholder, so this affects no installed app today.

## v1.3.0 - 2026-06-25

### Added
- AppFlowy is now in the stable catalog: a self-hosted, open-source alternative
  to Notion for notes, wikis, projects, and AI-assisted writing. The template
  runs the full self-hosting stack — PostgreSQL, a Redis-compatible cache, MinIO
  object storage, the GoTrue authentication service, the AppFlowy Cloud
  API/WebSocket server, a background worker, the web UI, the admin console, and
  an angie reverse proxy. The admin account is created on first run with a
  generated password stored in the stack's `.env`; SMTP and OAuth are optional
  and enabled by adding keys to `~/docker/appflowy/.env.user`.
- `wdm apps install --force` opt-in recovery for a stack directory left behind
  by a hard-killed install (for example a power loss mid-install), which
  otherwise blocks a reinstall. It is fail-closed: it removes the directory only
  after proving the stack is not running and its `.wdm.lock` is empty or corrupt
  (an interrupted install), refuses a properly managed stack (uninstall it
  instead), refuses a non-`wdm` directory that is not empty, and never deletes
  named Docker volumes.

## v1.2.1 - 2026-06-25

### Fixed
- `wdm apps update` for baserow, serpbear, and docuseal no longer fails closed
  with `placeholder "<APP>_DOMAIN" is absent from the existing .env`. Each
  template now persists its `*_DOMAIN` value as its own `.env` key (it was
  previously consumed only inside a derived URL line), so the update precheck
  can re-resolve it — the same class of fix already applied to vaultwarden in
  v1.2.0. stoat's `VAPID_PRIVATE_KEY`/`VAPID_PUBLIC_KEY` are persisted for the
  same reason.

### Security
- The stoat `VAPID_PRIVATE_KEY` placeholder is now marked `sensitive: true`, so
  a user-supplied web-push private key is value-redacted from logs, `view-env`,
  and error output and is covered by the non-secret leak-check. It stays a
  persisted `.env` string (the public key remains plain) so the update precheck
  still resolves it. The key is empty by default, so no prior release exposed
  one.

## v1.2.0 - 2026-06-24

### Added
- User-editable stacks: every managed stack now carries two user-owned files
  that wdm creates but never regenerates, so your changes survive `wdm update`.
  `.env.user` (0600) is a flat env file for adding new variables or overriding
  non-pinned values; it is merged into every service via `env_file`.
  `docker-compose.override.yml` (0644) is merged over the wdm base by native
  Compose, for structural changes such as adding services, volumes, networks,
  ports, or labels. The mental model is `.env.user` to add knobs,
  `docker-compose.override.yml` to change or restructure. Compose precedence
  (`environment:` over `env_file:`) means a value wdm pins in `environment:`
  (secrets and hardened config) cannot be overridden from `.env.user`; change a
  pinned value through the override's `environment:` instead.
- Added `wdm edit <app> --compose|--env [--print-path]`. It resolves the chosen
  overlay (creating it if absent) and opens it in your editor, honoring
  `$VISUAL`, then `$EDITOR`, then `nano`, with a typed argv (no shell). The
  stack is validated on return, warn-but-allow. `--print-path` prints the
  resolved path and exits for scripting; without it, a non-interactive terminal
  fails with guidance. `--compose` prints a one-line warning that an override
  can re-add capabilities, expose ports, or break wdm tracking. Editing
  `.env.user` on a stack installed before this feature offers a one-time
  migration that re-renders the compose to activate the overlay, leaving images
  and secrets unchanged.
- Added `wdm view-env <app> [--json]`, a read-only view of a stack's effective
  environment (base `.env` merged with `.env.user`) with every secret value
  masked by the active redactor. `.env.user` may hold user secrets, so its
  values are also redacted in logs, validation, and error output.
- Added `wdm apps redeploy <app> [--yes] [--stack-path <path>]`, which applies
  your overlay edits by recreating the stack from its on-disk files
  (`docker compose up -d`): it re-reads the Compose file and your
  `docker-compose.override.yml` and re-evaluates each service's `.env.user`,
  recreating only the containers whose effective config changed. Unlike
  `wdm apps restart` (plain `docker compose restart`, which reuses the running
  containers without re-reading config and so does not pick up overlay edits),
  redeploy applies them — without re-rendering templates from the catalog and
  without changing images, versions, or secrets. The TUI exposes the same
  action as "Apply overlay changes".
- Wired the `.env.user` overlay into every curated catalog app: each service in
  every stack now merges `.env.user` via `env_file`, ordered ahead of any
  generated-secret env file so the overlay can add knobs without overriding
  generated secrets.

### Fixed
- `wdm apps update vaultwarden` no longer fails closed with `placeholder
  "VAULTWARDEN_DOMAIN" is absent from the existing .env`. The vaultwarden
  template now persists `VAULTWARDEN_DOMAIN` as its own `.env` key (matching
  nextcloud, meshcentral, and stoat), so the update precheck can re-resolve it.

### Security
- Self-update refuses any release that is not strictly newer than the running
  binary, in both the check and apply paths, so a validly signed but older
  release can no longer downgrade `wdm` (from-source/unstamped builds keep the
  prior "differs" behavior).
- String placeholder values are rejected when they contain control characters
  (CR/LF/NUL) before they reach the `.env` template, so a `--set` or default
  value cannot inject extra `KEY=VALUE` lines or override a generated secret.
- Removing or uninstalling a stack deletes a compose-declared external network
  only when it carries the `wdm.managed=true` label, so an operator's
  pre-existing network adopted at install is left in place while `wdm`'s own
  networks are still removed.
- `wdm apps logs` scrubs bare secret literals from `.env` out of container log
  output, mirroring config validation, and fails closed when `.env` is present
  but unreadable.
- Catalog bundle extraction fails closed on a duplicate normalized member, so a
  verified bundle cannot pass checks on one manifest while activating another.

## v1.1.0 - 2026-06-23

### Added
- Added Mira (Miracode) to the curated catalog — a self-hosted AI code reviewer
  that runs as a GitHub App and reviews pull requests with an LLM of your
  choice. It ships a hardened two-service stack (the mira app container plus a
  pinned `postgres:18.3-alpine` backend) with `cap_drop: ALL`,
  `no-new-privileges`, an internal database network, a loopback-only
  dashboard/webhook port, and two persisted volumes. The GitHub App ID, webhook
  secret, OpenRouter key, and App private-key path are supplied at install via
  `--set`; the database and admin passwords are generated. A step-by-step setup
  guide ships with the template.
- Added a catalog `sensitive` placeholder flag for user-supplied secret values.
  A `type: string` credential provided via `--set` — which wdm does not
  generate, so it cannot be `type: secret` — is now value-redacted from logs,
  errors, and JSON and refused if it would render inline into a non-secret
  artifact, the same protection generated secrets already receive, across
  install, update, and reconfigure. The flag is gated to `type: string` at the
  schema level. Mira's webhook secret and OpenRouter key use it.
- Added `scripts/ops/provision-rootless-docker-user.sh`, an operator helper that
  creates a dedicated Linux user and installs rootless Docker for it from pinned,
  checksum-verified static releases (Docker 29.6.0, Compose v5.1.2), starts the
  user-scoped Docker service, and verifies it with `hello-world`. Supports
  `--dry-run` and an unprivileged host-capability precheck.

## v1.0.7 - 2026-06-22

### Security
- Release artifacts now include SLSA build provenance, published as
  `wdm-linux-amd64.intoto.jsonl` (an in-toto/DSSE statement). This is an
  eighth, additive release asset alongside the existing checksums, signature,
  cosign bundle, attestation, and SBOM; verification of the binary and catalog
  is unchanged and still fails closed. See SECURITY.md for the cross-check.

### Changed
- Internal cleanup with no change to user-facing behavior: consolidated the
  destructive lifecycle verbs (delete, remove, restart, stop-all, uninstall)
  onto shared core helpers, extracted a shared CLI command preamble and a
  shared plain-TUI menu writer, merged the duplicate backup path validators,
  and removed a large amount of dead code across the catalog, registry,
  release, render, and logging packages.
- Hardened project CI and governance: scoped the CodeQL `security-events`
  permission to the analyze job, targeted Dependabot updates at `dev`, and
  documented the consolidated review gate and owner-only merge policy. Added
  `GOVERNANCE.md` and `ROADMAP.md`.

## v1.0.6 - 2026-06-21

### Added
- Implemented the on-disk diagnostic log sink (PRD §24). `wdm` now writes a
  redacted JSON log to `~/.local/state/wdm/logs/latest.log` (file mode 0600,
  dir 0700). On each start the prior `latest.log` is archived as
  `wdm-YYYY-MM-DD-HHMMSS.log`, and archives are pruned to the retention
  intersection — kept only when both within 30 days and among the 50 newest
  files, with `latest.log` always kept. The sink fails soft: if it cannot be
  opened, the CLI falls back to stderr and the TUI discards (so the display is
  never corrupted), and a logging fault never blocks an operation.
- State-changing operations now emit PRD §24 normal-log content. Install logs
  the full field set (wdm version, OS and architecture, action, app, stack
  path, checks performed, command names, and the failing step on failure);
  update, reconfigure, and uninstall log start and result lines. Generated
  secrets are never logged — only the fact that a secret was minted for a
  named placeholder — and the per-install records pass through a redactor that
  also scrubs that run's generated values as defense-in-depth.
- Added the global `--debug` flag (PRD §24): it raises the log file to debug
  level with source attribution, surfacing command summaries and validation
  detail. Secrets stay redacted in debug mode.
- On operation failure, `wdm` now prints the log file path and reminds you to
  review the log before sharing it publicly (PRD §24).

## v1.0.5 - 2026-06-20

### Fixed
- Corrected the capability-hardening documentation in the catalog schema and
  type definitions: the `cap_drop: ALL` baseline is declared by each app's
  Compose template and enforced at install (a service that adds capabilities
  without it is rejected), not applied by the renderer, which injects only the
  `wdm.managed` and `wdm.app` labels. Documentation only — no behavior change.

## v1.0.4 - 2026-06-20

### Added
- Added per-app resource management: a top-level `wdm resources <app>` command
  and a "Resources" action in the app actions menu (between Restart and Remove).
  With no limit flags the command (and the screen) shows each adjustable
  service's memory, CPU, and PID limits currently in effect alongside the
  catalog's allowed bands. With `--memory`, `--cpus`, or `--pids` (and
  `--service`, `--yes`, `--stack-path`, `--json`) it changes the selected
  service's limits: `wdm` validates the request against the catalog bands, backs
  up the config, rewrites only the resource variables in the stack's `.env`
  in place (every secret, derived value, and unrelated line is preserved
  byte-for-byte; the Compose file is left unchanged), re-checks the on-disk
  Compose against the catalog policy, validates it fail-closed, and recreates
  the container (a brief downtime). Limits left unset are kept as-is; an
  explicit empty or zero value is rejected, and an out-of-band value is
  reported with the allowed range. `--service` defaults to the app's primary
  (first adjustable) service.
  The new limits live in the `.env`, so they survive catalog updates, and no new
  stack or override file is created.
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
  `docker compose down` never removes them), then sweeps every remaining network
  carrying the `wdm.managed=true` label — including ones orphaned by an app you
  deleted earlier, whose compose file is gone — so a self-uninstall leaves no
  labeled networks behind. The sweep is best-effort: a network already gone
  counts as removed, one that cannot be removed is reported with the exact
  `docker network rm <name>` command to run manually, and a daemon problem
  listing the networks degrades gracefully without aborting the uninstall.
  Networks created before label support (pre-`wdm.managed=true`) are not matched
  by the sweep and must be removed manually. Named volumes and every
  `~/docker/<app>/` stack directory are kept;
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
