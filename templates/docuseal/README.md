# DocuSeal

<p align="center">
  <a href="https://www.docuseal.com/">Website</a> •
  <a href="https://www.docuseal.com/docs">Documentation</a> •
  <a href="https://github.com/docusealco/docuseal">GitHub</a> •
  <a href="https://discord.gg/qygYCDGck9">Community</a>
</p>

---

[DocuSeal](https://www.docuseal.com/) is an open-source document-signing platform — a self-hosted alternative to DocuSign. Build PDF forms, send documents for signature, and collect legally binding e-signatures, all under your own control.

## Features

- **Document Signing** — Collect legally binding e-signatures
- **PDF Form Builder** — Create fillable forms with a WYSIWYG editor
- **13 Field Types** — Signature, date, file, checkbox, and more
- **Multiple Submitters** — Route a document through several signers
- **Email & SMS Delivery** — Send signing requests directly to recipients
- **Self-Hosted** — Full control over your documents and data
- **REST API & Webhooks** — Integrate signing into your own workflows

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `docuseal-front` network plus `docuseal-db` (`--internal`, no internet egress), generates the three secrets (`POSTGRES_PASSWORD`, `POSTGRES_NON_ROOT_PASSWORD`, `SECRET_KEY_BASE`) via `crypto/rand`, derives `HOST` / `FORCE_SSL` from `DOCUSEAL_DOMAIN` (bare hostname), and runs `docker compose up -d` at install time. The secrets are marked `regenerable=false` — rotating them post-install would require ALTER USER orchestration (the Postgres passwords) or invalidate every signed cookie/session (`SECRET_KEY_BASE`), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

After install, navigate to your configured domain and create your administrator account on first load. The first boot runs Rails database migrations and warms the bundled Redis/Sidekiq, so the container can take a minute before the UI is ready. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_USER` | Postgres superuser (used only for DB init) | Yes |
| `POSTGRES_PASSWORD` | Postgres superuser password | Yes |
| `POSTGRES_DB` | Database name (default: `docuseal`) | Yes |
| `POSTGRES_NON_ROOT_USER` | DocuSeal's DB user (least privilege) | Yes |
| `POSTGRES_NON_ROOT_PASSWORD` | DocuSeal DB user password | Yes |
| `DATABASE_URL` | Postgres connection URL (derived from the above) | Yes |
| `SECRET_KEY_BASE` | Rails session/cookie signing secret | Yes |
| `HOST` | External hostname for absolute URLs | Yes |
| `FORCE_SSL` | Hostname enforcing HTTPS + secure cookies | Yes |
| `TZ` | Container timezone | No (default: host zone) |

`HOST` and `FORCE_SSL` are both bound to the single `DOCUSEAL_DOMAIN` input the
installer supplies. `FORCE_SSL` takes the public hostname (not a bare `true`):
DocuSeal then emits secure cookies and redirects HTTP to HTTPS, which is what a
reverse-proxy-fronted, TLS-at-proxy deployment needs.

### Reverse Proxy (Caddy)

TLS terminates at the reverse proxy; DocuSeal runs HTTP behind it on
`127.0.0.1:3000`. A long read timeout keeps large-document signing sessions
from being cut off.

```
sign.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:3000 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 3000 | HTTP | Web interface (bound to `127.0.0.1`) |

## Data Persistence

| Storage | Description |
|------|-------------|
| `db_storage` (named volume) | PostgreSQL data (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `docuseal_data` (named volume) | DocuSeal app data: uploaded documents, generated PDFs, disk-service state (`/data/docuseal`) |

Both are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories, and a host bind would be created root-owned by Docker and
break the app's writes. wdm never runs `docker compose down -v`, so removing the
stack **preserves** both volumes — they are listed in the removal summary (as
`wdm-docuseal_db_storage` and `wdm-docuseal_docuseal_data`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres adds 5 init caps; docuseal adds none) | DocuSeal runs with zero added capabilities |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | `pids` 400 (pg) / 512 (docuseal) | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the other |
| Two-network split | `docuseal-db` created with `--internal` | Postgres has no internet egress |
| Port exposure | `127.0.0.1:3000` only | Only the reverse proxy can reach DocuSeal |
| DB user | `POSTGRES_NON_ROOT_USER` (created by `init-data.sh`) | DocuSeal never has Postgres superuser |
| Postgres auth | `SCRAM-SHA-256` (`POSTGRES_HOST_AUTH_METHOD`) | Stronger than the default md5 |
| Healthchecks | `pg_isready` / Rails `/up` endpoint | No credentials on the command line |
| Secure cookies | `FORCE_SSL` set to the public hostname | HTTPS-only cookies, HTTP redirected |

> **All-in-one container.** The DocuSeal image bundles the Rails web app, a
> Redis instance, and the Sidekiq background-job worker in a single container —
> no separate Redis service is required. wdm points it at an external Postgres
> via `DATABASE_URL` instead of the image's default SQLite.

> **First boot runs migrations.** DocuSeal runs Rails database migrations and
> warms the bundled Redis/Sidekiq on first start, so the container can take
> ~40-60s to report healthy. The compose healthcheck uses a 120s `start_period`
> to account for this.

> **Postgres image upgrade (optional):** swap `postgres:18.4` for
> `dhi.io/postgres:18` (Docker Hardened Images) for a distroless base with
> faster CVE patches. Requires a DHI subscription. Same env vars and PGDATA
> layout — drop-in compatible.

## Support the Project

- ☁️ [DocuSeal Cloud](https://www.docuseal.com/pricing) — Managed hosting
- ⭐ [Star on GitHub](https://github.com/docusealco/docuseal)
- 💬 [Join Discord](https://discord.gg/qygYCDGck9)
- 📖 [Documentation](https://www.docuseal.com/docs)

## License

DocuSeal is released under the [AGPL-3.0 License](https://github.com/docusealco/docuseal/blob/master/LICENSE).
