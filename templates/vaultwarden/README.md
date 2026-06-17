# Vaultwarden

<p align="center">
  <a href="https://github.com/dani-garcia/vaultwarden">GitHub</a> •
  <a href="https://github.com/dani-garcia/vaultwarden/wiki">Documentation</a> •
  <a href="https://vaultwarden.discourse.group/">Community</a> •
  <a href="https://bitwarden.com/">Bitwarden Clients</a>
</p>

---

[Vaultwarden](https://github.com/dani-garcia/vaultwarden) is an unofficial, lightweight Bitwarden-compatible server written in Rust — a self-hosted password manager that works with the official Bitwarden browser extensions, desktop apps, and mobile apps. It implements the Bitwarden API at a fraction of the resource footprint of the official server.

## Features

- **Bitwarden-Compatible** — Works with the official Bitwarden clients
- **Lightweight** — A single Rust binary; idles around 10 MiB of RAM
- **Vaults & Organizations** — Personal vaults plus shared organization vaults
- **Two-Factor Auth** — TOTP, WebAuthn/U2F, email, and Duo
- **Attachments & Sends** — Encrypted file attachments and time-limited Sends
- **Admin Panel** — A built-in `/admin` page for user and server management
- **Self-Hosted** — Full control over your password vault and its data

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `vaultwarden-front` network plus `vaultwarden-db` (`--internal`, no internet egress), generates the secrets (`POSTGRES_PASSWORD`, `POSTGRES_NON_ROOT_PASSWORD`, `ADMIN_TOKEN`) via `crypto/rand`, derives `DOMAIN` from `VAULTWARDEN_DOMAIN` (full URL with scheme, no trailing slash), and runs `docker compose up -d` at install time. The secrets are marked `regenerable=false` — rotating them post-install would require ALTER USER orchestration (the Postgres passwords) or lock you out of the `/admin` panel (`ADMIN_TOKEN`), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

> **The admin token is shown once.** `ADMIN_TOKEN` is an `argon2id` PHC hash, not a plaintext token: wdm generates a strong random plaintext, stores only the one-way hash in the `.env`, and prints the plaintext **once** on the install finish screen. It cannot be recovered afterwards — record it before you leave that screen. You use the plaintext to sign in at `https://<your-domain>/admin`. If you lose it, re-run an install/regeneration to mint a new one.

After install, navigate to your configured domain through your reverse proxy and create your account on first load. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_USER` | Postgres superuser (used only for DB init) | Yes |
| `POSTGRES_PASSWORD` | Postgres superuser password | Yes |
| `POSTGRES_DB` | Database name (default: `vaultwarden`) | Yes |
| `POSTGRES_NON_ROOT_USER` | Vaultwarden's DB user (least privilege) | Yes |
| `POSTGRES_NON_ROOT_PASSWORD` | Vaultwarden DB user password | Yes |
| `DATABASE_URL` | Postgres connection URL (derived from the above) | Yes |
| `ADMIN_TOKEN` | argon2id PHC gating the `/admin` panel | Yes |
| `DOMAIN` | Full external URL with scheme (no trailing slash) | Yes |
| `ROCKET_PORT` | Internal listen port (default: `8080`) | Yes |
| `TZ` | Container timezone | No (default: host zone) |

`DOMAIN` is the full external URL **with scheme** (`https://vault.example.com`),
derived from the single `VAULTWARDEN_DOMAIN` input the installer supplies. It is
required for WebAuthn/U2F two-factor, push notifications, and the links
vaultwarden emits — no trailing slash.

### Reverse Proxy (Caddy)

TLS terminates at the reverse proxy; vaultwarden runs HTTP behind it on
`127.0.0.1:8080`.

```
vault.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:8080 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8080 | HTTP | Web interface (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `db_storage` (named volume) | PostgreSQL data (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `vaultwarden_data` (named volume) | Vaultwarden app data: RSA keys, attachments, icon cache, Sends (`/data`) |

Both are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and a host bind would be created root-owned by Docker and
break the app's writes. wdm never runs `docker compose down -v`, so removing the
stack **preserves** both volumes — they are listed in the removal summary (as
`wdm-vaultwarden_db_storage` and `wdm-vaultwarden_vaultwarden_data`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 5 init caps; vaultwarden adds none) | Vaultwarden runs with zero added capabilities |
| Immutable rootfs | `read_only: true` on vaultwarden (only `/tmp` tmpfs + `/data` volume writable) | A compromised process cannot tamper with the container's binaries or config |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids` 400 (pg) / 200 (vaultwarden) | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the other |
| Two-network split | `vaultwarden-db` created with `--internal` | Postgres has no internet egress |
| Port exposure | `127.0.0.1:8080` only | Only the reverse proxy can reach Vaultwarden |
| DB user | `POSTGRES_NON_ROOT_USER` (created by `init-data.sh`) | Vaultwarden never has Postgres superuser |
| Postgres auth | `SCRAM-SHA-256` (`POSTGRES_HOST_AUTH_METHOD`) | Stronger than the default md5 |
| Admin credential | `ADMIN_TOKEN` stored as an `argon2id` PHC hash | The plaintext is never persisted; only the one-way hash lives in the `.env` |
| Healthchecks | `pg_isready` / the image's `/healthcheck.sh` | No credentials on the command line |

> **External Postgres, not SQLite.** Vaultwarden defaults to a bundled SQLite
> database. wdm points it at the dedicated `postgres:18.4` backend via
> `DATABASE_URL` instead, with a least-privilege non-root user created by
> `init-data.sh`.

> **The `/admin` panel is token-gated.** Reach it at
> `https://<your-domain>/admin` and sign in with the `ADMIN_TOKEN` plaintext
> shown once at install. From there you can manage users, invitations, and
> server settings. The token gates the panel only — it is not your vault
> password.

> **Postgres image upgrade (optional):** swap `postgres:18.4` for
> `dhi.io/postgres:18` (Docker Hardened Images) for a distroless base with
> faster CVE patches. Requires a DHI subscription. Same env vars and PGDATA
> layout — drop-in compatible.

## Support the Project

- ⭐ [Star on GitHub](https://github.com/dani-garcia/vaultwarden)
- 💬 [Join the Community](https://vaultwarden.discourse.group/)
- 📖 [Documentation Wiki](https://github.com/dani-garcia/vaultwarden/wiki)
- 🐝 [Bitwarden Clients](https://bitwarden.com/download/)

## License

Vaultwarden is released under the [AGPL-3.0 License](https://github.com/dani-garcia/vaultwarden/blob/main/LICENSE.txt).
