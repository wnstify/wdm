# Authentik

<p align="center">
  <a href="https://goauthentik.io/">Website</a> •
  <a href="https://docs.goauthentik.io/">Documentation</a> •
  <a href="https://github.com/goauthentik/authentik">GitHub</a> •
  <a href="https://github.com/goauthentik/authentik/discussions">Community</a>
</p>

---

[Authentik](https://goauthentik.io/) is an open-source identity provider — a self-hosted alternative to Okta, Auth0, and Keycloak. It centralizes authentication for your applications with support for SAML, OAuth2/OIDC, LDAP, SCIM, and forward-auth (proxy) flows, a visual flow builder, and a full admin UI.

## Features

- **Single Sign-On** — One login for all your applications
- **Protocols** — SAML 2.0, OAuth2 / OpenID Connect, LDAP, SCIM, forward-auth
- **Flow Builder** — Customize login, enrollment, and recovery flows visually
- **Multi-Factor Auth** — TOTP, WebAuthn, SMS, and Duo
- **Self-Hosted** — Full control over your identity data
- **Policies & Outposts** — Fine-grained access control and proxy providers

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `authentik-front` network plus `authentik-db` (`--internal`, no internet egress), generates the three secrets (`POSTGRES_PASSWORD`, `POSTGRES_NON_ROOT_PASSWORD`, `AUTHENTIK_SECRET_KEY`) via `crypto/rand`, and runs `docker compose up -d` at install time. The secrets are marked `regenerable=false` — rotating them post-install would require ALTER USER orchestration (the Postgres passwords) or invalidate every session (`AUTHENTIK_SECRET_KEY`), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, complete the **initial-setup flow** (see below) to create your `akadmin` administrator account. The stack is three services — a Postgres backend, the authentik **server** (web UI + API), and the authentik **worker** (background tasks) — and the first boot runs database migrations and warms both processes, so it can take roughly 60–90 seconds before the server reports healthy. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

> **No Redis.** Authentik 2026.5.3 removed the Redis dependency (dropped in
> 2025.10), so this stack runs without a Redis service or any
> `AUTHENTIK_REDIS__*` configuration.

## First-run setup

Authentik does not ship a default password. On first boot, open the
initial-setup flow through your reverse proxy and set the `akadmin` password:

```
https://auth.example.com/if/flow/initial-setup/
```

Set a strong password for the `akadmin` account, then sign in at
`https://auth.example.com/`. From there you can configure providers,
applications, and additional users in the admin interface.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_USER` | Postgres superuser (used only for DB init) | Yes |
| `POSTGRES_PASSWORD` | Postgres superuser password | Yes |
| `POSTGRES_DB` | Database name (default: `authentik`) | Yes |
| `POSTGRES_NON_ROOT_USER` | Authentik's DB user (least privilege) | Yes |
| `POSTGRES_NON_ROOT_PASSWORD` | Authentik DB user password | Yes |
| `AUTHENTIK_SECRET_KEY` | Signs sessions and tokens (server + worker) | Yes |
| `AUTHENTIK_POSTGRESQL__HOST` | Postgres service name (`postgres`) | Yes |
| `TZ` | Container timezone | No (default: host zone) |

Authentik derives its external URL from the reverse proxy's forwarded headers,
so no domain environment variable is required.

### Reverse Proxy (Caddy)

Forward the standard proxy headers so authentik can build correct absolute URLs:

```
auth.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:9000 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 9000 | HTTP | Web interface + API (bound to `127.0.0.1`) |

Only HTTP/9000 is exposed. Authentik also offers an HTTPS listener on 9443, but
TLS terminates at your reverse proxy, so this template binds 9000 only — the
root UI and the `/-/health/live/` and `/-/health/ready/` endpoints are all
served over 9000.

## Data Persistence

| Storage | Description |
|------|-------------|
| `db_storage` (named volume) | PostgreSQL data (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `authentik_media` (named volume) | Uploaded icons, flow backgrounds, generated reports (`/media`) — shared by the server and worker |
| `authentik_templates` (named volume) | Custom email / flow templates (`/templates`) — shared by the server and worker |
| `authentik_certs` (named volume) | External certificates the worker imports (`/certs`) — worker only |

All are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and the authentik image runs as the unprivileged UID 1000
with no chowning entrypoint, so a root-owned host bind would break its writes.
Docker seeds the named volumes UID-1000-writable from the image. wdm never runs
`docker compose down -v`, so removing the stack **preserves** every volume —
they are listed in the removal summary (as `wdm-authentik_db_storage`,
`wdm-authentik_authentik_media`, `wdm-authentik_authentik_templates`, and
`wdm-authentik_authentik_certs`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 5 init caps; server + worker add NONE) | The server and worker run with zero capabilities (`CapEff: 0`) |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| Docker socket | No `/var/run/docker.sock` mount anywhere | The embedded Docker outpost is out of scope; the worker holds no socket |
| Run-as user | Server + worker run as the image default UID 1000 | No `user: root`, even for the worker |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids` 400 (pg) / 600 (server) / 400 (worker) | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the others |
| Two-network split | `authentik-db` created with `--internal` | Postgres has no internet egress |
| Port exposure | `127.0.0.1:9000` only | Only the reverse proxy can reach authentik |
| DB user | `POSTGRES_NON_ROOT_USER` (created by `init-data.sh`) | Authentik never has Postgres superuser |
| Postgres auth | `SCRAM-SHA-256` (`POSTGRES_HOST_AUTH_METHOD`) | Stronger than the default md5 |
| Healthchecks | `ak healthcheck` (built-in command) | No credentials on the command line |

> **First boot is slow.** Authentik runs database migrations and warms both the
> server and worker on first start, so the stack can take roughly 60–90 seconds
> before the server reports healthy. The compose healthcheck uses a 120s
> `start_period` to account for this. The worker may log a brief, harmless
> "relation does not exist" until the server's migrations finish — this
> self-resolves and needs no action.

> **Scaling tuning (optional):** heavy installs with large flows or many
> concurrent logins can benefit from a larger `/dev/shm`. Add
> `shm_size: 256m` to the `server` (and `worker`) service if you observe
> shared-memory pressure under load — it is not required for a normal install.

> **Postgres image upgrade (optional):** swap `postgres:18.4` for
> `dhi.io/postgres:18` (Docker Hardened Images) for a distroless base with
> faster CVE patches. Requires a DHI subscription. Same env vars and PGDATA
> layout — drop-in compatible.

## Support the Project

- ☁️ [Authentik Cloud / Enterprise](https://goauthentik.io/pricing/) — Managed and supported hosting
- ⭐ [Star on GitHub](https://github.com/goauthentik/authentik)
- 💬 [Join the Discussion](https://github.com/goauthentik/authentik/discussions)
- 📖 [Documentation](https://docs.goauthentik.io/)

## License

Authentik is released under the [MIT License](https://github.com/goauthentik/authentik/blob/main/LICENSE) (Enterprise features under a separate license).
