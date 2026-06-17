# Nextcloud

<p align="center">
  <a href="https://nextcloud.com/">Website</a> •
  <a href="https://docs.nextcloud.com/">Documentation</a> •
  <a href="https://github.com/nextcloud/server">GitHub</a> •
  <a href="https://help.nextcloud.com/">Community</a>
</p>

---

[Nextcloud](https://nextcloud.com/) is an open-source content-collaboration platform — a self-hosted alternative to Google Drive, Dropbox, and Microsoft 365. Store and sync files across devices, share with links and groups, and extend it with calendar, contacts, notes, and hundreds of community apps.

## Features

- **File Sync & Share** — Desktop, mobile, and web clients keep files in sync
- **Collaboration** — Shared folders, link shares, comments, and versioning
- **Groupware** — Calendar, contacts, mail, and tasks via first-party apps
- **App Store** — Hundreds of community apps extend the platform
- **Self-Hosted** — Full control over your files and metadata
- **End-to-End Encryption** — Optional client-side encryption for sensitive data

## Prerequisites

- Docker and Docker Compose
- External Docker network
- Reverse proxy (Caddy, Nginx, Traefik)

## Installation

This template is managed by **wdm** — wdm renders `docker-compose.yml.tmpl` and `.env.tmpl` into your stack directory, pre-creates the `nextcloud-front` network plus `nextcloud-db` (`--internal`, no internet egress), generates the four secrets (`POSTGRES_PASSWORD`, `POSTGRES_NON_ROOT_PASSWORD`, `REDIS_PASSWORD`, `NEXTCLOUD_ADMIN_PASSWORD`) via `crypto/rand`, writes the `init-data.sh` and `nginx.conf` sidecars verbatim, and runs `docker compose up -d` at install time. The secrets are marked `regenerable=false` — rotating them post-install would require ALTER USER orchestration (the Postgres passwords), a coordinated restart (`REDIS_PASSWORD`), or would not change the already-installed account (`NEXTCLOUD_ADMIN_PASSWORD`), so wdm reuses the existing values via `state.ReadStackEnv` on every update. See the wdm documentation for the CLI surface; do not edit the rendered `.env` or `docker-compose.yml` by hand, since wdm regenerates them on update.

The stack is five services — a Postgres backend, a Redis cache, the Nextcloud **app** (PHP-FPM), an **nginx** front, and a **cron** sidecar that runs background jobs. The first boot rsyncs the application into the data volume and runs the `occ maintenance:install` auto-setup, so it typically takes **two minutes or less** before the app reports healthy. The reference content below describes the rendered stack (configuration knobs, security baseline, data layout) for troubleshooting and catalog-template work.

> **Admin login.** Nextcloud auto-creates the administrator account on first
> boot from `NEXTCLOUD_ADMIN_USER` (default `admin`) and a generated
> `NEXTCLOUD_ADMIN_PASSWORD`. That password is stored in plaintext in
> `~/docker/nextcloud/.env` (mode 0600) because the install command needs it
> directly — read it from that file, sign in, and change it in the UI. wdm also
> surfaces it once on the install finish screen.

## First-run setup

After install, open Nextcloud through your reverse proxy and sign in with the
admin credentials from `~/docker/nextcloud/.env`:

```
NEXTCLOUD_ADMIN_USER=admin
NEXTCLOUD_ADMIN_PASSWORD=<generated value>
```

Change the password in **Settings → Security** after the first login, then add
users, enable apps, and configure external storage as needed.

## Configuration

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `POSTGRES_USER` | Postgres superuser (used only for DB init) | Yes |
| `POSTGRES_PASSWORD` | Postgres superuser password | Yes |
| `POSTGRES_DB` | Database name (default: `nextcloud`) | Yes |
| `POSTGRES_NON_ROOT_USER` | Nextcloud's DB user (least privilege, owns the DB) | Yes |
| `POSTGRES_NON_ROOT_PASSWORD` | Nextcloud DB user password | Yes |
| `REDIS_PASSWORD` | Redis cache / file-lock password | Yes |
| `NEXTCLOUD_ADMIN_USER` | Admin login name (default: `admin`) | Yes |
| `NEXTCLOUD_ADMIN_PASSWORD` | Generated admin password (plaintext in `.env`) | Yes |
| `NEXTCLOUD_DOMAIN` | Public hostname (added to trusted domains) | Yes |
| `TZ` | Container timezone | No (default: host zone) |

The app's `NEXTCLOUD_TRUSTED_DOMAINS` is rendered as a space-separated list of
`127.0.0.1` (so the localhost healthcheck reaches `/status.php`) and your
`NEXTCLOUD_DOMAIN` (what the reverse proxy forwards).

### Reverse Proxy (Caddy)

Forward the standard proxy headers so Nextcloud builds correct absolute URLs.
The `Host` header must match `NEXTCLOUD_DOMAIN`:

```
nextcloud.example.com {
    encode zstd gzip
    reverse_proxy http://127.0.0.1:8081 {
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

## Ports

| Port | Service | Description |
|------|---------|-------------|
| 8081 | HTTP | Web interface via nginx (bound to `127.0.0.1`) |

Only the nginx front publishes a port (`127.0.0.1:8081` → container `8080`). The
app's PHP-FPM listener (`9000`) and Redis (`6379`) stay on the internal-only
`nextcloud-db` network and are never exposed to the host.

## Data Persistence

| Storage | Description |
|------|-------------|
| `pg_data` (named volume) | PostgreSQL data (`/var/lib/postgresql/data`) — postgres self-chowns it on init |
| `redis_data` (named volume) | Redis persistence: cache + file locks (`/data`) |
| `nc_data` (named volume) | Nextcloud application + user data (`/var/www/html`) — shared read-only with nginx and read-write with cron |

All are **named volumes**, not host bind mounts: wdm does not pre-create host
bind directories. The Nextcloud image entrypoint runs as container root and
populates `/var/www/html` on first boot. wdm never runs `docker compose down -v`,
so removing the stack **preserves** every volume — they are listed in the
removal summary (as `wdm-nextcloud_pg_data`, `wdm-nextcloud_redis_data`, and
`wdm-nextcloud_nc_data`).

## Security Features

This template ships with a hardened default configuration:

| Layer | Setting | Effect |
|---|---|---|
| Capabilities | `cap_drop: ALL` (postgres, app, cron add the 5 init caps; redis + nginx add NONE) | No NET/SYS caps anywhere; redis + nginx run zero-cap |
| Privileges | `security_opt: no-new-privileges` on all containers | Setuid binaries cannot gain caps |
| Run-as user | redis runs as `redis`, nginx as `nginx` (image users) | Those services never run as root |
| IPC | `ipc: private` on all containers | Isolated SysV/POSIX IPC namespace |
| Process budget | per-container `pids` limits | Caps fork sprawl |
| Memory / CPU | Per-container limits | One service can't starve the others |
| Two-network split | `nextcloud-db` created with `--internal` | Postgres + Redis have no internet egress |
| Port exposure | `127.0.0.1:8081` only | Only the reverse proxy can reach Nextcloud |
| DB user | `POSTGRES_NON_ROOT_USER` (created by `init-data.sh`, owns the DB) | Nextcloud never has Postgres superuser |
| Postgres auth | `SCRAM-SHA-256` (`POSTGRES_HOST_AUTH_METHOD`) | Stronger than the default md5 |
| Redis auth | `--requirepass` | Cache + file locks are password-protected |
| Healthchecks | FastCGI socket probe / `wget /status.php` | No credentials on the command line |

> **Postgres ≤ 17 constraint.** Nextcloud 31 certifies PostgreSQL up to major
> 17, so this template pins `postgres:17-alpine`. Do **not** bump to Postgres
> 18 until Nextcloud certifies it — an unsupported server version risks
> migration failures.

> **First boot.** The app rsyncs Nextcloud into the data volume and runs the
> `occ maintenance:install` auto-setup on first start, so the container can take
> up to about two minutes to report healthy. The compose healthcheck uses a
> 180s `start_period` to account for this.

## Support the Project

- ☁️ [Nextcloud Hub](https://nextcloud.com/sign-up/) — Hosted and enterprise options
- ⭐ [Star on GitHub](https://github.com/nextcloud/server)
- 💬 [Community Forum](https://help.nextcloud.com/)
- 📖 [Documentation](https://docs.nextcloud.com/)

## License

Nextcloud is released under the [AGPL-3.0 License](https://github.com/nextcloud/server/blob/master/COPYING).
