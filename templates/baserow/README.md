# Baserow

<p align="center">
  <a href="https://baserow.io/">Website</a> •
  <a href="https://baserow.io/docs">Documentation</a> •
  <a href="https://github.com/bram2w/baserow">GitHub</a> •
  <a href="https://community.baserow.io/">Community</a>
</p>

---

[Baserow](https://baserow.io/) is an open-source, no-code database platform — a self-hosted alternative to Airtable. Create, manage, and collaborate on databases through a spreadsheet-like UI, or drive them through the REST API.

## Features

- **No-Code Database** — Build databases without writing code
- **Real-Time Collaboration** — Work together with your team
- **REST API** — Full API access for developers
- **Plugins & Extensions** — Extend functionality as needed
- **Self-Hosted** — Full control over your data
- **Role-Based Access** — Fine-grained permissions

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `baserow-front` network plus `baserow-db` (`--internal`, no internet egress), generates the four secrets (`POSTGRES_PASSWORD`, `POSTGRES_NON_ROOT_PASSWORD`, `BASEROW_JWT_SIGNING_KEY`, `REDIS_PASSWORD`, `SECRET_KEY`) via `crypto/rand`, derives `BASEROW_PUBLIC_URL` from `BASEROW_DOMAIN` (no trailing slash), and runs `docker compose up -d` at install time. The secrets are marked `regenerable=false` — rotating them post-install would require ALTER USER orchestration (the Postgres passwords), log every user out (`BASEROW_JWT_SIGNING_KEY`), or break the internal Redis (`REDIS_PASSWORD`), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, navigate to your configured domain and create an account. The first boot syncs ~156 built-in templates and can take several minutes before the UI is ready. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_USER` | Postgres superuser (used only for DB init) | Yes |
| `POSTGRES_PASSWORD` | Postgres superuser password | Yes |
| `POSTGRES_DB` | Database name (default: `baserow`) | Yes |
| `POSTGRES_NON_ROOT_USER` | Baserow's DB user (least privilege) | Yes |
| `POSTGRES_NON_ROOT_PASSWORD` | Baserow DB user password | Yes |
| `BASEROW_JWT_SIGNING_KEY` | Signs user-session JWTs | Yes |
| `SECRET_KEY` | Django secret for CSRF / signed cookies | Yes |
| `REDIS_PASSWORD` | Internal Redis password | Yes |
| `BASEROW_PUBLIC_URL` | Full external URL (no trailing slash) | Yes |
| `PUID` / `PGID` | Host UID/GID that owns `/baserow/data` | Yes |
| `TZ` | Container timezone | No (default: host zone) |

### Reverse Proxy (Caddy)

The proxy `Host` header **must** match `BASEROW_PUBLIC_URL` — Baserow's internal Caddy uses host-based routing.

```
baserow.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:8086 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8086 | HTTP | Web interface (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `db_storage` (named volume) | PostgreSQL data (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `baserow_data` (named volume) | Baserow app data: uploads, plugins, internal Redis dump, Caddy state (`/baserow/data`) |

Both are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and the baserow image runs its entrypoint as container root,
which chowns `/baserow/data` to `PUID:PGID` on first boot. wdm never runs
`docker compose down -v`, so removing the stack **preserves** both volumes —
they are listed in the removal summary (as `wdm-baserow_db_storage` and
`wdm-baserow_baserow_data`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 5 init caps; baserow adds NET_BIND_SERVICE for Caddy port 80 + CHOWN/SETUID/SETGID/DAC_OVERRIDE for the entrypoint chown) | No NET/SYS caps beyond NET_BIND_SERVICE |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids` 400 (pg) / 500 (baserow) | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the other |
| Two-network split | `baserow-db` created with `--internal` | Postgres has no internet egress |
| Port exposure | `127.0.0.1:8086` only | Only the reverse proxy can reach Baserow |
| DB user | `POSTGRES_NON_ROOT_USER` (created by `init-data.sh`) | Baserow never has Postgres superuser |
| Postgres auth | `SCRAM-SHA-256` (`POSTGRES_HOST_AUTH_METHOD`) | Stronger than the default md5 |
| Healthchecks | Built-in image scripts only | No credentials on the command line |
| Process model | s6-overlay drops workers to the `baserow` / `redis` users | Long-running processes are non-root |

> **First boot is slow.** Baserow syncs ~156 built-in templates on its first
> start, so the container can take several minutes to report healthy. The
> compose healthcheck uses a 300s `start_period` to account for this.

> **Postgres image upgrade (optional):** swap `postgres:18.4` for
> `dhi.io/postgres:18` (Docker Hardened Images) for a distroless base with
> faster CVE patches. Requires a DHI subscription. Same env vars and PGDATA
> layout — drop-in compatible.

## Support the Project

- ☁️ [Baserow Cloud](https://baserow.io/pricing) — Managed hosting
- ⭐ [Star on GitHub](https://github.com/bram2w/baserow)
- 💬 [Join Community](https://community.baserow.io/)
- 📖 [Documentation](https://baserow.io/docs)

## License

Baserow is released under the [MIT License](https://github.com/bram2w/baserow/blob/develop/LICENSE).
